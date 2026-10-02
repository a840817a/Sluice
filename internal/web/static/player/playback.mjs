// A channel's playback lifetime. Shaka and the media element are the production
// media adapter; tests supply the same operations with controlled completions.
// The page receives events, not recovery flags, and never schedules media work.
export const LIVE_BEHIND_THRESHOLD = 10;

const LIVE_STREAMING_CFG = {
  bufferingGoal: 90, rebufferingGoal: 2, bufferBehind: 90,
  stallEnabled: true, stallThreshold: 1, stallSkip: 0.1,
  liveSync: {
    enabled: true, targetLatency: 5, targetLatencyTolerance: 1,
    maxPlaybackRate: 1.1, minPlaybackRate: 0.9,
  },
};
const VOD_STREAMING_CFG = {
  bufferingGoal: 30, rebufferingGoal: 10, liveSync: { enabled: false },
};
const STALL_RECOVERY_MSG = '播放卡住，正在嘗試恢復…';
const AV_RESYNC_DEFER_MS = 400;
const disposedError = new Error('Playback disposed');

// source resolves {manifestURL, drmServers}. context reports presentation-only
// facts (dragging and whether the video is hidden, including PiP). clock is an
// internal seam for tests; its defaults are the browser clock.
export function createPlayback({ player, video, source, onEvent,
  context = { dragging: () => false, hidden: () => false },
  clock = {
    now: () => Date.now(),
    setTimeout: (fn, delay) => setTimeout(fn, delay),
    clearTimeout: id => clearTimeout(id),
    setInterval: (fn, delay) => setInterval(fn, delay),
    clearInterval: id => clearInterval(id),
  },
}) {
  let disposed = false;
  let starting = false;
  let reconnecting = false;
  let live = false;
  let ready = false;
  let failed = false;
  let startPromise = null;
  let sourceInfo = null;
  let recoveryTimer = null;
  let recoveryAttempts = 0;
  let progressTimer = null;
  let resyncTimer = null;
  let resyncNudgeTimer = null;
  let resyncPending = false;
  let initialResyncCleanup = null;
  let waitingSince = null;
  let mediaWaiting = false;
  let buffering = false;
  let lastPlaybackTime = 0;
  let stallRecovering = false;
  const timeouts = new Set();
  const intervals = new Set();
  const sleeps = new Map();
  const listeners = new Set();

  function emit(type, detail = {}) {
    if (!disposed) onEvent({ type, ...detail });
  }
  function state() {
    return {
      live, ready,
      phase: disposed ? 'disposed' : failed ? 'failed' : reconnecting ? 'recovering' : starting ? 'starting' : ready ? 'ready' : 'idle',
      busy: !disposed && !failed && !!(mediaWaiting || buffering || waitingSince !== null)
        && !video.paused && !video.ended && !context.dragging(),
    };
  }
  function publishState() { emit('state', { state: state() }); }
  function checkAlive() { if (disposed) throw disposedError; }
  function after(fn, delay) {
    if (disposed) return null;
    const id = clock.setTimeout(() => {
      timeouts.delete(id);
      if (!disposed) return fn();
    }, delay);
    timeouts.add(id);
    return id;
  }
  function cancelTimeout(id) {
    if (id === null) return;
    clock.clearTimeout(id);
    timeouts.delete(id);
  }
  function every(fn, delay) {
    if (disposed) return null;
    const id = clock.setInterval(() => { if (!disposed) fn(); }, delay);
    intervals.add(id);
    return id;
  }
  function cancelInterval(id) {
    if (id === null) return;
    clock.clearInterval(id);
    intervals.delete(id);
  }
  function sleep(delay) {
    return new Promise(resolve => {
      const id = after(() => { sleeps.delete(id); resolve(); }, delay);
      sleeps.set(id, resolve);
    });
  }
  function listen(target, type, fn, options) {
    const remove = () => {
      target.removeEventListener(type, guarded, options);
      listeners.delete(remove);
    };
    const guarded = event => {
      if (options?.once) remove();
      if (!disposed) fn(event);
    };
    target.addEventListener(type, guarded, options);
    listeners.add(remove);
    return remove;
  }

  // startTime goes to load itself. Seeking only after load loses to Shaka's
  // metadata-time start position and makes VOD recovery restart at zero.
  async function loadWithRetry(startTime = null) {
    const start = startTime !== null && Number.isFinite(startTime) ? Math.max(0, startTime) : undefined;
    let delay = 1000;
    for (let i = 0; i < 5; i++) {
      checkAlive();
      try {
        await player.load(sourceInfo.manifestURL, start);
        checkAlive();
        return;
      } catch (err) {
        checkAlive();
        if (i === 4) throw err;
        console.warn(`[shaka load retry ${i + 1}/5]`, err.message || err);
        await sleep(delay);
        checkAlive();
        delay = Math.min(delay * 2, 15000);
      }
    }
  }
  function clearRecovery() {
    cancelTimeout(recoveryTimer);
    recoveryTimer = null;
  }
  function resetRecovery() { clearRecovery(); recoveryAttempts = 0; }
  function clearWaiting() {
    waitingSince = null;
    mediaWaiting = false;
    publishState();
  }
  function clearBusy() { buffering = false; clearWaiting(); }
  function fail(message) {
    failed = true;
    buffering = false;
    clearWaiting();
    resetRecovery();
    stallRecovering = false;
    emit('error', { message });
  }
  function markWaiting() {
    if (!video.paused && !context.dragging() && !reconnecting && !stallRecovering) {
      if (waitingSince === null) waitingSince = clock.now();
    }
    publishState();
  }
  function playbackReady() { clearBusy(); scheduleResyncNudge(); }

  function playerIsLive() {
    try { return player.isLive(); } catch (_) { return live; }
  }
  function liveSeekTarget(target) {
    const sr = player.seekRange();
    const edgeGuard = Math.min(0.5, Math.max(0.1, sr.end - sr.start));
    return Math.max(sr.start, Math.min(Math.max(sr.start, sr.end - edgeGuard), target));
  }
  function syncLiveRate() {
    if (!live || disposed) return;
    try {
      const active = player.seekRange().end - video.currentTime <= LIVE_BEHIND_THRESHOLD;
      player.configure({ streaming: { liveSync: active
        ? { maxPlaybackRate: 1.1, minPlaybackRate: 0.9 }
        : { maxPlaybackRate: 1.0, minPlaybackRate: 1.0 } } });
    } catch (_) {}
  }
  // Absolute media time. All user seeks clear waiting and apply the same Live
  // guard and rate rule as recovery, including progress dragging.
  function seek(target) {
    if (disposed) return;
    clearWaiting();
    if (live) video.currentTime = liveSeekTarget(target);
    else {
      const duration = Number.isFinite(video.duration) ? video.duration : 0;
      video.currentTime = Math.max(0, Math.min(duration, target));
    }
    syncLiveRate();
  }
  function recoveryOptions() {
    const base = { showAutoplayHint: true, resumePlayback: !video.paused && !video.ended };
    if (!playerIsLive()) return { ...base, preserveTime: video.currentTime };
    try {
      const sr = player.seekRange();
      const behind = sr.end - video.currentTime;
      if (video.currentTime >= sr.start && video.currentTime <= sr.end && behind > LIVE_BEHIND_THRESHOLD) {
        return { ...base, preserveTime: video.currentTime };
      }
    } catch (_) {}
    return { ...base, seekToLiveEdge: true };
  }

  function stopLiveTimers() {
    cancelInterval(progressTimer); progressTimer = null;
    cancelInterval(resyncTimer); resyncTimer = null;
    cancelTimeout(resyncNudgeTimer); resyncNudgeTimer = null;
    resyncPending = false;
  }
  function applyMode() {
    player.configure({ streaming: live ? LIVE_STREAMING_CFG : VOD_STREAMING_CFG });
    stopLiveTimers();
    if (live) {
      progressTimer = every(() => emit('progress'), 1000);
      resyncTimer = every(() => {
        resyncPending = true;
        if (context.hidden()) scheduleResyncNudge(0);
      }, 5 * 60 * 1000);
    }
    publishState();
  }
  function nudgeForResync() {
    if (!live || video.paused || video.seeking || context.dragging() || reconnecting || stallRecovering || waitingSince !== null) return false;
    // HAVE_FUTURE_DATA, without requiring a browser global in the media seam.
    if (video.readyState < 3) return false;
    try {
      const sr = player.seekRange();
      if (video.currentTime < sr.start || video.currentTime > sr.end) return false;
      const safeEnd = Math.max(sr.start, sr.end - 0.1);
      const target = Math.max(sr.start, Math.min(safeEnd, video.currentTime + 0.001));
      if (target <= video.currentTime) return false;
      video.currentTime = target;
      return true;
    } catch (err) {
      console.warn('[av resync nudge failed]', err);
      return false;
    }
  }
  function scheduleResyncNudge(delay = AV_RESYNC_DEFER_MS, force = false) {
    if (!force && !resyncPending) return;
    cancelTimeout(resyncNudgeTimer);
    resyncNudgeTimer = after(() => {
      resyncNudgeTimer = null;
      if (!force && !resyncPending) return;
      if (nudgeForResync()) resyncPending = false;
    }, delay);
  }
  function clearInitialResync() {
    if (initialResyncCleanup) initialResyncCleanup();
    initialResyncCleanup = null;
  }
  function scheduleInitialResync() {
    clearInitialResync();
    let timeout = null;
    const remove = listen(video, 'playing', () => {
      timeout = after(() => {
        initialResyncCleanup = null;
        scheduleResyncNudge(0, true);
      }, AV_RESYNC_DEFER_MS);
    }, { once: true });
    initialResyncCleanup = () => { remove(); cancelTimeout(timeout); };
  }

  async function play(showHint = true) {
    if (disposed) return { ok: false, blocked: false };
    try {
      await video.play();
      checkAlive();
      emit('autoplay', { started: true });
      return { ok: true, blocked: false };
    } catch (err) {
      if (disposed) return { ok: false, blocked: false };
      const blocked = err && err.name === 'NotAllowedError';
      clearWaiting();
      if (blocked && showHint && !video.muted) {
        video.muted = true;
        try {
          await video.play();
          checkAlive();
          emit('autoplay', { started: true, muted: true });
          return { ok: true, blocked: false };
        } catch (_) {
          if (disposed) return { ok: false, blocked: false };
          video.muted = false;
        }
      }
      emit('autoplay', { started: false, blocked: !!(blocked && showHint) });
      console.warn('[autoplay failed]', err);
      return { ok: false, blocked, err };
    }
  }
  async function reconnect(opts) {
    if (reconnecting || disposed) return false;
    const { preserveTime = null, seekToLiveEdge = false, showAutoplayHint = false, resumePlayback = true } = opts;
    reconnecting = true;
    failed = false;
    publishState();
    emit('recovering');
    try {
      try { await player.unload(); } catch (_) {}
      checkAlive();
      await loadWithRetry(preserveTime);
      checkAlive();
      live = playerIsLive();
      applyMode();
      emit('loaded', { initial: false });
      if (live && seekToLiveEdge) {
        if (typeof player.goToLive === 'function') player.goToLive();
        else video.currentTime = liveSeekTarget(player.seekRange().end);
        syncLiveRate();
      } else if (preserveTime !== null && Number.isFinite(preserveTime)) {
        try {
          if (live) { video.currentTime = liveSeekTarget(preserveTime); syncLiveRate(); }
          else {
            const duration = Number.isFinite(video.duration) ? video.duration : preserveTime;
            video.currentTime = Math.max(0, Math.min(duration, preserveTime));
          }
        } catch (err) { console.warn('[restore position failed]', err); }
      }
      if (live) scheduleInitialResync();
      clearWaiting();
      if (resumePlayback) {
        const autoplay = await play(showAutoplayHint);
        checkAlive();
        if (!autoplay.ok && !autoplay.blocked) return false;
      } else emit('pause');
      resetRecovery();
      return true;
    } catch (err) {
      if (!disposed) console.error('[reconnect failed]', err);
      return false;
    } finally {
      reconnecting = false;
      publishState();
    }
  }
  function scheduleRecovery(opts = recoveryOptions(), message = STALL_RECOVERY_MSG, immediate = false) {
    if (disposed || reconnecting || recoveryTimer !== null) return;
    const attempt = recoveryAttempts + 1;
    if (attempt > 6) { fail('播放器多次恢復失敗，請稍後重新整理頁面。'); return; }
    recoveryAttempts = attempt;
    const delay = immediate && attempt === 1 ? 0 : Math.min(1000 * 2 ** (attempt - 1), 30000);
    if (message) emit('toast', {
      message: delay > 0 ? `${message} ${Math.ceil(delay / 1000)} 秒後重試…` : message,
      duration: Math.max(1600, Math.min(delay, 2800)),
    });
    recoveryTimer = after(async () => {
      recoveryTimer = null;
      // The first unload pauses the element itself. Read user intent before
      // that unload, then retain it for every retry of this recovery attempt.
      if (attempt === 1) opts = { ...opts, resumePlayback: !video.paused && !video.ended };
      const ok = await reconnect(opts);
      if (!ok) scheduleRecovery(opts, message);
    }, delay);
  }
  function handleError(detail) {
    if (starting) return;
    // Shaka Error.Severity.CRITICAL is 2; the media adapter forwards its error.
    if (detail.severity !== 2) { console.warn('[shaka recoverable]', detail); return; }
    console.error('[shaka critical]', detail);
    scheduleRecovery(recoveryOptions(), '播放發生錯誤，正在重試…');
  }
  function recoverStall() {
    if (stallRecovering || reconnecting || disposed) return;
    stallRecovering = true;
    try {
      live = playerIsLive();
      const opts = recoveryOptions();
      if (live && opts.seekToLiveEdge) {
        let target;
        try { target = liveSeekTarget(player.seekRange().end); }
        catch (_) { scheduleRecovery(opts, STALL_RECOVERY_MSG, true); return; }
        if (Math.abs(video.currentTime - target) > 0.25) {
          video.currentTime = target;
          syncLiveRate();
          waitingSince = clock.now();
        } else scheduleRecovery(opts, STALL_RECOVERY_MSG, true);
      } else {
        if (live) clearWaiting();
        scheduleRecovery(opts, STALL_RECOVERY_MSG, true);
      }
    } finally { stallRecovering = false; }
  }
  function installListeners() {
    listen(player, 'error', e => handleError(e.detail));
    listen(player, 'manifestparsed', () => player.configure({ streaming: playerIsLive() ? LIVE_STREAMING_CFG : VOD_STREAMING_CFG }));
    listen(player, 'buffering', e => {
      buffering = !!(e.buffering ?? e.detail?.buffering);
      if (buffering) markWaiting();
      else { if (!mediaWaiting) waitingSince = null; publishState(); }
    });
    listen(video, 'waiting', () => { mediaWaiting = true; markWaiting(); });
    listen(video, 'stalled', () => { mediaWaiting = true; markWaiting(); });
    for (const type of ['playing', 'canplay', 'seeked']) listen(video, type, playbackReady);
    listen(video, 'play', () => { publishState(); emit('play'); });
    listen(video, 'pause', () => { clearBusy(); emit('pause'); });
    listen(video, 'ended', clearBusy);
    listen(video, 'timeupdate', () => {
      if (!video.paused && !video.seeking && video.currentTime > lastPlaybackTime + 0.05) clearBusy();
      lastPlaybackTime = video.currentTime;
      emit('progress');
    });
  }
  async function begin() {
    starting = true;
    publishState();
    try {
      await player.attach(video);
      checkAlive();
      sourceInfo = await source();
      checkAlive();
      player.configure({ drm: { servers: sourceInfo.drmServers }, streaming: { bufferingGoal: 30, rebufferingGoal: 10 } });
      installListeners();
      await loadWithRetry();
      checkAlive();
      ready = true;
      live = playerIsLive();
      applyMode();
      emit('loaded', { initial: true });
      if (live) scheduleInitialResync();
      every(() => {
        const nowLive = player.isLive();
        if (nowLive !== live) { live = nowLive; applyMode(); }
      }, 5000);
      every(() => {
        if (waitingSince === null || reconnecting || stallRecovering) return;
        if (video.paused || video.seeking || context.dragging()) return;
        if (clock.now() - waitingSince >= 8000) recoverStall();
      }, 2000);
      const autoplay = await play(true);
      checkAlive();
      if (!autoplay.ok && !autoplay.blocked) {
        scheduleRecovery(recoveryOptions(), '播放啟動失敗，正在重試…');
        return;
      }
      resetRecovery();
    } catch (err) {
      if (!disposed) fail(`載入失敗: ${err.message || err}`);
    } finally {
      starting = false;
      publishState();
    }
  }
  function start() {
    if (disposed) return Promise.resolve();
    if (!startPromise) startPromise = begin();
    return startPromise;
  }
  function visibilityChanged() { if (context.hidden()) scheduleResyncNudge(0); }
  function dispose() {
    if (disposed) return;
    disposed = true;
    clearInitialResync();
    for (const id of timeouts) clock.clearTimeout(id);
    for (const id of intervals) clock.clearInterval(id);
    timeouts.clear(); intervals.clear();
    for (const resolve of sleeps.values()) resolve();
    sleeps.clear();
    for (const remove of listeners) remove();
    listeners.clear();
    onEvent({ type: 'state', state: state() });
    return player.destroy().catch(() => {});
  }
  return { start, play, seek, visibilityChanged, dispose };
}

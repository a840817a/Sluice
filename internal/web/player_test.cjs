// Run with: node --test internal/web/player_test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(`${__dirname}/static/player.html`, 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
new vm.Script(script);

// Exercise the page's actual functions with deterministic media and timer seams.
function harness(names, values) {
  const context = vm.createContext({ console, ...values });
  for (const name of names) {
    const match = script.match(new RegExp(`(?:async )?function ${name}\\(`));
    assert.ok(match, name);
    const end = script.indexOf('\n}\n', match.index) + 3;
    vm.runInContext(script.slice(match.index, end), context);
  }
  return context;
}

function recoveryHarness(extra = {}) {
  const timers = [];
  const context = harness(['scheduleRecovery', 'handleShakaError', 'shouldResumeAfterRecovery'], {
    player: {}, initializing: false, reconnecting: false, recoveryTimer: null,
    recoveryAttempts: 0, RECOVERY_MAX_ATTEMPTS: 6, STALL_RECOVERY_MSG: '',
    shaka: { util: { Error: { Severity: { CRITICAL: 2 } } } },
    video: { paused: false, ended: false },
    currentRecoveryOptions: () => ({ resumePlayback: true }),
    recoveryDelay: () => 1000, showToast() {}, showError() {},
    setTimeout(callback) { timers.push(callback); return timers.length; },
    ...extra,
  });
  return { context, timers };
}

test('critical errors during initial load do not start a competing reconnect', async () => {
  let releaseLoad;
  const pendingLoad = new Promise(resolve => { releaseLoad = resolve; });
  const { context, timers } = recoveryHarness();
  Object.assign(context, {
    shaka: {
      polyfill: { installAll() {} },
      util: { Error: { Severity: { CRITICAL: 2 } } },
      Player: class {
        static isBrowserSupported() { return true; }
        async attach() {} configure() {} addEventListener() {} isLive() { return false; }
      },
    },
    video: { addEventListener() {} }, document: { addEventListener() {} },
    loadDRMServers: async () => ({}),
    loadWithRetry: async () => {
      context.handleShakaError({ severity: 2 });
      await pendingLoad;
    },
    attemptAutoplay: async () => ({ ok: true }),
  });
  for (const name of ['restoreVolume', 'refreshVolumeUI', 'handleTracksChanged',
    'refreshQualityActive', 'handleShakaBuffering', 'handlePlaybackReady',
    'refreshPlayIcon', 'handleTimeUpdate', 'clearPlaybackBusyState',
    'handleEnterPip', 'handleLeavePip', 'registerAutoPip', 'refreshPipButton',
    'applyModeConfig', 'restoreSavedQuality', 'restoreQualityPreference',
    'buildQualityMenu', 'hideError', 'startModeWatcher', 'startStallWatchdog',
    'resetRecoveryState']) context[name] = () => {};
  const initMatch = script.match(/async function init\(/);
  vm.runInContext(script.slice(initMatch.index, script.indexOf('\n}\n', initMatch.index) + 3), context);
  const initializing = context.init();
  await new Promise(resolve => setImmediate(resolve));
  const competingTimers = timers.length;
  releaseLoad();
  await initializing;
  assert.equal(competingTimers, 0);
  assert.equal(context.initializing, false);
});

test('pause during retry delay is honored, including subsequent failed retries', async () => {
  const resumed = [];
  const { context, timers } = recoveryHarness({
    reconnect: async opts => { resumed.push(opts.resumePlayback); return resumed.length > 1; },
  });
  context.scheduleRecovery({ resumePlayback: true });
  context.video.paused = true;
  await timers.shift()();
  await timers.shift()();
  assert.deepEqual(resumed, [false, false]);
});

test('pointer release applies its final position before a queued frame', () => {
  const seeks = [];
  const listeners = {};
  let frame;
  const context = harness(['scheduleSeek', 'endProgressDrag'], {
    pendingSeekX: null, rafSeekPending: false, dragging: true, activePointerId: 1,
    progressHovering: true, progWrap: { classList: { remove() {} },
      addEventListener: (type, callback) => { listeners[type] = callback; } },
    refreshProgress() {}, seekFromPointer: x => seeks.push(x),
    requestAnimationFrame: callback => { frame = callback; },
  });
  vm.runInContext(script.match(/^progWrap.addEventListener\('pointerup'.*$/m)[0], context);
  context.scheduleSeek(800);
  listeners.pointerup({ pointerId: 1, clientX: 900 });
  frame();
  assert.deepEqual(seeks, [900]);
});


test('unload-induced pause does not cancel playback on a later retry', async () => {
  const resumed = [];
  const { context, timers } = recoveryHarness();
  context.reconnect = async opts => {
    resumed.push(opts.resumePlayback);
    context.video.paused = true; // Media element pauses when unloaded.
    return resumed.length > 1;
  };
  context.scheduleRecovery({ resumePlayback: true });
  await timers.shift()();
  await timers.shift()();
  assert.deepEqual(resumed, [true, true]);
});

// Run with: node --test internal/web/player_test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const playbackModule = import('./static/player/playback.mjs');
const settle = async () => { for (let i = 0; i < 30; i++) await Promise.resolve(); };
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};

class Clock {
  time = 0;
  next = 0;
  timers = new Map();
  now() { return this.time; }
  setTimeout(fn, delay) { return this.add(fn, delay, false); }
  clearTimeout(id) { this.timers.delete(id); }
  setInterval(fn, delay) { return this.add(fn, delay, true); }
  clearInterval(id) { this.timers.delete(id); }
  add(fn, delay, repeat) {
    const id = ++this.next;
    this.timers.set(id, { fn, at: this.time + delay, delay, repeat });
    return id;
  }
  async advance(ms) {
    const until = this.time + ms;
    for (;;) {
      const next = [...this.timers.entries()].filter(([, t]) => t.at <= until)
        .sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
      if (!next) break;
      const [id, timer] = next;
      this.time = timer.at;
      if (timer.repeat) timer.at += timer.delay;
      else this.timers.delete(id);
      timer.fn();
      await settle();
    }
    this.time = until;
    await settle();
  }
}
class Events extends EventTarget {
  registrations = new Set();
  addEventListener(type, fn, options) {
    super.addEventListener(type, fn, options);
    this.registrations.add(fn);
  }
  removeEventListener(type, fn, options) {
    super.removeEventListener(type, fn, options);
    this.registrations.delete(fn);
  }
  emit(type, detail) {
    const event = new Event(type);
    event.detail = detail;
    this.dispatchEvent(event);
  }
}
class Video extends Events {
  paused = true;
  ended = false;
  seeking = false;
  muted = false;
  currentTime = 0;
  duration = 100;
  readyState = 4;
  played = [];
  playHandler = async () => {};
  async play() {
    await this.playHandler();
    this.paused = false;
    this.played.push({ time: this.currentTime, muted: this.muted });
    this.emit('play'); this.emit('playing');
  }
  pause() { this.paused = true; this.emit('pause'); }
}
class Player extends Events {
  loads = [];
  configs = [];
  unloads = 0;
  destroys = 0;
  live = false;
  range = { start: 10, end: 100 };
  attachHandler = async () => {};
  loadHandler = async () => {};
  async attach(video) { this.video = video; await this.attachHandler(); }
  configure(config) { this.configs.push(config); }
  isLive() { return this.live; }
  seekRange() { return this.range; }
  async load(url, start) {
    this.loads.push({ url, start });
    this.emit('manifestparsed');
    await this.loadHandler(this.loads.length);
    this.video.currentTime = start ?? 0;
  }
  async unload() { this.unloads++; this.video.pause(); }
  async destroy() { this.destroys++; }
}
async function harness(t, { live = false, source, onEvent } = {}) {
  const { createPlayback } = await playbackModule;
  t.mock.method(console, 'warn', () => {});
  t.mock.method(console, 'error', () => {});
  const clock = new Clock(), player = new Player(), video = new Video(), events = [];
  const context = { isDragging: false, isHidden: false,
    dragging() { return this.isDragging; }, hidden() { return this.isHidden; } };
  player.live = live;
  const playback = createPlayback({
    player, video, clock, context,
    source: source || (async () => ({ manifestURL: '/channel/master.m3u8', drmServers: {} })),
    onEvent: event => { events.push(event); if (onEvent) onEvent(event); },
  });
  t.after(() => playback.dispose());
  return { player, video, clock, context, events, playback,
    state: () => events.findLast(e => e.type === 'state').state };
}

// All lifecycle tests cross the same interface as the page, run the complete
// implementation, and control only its media/source/clock dependencies.
test('critical errors during initial load do not start a competing reconnect', async t => {
  const h = await harness(t), load = deferred();
  h.player.loadHandler = async () => {
    h.player.emit('error', { severity: 2 });
    await load.promise;
  };
  const started = h.playback.start();
  assert.equal(h.playback.start(), started, 'starting twice shares the same load');
  await settle(); await h.clock.advance(10000);
  assert.equal(h.player.unloads, 0);
  assert.equal(h.player.loads.length, 1);
  load.resolve(); await started;
  assert.equal(h.state().phase, 'ready');
  assert.equal(h.video.paused, false);
});

test('pause during retry delay is honored, including subsequent failed retries', async t => {
  const h = await harness(t);
  await h.playback.start(); h.video.currentTime = 42;
  h.player.loadHandler = async attempt => { if (attempt <= 6) throw new Error('offline'); };
  h.player.emit('error', { severity: 2 });
  h.video.pause();
  await h.clock.advance(18000); // 1s recovery + 15s load retries + 2s recovery.
  assert.equal(h.player.unloads, 2);
  assert.equal(h.player.loads.length, 7);
  assert.equal(h.video.paused, true);
  assert.equal(h.video.currentTime, 42);
  assert.equal(h.video.played.length, 1, 'recovery must not autoplay after user pause');
  assert.equal(h.state().phase, 'ready');
});

test('unload-induced pause does not cancel playback on a later retry', async t => {
  const h = await harness(t);
  await h.playback.start(); h.video.currentTime = 57;
  h.player.loadHandler = async attempt => { if (attempt <= 6) throw new Error('offline'); };
  h.player.emit('error', { severity: 2 });
  await h.clock.advance(18000);
  assert.equal(h.player.unloads, 2);
  assert.equal(h.video.paused, false);
  assert.equal(h.video.played.length, 2);
  assert.equal(h.video.played[1].time, 57);
  assert.ok(h.player.loads.slice(1).every(load => load.start === 57), 'Shaka receives the VOD start position on every retry');
});

test('initial load retries with backoff and reports failure without reconnecting', async t => {
  const h = await harness(t);
  h.player.loadHandler = async () => { throw new Error('origin offline'); };
  const started = h.playback.start(); await settle();
  for (const delay of [1000, 2000, 4000, 8000]) {
    const before = h.player.loads.length;
    await h.clock.advance(delay - 1);
    assert.equal(h.player.loads.length, before);
    await h.clock.advance(1);
    assert.equal(h.player.loads.length, before + 1);
  }
  await started;
  assert.equal(h.state().phase, 'failed');
  assert.match(h.events.findLast(e => e.type === 'error').message, /origin offline/);
  assert.equal(h.player.unloads, 0);
  assert.equal(h.clock.timers.size, 0);
});

test('runtime recovery stops after six attempts and shows a retryable error', async t => {
  const h = await harness(t);
  await h.playback.start();
  h.player.loadHandler = async () => { throw new Error('offline'); };
  h.player.emit('error', { severity: 2 });
  await h.clock.advance(200000);
  assert.equal(h.player.unloads, 6);
  assert.equal(h.player.loads.length, 31);
  assert.equal(h.state().phase, 'failed');
  assert.match(h.events.findLast(e => e.type === 'error').message, /多次恢復失敗/);
  await h.clock.advance(60000);
  assert.equal(h.player.loads.length, 31);
});

for (const { time, expectedStart, expectedTime } of [
  { time: 45, expectedStart: 45, expectedTime: 45 },
  { time: 95, expectedStart: undefined, expectedTime: 99.5 },
]) test(`Live recovery at ${time}s preserves timeshift or returns to the guarded edge`, async t => {
  const h = await harness(t, { live: true });
  await h.playback.start(); h.video.currentTime = time;
  h.player.emit('error', { severity: 2 });
  await h.clock.advance(1000);
  assert.equal(h.player.loads[1].start, expectedStart);
  assert.equal(h.video.currentTime, expectedTime);
  assert.equal(h.video.paused, false);
  assert.equal(h.state().live, true);
});

test('stall recovery waits eight seconds and respects dragging', async t => {
  const h = await harness(t);
  await h.playback.start(); h.video.currentTime = 35;
  h.video.emit('waiting');
  assert.equal(h.state().busy, true);
  await h.clock.advance(7999); assert.equal(h.player.unloads, 0);
  h.context.isDragging = true;
  await h.clock.advance(2001); assert.equal(h.player.unloads, 0);
  h.context.isDragging = false;
  await h.clock.advance(2000);
  assert.equal(h.player.unloads, 1);
  assert.equal(h.video.currentTime, 35);
  assert.equal(h.state().busy, false);
});

test('Live stall first seeks to the edge, then reloads if playback remains stuck', async t => {
  const h = await harness(t, { live: true });
  await h.playback.start(); h.video.currentTime = 95;
  h.video.emit('waiting');
  await h.clock.advance(8000);
  assert.equal(h.player.unloads, 0);
  assert.equal(h.video.currentTime, 99.5);
  await h.clock.advance(8000);
  assert.equal(h.player.unloads, 1);
});

test('autoplay falls back to muted playback and notifies the page', async t => {
  const h = await harness(t);
  h.video.playHandler = async () => { if (!h.video.muted) throw Object.assign(new Error('gesture required'), { name: 'NotAllowedError' }); };
  await h.playback.start();
  assert.equal(h.video.paused, false); assert.equal(h.video.muted, true);
  assert.deepEqual(h.events.findLast(e => e.type === 'autoplay'), { type: 'autoplay', started: true, muted: true });
  assert.equal(h.player.unloads, 0);
});

test('blocked muted autoplay waits for a gesture instead of reloading', async t => {
  const h = await harness(t);
  h.video.playHandler = async () => { throw Object.assign(new Error('gesture required'), { name: 'NotAllowedError' }); };
  await h.playback.start(); await h.clock.advance(60000);
  assert.equal(h.video.paused, true); assert.equal(h.video.muted, false);
  assert.equal(h.events.findLast(e => e.type === 'autoplay').blocked, true);
  assert.equal(h.player.unloads, 0);
  h.video.playHandler = async () => {};
  const result = await h.playback.play();
  assert.equal(result.ok, true); assert.equal(h.video.paused, false);
});

test('Live becoming VOD changes streaming config and stops Live timers', async t => {
  const h = await harness(t, { live: true });
  await h.playback.start(); await h.clock.advance(400);
  h.player.live = false;
  await h.clock.advance(4600);
  assert.equal(h.state().live, false);
  assert.equal(h.player.configs.findLast(c => c.streaming).streaming.liveSync.enabled, false);
  const progressCount = h.events.filter(e => e.type === 'progress').length;
  await h.clock.advance(5000);
  assert.equal(h.events.filter(e => e.type === 'progress').length, progressCount);
});

test('AV resync waits for a playback-ready event or a hidden video', async t => {
  const h = await harness(t, { live: true });
  await h.playback.start(); h.video.currentTime = 40;
  await h.clock.advance(400);
  assert.equal(h.video.currentTime, 40.001);
  await h.clock.advance(299600);
  assert.equal(h.video.currentTime, 40.001, 'visible playback is not nudged by the periodic timer');
  h.context.isHidden = true; h.playback.visibilityChanged();
  await h.clock.advance(0);
  assert.ok(Math.abs(h.video.currentTime - 40.002) < 1e-8);
});

for (const stage of ['source', 'attach', 'load', 'load retry delay', 'recovery delay', 'unload', 'recovery load', 'autoplay', 'running Live timers']) {
  test(`dispose during ${stage} stops all work and late completions`, { timeout: 2000 }, async t => {
    const pending = deferred();
    const h = await harness(t, { live: true, source: stage === 'source'
      ? () => pending.promise : undefined });
    if (stage === 'attach') h.player.attachHandler = () => pending.promise;
    if (stage === 'load') h.player.loadHandler = () => pending.promise;
    if (stage === 'load retry delay') h.player.loadHandler = async () => { throw new Error('offline'); };
    if (stage === 'autoplay') h.video.playHandler = () => pending.promise;
    const started = h.playback.start(); await settle();
    if (['recovery delay', 'unload', 'recovery load', 'running Live timers'].includes(stage)) {
      await started;
      if (stage !== 'running Live timers') {
        if (stage === 'unload') h.player.unload = async () => { h.player.unloads++; await pending.promise; };
        if (stage === 'recovery load') h.player.loadHandler = () => pending.promise;
        h.player.emit('error', { severity: 2 });
        if (stage !== 'recovery delay') await h.clock.advance(1000);
      }
    }
    const loads = h.player.loads.length, played = h.video.played.length;
    await h.playback.dispose();
    const count = h.events.length;
    assert.equal(h.player.loads.length, loads, 'dispose must not start another load');
    assert.equal(h.state().phase, 'disposed'); assert.equal(h.clock.timers.size, 0);
    assert.equal(h.player.destroys, 1);
    assert.equal(h.player.registrations.size, 0); assert.equal(h.video.registrations.size, 0);
    // A completed media call can settle, but the module cannot emit, reload,
    // recreate timers, or call play again after the page has gone away.
    pending.resolve({ manifestURL: '/late', drmServers: {} });
    await settle();
    assert.equal(h.player.loads.length, loads, 'late completion must not start another load');
    assert.equal(h.player.registrations.size, 0); assert.equal(h.video.registrations.size, 0);
    await started; await h.clock.advance(400000);
    h.player.emit('error', { severity: 2 }); h.video.emit('waiting');
    h.playback.visibilityChanged(); await h.playback.start(); await h.playback.dispose();
    assert.equal(h.events.length, count); assert.equal(h.player.loads.length, loads);
    assert.equal(h.clock.timers.size, 0); assert.equal(h.player.destroys, 1);
    assert.equal(h.video.played.length, played + (stage === 'autoplay' ? 1 : 0));
  });
}

// The control-row regression remains separate from playback's lifecycle seam.
test('pointer release applies its final position before a queued frame', () => {
  const html = fs.readFileSync(`${__dirname}/static/player.html`, 'utf8');
  const script = html.match(/<script type="module">([\s\S]*?)<\/script>/)[1].replace(/^import .*;\n/m, '');
  new vm.Script(script);
  const seeks = [], listeners = {};
  let frame;
  const context = vm.createContext({
    pendingSeekX: null, rafSeekPending: false, dragging: true, activePointerId: 1,
    progressHovering: true, progWrap: { classList: { remove() {} },
      addEventListener: (type, callback) => { listeners[type] = callback; } },
    refreshProgress() {}, seekFromPointer: x => seeks.push(x),
    requestAnimationFrame: callback => { frame = callback; },
  });
  for (const name of ['scheduleSeek', 'endProgressDrag']) {
    const match = script.match(new RegExp(`function ${name}\\(`));
    vm.runInContext(script.slice(match.index, script.indexOf('\n}\n', match.index) + 3), context);
  }
  vm.runInContext(script.match(/^progWrap.addEventListener\('pointerup'.*$/m)[0], context);
  context.scheduleSeek(800);
  listeners.pointerup({ pointerId: 1, clientX: 900 }); frame();
  assert.deepEqual(seeks, [900]);
});

// Run with: node --test internal/web/admin_test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = name => fs.readFileSync(`${__dirname}/static/admin/js/${name}.js`, 'utf8')
  .replace(/^import .*;\n/gm, '').replace(/^export /gm, '');
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
function formsHarness() {
  const nodes = new Map(), calls = [], messages = [];
  function node(id) {
    if (!nodes.has(id)) {
      const classes = new Set();
      const fields = new Proxy({}, { get: (target, key) => target[key] ||= { value: '', checked: false } });
      nodes.set(id, {
        elements: fields, listeners: {}, button: { disabled: false }, resets: 0,
        classList: { add: x => classes.add(x), remove: x => classes.delete(x), contains: x => classes.has(x) },
        addEventListener(type, fn) { this.listeners[type] = fn; },
        querySelector(selector) { return selector.includes('submit') ? this.button : null; },
        querySelectorAll() { return []; }, replaceChildren() {},
        reset() { this.resets++; }, setAttribute() {}, removeAttribute() {},
      });
    }
    return nodes.get(id);
  }
  function request(...args) { const d = deferred(); calls.push({ ...d, args }); return d.promise; }
  const ctx = vm.createContext({
    document: { getElementById: node },
    FormData: class { entries() { return [['title', 'draft']]; } },
    api: { createChannel: request, updateChannel: request }, configFor: () => ({}),
  });
  vm.runInContext(source('forms'), ctx);
  // Template mounting is unrelated to request ownership; keep real form handlers.
  vm.runInContext('mountFields = () => {}; bindSourceHint = () => {};', ctx);
  ctx.configure({ message: (...args) => messages.push(args), changed() {} });
  ctx.initAddForm(); ctx.initEditModal();
  return { ctx, node, calls, messages, submit: id => node(id).listeners.submit({ preventDefault() {} }) };
}

test('add submission is single-flight and can retry after failure', async () => {
  const h = formsHarness(); h.ctx.openAdd();
  const pending = h.submit('addForm');
  h.submit('addForm');
  assert.equal(h.calls.length, 1);
  assert.equal(h.node('addForm').button.disabled, true);
  h.calls[0].reject(new Error('offline')); await pending;
  assert.equal(h.node('addForm').button.disabled, false);
  const retry = h.submit('addForm');
  assert.equal(h.calls.length, 2);
  h.calls[1].resolve({ id: 'created' }); await retry;
  assert.equal(h.node('addForm').resets, 1);
});

for (const nextID of ['b', 'a']) test(`saving A cannot close a new edit session for ${nextID}`, async () => {
  const h = formsHarness(); h.ctx.openEdit('a');
  const pending = h.submit('editForm');
  h.ctx.closeEdit(); h.ctx.openEdit(nextID);
  h.calls[0].resolve({ restarted: false }); await pending;
  assert.equal(h.node('editOverlay').classList.contains('open'), true);
  const next = h.submit('editForm');
  assert.equal(h.calls[1].args[0], nextID);
  h.calls[1].resolve({}); await next;
});

test('old create response does not reset or close a reopened add drawer', async () => {
  const h = formsHarness(); h.ctx.openAdd();
  const pending = h.submit('addForm');
  h.ctx.closeAdd(); h.ctx.openAdd();
  h.calls[0].resolve({ id: 'created' }); await pending;
  assert.equal(h.node('addOverlay').classList.contains('open'), true);
  assert.equal(h.node('addForm').resets, 0);
});

function pollHarness() {
  const timers = new Map(), requests = [], gateway = [], messages = [];
  let next = 0;
  const ctx = vm.createContext({
    AbortController, document: { hidden: false, getElementById: () => ({}) },
    forms: {}, api: { listChannels(signal) {
      const d = deferred(); requests.push({ ...d, signal });
      signal.addEventListener('abort', () => d.reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
      return d.promise;
    } },
    setTimeout(fn, delay) { timers.set(++next, { fn, delay }); return next; },
    clearTimeout(id) { timers.delete(id); }, renderCards() {}, summarize: () => ({}),
  });
  vm.runInContext(source('main').split('// ── boot')[0], ctx);
  ctx.setGateway = ok => gateway.push(ok);
  ctx.showMsg = text => messages.push(text);
  ctx.clearMsg = () => {};
  ctx.renderSummary = () => {}; ctx.applyFilter = () => {};
  return { ctx, timers, requests, gateway, messages };
}

test('hung poll times out, reports outage, and resumes after retry', async () => {
  const h = pollHarness();
  const pending = h.ctx.poll();
  const deadline = [...h.timers.entries()].find(([, t]) => t.delay === 10000);
  assert.ok(deadline, 'poll must have a deadline');
  h.timers.delete(deadline[0]); deadline[1].fn(); await pending;
  assert.equal(h.requests[0].signal.aborted, true);
  assert.deepEqual(h.gateway, [false]);
  assert.match(h.messages[0], /逾時/);
  const retry = [...h.timers.entries()].find(([, t]) => t.delay === 5000);
  assert.ok(retry);
  h.timers.delete(retry[0]); const recovered = retry[1].fn();
  h.requests[1].resolve({}); await recovered;
  assert.deepEqual(h.gateway, [false, true]);
  assert.deepEqual([...h.timers.values()].map(t => t.delay), [2000]);
});

test('superseding a poll is silent and clears its deadline', async () => {
  const h = pollHarness();
  const old = h.ctx.poll(); const current = h.ctx.poll();
  await old;
  assert.equal(h.requests[0].signal.aborted, true);
  assert.deepEqual(h.gateway, []);
  h.requests[1].resolve({}); await current;
  assert.deepEqual(h.gateway, [true]);
  assert.deepEqual([...h.timers.values()].map(t => t.delay), [2000]);
});

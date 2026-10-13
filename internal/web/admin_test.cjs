// Run with: node --test internal/web/admin_test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = name => fs.readFileSync(`${__dirname}/static/admin/js/${name}.js`, 'utf8')
  .replace(/^import .*;\n/gm, '').replace(/^export /gm, '');
// Values built inside the vm context have another realm's prototypes.
const plain = v => JSON.parse(JSON.stringify(v));
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
    api: { createChannel: request, updateChannel: request, addKeys: request }, configFor: () => ({}),
  });
  vm.runInContext(source('forms'), ctx);
  // Template mounting is unrelated to request ownership; keep real form handlers.
  vm.runInContext('mountFields = () => {}; bindVisibility = () => {};', ctx);
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

test('field visibility follows source type and key mode', () => {
  const h = formsHarness();
  const show = (type, mode) => {
    h.node('addSourceType').value = type; h.node('addKeyMode').value = mode;
    h.ctx.applyVisibility('add');
    return {
      keys: !h.node('addKeysField').hidden, drm: !h.node('addDrmSection').hidden,
      placeholder: h.node('addSourceURL').placeholder,
    };
  };
  assert.deepEqual(plain(show('dash', 'decrypt')), { keys: true, drm: true, placeholder: 'https://cdn.example.com/stream.mpd' });
  assert.equal(show('dash', 'passthrough').keys, false);
  const hls = show('hls', 'decrypt');
  assert.equal(hls.keys, false); assert.equal(hls.drm, false);
  assert.equal(hls.placeholder, 'https://cdn.example.com/master.m3u8');
  assert.equal(show('dash', 'passthrough').drm, true);
});

test('create form sends content keys as a list, only for dash decrypt', async () => {
  const h = formsHarness();
  const f = h.node('addForm').elements;
  const submit = async (type, mode, text) => {
    f.source_type.value = type; f.key_mode.value = mode; f.content_keys.value = text;
    h.ctx.openAdd();
    const pending = h.submit('addForm');
    const body = h.calls[h.calls.length - 1].args[0];
    h.calls[h.calls.length - 1].resolve({ id: 'x' }); await pending;
    return body;
  };
  assert.deepEqual(plain((await submit('dash', 'decrypt', ' k1:v1 \n\n k2:v2\n')).content_keys), ['k1:v1', 'k2:v2']);
  assert.equal('content_keys' in await submit('dash', 'decrypt', ''), false);
  assert.equal('content_keys' in await submit('dash', 'passthrough', 'k1:v1'), false);
  assert.equal('content_keys' in await submit('hls', 'decrypt', 'k1:v1'), false);
});

test('add-keys form posts the lines and shows the server error on the card', async () => {
  const h = formsHarness();
  const box = { textContent: '', hidden: true };
  const area = { value: 'k1:v1\nk2:v2' };
  const form = {
    dataset: {}, elements: { content_keys: area }, button: { disabled: false },
    closest: () => ({ dataset: { channelId: 'ch1' } }),
    querySelector(sel) { return sel.includes('submit') ? this.button : box; },
    setAttribute() {},
  };
  const first = h.ctx.submitKeys(form);
  assert.deepEqual(plain(h.calls[0].args), ['ch1', ['k1:v1', 'k2:v2']]);
  assert.equal(form.button.disabled, true);
  h.calls[0].reject(new Error('content key already exists: KID 00')); await first;
  assert.equal(box.hidden, false);
  assert.match(box.textContent, /already exists/);
  assert.equal(area.value, 'k1:v1\nk2:v2', 'a rejected request keeps the text');
  assert.equal(form.button.disabled, false);

  const second = h.ctx.submitKeys(form);
  assert.equal(box.hidden, true);
  h.calls[1].resolve({ added_kids: ['a', 'b'] }); await second;
  assert.equal(area.value, '');

  area.value = '  \n';
  await h.ctx.submitKeys(form);
  assert.equal(h.calls.length, 2, 'blank input sends nothing');
  assert.equal(box.hidden, false);
});

function progressHarness() {
  const ctx = vm.createContext({ fmt: {}, playerURL: () => '' });
  vm.runInContext(source('cards'), ctx);
  const refs = new Proxy({}, {
    get: (target, key) => target[key] ||= { textContent: '', hidden: false, className: '', classList: { toggle() {} } },
  });
  return { ctx, refs };
}

test('decrypt warnings show on the card in every mode, and only while running', () => {
  const h = progressHarness();
  const warnings = [
    { period: '', adaptation_set: '1', kind: 'missing_key', kids: ['2020'], message: 'no content key held for these KIDs' },
    { period: '', kind: 'key_unmatched', kids: ['3030'], message: 'no init segment of this channel uses these keys' },
  ];
  h.ctx.renderProgress(h.refs, { running: true, mode: 'live', decrypt_warnings: warnings });
  assert.equal(h.refs.decryptWarning.hidden, false);
  assert.match(h.refs.decryptWarning.textContent, /missing_key \(period —, AS 1\): no content key held for these KIDs \[2020\]/);
  assert.match(h.refs.decryptWarning.textContent, /key_unmatched: .*\[3030\]/);
  assert.equal(h.refs.vodWarning.hidden, true, 'decrypt warnings are not VOD warnings');

  h.ctx.renderProgress(h.refs, { running: true, mode: 'vod', decrypt_warnings: [warnings[0]] });
  assert.equal(h.refs.decryptWarning.hidden, false);
  assert.doesNotMatch(h.refs.decryptWarning.textContent, /key_unmatched/);

  h.ctx.renderProgress(h.refs, { running: true, mode: 'live' });
  assert.equal(h.refs.decryptWarning.hidden, true);
  h.ctx.renderProgress(h.refs, { running: false, decrypt_warnings: warnings });
  assert.equal(h.refs.decryptWarning.hidden, true);
});

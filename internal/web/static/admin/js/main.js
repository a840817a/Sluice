// Boot and the poll loop.

import * as api from './api.js';
import { renderCards, summarize } from './cards.js';
import * as fmt from './format.js';
import * as forms from './forms.js';

/** How often the channel list is refreshed while the tab is visible. */
const POLL_INTERVAL = 2000;

/** Bound a stalled response, including a body that never finishes arriving. */
const POLL_TIMEOUT = 10000;

/** Delay after the first failed poll, before backoff starts widening it. */
const POLL_ERROR_INTERVAL = 5000;

/**
 * Ceiling for the retry backoff. It also bounds how long a recovered gateway
 * keeps showing an outage notice, which is why it is not larger: this is a
 * single admin page, so retrying every 15s costs nothing, while a longer ceiling
 * would leave a stale error on screen well after everything was working again.
 * Returning to the tab retries immediately regardless.
 */
const POLL_ERROR_MAX = 15000;

// ── messages ────────────────────────────────────────────────────────────────

let msgTimer = null;

/**
 * Shows a message in the status bar.
 *
 * Transient messages clear themselves after a few seconds; the timer handle is
 * kept so a new message cancels the previous countdown, otherwise an older
 * timeout fires and wipes the newer message off the screen.
 *
 * A sticky message has no timer at all. That is for conditions that are still
 * true while they are displayed — a gateway that is not answering — where
 * auto-dismissing would just make the message reappear on the next retry and
 * flash indefinitely.
 */
function showMsg(text, type, { sticky = false } = {}) {
  const el = document.getElementById('msg');
  el.textContent = text;
  el.className = type;
  if (msgTimer) {
    clearTimeout(msgTimer);
    msgTimer = null;
  }
  if (!sticky) {
    msgTimer = setTimeout(clearMsg, 4000);
  }
}

function clearMsg() {
  const el = document.getElementById('msg');
  el.className = '';
  el.textContent = '';
  if (msgTimer) {
    clearTimeout(msgTimer);
    msgTimer = null;
  }
}

// ── poll loop ───────────────────────────────────────────────────────────────

let pollTimer = null;
let inFlight = null;
let stopped = false;

/** Consecutive failed polls. Drives both the message and the retry backoff. */
let pollFailures = 0;

function schedule(delay) {
  if (pollTimer) clearTimeout(pollTimer);
  if (stopped) return;
  pollTimer = setTimeout(poll, delay);
}

/** Retry delay after n consecutive failures: 5s, 10s, then 15s forever. */
function retryDelay(n) {
  return Math.min(POLL_ERROR_INTERVAL * 2 ** (n - 1), POLL_ERROR_MAX);
}

/**
 * Turns a failed poll into something an operator can act on. A dropped
 * connection surfaces as the browser's bare "Failed to fetch", which says
 * nothing about which end is at fault.
 */
function pollErrorText(err) {
  if (err instanceof TypeError) {
    return '無法連線到 gateway（可能正在重啟或已停止），將持續重試…';
  }
  if (err.status === 401) {
    return '登入憑證已失效，請重新整理頁面登入。';
  }
  return `無法取得 channel 狀態：${err.message}，將持續重試…`;
}

/**
 * Fetches the channel list and renders it.
 *
 * The loop is a chain of setTimeout calls rather than a setInterval: at a
 * 2-second cadence a slow response would otherwise let requests pile up on top of
 * each other. Each poll also aborts the one before it, so a delayed response can
 * never land after a newer one and show stale numbers.
 */
async function poll() {
  if (stopped) return;

  // Nothing is visible, so nothing needs refreshing. The visibilitychange
  // handler resumes immediately when the tab comes back.
  if (document.hidden) {
    schedule(POLL_INTERVAL);
    return;
  }

  if (inFlight) inFlight.abort();
  const controller = new AbortController();
  inFlight = controller;
  let timedOut = false;
  const deadline = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, POLL_TIMEOUT);

  try {
    const data = await api.listChannels(controller.signal);
    if (controller.signal.aborted) return;
    // Recovered: drop the outage notice as soon as one poll gets through, so the
    // operator does not have to wonder whether it is still current.
    if (pollFailures > 0) {
      pollFailures = 0;
      clearMsg();
    }
    renderCards(document.getElementById('channelList'), data || {});
    renderSummary(summarize(data || {}));
    applyFilter();
    setGateway(true);
    schedule(POLL_INTERVAL);
  } catch (err) {
    if (inFlight !== controller) return; // superseded by a newer poll
    if (err.name === 'AbortError' && !timedOut) return;
    pollFailures++;
    // Announced once, on the transition from healthy to failing, and left on
    // screen until it recovers. Re-announcing every retry made the message
    // re-trigger every few seconds and never settle — the outage is one event,
    // not one per attempt.
    if (pollFailures === 1) {
      showMsg(timedOut ? '取得 channel 狀態逾時，將持續重試…' : pollErrorText(err), 'err', { sticky: true });
    }
    setGateway(false);
    schedule(retryDelay(pollFailures));
  } finally {
    clearTimeout(deadline);
    if (inFlight === controller) inFlight = null;
  }
}

/** Refreshes now, resetting the cadence. Used after a mutating action. */
function refreshNow() {
  schedule(0);
}

// ── summary, filters, gateway status ────────────────────────────────────────

function setSum(key, text) {
  const el = document.querySelector(`[data-sum="${key}"]`);
  if (el && el.textContent !== text) el.textContent = text;
}

function setCount(key, n) {
  const el = document.querySelector(`[data-count="${key}"]`);
  const s = fmt.int(n);
  if (el && el.textContent !== s) el.textContent = s;
}

/** Fills the four tiles and the filter-chip counts from one poll's summary. */
function renderSummary(s) {
  setSum('total', fmt.int(s.total));
  setSum('running', fmt.int(s.running));
  setSum('degraded', fmt.int(s.degraded));
  setSum('degradedNames', s.degradedNames.join('、'));
  document.querySelector('[data-sum-tile="degraded"]').classList.toggle('alert', s.degraded > 0);

  const tp = document.querySelector('[data-sum="throughput"]');
  const rate = s.bytesPerSec > 0 ? fmt.bytes(s.bytesPerSec) : '—';
  // Split value and unit so the unit can be set small without markup in data.
  const [num, unit] = rate === '—' ? ['—', ''] : rate.split(' ');
  const want = unit ? `${num} ${unit}/s` : num;
  if (tp.dataset.v !== want) {
    tp.dataset.v = want;
    tp.textContent = '';
    tp.append(num);
    if (unit) {
      const u = document.createElement('span');
      u.className = 'unit';
      u.textContent = ` ${unit}/s`;
      tp.append(u);
    }
  }
  setSum('stored', s.segments ? `${fmt.int(s.segments)} 段 · ${fmt.bytes(s.bytes)}` : '');

  setCount('all', s.total);
  setCount('live', s.live);
  setCount('ingesting', s.ingesting);
  setCount('vod', s.vod);
  setCount('degraded', s.degraded);
  setCount('stopped', s.stopped);
}

let activeFilter = 'all';
let query = '';

const FILTER_MODE = {
  live: 'live',
  ingesting: 'static_ingesting',
  vod: 'vod',
  stopped: 'stopped',
};

/**
 * Hides cards that fail the active chip or the search box. Cards carry their
 * keys as data attributes, written by cards.js on every render, so this never
 * needs the payload and can run on every keystroke.
 */
function applyFilter() {
  const list = document.getElementById('channelList');
  let shown = 0;
  list.querySelectorAll('.ch-card').forEach(card => {
    let ok = true;
    if (activeFilter === 'degraded') ok = card.dataset.degraded === '1';
    else if (activeFilter !== 'all') ok = card.dataset.mode === FILTER_MODE[activeFilter];
    if (ok && query) ok = (card.dataset.search || '').includes(query);
    card.hidden = !ok;
    if (ok) shown++;
  });

  // "No match" is distinct from "no channels": the latter is rendered by
  // cards.js and must not be duplicated here.
  let note = list.querySelector('.empty.filtered');
  const hasCards = list.querySelector('.ch-card') !== null;
  if (hasCards && shown === 0) {
    if (!note) {
      note = document.createElement('p');
      note.className = 'empty filtered';
      note.textContent = '沒有符合條件的頻道。';
      list.appendChild(note);
    }
  } else if (note) {
    note.remove();
  }
}

function initFilters() {
  const chips = document.getElementById('filters');
  chips.addEventListener('click', e => {
    const btn = e.target.closest('[data-filter]');
    if (!btn) return;
    activeFilter = btn.dataset.filter;
    chips.querySelectorAll('[data-filter]').forEach(b => {
      b.classList.toggle('on', b === btn);
    });
    applyFilter();
  });

  const search = document.getElementById('search');
  search.addEventListener('input', () => {
    query = search.value.trim().toLowerCase();
    applyFilter();
  });
}

function setGateway(ok) {
  const box = document.getElementById('gwStatus');
  const state = ok ? 'ok' : 'err';
  if (box.dataset.state !== state) {
    box.dataset.state = state;
    document.getElementById('gwText').textContent = ok ? 'Gateway 已連線' : 'Gateway 無回應';
  }
}

// ── card actions ────────────────────────────────────────────────────────────

const ACTIONS = {
  edit: forms.openEdit,
  restart: forms.restartChannel,
  delete: forms.deleteChannel,
  vod: forms.transitionToVOD,
  'restore-live': forms.restoreToLive,
};

/**
 * One delegated listener for every card button, attached to the container rather
 * than to the buttons. Cards are reconciled in place and can be added or removed
 * on any tick, so per-button handlers would need re-binding constantly; this one
 * survives every update.
 */
function initCardActions() {
  document.getElementById('channelList').addEventListener('click', e => {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    const card = btn.closest('[data-channel-id]');
    if (!card) return;
    const fn = ACTIONS[btn.dataset.action];
    if (fn) fn(card.dataset.channelId);
  });
}

// ── boot ────────────────────────────────────────────────────────────────────

forms.configure({ message: showMsg, changed: refreshNow });
forms.initForms();
initCardActions();
initFilters();

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) refreshNow();
});

poll();

// Boot and the poll loop.

import * as api from './api.js';
import { renderCards } from './cards.js';
import * as forms from './forms.js';

/** How often the channel list is refreshed while the tab is visible. */
const POLL_INTERVAL = 2000;

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

/** Retry delay after n consecutive failures: 5s, 10s, 20s, then 30s forever. */
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
    schedule(POLL_INTERVAL);
  } catch (err) {
    if (err.name === 'AbortError') return; // superseded by a newer poll
    pollFailures++;
    // Announced once, on the transition from healthy to failing, and left on
    // screen until it recovers. Re-announcing every retry made the message
    // re-trigger every few seconds and never settle — the outage is one event,
    // not one per attempt.
    if (pollFailures === 1) {
      showMsg(pollErrorText(err), 'err', { sticky: true });
    }
    schedule(retryDelay(pollFailures));
  } finally {
    if (inFlight === controller) inFlight = null;
  }
}

/** Refreshes now, resetting the cadence. Used after a mutating action. */
function refreshNow() {
  schedule(0);
}

// ── card actions ────────────────────────────────────────────────────────────

const ACTIONS = {
  edit: forms.openEdit,
  restart: forms.restartChannel,
  delete: forms.deleteChannel,
  vod: forms.transitionToVOD,
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

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) refreshNow();
});

poll();

// Channel card rendering.
//
// The list is reconciled, not rebuilt. The previous implementation reassigned
// innerHTML on the whole table every tick, which threw away hover, focus, text
// selection and scroll position, and — at a 2-second poll — would make the page
// unusable. Here each channel owns a persistent element, and a tick only writes
// the individual text nodes whose values actually changed.
//
// Nothing in this file ever assigns innerHTML or interpolates server data into
// markup. Channel titles and URLs are operator-supplied strings that reach this
// page unescaped, so they are written exclusively via textContent.

import * as fmt from './format.js';
import { playerURL } from './api.js';

/** id → { root, refs, trackRows } for every card currently in the DOM. */
const cards = new Map();

/** id → config, as of the last poll. The edit modal reads from here rather than
 *  from a serialised copy stashed in the DOM. */
const configs = new Map();

/** Returns the last-seen config for a channel, or null. */
export function configFor(id) {
  return configs.get(id) || null;
}

// ── patch helpers ───────────────────────────────────────────────────────────

function patchText(el, value) {
  if (!el) return;
  const s = value == null ? '' : String(value);
  if (el.textContent !== s) el.textContent = s;
}

function patchAttr(el, name, value) {
  if (!el) return;
  const s = value == null ? '' : String(value);
  if (el.getAttribute(name) !== s) el.setAttribute(name, s);
}

function patchHidden(el, hidden) {
  if (!el) return;
  if (el.hidden !== hidden) el.hidden = hidden;
}

/** Replaces the variable part of an element's class list, keeping fixed classes. */
function patchVariantClass(el, fixed, variant) {
  if (!el) return;
  const next = variant ? `${fixed} ${variant}` : fixed;
  if (el.className !== next) el.className = next;
}

/** Collects every [data-f] descendant into a lookup, done once per cloned node. */
function collectRefs(root) {
  const refs = {};
  root.querySelectorAll('[data-f]').forEach(el => {
    refs[el.dataset.f] = el;
  });
  return refs;
}

// ── progress rendering ──────────────────────────────────────────────────────

/**
 * Fills the progress block for one channel.
 *
 * The mode decides the shape, and the important case is `live`: it gets a
 * cumulative count and no bar at all. A live stream has no total, and the bug
 * this replaces came from inventing a denominator for it — so the fix is to stop
 * dividing rather than to divide more carefully.
 */
function renderProgress(refs, status) {
  const p = status.progress;
  const mode = status.mode || 'live';

  if (!status.running || !p) {
    patchText(refs.progMain, '—');
    patchText(refs.progUnit, '');
    patchHidden(refs.progBar, true);
    ['metaRate', 'metaEta', 'metaQueue', 'metaUptime', 'metaExtra']
      .forEach(k => patchText(refs[k], ''));
    return;
  }

  const stored = p.segments_stored || 0;
  const total = p.total_segments || 0;
  const hasTotal = !!p.total_known && total > 0 && !p.total_capped;

  refs.progMain.classList.toggle('prog-done', mode === 'vod' || !!p.finalized);

  if (mode === 'static_ingesting' && p.total_known && total > 0) {
    // A capped total is a floor, so it is shown with a ≥ and drives no bar.
    if (p.total_capped) {
      patchText(refs.progMain, `${fmt.int(stored)} / ≥${fmt.int(total)}`);
      patchText(refs.progUnit, '段（上游數量過大，總數為下限）');
      patchHidden(refs.progBar, true);
    } else {
      patchText(refs.progMain, `${fmt.int(stored)} / ${fmt.int(total)}`);
      patchText(refs.progUnit, `段（${fmt.pct(stored, total)}%）`);
      patchHidden(refs.progBar, false);
      patchAttr(refs.progBar, 'max', total);
      patchAttr(refs.progBar, 'value', Math.min(stored, total));
      refs.progBar.classList.toggle('done', stored >= total);
    }
  } else if (mode === 'transitioning') {
    patchText(refs.progMain, '正在重建 VOD 索引…');
    patchText(refs.progUnit, '');
    // No value attribute: an indeterminate bar, because the rebuild reports no
    // intermediate progress and a fake percentage would be worse than none.
    patchHidden(refs.progBar, false);
    refs.progBar.removeAttribute('value');
    refs.progBar.classList.remove('done');
  } else if (mode === 'vod') {
    patchText(refs.progMain, `完成 · ${fmt.int(stored)}`);
    patchText(refs.progUnit, hasTotal && total !== stored
      ? `/ ${fmt.int(total)} 段`
      : '段');
    patchHidden(refs.progBar, true);
  } else {
    // live: a cumulative count, deliberately with no denominator and no bar.
    patchText(refs.progMain, `已處理 ${fmt.int(stored)}`);
    patchText(refs.progUnit, `段 · ${fmt.bytes(p.bytes_stored || 0)}`);
    patchHidden(refs.progBar, true);
  }

  // ── secondary line ──
  const rate = fmt.segRate(p.segments_per_sec);
  const bps = fmt.byteRate(p.bytes_per_sec);
  patchText(refs.metaRate, rate ? (bps ? `${rate}（${bps}）` : rate) : '');

  patchText(refs.metaEta,
    p.eta_seconds != null ? `剩餘約 ${fmt.duration(p.eta_seconds)}` : '');

  patchText(refs.metaQueue, p.queue_depth ? `佇列 ${fmt.int(p.queue_depth)}` : '');

  const up = fmt.since(p.started_at);
  patchText(refs.metaUptime, up ? `已運行 ${up}` : '');

  // Only surface failures and drops when there are some: a permanent "0 失敗"
  // is noise that trains the eye to skip the line where it matters.
  const extra = [];
  if (p.segments_dropped) extra.push(`已放棄 ${fmt.int(p.segments_dropped)} 段`);
  if (p.segments_failed) extra.push(`失敗 ${fmt.int(p.segments_failed)} 次`);
  patchText(refs.metaExtra, extra.join(' · '));

  // ── errors ──
  const srcErr = p.source_error;
  patchHidden(refs.sourceError, !srcErr);
  if (srcErr) {
    patchText(refs.sourceError,
      `上游異常（${srcErr.stage}）：${srcErr.message}`);
  }

  const segErr = p.last_segment_error;
  patchHidden(refs.segError, !segErr);
  if (segErr) {
    const where = segErr.rep_id ? ` ${segErr.rep_id}#${segErr.seg_no}` : '';
    patchText(refs.segError,
      `最後一次分段錯誤（${segErr.stage}${where}）：${segErr.message}`);
  }
}

// ── track rows ──────────────────────────────────────────────────────────────

const trackRowTpl = () => document.getElementById('trackRowTpl');

/**
 * Reconciles the per-track table for one card, keyed on the track's identity.
 * The server sorts tracks stably, so rows keep their positions across polls.
 */
function renderTracks(card, tracks) {
  const list = tracks || [];
  patchHidden(card.refs.tracksBox, list.length === 0);
  if (!list.length) {
    card.trackRows.forEach(row => row.root.remove());
    card.trackRows.clear();
    return;
  }

  patchText(card.refs.tracksSummary, `每軌明細（${list.length}）`);

  const seen = new Set();
  list.forEach(t => {
    const key = `${t.period_id}/${t.as_id}/${t.rep_id}`;
    seen.add(key);

    let row = card.trackRows.get(key);
    if (!row) {
      const frag = trackRowTpl().content.cloneNode(true);
      const root = frag.querySelector('tr');
      row = { root, refs: collectRefs(root) };
      card.trackRows.set(key, row);
      card.refs.tracksBody.appendChild(root);
    }

    patchText(row.refs.repId, t.rep_id);
    patchText(row.refs.mediaType, fmt.mediaLabel(t.media_type));
    patchText(row.refs.spec, fmt.trackSpec(t));
    patchText(row.refs.language, t.language || t.name || '—');
    patchText(row.refs.published, fmt.int(t.published));
    patchText(row.refs.committed, fmt.int(t.committed));
    patchText(row.refs.expired, fmt.int(t.expired));
    patchText(row.refs.maxSegNo, fmt.int(t.max_seg_no));
  });

  card.trackRows.forEach((row, key) => {
    if (!seen.has(key)) {
      row.root.remove();
      card.trackRows.delete(key);
    }
  });
}

// ── card rendering ──────────────────────────────────────────────────────────

function renderCard(card, id, cfg, status) {
  const refs = card.refs;

  patchText(refs.title, cfg.title || id);
  patchText(refs.url, cfg.mpd_url || '');
  patchAttr(refs.url, 'title', cfg.mpd_url || '');
  patchText(refs.id, id);

  patchVariantClass(refs.sourceBadge, 'badge src', fmt.sourceClass(cfg.source_type));
  patchText(refs.sourceBadge, fmt.sourceLabel(cfg.source_type));

  const degraded = !!(status.progress && status.progress.source_error);
  patchVariantClass(refs.runBadge, 'badge',
    status.running ? (degraded ? 'degraded' : 'running') : 'stopped');
  patchText(refs.runBadge, status.running ? (degraded ? '運行中（異常）' : '運行中') : '已停止');

  const mode = status.mode || 'live';
  patchHidden(refs.modeBadge, !status.running);
  patchVariantClass(refs.modeBadge, 'badge', fmt.modeClass(mode));
  patchText(refs.modeBadge, fmt.modeLabel(mode));

  patchAttr(refs.playLink, 'href', playerURL(id));

  // Manual VOD conversion only applies to a running live channel.
  patchHidden(refs.vodBtn, !(status.running && mode === 'live'));

  renderProgress(refs, status);
  renderTracks(card, status.tracks);
}

/**
 * Renders the whole channel list from one poll response.
 *
 * @param {HTMLElement} container the #channelList element
 * @param {Object} data the /admin/api/channels payload: id → {config, status}
 */
export function renderCards(container, data) {
  const ids = Object.keys(data).sort();

  configs.clear();
  ids.forEach(id => configs.set(id, data[id].config || {}));

  if (!ids.length) {
    cards.forEach(card => card.root.remove());
    cards.clear();
    container.textContent = '';
    const p = document.createElement('p');
    p.className = 'empty';
    p.textContent = '尚無 channel，請先新增。';
    container.appendChild(p);
    return;
  }

  // Drop the placeholder/empty paragraph once there is something to show.
  container.querySelectorAll('.empty').forEach(el => el.remove());

  const tpl = document.getElementById('channelCardTpl');

  ids.forEach(id => {
    let card = cards.get(id);
    if (!card) {
      const frag = tpl.content.cloneNode(true);
      const root = frag.querySelector('.ch-card');
      root.dataset.channelId = id;
      card = { root, refs: collectRefs(root), trackRows: new Map() };
      cards.set(id, card);
      container.appendChild(root);
    }
    renderCard(card, id, data[id].config || {}, data[id].status || {});
  });

  // Remove cards for channels that are gone.
  cards.forEach((card, id) => {
    if (!(id in data)) {
      card.root.remove();
      cards.delete(id);
    }
  });

  // Re-assert order only when it actually changed. Appending an element that is
  // already in the DOM moves it rather than recreating it, so this keeps focus
  // and selection intact even when it does run.
  const domOrder = Array.from(container.children)
    .filter(el => el.dataset && el.dataset.channelId)
    .map(el => el.dataset.channelId);
  if (domOrder.join(' ') !== ids.join(' ')) {
    ids.forEach(id => container.appendChild(cards.get(id).root));
  }
}

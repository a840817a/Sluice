// The add form, the edit modal, and the header key/value editor.

import * as api from './api.js';
import { configFor } from './cards.js';

// Checkbox fields, which FormData omits entirely when unchecked and so must be
// read from the elements rather than from the form data.
const CHECKBOXES = [
  'enable_playready',
  'enable_widevine',
  'keep_all_segments',
  'forward_client_ip',
];

// Fields the gateway freezes at channel start; changing one forces a restart.
// A VOD channel is more permissive, but the modal has no reliable mode at hand,
// so it warns conservatively and the server decides.
const RESTART_FIELDS = {
  source_type: '來源類型',
  mpd_url: '來源 URL',
  keep_all_segments: '保留所有片段',
};

let showMsg = () => {};
let onChanged = () => {};

/** Wires in the toast function and the "reload now" callback from main.js. */
export function configure({ message, changed }) {
  showMsg = message;
  onChanged = changed;
}

// ── header rows ─────────────────────────────────────────────────────────────

export function addHeaderRow(containerId, name = '', value = '') {
  const tpl = document.getElementById('headerRowTpl');
  const frag = tpl.content.cloneNode(true);
  const row = frag.querySelector('.header-row');
  row.querySelector('.hdr-name').value = name;
  row.querySelector('.hdr-value').value = value;
  row.querySelector('.btn-remove').addEventListener('click', () => row.remove());
  document.getElementById(containerId).appendChild(row);
}

function collectHeaders(containerId) {
  const rows = document.getElementById(containerId).querySelectorAll('.header-row');
  const result = [];
  rows.forEach(row => {
    const name = row.querySelector('.hdr-name').value.trim();
    const value = row.querySelector('.hdr-value').value.trim();
    if (name) result.push({ name, value });
  });
  return result;
}

function clearHeaders(containerId) {
  document.getElementById(containerId).replaceChildren();
}

function loadHeaders(containerId, headers) {
  clearHeaders(containerId);
  (headers || []).forEach(h => addHeaderRow(containerId, h.name, h.value));
}

/** Splits a textarea of one key per line into a list, dropping blank lines. */
function parseKeyLines(text) {
  return String(text || '').split('\n').map(l => l.trim()).filter(Boolean);
}

/**
 * Licence headers to submit from the form with this prefix, or null when its
 * DRM section is hidden (applyVisibility): they belong to the same licence
 * servers as the DRM fields, so they follow the same rule.
 */
function licenceHeaders(prefix) {
  if (document.getElementById(`${prefix}DrmSection`).hidden) return null;
  return collectHeaders(`${prefix}LicenseHeaders`);
}

/** Whether the form's source and key mode are the ones that take content keys. */
function takesKeys(form) {
  return form.elements['source_type'].value === 'dash' && form.elements['key_mode'].value === 'decrypt';
}

/** Builds the request body shared by the add and edit forms. */
function formBody(form, { keepEmptyHeaders }) {
  const body = {};
  for (const [k, v] of new FormData(form).entries()) {
    if (keepEmptyHeaders || v) body[k] = v;
  }
  body.enabled = true;
  // The textarea holds one kid:key per line and the API wants a list. Keys only
  // apply to a DASH source that decrypts, so text left in a hidden box is dropped.
  delete body.content_keys;
  const keys = parseKeyLines(form.elements['content_keys'] && form.elements['content_keys'].value);
  if (keys.length && takesKeys(form)) body.content_keys = keys;
  // A disabled checkbox belongs to a section that does not apply, like any
  // other disabled field FormData leaves out.
  CHECKBOXES.forEach(name => {
    const el = form.elements[name];
    if (el && !el.disabled) body[name] = el.checked;
  });
  return body;
}

// ── shared fields ───────────────────────────────────────────────────────────

/**
 * Stamps the shared channel-fields template into a form. Both forms carry the
 * same sections, so the markup lives once in #channelFieldsTpl and the ids the
 * rest of this file relies on (`addFetchHeaders`, `editSourceType`, ...) are
 * derived here from the prefix.
 */
function mountFields(form, prefix, { showRestart, editing = false }) {
  const tpl = document.getElementById('channelFieldsTpl');
  const frag = tpl.content.cloneNode(true);

  const ids = {
    'source-type': `${prefix}SourceType`,
    'source-url': `${prefix}SourceURL`,
    'fetch-headers': `${prefix}FetchHeaders`,
    'license-headers': `${prefix}LicenseHeaders`,
    'key-mode': `${prefix}KeyMode`,
    'key-mode-hint': `${prefix}KeyModeHint`,
    'keys-field': `${prefix}KeysField`,
    'drm-section': `${prefix}DrmSection`,
  };
  frag.querySelectorAll('[data-role]').forEach(el => {
    el.id = ids[el.dataset.role];
  });
  // Each "+ 新增" button targets the header list it sits under.
  frag.querySelectorAll('[data-add-header]').forEach(btn => {
    btn.dataset.addHeader = btn.previousElementSibling.id;
  });
  if (editing) {
    // The key mode is fixed at creation and keys are added from the channel
    // card, so an edit shows the mode read-only (a disabled field is not
    // submitted) and has no key input.
    frag.querySelector('[data-role="key-mode"]').disabled = true;
    frag.querySelector('[data-role="key-mode-hint"]').hidden = false;
    frag.querySelector('[data-role="keys-field"]').remove();
  }
  if (showRestart) {
    frag.querySelectorAll('[data-restart]').forEach(el => { el.hidden = false; });
  }

  // Prepend, so anything already in the container (the edit form's restart
  // note) stays below the fields.
  form.querySelector('[data-fields]').prepend(frag);
}

/**
 * Shows only the fields that apply to the chosen source and key mode: the URL
 * hint follows the protocol, DRM licence servers are meaningless for HLS, and
 * content keys exist only for DASH that decrypts. The hidden DRM fields are
 * disabled too, so the form does not submit them, and licenceHeaders submits
 * no licence headers while the section is hidden.
 */
function applyVisibility(prefix) {
  const sourceType = document.getElementById(`${prefix}SourceType`).value;
  const keyMode = document.getElementById(`${prefix}KeyMode`).value;
  document.getElementById(`${prefix}SourceURL`).placeholder = sourceType === 'hls'
    ? 'https://cdn.example.com/master.m3u8'
    : 'https://cdn.example.com/stream.mpd';
  const drm = document.getElementById(`${prefix}DrmSection`);
  drm.hidden = sourceType === 'hls';
  drm.querySelectorAll('input, select, textarea').forEach(el => { el.disabled = drm.hidden; });
  const keysField = document.getElementById(`${prefix}KeysField`);
  if (keysField) keysField.hidden = !(sourceType === 'dash' && keyMode === 'decrypt');
}

function bindVisibility(prefix) {
  ['SourceType', 'KeyMode'].forEach(name => {
    document.getElementById(`${prefix}${name}`).addEventListener('change', () => applyVisibility(prefix));
  });
  applyVisibility(prefix);
}

// ── drawers ─────────────────────────────────────────────────────────────────

function openDrawer(id) {
  document.getElementById(id).classList.add('open');
  const first = document.getElementById(id).querySelector('input:not([type="checkbox"])');
  if (first) first.focus();
}

function closeDrawer(id) {
  document.getElementById(id).classList.remove('open');
}

function isOpen(id) {
  return document.getElementById(id).classList.contains('open');
}

// Each opening owns its completion effects, even when the same channel is
// reopened before an earlier save returns. Pending requests still finish.
let addSession = 0;
let editSession = 0;
let adding = false;
let saving = false;

function setSubmitting(form, pending) {
  form.querySelector('button[type="submit"]').disabled = pending;
  form.setAttribute('aria-busy', String(pending));
}

// ── add form ────────────────────────────────────────────────────────────────

export function openAdd() {
  addSession++;
  openDrawer('addOverlay');
}

export function closeAdd() {
  addSession++;
  closeDrawer('addOverlay');
}

function initAddForm() {
  const form = document.getElementById('addForm');
  mountFields(form, 'add', { showRestart: false });
  bindVisibility('add');

  document.getElementById('openAdd').addEventListener('click', openAdd);
  document.getElementById('addOverlay').addEventListener('click', e => {
    if (e.target === e.currentTarget) closeAdd();
  });

  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (adding) return;
    const session = addSession;
    const body = formBody(form, { keepEmptyHeaders: false });

    const fetchHeaders = collectHeaders('addFetchHeaders');
    if (fetchHeaders.length) body.fetch_headers = fetchHeaders;
    const licenseHeaders = licenceHeaders('add');
    if (licenseHeaders && licenseHeaders.length) body.license_headers = licenseHeaders;

    adding = true;
    setSubmitting(form, true);
    try {
      const d = await api.createChannel(body);
      showMsg(`頻道「${(d && (d.title || d.id)) || ''}」已建立`, 'ok');
      if (session === addSession) {
        form.reset();
        applyVisibility('add');
        clearHeaders('addFetchHeaders');
        clearHeaders('addLicenseHeaders');
        closeAdd();
      }
      onChanged();
    } catch (err) {
      showMsg(err.message, 'err');
    } finally {
      adding = false;
      setSubmitting(form, false);
    }
  });
}

// ── edit modal ──────────────────────────────────────────────────────────────

let editingID = null;
// editingCfg is the config as loaded, so the form can tell which restart-forcing
// fields the operator has actually changed. Mirrors channel.RestartRequired on
// the server, which remains the authority — this is only a preview.
let editingCfg = null;

function currentEditValues() {
  const f = document.getElementById('editForm');
  return {
    source_type: f.elements['source_type'].value,
    mpd_url: f.elements['mpd_url'].value,
    keep_all_segments: f.elements['keep_all_segments'].checked,
  };
}

function refreshRestartNote() {
  if (!editingCfg) return;
  const now = currentEditValues();
  const orig = {
    source_type: editingCfg.source_type || 'dash',
    mpd_url: editingCfg.mpd_url || '',
    // Absent means "keep": the same default the gateway applies, so merely
    // opening the modal must not read as a change.
    keep_all_segments: editingCfg.keep_all_segments !== false,
  };
  const changed = Object.keys(RESTART_FIELDS).filter(k => now[k] !== orig[k]);
  const note = document.getElementById('editRestartNote');
  if (changed.length) {
    note.textContent = `儲存後會重啟此頻道（${changed.map(k => RESTART_FIELDS[k]).join('、')} 已變更），播放會短暫中斷。`;
    note.classList.add('on');
  } else {
    note.classList.remove('on');
  }
}

/**
 * Opens the edit modal for a channel. The config comes from the last poll's data
 * held in cards.js — it is never round-tripped through a DOM attribute, which is
 * both an escaping hazard and pointless work on every tick.
 */
export function openEdit(id) {
  const cfg = configFor(id);
  if (!cfg) {
    showMsg('找不到此 channel 的設定，請重新載入', 'err');
    return;
  }
  editSession++;
  editingID = id;
  editingCfg = cfg;

  document.getElementById('editTitle').textContent = cfg.title || id;
  const f = document.getElementById('editForm');
  ['title', 'mpd_url', 'playready_license_url', 'widevine_license_url'].forEach(k => {
    if (f.elements[k]) f.elements[k].value = cfg[k] || '';
  });
  // Configs written before HLS support have no source_type and mean DASH.
  f.elements['source_type'].value = cfg.source_type || 'dash';
  f.elements['key_mode'].value = cfg.key_mode || 'passthrough';
  f.elements['keep_all_segments'].checked = cfg.keep_all_segments !== false;
  f.elements['enable_playready'].checked = cfg.enable_playready !== false;
  f.elements['enable_widevine'].checked = cfg.enable_widevine !== false;
  f.elements['forward_client_ip'].checked = !!cfg.forward_client_ip;
  loadHeaders('editFetchHeaders', cfg.fetch_headers);
  loadHeaders('editLicenseHeaders', cfg.license_headers);

  applyVisibility('edit');
  refreshRestartNote();
  openDrawer('editOverlay');
}

export function closeEdit() {
  editSession++;
  editingID = null;
  editingCfg = null;
  document.getElementById('editRestartNote').classList.remove('on');
  closeDrawer('editOverlay');
}

function initEditModal() {
  const overlay = document.getElementById('editOverlay');
  const form = document.getElementById('editForm');
  mountFields(form, 'edit', { showRestart: true, editing: true });
  bindVisibility('edit');

  overlay.addEventListener('click', e => {
    if (e.target === e.currentTarget) closeEdit();
  });

  // Re-evaluate on every edit so the warning appears the moment a restart-forcing
  // field diverges, rather than only on submit.
  form.addEventListener('input', refreshRestartNote);
  form.addEventListener('change', refreshRestartNote);

  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (!editingID || saving) return;
    const session = editSession;
    const id = editingID;

    const body = formBody(form, { keepEmptyHeaders: true });
    // Sent even when empty: an omitted list would leave the old headers in place,
    // making it impossible to remove the last one. A source without licence
    // servers sends no licence headers.
    body.fetch_headers = collectHeaders('editFetchHeaders');
    const licenseHeaders = licenceHeaders('edit');
    if (licenseHeaders) body.license_headers = licenseHeaders;

    saving = true;
    setSubmitting(form, true);
    try {
      const d = await api.updateChannel(id, body);
      // Report what the server actually did: most edits no longer interrupt the
      // channel, and claiming a restart that did not happen is misleading.
      const fields = ((d && d.restart_fields) || []).map(k => RESTART_FIELDS[k] || k);
      showMsg(d && d.restarted
        ? (fields.length ? `已更新並重啟（${fields.join('、')} 已變更）` : '已更新並重啟')
        : '已更新（即時套用，未中斷）', 'ok');
      if (session === editSession) closeEdit();
      onChanged();
    } catch (err) {
      showMsg(err.message, 'err');
    } finally {
      saving = false;
      setSubmitting(form, false);
    }
  });
}

// ── channel actions ─────────────────────────────────────────────────────────

export async function deleteChannel(id) {
  const cfg = configFor(id);
  const label = (cfg && cfg.title) || id;
  if (!confirm(`確定刪除 channel「${label}」？`)) return;
  try {
    await api.deleteChannel(id);
    showMsg('已刪除', 'ok');
    onChanged();
  } catch (err) {
    showMsg(err.message, 'err');
  }
}

export async function restartChannel(id) {
  if (!confirm(`重啟 channel「${id}」？播放會短暫中斷。`)) return;
  try {
    await api.restartChannel(id);
    showMsg('已重啟', 'ok');
    onChanged();
  } catch (err) {
    showMsg(err.message || '重啟失敗', 'err');
  }
}

export async function transitionToVOD(id) {
  if (!confirm(`將 channel「${id}」手動轉換為 VOD 模式？`)) return;
  try {
    await api.transitionToVOD(id);
    showMsg('已觸發 VOD 轉換', 'ok');
    onChanged();
  } catch (err) {
    showMsg(err.message || '轉換失敗', 'err');
  }
}

export async function restoreToLive(id) {
  if (!confirm(
    `將 channel「${id}」還原為直播模式？\n\n` +
    '已錄下的片段都會保留。若此頻道未勾選「保留所有片段」，還原後只會留下滑動視窗' +
    '內的片段，且在 cleanup: on_expire 設定下舊片段會開始被刪除。'
  )) return;
  try {
    await api.restoreToLive(id);
    showMsg('已還原為直播模式', 'ok');
    onChanged();
  } catch (err) {
    showMsg(err.message || '還原失敗', 'err');
  }
}

/**
 * Submits the card's "add keys" form. Failures (a KID already stored is a 409)
 * are shown on the card as well as in the toast, because the toast clears itself
 * and the operator still has the rejected text in front of them.
 */
async function submitKeys(form) {
  const card = form.closest('[data-channel-id]');
  if (!card || form.dataset.busy) return;
  const id = card.dataset.channelId;
  const box = form.querySelector('[data-f="keysError"]');
  const keys = parseKeyLines(form.elements['content_keys'].value);
  const fail = text => { box.textContent = text; box.hidden = false; showMsg(text, 'err'); };
  if (!keys.length) { fail('請輸入至少一組 kid:key'); return; }

  box.hidden = true;
  form.dataset.busy = '1';
  setSubmitting(form, true);
  try {
    const d = await api.addKeys(id, keys);
    form.elements['content_keys'].value = '';
    showMsg(`已新增 ${((d && d.added_kids) || keys).length} 把 key`, 'ok');
    onChanged();
  } catch (err) {
    fail(err.message);
  } finally {
    delete form.dataset.busy;
    setSubmitting(form, false);
  }
}

// ── init ────────────────────────────────────────────────────────────────────

export function initForms() {
  initAddForm();
  initEditModal();

  // One delegated handler for every "+ 新增" button, in both the add form and the
  // modal, so no markup needs an inline onclick (which a module cannot provide).
  document.addEventListener('click', e => {
    const btn = e.target.closest('[data-add-header]');
    if (btn) addHeaderRow(btn.dataset.addHeader);
  });

  document.addEventListener('click', e => {
    if (e.target.closest('[data-action="close-edit"]')) closeEdit();
    if (e.target.closest('[data-action="close-add"]')) closeAdd();
  });

  document.addEventListener('submit', e => {
    const form = e.target.closest && e.target.closest('[data-keys-form]');
    if (!form) return;
    e.preventDefault();
    submitKeys(form);
  });

  document.addEventListener('keydown', e => {
    if (e.key !== 'Escape') return;
    if (isOpen('editOverlay')) closeEdit();
    else if (isOpen('addOverlay')) closeAdd();
  });
}

// The add form, the edit modal, and the header key/value editor.

import * as api from './api.js';
import { configFor } from './cards.js';

// Checkbox fields, which FormData omits entirely when unchecked and so must be
// read from the elements rather than from the form data.
const CHECKBOXES = [
  'enable_playready',
  'enable_widevine',
  'enable_vod_transition',
  'keep_all_segments',
  'forward_client_ip',
];

// Fields the gateway freezes at channel start; changing one forces a restart.
// A VOD channel is more permissive, but the modal has no reliable mode at hand,
// so it warns conservatively and the server decides.
const RESTART_FIELDS = {
  source_type: '來源類型',
  mpd_url: '來源 URL',
  hls_key_mode: 'HLS AES-128 處理',
  enable_vod_transition: '自動轉 VOD',
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

/** Builds the request body shared by the add and edit forms. */
function formBody(form, { keepEmptyHeaders }) {
  const body = {};
  for (const [k, v] of new FormData(form).entries()) {
    if (keepEmptyHeaders || v) body[k] = v;
  }
  body.enabled = true;
  CHECKBOXES.forEach(name => {
    if (form.elements[name]) body[name] = form.elements[name].checked;
  });
  return body;
}

// ── add form ────────────────────────────────────────────────────────────────

function initAddForm() {
  const form = document.getElementById('addForm');
  const sourceType = document.getElementById('addSourceType');
  const sourceURL = document.getElementById('addSourceURL');

  // Swap the URL hint so the expected format is obvious for the chosen protocol.
  sourceType.addEventListener('change', () => {
    sourceURL.placeholder = sourceType.value === 'hls'
      ? 'https://cdn.example.com/master.m3u8'
      : 'https://cdn.example.com/stream.mpd';
  });

  form.addEventListener('submit', async e => {
    e.preventDefault();
    const body = formBody(form, { keepEmptyHeaders: false });

    const fetchHeaders = collectHeaders('addFetchHeaders');
    if (fetchHeaders.length) body.fetch_headers = fetchHeaders;
    const licenseHeaders = collectHeaders('addLicenseHeaders');
    if (licenseHeaders.length) body.license_headers = licenseHeaders;

    try {
      const d = await api.createChannel(body);
      showMsg(`Channel「${(d && (d.title || d.id)) || ''}」已建立`, 'ok');
      form.reset();
      clearHeaders('addFetchHeaders');
      clearHeaders('addLicenseHeaders');
      onChanged();
    } catch (err) {
      showMsg(err.message, 'err');
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
    hls_key_mode: f.elements['hls_key_mode'].value,
    enable_vod_transition: f.elements['enable_vod_transition'].checked,
    keep_all_segments: f.elements['keep_all_segments'].checked,
  };
}

function refreshRestartNote() {
  if (!editingCfg) return;
  const now = currentEditValues();
  const orig = {
    source_type: editingCfg.source_type || 'dash',
    mpd_url: editingCfg.mpd_url || '',
    hls_key_mode: editingCfg.hls_key_mode || 'passthrough',
    enable_vod_transition: !!editingCfg.enable_vod_transition,
    keep_all_segments: !!editingCfg.keep_all_segments,
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
  editingID = id;
  editingCfg = cfg;

  document.getElementById('editTitle').textContent = cfg.title || id;
  const f = document.getElementById('editForm');
  ['title', 'mpd_url', 'playready_license_url', 'widevine_license_url'].forEach(k => {
    if (f.elements[k]) f.elements[k].value = cfg[k] || '';
  });
  // Configs written before HLS support have no source_type and mean DASH.
  f.elements['source_type'].value = cfg.source_type || 'dash';
  f.elements['hls_key_mode'].value = cfg.hls_key_mode || 'passthrough';
  f.elements['enable_vod_transition'].checked = !!cfg.enable_vod_transition;
  f.elements['keep_all_segments'].checked = !!cfg.keep_all_segments;
  f.elements['enable_playready'].checked = cfg.enable_playready !== false;
  f.elements['enable_widevine'].checked = cfg.enable_widevine !== false;
  f.elements['forward_client_ip'].checked = !!cfg.forward_client_ip;
  loadHeaders('editFetchHeaders', cfg.fetch_headers);
  loadHeaders('editLicenseHeaders', cfg.license_headers);

  refreshRestartNote();
  document.getElementById('editOverlay').classList.add('open');
}

export function closeEdit() {
  editingID = null;
  editingCfg = null;
  document.getElementById('editRestartNote').classList.remove('on');
  document.getElementById('editOverlay').classList.remove('open');
}

function initEditModal() {
  const overlay = document.getElementById('editOverlay');
  const form = document.getElementById('editForm');

  overlay.addEventListener('click', e => {
    if (e.target === e.currentTarget) closeEdit();
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && overlay.classList.contains('open')) closeEdit();
  });

  // Re-evaluate on every edit so the warning appears the moment a restart-forcing
  // field diverges, rather than only on submit.
  form.addEventListener('input', refreshRestartNote);
  form.addEventListener('change', refreshRestartNote);

  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (!editingID) return;

    const body = formBody(form, { keepEmptyHeaders: true });
    // Sent even when empty: an omitted list would leave the old headers in place,
    // making it impossible to remove the last one.
    body.fetch_headers = collectHeaders('editFetchHeaders');
    body.license_headers = collectHeaders('editLicenseHeaders');

    try {
      const d = await api.updateChannel(editingID, body);
      // Report what the server actually did: most edits no longer interrupt the
      // channel, and claiming a restart that did not happen is misleading.
      const fields = ((d && d.restart_fields) || []).map(k => RESTART_FIELDS[k] || k);
      showMsg(d && d.restarted
        ? (fields.length ? `已更新並重啟（${fields.join('、')} 已變更）` : '已更新並重啟')
        : '已更新（即時套用，未中斷）', 'ok');
      closeEdit();
      onChanged();
    } catch (err) {
      showMsg(err.message, 'err');
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
  });
}

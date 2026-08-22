// Pure formatting helpers. Nothing here touches the DOM or fetches anything, so
// each function can be reasoned about (and eyeballed in a console) on its own.

const NUM = new Intl.NumberFormat('zh-Hant');

/** Thousands-separated integer. */
export function int(n) {
  return NUM.format(Math.round(Number(n) || 0));
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/** Human byte size, e.g. 34.2 GiB. */
export function bytes(n) {
  let v = Number(n) || 0;
  if (v < 1024) return `${Math.round(v)} B`;
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${BYTE_UNITS[i]}`;
}

/** Throughput in segments per second, or '' when idle. */
export function segRate(segPerSec) {
  const v = Number(segPerSec) || 0;
  if (v <= 0) return '';
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} 段/秒`;
}

/** Throughput in bytes per second, or '' when idle. */
export function byteRate(bps) {
  const v = Number(bps) || 0;
  if (v <= 0) return '';
  return `${bytes(v)}/秒`;
}

/**
 * Coarse duration in Traditional Chinese. Deliberately never more precise than
 * two units — an ETA good to the second implies confidence the estimate has not
 * earned.
 */
export function duration(totalSeconds) {
  let s = Math.max(0, Math.round(Number(totalSeconds) || 0));
  if (s < 60) return `${s} 秒`;
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return h > 0 ? `${d} 天 ${h} 小時` : `${d} 天`;
  if (h > 0) return m > 0 ? `${h} 小時 ${m} 分` : `${h} 小時`;
  return `${m} 分`;
}

/** Percentage of a/b as an integer, clamped to 0..100. */
export function pct(a, b) {
  const den = Number(b) || 0;
  if (den <= 0) return 0;
  const v = Math.round((Number(a) || 0) / den * 100);
  return Math.max(0, Math.min(100, v));
}

/** Elapsed time since an ISO timestamp, or '' if absent/unparseable. */
export function since(iso) {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  return duration((Date.now() - t) / 1000);
}

export const MODE_LABEL = {
  live: 'LIVE 中',
  vod: 'VOD',
  static_ingesting: '下載中',
  transitioning: '轉換中',
};

export const MODE_CLASS = {
  live: 'live',
  vod: 'vod',
  static_ingesting: 'ingesting',
  transitioning: 'transition',
};

export function modeLabel(mode) {
  return MODE_LABEL[mode] || mode || '';
}

export function modeClass(mode) {
  return MODE_CLASS[mode] || '';
}

/**
 * Upstream protocol label. An absent source_type means DASH, which is what every
 * channel created before HLS support has.
 */
export function sourceLabel(sourceType) {
  return sourceType === 'hls' ? 'HLS' : 'DASH';
}

export function sourceClass(sourceType) {
  return sourceType === 'hls' ? 'src-hls' : 'src-dash';
}

const MEDIA_LABEL = { video: '影像', audio: '音訊', subtitle: '字幕', text: '字幕' };

export function mediaLabel(mediaType) {
  return MEDIA_LABEL[mediaType] || mediaType || '—';
}

/**
 * One-line technical spec for a track: resolution, frame rate and bitrate as
 * available. Returns '—' when the manifest gave nothing to show.
 */
export function trackSpec(track) {
  const parts = [];
  if (track.width && track.height) parts.push(`${track.width}×${track.height}`);
  if (track.frame_rate) parts.push(`${Math.round(track.frame_rate)}fps`);
  if (track.bandwidth) parts.push(`${(track.bandwidth / 1_000_000).toFixed(2)} Mbps`);
  if (track.channels) parts.push(`${track.channels}ch`);
  if (!parts.length && track.codecs) parts.push(track.codecs);
  return parts.length ? parts.join(' · ') : '—';
}

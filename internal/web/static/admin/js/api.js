// Admin REST calls. Every request funnels through request(), so there is exactly
// one place that decides what an error looks like.

const BASE = '/admin/api/channels';

/**
 * Thrown for any non-2xx response, carrying the server's own error message when
 * it sent one so callers never have to re-parse the body.
 */
export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function request(path, { method = 'GET', body, signal } = {}) {
  const init = { method, signal, headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }

  const res = await fetch(path, init);

  // 204 and empty bodies are legal; treat an unparseable body as no data rather
  // than as a failure, so a successful action is never reported as an error.
  let data = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }

  if (!res.ok) {
    throw new ApiError((data && data.error) || `HTTP ${res.status}`, res.status);
  }
  return data;
}

/** Lists every configured channel with its config and live status. */
export function listChannels(signal) {
  return request(BASE, { signal });
}

export function createChannel(cfg) {
  return request(BASE, { method: 'POST', body: cfg });
}

export function updateChannel(id, cfg) {
  return request(`${BASE}/${encodeURIComponent(id)}`, { method: 'PUT', body: cfg });
}

export function deleteChannel(id) {
  return request(`${BASE}/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function restartChannel(id) {
  return request(`${BASE}/${encodeURIComponent(id)}/restart`, { method: 'POST' });
}

export function transitionToVOD(id) {
  return request(`${BASE}/${encodeURIComponent(id)}/transition-to-vod`, { method: 'POST' });
}

/** Public player URL for a channel. */
export function playerURL(id) {
  return `/player/${encodeURIComponent(id)}`;
}

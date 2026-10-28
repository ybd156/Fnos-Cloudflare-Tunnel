/*
 * Backend request helper.
 *
 * Plain ES2020, no build step, no dependencies. The gateway prefix is injected
 * by the backend through window.__CF_BASE__ so the same code works both behind
 * the fnOS unified gateway (/app/cloudflared/...) and on a direct port.
 *
 * This is the only module that knows the /api prefix; every caller passes a
 * path that starts with a slash.
 */

const BASE = window.__CF_BASE__ || '';
export const API = BASE + '/api';

export async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(API + path, opts);
  const text = await res.text();
  let data = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch (err) {
    data = { error: text.slice(0, 200) };
  }
  if (!res.ok) throw new Error(data.error || `请求失败 (${res.status})`);
  return data;
}

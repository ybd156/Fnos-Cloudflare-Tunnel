/*
 * Shared UI state and the few helpers every other module needs.
 *
 * This module is why the split needs no import cycles: state, the `$` lookup,
 * the toast strip and the clipboard fallback are the only things the renderer,
 * the modal and the per-view modules have in common. The dependency graph is a
 * straight line
 *
 *     core <- api <- render <- modal <- views <- app
 *
 * and nothing here talks to the network.
 */

/* The single mutable snapshot the whole page renders from. Polling replaces
   parts of it and the renderers compare against what is already on screen. */
export const state = {
  meta: null,
  settings: null,
  tunnels: [],
  account: null,
  accountTunnels: [],
  accountQuery: '',
  editing: null,
  filter: 'all',
  logs: {},
  appLogLines: [],
  openLogs: {},
  /* Which cards have their "more information" fold open. Kept here so a poll
     that rebuilds the list does not slam the fold shut under the reader. */
  openDetails: {},
  /* Line numbers are a reading aid, not a per-tunnel setting: one switch for
     every log box keeps the cards from disagreeing with each other. */
  logNumbers: false,
  busy: {},
};

export const $ = (sel) => document.querySelector(sel);

let toastTimer = null;
export function toast(message, kind) {
  const el = $('#toast');
  el.textContent = message;
  el.className = 'toast ' + (kind || '');
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add('hidden'), kind === 'err' ? 6000 : 2800);
}

export function setBusy(key, value) {
  state.busy[key] = value;
}

export function isBusy(key) {
  return !!state.busy[key];
}

/* True while the tab is hidden. Polling is skipped then: a background tab has
   no one reading it, and on a NAS the wakeups are pure waste. */
export function isHidden() {
  return document.visibilityState === 'hidden';
}

/* Copying to the clipboard has to work on plain http (a NAS on the LAN is not
   a secure context), where navigator.clipboard does not exist. The textarea
   fallback is the only path that works there. */
export async function copyText(text) {
  const body = String(text == null ? '' : text);
  if (navigator.clipboard && navigator.clipboard.writeText && window.isSecureContext) {
    await navigator.clipboard.writeText(body);
    return;
  }
  const area = document.createElement('textarea');
  area.value = body;
  area.setAttribute('readonly', '');
  area.style.position = 'fixed';
  area.style.top = '-1000px';
  document.body.appendChild(area);
  area.select();
  const ok = document.execCommand('copy');
  area.remove();
  if (!ok) throw new Error('浏览器拒绝了复制操作');
}

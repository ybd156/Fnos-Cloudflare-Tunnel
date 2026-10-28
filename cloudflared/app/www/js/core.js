/*
 * Shared UI state and the few helpers every other module needs.
 *
 * This module is why the split needs no import cycles: state, the `$` lookup
 * and the toast strip are the only things the renderer, the modal and the
 * per-view modules have in common. The dependency graph is a straight line
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
  openLogs: {},
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

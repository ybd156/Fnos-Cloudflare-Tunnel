/*
 * Cloudflare Tunnel for fnOS — web UI entry point.
 *
 * Plain ES2020 modules, no build step, no dependencies. index.html loads this
 * file with <script type="module">, which also means the whole DOM exists by
 * the time anything here runs -- the handlers below are bound once, at module
 * evaluation, exactly like the single-file version did from the end of <body>.
 *
 * Module map (dependencies point only one way):
 *
 *   core.js    shared state, `$`, toast, busy flags, tab-visibility helper
 *   api.js     fetch wrapper for /api, the only place that knows the prefix
 *   render.js  tunnel list markup, status summary, in-place log boxes
 *   modal.js   tunnel form and the modal shell (also the one-line prompt)
 *   views.js   account / settings / app-log views, tab switching
 *   app.js     this file: card actions, event wiring, polling, boot
 *
 * The rendering contract that the poll loop depends on is documented at the top
 * of render.js: unchanged markup is never written, log text never enters the
 * list markup, and a log box follows the tail only when the reader is already
 * at the bottom.
 */

import { state, $, toast, setBusy, isHidden } from './core.js';
import { api } from './api.js';
import { renderTunnels, refreshTunnels, loadTunnelLog, logBoxFor, refreshMeta } from './render.js';
import { openTunnelModal, closeTunnelModal, handleModalSave, trapModalFocus } from './modal.js';
import {
  switchView,
  pollAccount,
  onAccountTunnelClick,
  onAccountSearchInput,
  onAccountRefreshClick,
  onCreateTunnelClick,
  saveSettings,
  updateBinary,
  refreshAppLog,
  copyAppLog,
  openNewTunnelModal,
} from './views.js';

/* ── card actions ─────────────────────────────────────────────────────── */

async function tunnelAction(id, act) {
  const tunnel = state.tunnels.find((t) => t.id === id);
  if (!tunnel) return;
  const key = `tunnel:${id}`;

  if (act === 'delete') {
    if (!confirm(`确定删除隧道「${tunnel.name}」吗？此操作只移除本地配置，不会删除 Cloudflare 上的隧道。`)) return;
    setBusy(key, true);
    try {
      await api('DELETE', `/tunnels/${encodeURIComponent(id)}`);
      delete state.openLogs[id];
      delete state.logs[id];
      toast('已删除');
      await refreshTunnels();
    } catch (err) {
      toast(err.message, 'err');
    } finally {
      setBusy(key, false);
    }
    return;
  }

  if (act === 'logs') {
    state.openLogs[id] = !state.openLogs[id];
    if (state.openLogs[id]) {
      try {
        await loadTunnelLog(id);
      } catch (err) {
        toast(err.message, 'err');
      }
    }
    renderTunnels();
    /* A freshly opened box should show the newest lines, not the oldest. */
    const box = logBoxFor(id);
    if (box) box.scrollTop = box.scrollHeight;
    return;
  }

  if (act === 'edit') {
    openTunnelModal(tunnel);
    return;
  }

  setBusy(key, true);
  renderTunnels();
  try {
    const data = await api('POST', `/tunnels/${encodeURIComponent(id)}/${act}`);
    if (act === 'dns') toast(data.message || 'DNS 路由已检查');
    else if (act === 'start') toast('已发送启动指令');
    else if (act === 'stop') toast('已发送停止指令');
    else toast('正在重启');
    await new Promise((r) => setTimeout(r, 900));
    await refreshTunnels();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    setBusy(key, false);
    renderTunnels();
  }
}

/* ── event wiring ─────────────────────────────────────────────────────── */

$('#tunnel-list').addEventListener('click', (ev) => {
  const btn = ev.target.closest('button[data-act]');
  if (!btn) return;
  const card = btn.closest('.tunnel-card');
  if (!card) return;
  tunnelAction(card.dataset.id, btn.dataset.act);
});

$('#tunnel-summary').addEventListener('click', (ev) => {
  const chip = ev.target.closest('[data-filter]');
  if (!chip) return;
  state.filter = chip.dataset.filter;
  renderTunnels();
});

/* The modal footer serves the tunnel form and the one-line prompt; modal.js
   decides which one is open. */
$('#modal-save').addEventListener('click', handleModalSave);
$('#modal-cancel').addEventListener('click', closeTunnelModal);
$('#modal-close').addEventListener('click', closeTunnelModal);
$('#modal-backdrop').addEventListener('click', (ev) => {
  if (ev.target === $('#modal-backdrop')) closeTunnelModal();
});
$('#modal-backdrop').addEventListener('keydown', trapModalFocus);

$('#account-tunnel-list').addEventListener('click', onAccountTunnelClick);

const accountSearch = $('#account-search');
if (accountSearch) {
  accountSearch.addEventListener('input', onAccountSearchInput);
}

$('#btn-account-refresh').addEventListener('click', onAccountRefreshClick);
$('#btn-create-tunnel').addEventListener('click', onCreateTunnelClick);

$('#btn-save-settings').addEventListener('click', saveSettings);
$('#btn-update-bin').addEventListener('click', updateBinary);

$('#btn-refresh-applog').addEventListener('click', refreshAppLog);
$('#btn-copy-applog').addEventListener('click', copyAppLog);

document.querySelectorAll('.tab').forEach((tab) => {
  tab.addEventListener('click', () => switchView(tab.dataset.view));
});

$('#btn-add').addEventListener('click', openNewTunnelModal);
$('#btn-add-empty').addEventListener('click', openNewTunnelModal);

document.querySelectorAll('[data-goto]').forEach((btn) => {
  btn.addEventListener('click', () => switchView(btn.dataset.goto));
});

$('#btn-refresh').addEventListener('click', async () => {
  try {
    await refreshTunnels();
    toast('已刷新');
  } catch (err) {
    toast(err.message, 'err');
  }
});

document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && !$('#modal-backdrop').classList.contains('hidden')) closeTunnelModal();
});

/* ── polling ──────────────────────────────────────────────────────────── */

let polling = false;

/* Live status refresh while the tunnel list is visible.
 *
 * renderTunnels() already skips the DOM when nothing changed, so the remaining
 * cost is the request itself. The guards below keep a backgrounded tab from
 * polling a NAS for nothing, and stop a refresh from fighting the user while a
 * modal is open. */
setInterval(async () => {
  if (polling) return;
  if (isHidden()) return;
  if (!$('#view-tunnels').classList.contains('is-active')) return;
  if (!$('#modal-backdrop').classList.contains('hidden')) return;
  polling = true;
  try {
    await refreshTunnels();
    for (const id of Object.keys(state.openLogs)) {
      if (state.openLogs[id]) await loadTunnelLog(id);
    }
    renderTunnels();
  } catch (err) {
    /* transient errors are ignored during polling */
  } finally {
    polling = false;
  }
}, 3000);

/* Account polling while the account tab is visible and login is pending. */
setInterval(async () => {
  if (isHidden()) return;
  if (!$('#view-account').classList.contains('is-active')) return;
  if (state.account && state.account.loggedIn) return;
  if (!state.account || state.account.status !== 'waiting') return;
  const acc = await pollAccount();
  if (acc.loggedIn) toast('登录成功');
}, 3000);

/* Coming back to a backgrounded tab should show current data immediately rather
   than waiting out the next tick. */
document.addEventListener('visibilitychange', () => {
  if (isHidden()) return;
  if ($('#view-tunnels').classList.contains('is-active')) {
    refreshTunnels().catch(() => {});
  }
  if ($('#view-logs').classList.contains('is-active')) {
    refreshAppLog().catch(() => {});
  }
});

/* ── boot ─────────────────────────────────────────────────────────────── */

(async function boot() {
  try {
    await refreshMeta();
  } catch (err) {
    toast(`无法连接后端：${err.message}`, 'err');
  }
  await switchView('tunnels');
})();

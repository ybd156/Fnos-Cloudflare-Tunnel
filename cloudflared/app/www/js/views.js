/*
 * The account, settings and app-log views, plus the tab switcher that decides
 * which of them is on screen.
 *
 * Every view follows the same shape: fetch into state, then render from state.
 * The event handlers live next to the markup they act on and are wired up by
 * app.js, which keeps the DOM contract in one place.
 */

import { state, $, toast } from './core.js';
import { api } from './api.js';
import { esc, refreshTunnels, refreshMeta } from './render.js';
import { openTunnelModal, openPromptModal, closeTunnelModal } from './modal.js';

/* ── account view ─────────────────────────────────────────────────────── */

export function renderAccount() {
  const body = $('#account-body');
  const card = $('#account-tunnels-card');
  const acc = state.account;

  if (!acc) {
    body.innerHTML = '<p class="hint"><span class="spin"></span> 正在读取账户状态…</p>';
    return;
  }

  if (!acc.loggedIn) {
    card.classList.add('hidden');
    const waiting = acc.status === 'waiting' && acc.url;
    body.innerHTML = `
      <div class="alert info"><span class="alert-body">本地管理隧道需要先在 Cloudflare 账户上授权。授权文件保存在 ${esc(acc.certPath)}。</span></div>
      ${waiting
        ? `<p>请复制下面的链接到浏览器完成授权（选择一个域名）：</p>
           <p style="margin:10px 0"><a class="link" href="${esc(acc.url)}" target="_blank" rel="noreferrer">${esc(acc.url)}</a></p>
           <p class="hint"><span class="spin"></span> 等待授权完成… 授权成功后本页会自动刷新。</p>`
        : `<button class="btn btn-primary" id="btn-login">登录 Cloudflare</button>`}
      ${acc.error ? `<div class="alert err" style="margin-top:12px"><span class="alert-body">${esc(acc.error)}</span></div>` : ''}`;

    const loginBtn = $('#btn-login');
    if (loginBtn) {
      loginBtn.addEventListener('click', async () => {
        try {
          await api('POST', '/account/login');
          toast('已生成授权链接');
          await pollAccount();
        } catch (err) {
          toast(err.message, 'err');
        }
      });
    }
    return;
  }

  card.classList.remove('hidden');
  body.innerHTML = `
    <div class="alert ok"><span class="alert-body">已登录，授权文件：${esc(acc.certPath)}</span></div>
    <button class="btn btn-danger" id="btn-logout">退出登录（删除 cert.pem）</button>`;

  const logoutBtn = $('#btn-logout');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', async () => {
      if (!confirm('退出登录会删除本机的 cert.pem，已运行的隧道不受影响。确定继续？')) return;
      try {
        await api('POST', '/account/logout');
        toast('已退出登录');
        state.accountTunnels = [];
        await pollAccount();
      } catch (err) {
        toast(err.message, 'err');
      }
    });
  }

  renderAccountTunnels();
}

export function renderAccountTunnels() {
  const list = $('#account-tunnel-list');
  const q = state.accountQuery.trim().toLowerCase();
  const all = state.accountTunnels;
  const tunnels = q ? all.filter((t) => String(t.name || '').toLowerCase().includes(q)) : all;

  if (!all.length) {
    list.innerHTML = '<p class="hint">还没有命名隧道，点击右上角「+ 新建隧道」创建一个。</p>';
    return;
  }
  if (!tunnels.length) {
    list.innerHTML = `<p class="hint">没有匹配「${esc(state.accountQuery)}」的隧道。</p>`;
    return;
  }
  list.innerHTML = tunnels
    .map((t) => `
<div class="row">
  <div class="row-main">
    <strong>${esc(t.name)}</strong>
    <span>${esc(t.id)} · ${t.connections} 个活动连接${t.createdAt ? ` · 创建于 ${esc(String(t.createdAt).slice(0, 10))}` : ''}</span>
  </div>
  <div class="row-actions">
    <button class="btn btn-sm" data-account-act="use" data-name="${esc(t.name)}">配置隧道</button>
    <button class="btn btn-sm btn-danger" data-account-act="delete" data-name="${esc(t.name)}">删除</button>
  </div>
</div>`)
    .join('');
}

export async function refreshAccountTunnels() {
  try {
    const data = await api('GET', '/account/tunnels');
    state.accountTunnels = data.tunnels || [];
  } catch (err) {
    state.accountTunnels = [];
  }
  renderAccountTunnels();
}

export async function pollAccount() {
  try {
    state.account = await api('GET', '/account');
  } catch (err) {
    state.account = { loggedIn: false, status: 'error', error: err.message, certPath: '' };
  }
  renderAccount();
  if (state.account.loggedIn && !state.accountTunnels.length) await refreshAccountTunnels();
  return state.account;
}

export async function onAccountTunnelClick(ev) {
  const btn = ev.target.closest('button[data-account-act]');
  if (!btn) return;
  const name = btn.dataset.name;
  const act = btn.dataset.accountAct;

  if (act === 'delete') {
    if (!confirm(`确定删除 Cloudflare 上的隧道「${name}」吗？该操作不可恢复。`)) return;
    try {
      await api('DELETE', `/account/tunnels/${encodeURIComponent(name)}`);
      toast('已删除');
      await refreshAccountTunnels();
    } catch (err) {
      toast(err.message, 'err');
    }
    return;
  }

  if (act === 'use') {
    // 该云端隧道若已有本地配置，直接进入编辑；否则新建并预填。
    // 注意：必须把预填对象作为第二个参数传入，否则 openTunnelModal 会把它
    // 当成「正在编辑的隧道」（没有 id），保存时会 PUT /tunnels/undefined。
    const existing = (state.tunnels || []).find((t) => t.type === 'local' && t.tunnelName === name);
    if (existing) {
      openTunnelModal(existing);
      return;
    }
    openTunnelModal(null, {
      type: 'local',
      name,
      tunnelName: name,
      autoStart: true,
      ingress: [{ hostname: '', service: '', noTLSVerify: false }],
      catchAll: 'http_status:404',
    });
  }
}

export function onAccountSearchInput(ev) {
  state.accountQuery = ev.target.value;
  renderAccountTunnels();
}

export async function onAccountRefreshClick() {
  await pollAccount();
  if (state.account && state.account.loggedIn) await refreshAccountTunnels();
  toast('已刷新');
}

// 点按钮才弹窗问名字：常驻输入框占地方，而创建隧道是低频操作。
export function onCreateTunnelClick() {
  openPromptModal({
    title: '新建隧道',
    label: '隧道名称',
    placeholder: 'nas',
    hint: '只能用字母、数字、点、下划线、短横线。创建后会显示在上方列表里。',
    onSave: async (raw) => {
      const name = (raw || '').trim();
      if (!name) {
        toast('请填写隧道名称', 'err');
        return;
      }
      const btn = $('#btn-create-tunnel');
      btn.disabled = true;
      btn.textContent = '创建中…';
      try {
        await api('POST', '/account/tunnels', { name });
        closeTunnelModal();
        toast('隧道已创建');
        await refreshAccountTunnels();
      } catch (err) {
        toast(err.message, 'err');
      } finally {
        btn.disabled = false;
        btn.textContent = '+ 新建隧道';
      }
    },
  });
}

/* ── settings view ────────────────────────────────────────────────────── */

export function renderSettings() {
  const s = state.settings;
  if (!s) return;
  $('#set-protocol').value = s.protocol || 'auto';
  $('#set-edge').value = s.edgeIpVersion || '';
  $('#set-loglevel').value = s.logLevel || 'info';
  $('#set-metrics').value = s.metricsPort || 0;
  $('#set-region').value = s.region || '';
  $('#set-extra').value = s.extraArgs || '';
  $('#set-autostart').checked = s.autoStart !== false;
  $('#set-autorestart').checked = s.autoRestart !== false;
}

export async function saveSettings() {
  const payload = {
    protocol: $('#set-protocol').value,
    edgeIpVersion: $('#set-edge').value,
    logLevel: $('#set-loglevel').value,
    metricsPort: parseInt($('#set-metrics').value, 10) || 0,
    region: $('#set-region').value.trim(),
    extraArgs: $('#set-extra').value.trim(),
    autoStart: $('#set-autostart').checked,
    autoRestart: $('#set-autorestart').checked,
  };
  try {
    const data = await api('PUT', '/settings', payload);
    state.settings = data.settings;
    toast('设置已保存');
  } catch (err) {
    toast(err.message, 'err');
  }
}

export async function updateBinary() {
  const btn = $('#btn-update-bin');
  if (!confirm('将从 GitHub 下载最新的 cloudflared 并替换当前程序，确定继续？')) return;
  btn.disabled = true;
  btn.textContent = '更新中…';
  try {
    const data = await api('POST', '/binary/update');
    toast(`已更新到 ${data.version}`);
    await refreshMeta();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = '检查并更新到最新版';
  }
}

/* ── app log view ─────────────────────────────────────────────────────── */

let lastAppLogText = null;

export async function refreshAppLog() {
  const box = $('#app-log');
  try {
    const data = await api('GET', '/logs');
    const lines = data.lines || [];
    const text = lines.length ? lines.join('\n') : '暂无日志';
    if (text !== lastAppLogText) {
      const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
      box.textContent = text;
      lastAppLogText = text;
      if (atBottom) box.scrollTop = box.scrollHeight;
    }
    const meta = $('#applog-meta');
    if (meta) meta.textContent = `共 ${lines.length} 行 · 更新于 ${new Date().toLocaleTimeString('zh-CN')}`;
  } catch (err) {
    box.textContent = `读取失败：${err.message}`;
    lastAppLogText = null;
  }
}

export async function copyAppLog() {
  const text = $('#app-log').textContent || '';
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement('textarea');
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
    }
    toast('日志已复制');
  } catch (err) {
    toast('复制失败，请手动选中日志内容', 'err');
  }
}

/* ── tab switching ────────────────────────────────────────────────────── */

const VIEW_LOADERS = {
  tunnels: refreshTunnels,
  account: async () => {
    await pollAccount();
    if (state.account && state.account.loggedIn) await refreshAccountTunnels();
  },
  settings: async () => {
    state.settings = await api('GET', '/settings');
    renderSettings();
  },
  logs: refreshAppLog,
};

export function switchView(name) {
  document.querySelectorAll('.tab').forEach((tab) => {
    const active = tab.dataset.view === name;
    tab.classList.toggle('is-active', active);
    tab.setAttribute('aria-selected', String(active));
  });
  document.querySelectorAll('.view').forEach((view) => view.classList.toggle('is-active', view.id === `view-${name}`));
  const loader = VIEW_LOADERS[name];
  if (loader) loader().catch((err) => toast(err.message, 'err'));
}

export function openNewTunnelModal() {
  if (state.account && state.account.loggedIn && !state.accountTunnels.length) {
    refreshAccountTunnels().then(() => openTunnelModal(null));
    return;
  }
  openTunnelModal(null);
}

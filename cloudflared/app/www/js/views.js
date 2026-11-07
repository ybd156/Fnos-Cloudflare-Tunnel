/*
 * The account, settings and app-log views, plus the tab switcher that decides
 * which of them is on screen.
 *
 * Every view follows the same shape: fetch into state, then render from state.
 * The event handlers live next to the markup they act on and are wired up by
 * app.js, which keeps the DOM contract in one place.
 */

import { state, $, toast, copyText } from './core.js';
import { api } from './api.js';
import { esc, refreshTunnels, refreshMeta, paintLog } from './render.js';
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
      <div class="btn-row" style="margin-top:12px">
        <button class="btn" id="btn-netcheck">网络自检</button>
      </div>
      <div id="net-check-list"></div>
      ${acc.error ? `<div class="alert err" style="margin-top:12px"><span class="alert-body">${esc(acc.error)}</span></div>` : ''}`;

    const netBtn = $('#btn-netcheck');
    if (netBtn) {
      netBtn.addEventListener('click', async () => {
        const box = $('#net-check-list');
        box.innerHTML = '<p class="hint"><span class="spin"></span> 正在检查 DNS 与连通性…</p>';
        try {
          const data = await api('GET', '/diagnose');
          box.innerHTML = data.checks
            .map((c) => `
              <div class="kv">
                <div><span>${esc(c.name)}</span><strong>${c.ok ? '通过' : '失败'}</strong></div>
                <div><code>${esc(c.detail)}</code></div>
              </div>`)
            .join('') +
            (data.ok
              ? ''
              : `<div class="alert err" style="margin-top:10px"><span class="alert-body">解析或连接失败通常不是应用的问题：请检查 NAS 系统网络设置里的 DNS 服务器，或在路由器上放通 UDP 53。</span></div>`);
        } catch (err) {
          box.innerHTML = `<div class="alert err"><span class="alert-body">${esc(err.message)}</span></div>`;
        }
      });
    }

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
  applyRegion(s.region);
  $('#set-extra').value = s.extraArgs || '';
  $('#set-autostart').checked = s.autoStart !== false;
  $('#set-autorestart').checked = s.autoRestart !== false;
}

/* The region is a select because Cloudflare documents exactly two choices.
 *
 * A saved value outside that list must survive, though: an older config.json (or
 * a hand-edited one) can carry a region this build does not list, and silently
 * rewriting it to "global" would change how every tunnel connects without the
 * user asking. Such a value gets an extra option so it stays visible and
 * selectable instead of being dropped on the next save.
 */
function applyRegion(raw) {
  const select = $('#set-region');
  const region = String(raw || '').trim();
  const custom = select.querySelector('option[data-custom]');
  const known = Array.from(select.options).some((opt) => opt.value === region);

  if (region && !known) {
    const opt = custom || document.createElement('option');
    opt.dataset.custom = '1';
    opt.value = region;
    opt.textContent = `自定义（${region}）`;
    if (!custom) select.appendChild(opt);
  } else if (custom) {
    custom.remove();
  }
  select.value = region;
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

/* Drop the in-place update so the binary shipped in the package is used again.
   Without this a bad update would be permanent short of a reinstall. */
export async function resetBinary() {
  const btn = $('#btn-reset-bin');
  if (!confirm('将删除已更新的 cloudflared，改用安装包内置的版本。确定继续？')) return;
  btn.disabled = true;
  btn.textContent = '恢复中…';
  try {
    const data = await api('POST', '/binary/reset');
    toast(`已恢复内置版本 ${data.version}`);
    await refreshMeta();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = '恢复内置版本';
  }
}

/* ── app log view ─────────────────────────────────────────────────────── */

/* The log is painted through the same in-place painter the tunnel cards use,
   so both panels get error highlighting, line numbers and the "follow the tail
   only when already at the bottom" rule from one implementation. */
export async function refreshAppLog() {
  const box = $('#app-log');
  try {
    const data = await api('GET', '/logs');
    const lines = data.lines || [];
    state.appLogLines = lines;
    paintLog(box, lines);
    const meta = $('#applog-meta');
    if (meta) meta.textContent = `共 ${lines.length} 行 · 更新于 ${new Date().toLocaleTimeString('zh-CN')}`;
  } catch (err) {
    state.appLogLines = [];
    paintLog(box, [`读取失败：${err.message}`]);
  }
}

export async function copyAppLog() {
  /* Read from the model, not from the DOM: the painted lines are elements, and
     the textContent of a run of block-level spans has no newlines in it. */
  const lines = state.appLogLines || [];
  const text = lines.length ? lines.join('\n') : ($('#app-log').textContent || '');
  try {
    await copyText(text);
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

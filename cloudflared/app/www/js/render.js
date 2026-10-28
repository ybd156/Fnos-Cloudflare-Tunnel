/*
 * Tunnel list rendering: escaping, card markup, the status summary chips and
 * the in-place log boxes.
 *
 * Rendering contract, because it drives both performance and behaviour:
 *
 *   1. renderTunnels() builds the whole list as one HTML string and writes it
 *      only when that string actually changed. Polling every few seconds is the
 *      normal case, and most polls change nothing -- with the string compare a
 *      quiet poll costs no DOM work at all, so open log boxes keep their scroll
 *      position, focus is not stolen, and no animation restarts.
 *   2. Tunnel log text is deliberately NOT part of that string. A running tunnel
 *      prints lines constantly; if the text were inlined, every poll would
 *      rebuild the list. Instead each open log box is updated in place.
 *   3. An updated log box follows the tail only when the user was already at the
 *      bottom, so scrolling back to read an error is not yanked away.
 */

import { state, $, isBusy } from './core.js';
import { api } from './api.js';

const TYPE_LABEL = {
  token: 'Token 隧道',
  quick: '快速隧道',
  local: '本地管理隧道',
  config: '自定义配置',
};

const STATUS_LABEL = {
  running: '运行中',
  starting: '启动中',
  stopping: '停止中',
  stopped: '已停止',
  error: '异常',
};

/* ── helpers ──────────────────────────────────────────────────────────── */

export function esc(value) {
  return String(value == null ? '' : value).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  }[c]));
}

export function relTime(iso) {
  if (!iso) return '—';
  const diff = Date.now() - Date.parse(iso);
  if (Number.isNaN(diff)) return '—';
  const s = Math.max(0, Math.round(diff / 1000));
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
}

/* ── top bar ──────────────────────────────────────────────────────────── */

export function renderMeta() {
  const meta = state.meta;
  if (!meta) return;
  const cf = $('#pill-cf');
  cf.textContent = meta.cloudflared ? `cloudflared ${meta.cloudflared}` : 'cloudflared 缺失';
  cf.className = 'pill ' + (meta.binaryExists ? 'ok' : 'err');
  cf.title = meta.binaryExists ? `程序路径：${meta.binary}` : `未找到可执行文件：${meta.binary}`;

  const acc = $('#pill-account');
  acc.textContent = meta.loggedIn ? '账户已登录' : '账户未登录';
  acc.className = 'pill ' + (meta.loggedIn ? 'ok' : '');

  $('#bin-version').textContent = meta.cloudflared || '未知';
  $('#bin-path').textContent = meta.binary || '—';
  $('#brand-sub').textContent = `飞牛 fnOS 内网穿透 · Go ${meta.runtime} · ${meta.arch}`;
}

export async function refreshMeta() {
  state.meta = await api('GET', '/meta');
  renderMeta();
}

/* ── tunnels view ─────────────────────────────────────────────────────── */

function tunnelSummary(t) {
  if (t.type === 'token') return t.hasToken ? `Token ${esc(t.tokenPreview)}` : '未配置 Token';
  if (t.type === 'quick') return `本地服务 ${esc(t.target || '未配置')}`;
  if (t.type === 'local') {
    const rules = (t.ingress || []).filter((r) => r.hostname).length;
    return `隧道 ${esc(t.tunnelName || t.tunnelId || '未选择')} · ${rules} 条入站规则`;
  }
  return `配置文件 ${esc(t.configFile || '未配置')}`;
}

// 只有本地管理隧道才有 DNS 路由可言（需要 CNAME 到 <UUID>.cfargotunnel.com）。
// 没有状态时返回空串，卡片上就不显示这一行。
function dnsRowHtml(t) {
  if (t.type !== 'local') return '';
  const enabled = t.autoDns !== false;
  const status = t.dnsStatus || '';
  let value = enabled ? '已开启（未检查）' : '已关闭';
  if (status === 'checking') value = '检查中…';
  else if (status === 'ok') value = '正常';
  else if (status === 'error') value = '异常';
  else if (status === 'skipped') value = '已跳过';
  const msg = t.dnsMessage ? `<span class="hint">${esc(t.dnsMessage)}</span>` : '';
  return `
      <div><span class="label">DNS 路由</span><span class="value">${esc(value)}${msg}</span></div>`;
}

/* One card's markup. Log text is intentionally absent -- see the file header. */
function tunnelCardHtml(t) {
  const status = t.status || 'stopped';
  const busyKey = `tunnel:${t.id}`;
  const busy = isBusy(busyKey);
  const canStart = status === 'stopped' || status === 'error';
  const canStop = status === 'running' || status === 'starting' || status === 'stopping';
  const logOpen = !!state.openLogs[t.id];
  return `
<article class="card tunnel-card is-${esc(status)}" data-id="${esc(t.id)}">
  <div class="card-head">
    <div class="tunnel-title">
      <h3>${esc(t.name)}</h3>
      <span class="badge">${esc(TYPE_LABEL[t.type] || t.type)}</span>
      <span class="status ${esc(status)}">${esc(STATUS_LABEL[status] || status)}</span>
      ${t.pid ? `<span class="badge mono">PID ${esc(t.pid)}</span>` : ''}
    </div>
    <div class="card-actions">
      ${canStart ? `<button class="btn btn-sm" data-act="start" ${busy ? 'disabled' : ''}>启动</button>` : ''}
      ${canStop ? `<button class="btn btn-sm" data-act="stop" ${busy ? 'disabled' : ''}>停止</button>` : ''}
      <button class="btn btn-sm" data-act="restart" ${busy ? 'disabled' : ''}>重启</button>
      <button class="btn btn-sm" data-act="logs" aria-expanded="${logOpen}">${logOpen ? '收起日志' : '查看日志'}</button>
      <button class="btn btn-sm" data-act="edit">编辑</button>
      ${t.type === 'local' ? `<button class="btn btn-sm" data-act="dns" ${busy ? 'disabled' : ''}>修复 DNS 路由</button>` : ''}
      <button class="btn btn-sm btn-danger" data-act="delete" ${busy ? 'disabled' : ''}>删除</button>
    </div>
  </div>
  <div class="card-body">
    ${t.error ? `<div class="alert err"><span class="alert-body">${esc(t.error)}</span></div>` : ''}
    ${t.url ? `<div class="alert ok"><span class="alert-body">公网地址：<a class="link" href="${esc(t.url)}" target="_blank" rel="noreferrer">${esc(t.url)}</a></span></div>` : ''}
    <div class="meta-grid">
      <div><span class="label">类型</span><span class="value">${esc(TYPE_LABEL[t.type] || t.type)}</span></div>
      <div><span class="label">配置</span><span class="value">${tunnelSummary(t)}</span></div>
      <div><span class="label">开机自启</span><span class="value">${t.autoStart ? '是' : '否'}</span></div>
      <div><span class="label">启动于</span><span class="value">${t.startedAt ? esc(relTime(t.startedAt)) : '—'}</span></div>
      <div><span class="label">上次退出</span><span class="value">${t.lastExitAt ? `${esc(relTime(t.lastExitAt))}（退出码 ${esc(t.lastExitCode)}）` : '—'}</span></div>
      ${t.attempts ? `<div><span class="label">重试次数</span><span class="value">${esc(t.attempts)}</span></div>` : ''}
      ${t.note ? `<div><span class="label">备注</span><span class="value">${esc(t.note)}</span></div>` : ''}
      ${dnsRowHtml(t)}
    </div>
    ${logOpen ? '<pre class="logbox tunnel-log" tabindex="0">暂无日志</pre>' : ''}
  </div>
</article>`;
}

function matchesFilter(t) {
  const f = state.filter;
  if (!f || f === 'all') return true;
  return (t.status || 'stopped') === f;
}

const FILTER_DEFS = [
  { key: 'running', label: '运行中', dot: 'running' },
  { key: 'starting', label: '启动中', dot: 'starting' },
  { key: 'stopping', label: '停止中', dot: 'stopping' },
  { key: 'error', label: '异常', dot: 'error' },
  { key: 'stopped', label: '已停止', dot: 'stopped' },
];

function renderSummary(tunnels) {
  const box = $('#tunnel-summary');
  if (!tunnels.length) {
    box.classList.add('hidden');
    box.innerHTML = '';
    return;
  }
  const counts = {};
  for (const t of tunnels) {
    const s = t.status || 'stopped';
    counts[s] = (counts[s] || 0) + 1;
  }
  /* A filter whose last member just left would hide the whole list with no way
     back, so fall back to "all" as soon as its category empties. */
  if (state.filter !== 'all' && !counts[state.filter]) state.filter = 'all';

  const chips = [
    `<button class="summary-chip ${state.filter === 'all' ? 'is-active' : ''}" data-filter="all" aria-pressed="${state.filter === 'all'}">全部<span class="count">${tunnels.length}</span></button>`,
  ];
  for (const def of FILTER_DEFS) {
    const n = counts[def.key] || 0;
    if (!n) continue;
    const active = state.filter === def.key;
    chips.push(
      `<button class="summary-chip ${active ? 'is-active' : ''}" data-filter="${def.key}" aria-pressed="${active}">` +
        `<span class="dot ${def.dot}"></span>${def.label}<span class="count">${n}</span></button>`,
    );
  }
  const html = chips.join('');
  if (box.innerHTML !== html) box.innerHTML = html;
  box.classList.remove('hidden');
}

/* Remembers the last list markup so an unchanged poll skips the DOM entirely. */
let lastListHtml = null;

export function renderTunnels() {
  const list = $('#tunnel-list');
  const empty = $('#tunnel-empty');
  const all = state.tunnels || [];
  renderSummary(all);

  if (!all.length) {
    if (lastListHtml !== '') {
      list.innerHTML = '';
      lastListHtml = '';
    }
    list.classList.add('hidden');
    empty.classList.remove('hidden');
    return;
  }
  empty.classList.add('hidden');
  list.classList.remove('hidden');

  const shown = all.filter(matchesFilter);
  const html = shown.length
    ? shown.map(tunnelCardHtml).join('')
    : '<div class="empty"><h3>没有符合条件的隧道</h3><p>换一个状态筛选，或点击「全部」。</p></div>';

  if (html !== lastListHtml) {
    const savedScroll = captureLogScroll();
    list.innerHTML = html;
    lastListHtml = html;
    /* The rebuild replaced every log box, so refill them and put the scroll
     * positions back in the same pass. */
    applyLogBoxes(shown, savedScroll);
    return;
  }

  /* Unchanged markup: the boxes survived, so only their text needs refreshing. */
  applyLogBoxes(shown, null);
}

/* One pass over the rendered cards, handing each caller the log box that
 * belongs to a tunnel id. Everything that touches log boxes goes through this:
 * looking each box up separately would rescan the whole list once per tunnel. */
export function eachCardBox(fn) {
  const list = $('#tunnel-list');
  if (!list || !list.querySelectorAll) return;
  for (const card of list.querySelectorAll('.tunnel-card')) {
    const id = card.dataset && card.dataset.id;
    if (!id) continue;
    fn(id, card.querySelector('.logbox'));
  }
}

export function logBoxFor(id) {
  let found = null;
  eachCardBox((cardId, box) => {
    if (cardId === id && box) found = box;
  });
  return found;
}

/* Update a log box in place. Follows the tail only when the reader was already
   at the bottom, so scrolling up to inspect an error is not undone by the next
   poll. */
export function updateLogBox(box, lines) {
  const text = lines.length ? lines.join('\n') : '暂无日志';
  if (box.textContent === text) return;
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
  box.textContent = text;
  if (atBottom) box.scrollTop = box.scrollHeight;
}

/* Refresh every open log box after the list markup was (re)built, restoring the
 * scroll position captured before the rebuild in the same pass. */
function applyLogBoxes(shown, savedScroll) {
  const wanted = new Map();
  for (const t of shown) {
    if (state.openLogs[t.id]) wanted.set(t.id, state.logs[t.id] || []);
  }
  if (!wanted.size && !savedScroll) return;
  eachCardBox((id, box) => {
    if (!box) return;
    if (savedScroll && savedScroll[id] !== undefined) box.scrollTop = savedScroll[id];
    if (wanted.has(id)) updateLogBox(box, wanted.get(id));
  });
}

/* Polling rebuilds the card DOM when something changed, which would drop an open
 * log box back to the top. Capture the scroll offsets first so the rebuild can
 * put them back. */
function captureLogScroll() {
  const saved = {};
  eachCardBox((id, box) => {
    if (box) saved[id] = box.scrollTop;
  });
  return saved;
}

export async function refreshTunnels() {
  const data = await api('GET', '/tunnels');
  state.tunnels = data.tunnels || [];
  renderTunnels();
}

export async function loadTunnelLog(id) {
  const data = await api('GET', `/tunnels/${encodeURIComponent(id)}/logs?lines=300`);
  state.logs[id] = data.lines || [];
}

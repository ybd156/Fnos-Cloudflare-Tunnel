/*
 * Tunnel list rendering: escaping, card markup, the status summary chips, the
 * origin-health panel and the in-place log boxes.
 *
 * Rendering contract, because it drives both performance and behaviour:
 *
 *   1. renderTunnels() builds the whole list as one HTML string and writes it
 *      only when that string actually changed. Polling every few seconds is the
 *      normal case, and most polls change nothing -- with the string compare a
 *      quiet poll costs no DOM work at all, so open log boxes keep their scroll
 *      position, focus is not stolen, and no animation restarts.
 *   2. The string must therefore be a pure function of the data. Nothing that
 *      drifts on its own may appear in it: a relative stamp such as "3 分钟前"
 *      is a different string one second later, which used to rebuild the whole
 *      list on every poll and made the UI twitch. Cards show absolute clock
 *      times; the relative text lives in its own <span class="rel"> and is
 *      refreshed in place by refreshRelTimes().
 *   3. Tunnel log text is deliberately NOT part of that string. A running tunnel
 *      prints lines constantly; if the text were inlined, every poll would
 *      rebuild the list. Instead each open log box is painted in place.
 *   4. An updated log box follows the tail only when the user was already at the
 *      bottom, so scrolling back to read an error is not yanked away.
 *   5. The log box has a fixed height in CSS. A max-height box grows line by
 *      line, and every growth step moved the card under the cursor.
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

const ORIGIN_LABEL = {
  ok: '源站可连',
  bad: '源站连不上',
  unknown: '源站未知',
};

/* Short forms for the facts strip, where the label is already "源站". */
const ORIGIN_SHORT = {
  ok: '可连',
  bad: '连不上',
  unknown: '未知',
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

/* Absolute clock time. This is what the card markup contains, because it does
   not change between two polls of the same data. */
export function absTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/* A relative stamp in its own span: refreshRelTimes() can rewrite its
   textContent without touching the markup the rebuild check compares. */
function relSpan(iso) {
  return `<span class="rel" data-since="${esc(iso)}">—</span>`;
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
  // Which copy is actually in use: the one shipped in the package, or one
  // updated in place into $PKGVAR. They live in different directories and
  // survive upgrades differently, so the user cannot tell them apart from the
  // version string alone -- 1.4.6 promised this line and the field went unused.
  const source = $('#bin-source');
  if (source) {
    source.textContent = meta.binarySource === 'updated' ? '已更新（数据目录）' : '内置（随应用安装）';
  }
  $('#bin-path').textContent = meta.binary || '—';
  $('#brand-sub').textContent = `飞牛 fnOS 内网穿透 · Go ${meta.runtime} · ${meta.arch}`;
}

export async function refreshMeta() {
  state.meta = await api('GET', '/meta');
  renderMeta();
}

/* ── origin health ────────────────────────────────────────────────────── */

/* The distinction the card has to make obvious: cloudflared reporting
   "running" only means the tunnel is connected to Cloudflare's edge. The
   service behind it can still be dead, and then every request returns 502.
   Two independent signals cover that:
 *
 *   originError  parsed from cloudflared's own log lines, so it works for every
 *                tunnel type including token tunnels. Sticky by design: a
 *                successful request logs nothing at the default level, so there
 *                is no line to clear it with.
 *   origins      an active TCP probe of the local ingress rules. Empty for a
 *                token tunnel, whose ingress lives in the Cloudflare dashboard.
 *
 * Both fields are new in the API, so every reader here tolerates them missing:
 * an older backend simply renders as "unknown". */
function originStatus(t) {
  const list = Array.isArray(t.origins) ? t.origins.filter((o) => o && typeof o === 'object') : [];
  const bad = list.filter((o) => o.ok === false);
  const err = t.originError && typeof t.originError === 'object' ? t.originError : null;
  if (bad.length) return { level: 'bad', list, bad, err };
  /* Nothing to probe but the log says the origin was unreachable: still bad. */
  if (err && !list.length) return { level: 'bad', list, bad, err };
  if (list.length) return { level: 'ok', list, bad, err };
  return { level: 'unknown', list, bad, err };
}

function originRowHtml(o) {
  const ok = o.ok !== false;
  return `
          <div class="origin-row ${ok ? 'is-ok' : 'is-bad'}">
            <span class="origin-flag ${ok ? 'is-ok' : 'is-bad'}">${ok ? '可连' : '连不上'}</span>
            <span class="origin-host mono">${o.hostname ? esc(o.hostname) : '（无域名）'}</span>
            <span class="origin-sep">→</span>
            <span class="origin-service mono">${esc(o.service || '—')}</span>
            ${!ok && o.error ? `<span class="origin-detail">${esc(o.error)}</span>` : ''}
          </div>`;
}

function originAlertHtml(origin, running) {
  const details = [];
  for (const o of origin.bad) {
    details.push(
      `<span class="origin-detail"><span class="mono">${esc(o.service || '未知服务')}</span>` +
        `${o.error ? ' — ' + esc(o.error) : ''}</span>`,
    );
  }
  if (!origin.bad.length && origin.err) {
    details.push(
      `<span class="origin-detail"><span class="mono">${esc(origin.err.service || '未知服务')}</span>` +
        ` — ${esc(origin.err.message || '无法连接')}</span>`,
    );
  }
  const title = running ? '隧道已连上 Cloudflare，但源站没有响应' : '上次运行时源站连不上';
  const tail = running
    ? '<span class="origin-detail">此时访问域名会拿到 Cloudflare 的 502，而不是隧道断开。</span>'
    : '';
  return `
        <div class="alert warn origin-alert">
          <span class="alert-body">
            <strong>${title}</strong>
            ${details.join('')}
            ${tail}
          </span>
        </div>`;
}

/* A log error that the probe no longer confirms: worth showing, but not a
   failure -- the service may simply have recovered. */
function originStaleHtml(err) {
  return `
        <p class="origin-note">
          最近一次源站报错（本地探测已可连）：
          <span class="mono">${esc(err.service || '未知服务')}</span>
          ${err.message ? `<span class="origin-detail">${esc(err.message)}</span>` : ''}
          ${err.at ? `<span class="hint">${esc(absTime(err.at))}</span>` : ''}
        </p>`;
}

function originNoteHtml(t, origin, running) {
  if (origin.level === 'ok') return '<p class="origin-note">本地探测全部可连。</p>';
  if (origin.level === 'bad') return '';
  if (!running) return '<p class="origin-note">隧道未运行，源站状态未知。</p>';
  if (t.type === 'token') {
    return '<p class="origin-note">Token 隧道的入站规则在 Cloudflare 云端，本地看不到，只能从 cloudflared 日志里发现源站报错。</p>';
  }
  return '<p class="origin-note">没有可探测的 TCP 源站（例如兜底规则 http_status）。</p>';
}

function originPanelHtml(t) {
  const origin = originStatus(t);
  const running = (t.status || 'stopped') === 'running';
  const busy = isBusy(`tunnel:${t.id}`);
  const rows = origin.list.map(originRowHtml).join('');
  return `
      <div class="origin-panel is-${origin.level}">
        <div class="origin-head">
          <span class="section-title">源站健康</span>
          <span class="origin-flag is-${origin.level}">${esc(ORIGIN_LABEL[origin.level])}</span>
          <button class="btn btn-sm" data-act="probe" ${busy ? 'disabled' : ''}>测试源站</button>
        </div>
        ${origin.level === 'bad' ? originAlertHtml(origin, running) : ''}
        ${rows ? `<div class="origin-list">${rows}</div>` : ''}
        ${origin.err && origin.level !== 'bad' ? originStaleHtml(origin.err) : ''}
        ${originNoteHtml(t, origin, running)}
      </div>`;
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

/* One "fact" in the card's top strip: a label, a value and an optional second
   line. Grouping the three things a user checks first (how long has it been
   up, where does it point, is the origin alive) keeps the card scannable
   instead of a flat dump of every field. */
function factHtml(label, valueHtml, subHtml) {
  return `
        <div class="fact">
          <span class="fact-label">${label}</span>
          <span class="fact-value">${valueHtml}</span>
          ${subHtml ? `<span class="fact-sub">${subHtml}</span>` : ''}
        </div>`;
}

/* Public addresses of a tunnel.
 *
 * `t.url` is only ever populated for a quick tunnel: it is scraped from the
 * "https://….trycloudflare.com" line cloudflared prints. A locally-managed or
 * custom-config tunnel serves whatever its ingress rules name, so its public
 * addresses are the hostnames in `t.ingress`. Reporting "未分配" for a tunnel that
 * is happily serving three domains was simply wrong.
 *
 * A token tunnel's ingress lives in the Cloudflare dashboard and never reaches
 * this app, so it has no addresses to show here -- that case must say so rather
 * than pretend nothing is configured.
 */
function publicAddresses(t) {
  const out = [];
  const seen = new Set();
  /* href may be empty for an address that cannot be opened directly (a wildcard
     hostname); such an entry is still listed, just not as a link. */
  const add = (label, href) => {
    const key = href || 'plain:' + label;
    if (!label || seen.has(key)) return;
    seen.add(key);
    out.push({ label, href });
  };

  if (t.url) add(t.url, t.url);
  for (const rule of Array.isArray(t.ingress) ? t.ingress : []) {
    const host = String((rule && rule.hostname) || '').trim();
    if (!host) continue;
    add(host, host.startsWith('*.') ? '' : 'https://' + host);
  }
  return out;
}

function factsHtml(t, origin) {
  const running = (t.status || 'stopped') === 'running';
  /* "运行时长" only means something while the process is up. For a stopped
     tunnel the honest fact is when it was last started, so the label changes
     with the state instead of showing a duration that no longer grows. */
  let uptime;
  if (running && t.startedAt) {
    uptime = factHtml('运行时长', relSpan(t.startedAt), `启动于 ${esc(absTime(t.startedAt))}`);
  } else if (t.startedAt) {
    uptime = factHtml('上次启动', esc(absTime(t.startedAt)), '');
  } else {
    uptime = factHtml('运行时长', '—', '尚未启动');
  }

  const addrs = publicAddresses(t);
  let url;
  if (addrs.length === 1) {
    const only = addrs[0];
    url = factHtml('公网地址', addressHtml(only), '');
  } else if (addrs.length > 1) {
    url = factHtml('公网地址', `<span class="addr-list">${addrs.map(addressHtml).join('')}</span>`, `${addrs.length} 个域名`);
  } else if (t.type === 'token') {
    url = factHtml('公网地址', '由云端管理', 'Token 隧道的域名在 Cloudflare 控制台配置');
  } else {
    url = factHtml('公网地址', '未分配', '尚未添加入站规则');
  }

  const originFact = factHtml(
    '源站',
    `<span class="origin-flag is-${origin.level}">${esc(ORIGIN_SHORT[origin.level])}</span>`,
    origin.level === 'bad' ? '见下方详情' : '',
  );

  return `
      <div class="tunnel-facts">
        ${uptime}
        ${url}
        ${originFact}
      </div>`;
}

/* One address as a link, or as plain text when it cannot be opened directly
   (a wildcard hostname). */
function addressHtml(addr) {
  if (!addr.href) return `<span class="mono">${esc(addr.label)}</span>`;
  return `<a class="link mono" href="${esc(addr.href)}" target="_blank" rel="noreferrer">${esc(addr.label)}</a>`;
}

/* The line-number switch is a reading aid, not tunnel data: like the fold, it
   is applied to the rendered boxes by applyLogNumbers() instead of being baked
   into the markup string. Baking it in would rebuild the whole list every time
   the switch flipped. */
function logPanelHtml() {
  return `
      <div class="log-panel">
        <div class="log-tools">
          <span class="section-title">运行日志</span>
          <button class="btn btn-sm" data-act="linenos" aria-pressed="false">行号</button>
          <button class="btn btn-sm" data-act="copy-log">复制</button>
        </div>
        <pre class="logbox tunnel-log" tabindex="0">暂无日志</pre>
      </div>`;
}

/* One card's markup. Log text is intentionally absent -- see the file header. */
function tunnelCardHtml(t) {
  const status = t.status || 'stopped';
  const busyKey = `tunnel:${t.id}`;
  const busy = isBusy(busyKey);
  const canStart = status === 'stopped' || status === 'error';
  const canStop = status === 'running' || status === 'starting' || status === 'stopping';
  const logOpen = !!state.openLogs[t.id];
  const origin = originStatus(t);
  const lastExit = t.lastExitAt
    ? `${relSpan(t.lastExitAt)}<span class="hint">（退出码 ${esc(t.lastExitCode)} · ${esc(absTime(t.lastExitAt))}）</span>`
    : '—';
  return `
<article class="card tunnel-card is-${esc(status)}${origin.level === 'bad' ? ' has-origin-error' : ''}" data-id="${esc(t.id)}">
  <div class="card-head">
    <div class="tunnel-title">
      <h3>${esc(t.name)}</h3>
      <span class="status ${esc(status)}">${esc(STATUS_LABEL[status] || status)}</span>
      <span class="badge">${esc(TYPE_LABEL[t.type] || t.type)}</span>
      ${t.pid ? `<span class="badge mono">PID ${esc(t.pid)}</span>` : ''}
      ${origin.level === 'bad' ? '<span class="badge badge-warn">源站异常</span>' : ''}
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
    ${factsHtml(t, origin)}
    ${originPanelHtml(t)}
    <details class="card-more" data-details="${esc(t.id)}">
      <summary>更多信息</summary>
      <div class="meta-grid">
        <div><span class="label">类型</span><span class="value">${esc(TYPE_LABEL[t.type] || t.type)}</span></div>
        <div><span class="label">配置</span><span class="value">${tunnelSummary(t)}</span></div>
        <div><span class="label">开机自启</span><span class="value">${t.autoStart ? '是' : '否'}</span></div>
        <div><span class="label">上次退出</span><span class="value">${lastExit}</span></div>
        ${t.attempts ? `<div><span class="label">重试次数</span><span class="value">${esc(t.attempts)}</span></div>` : ''}
        ${t.note ? `<div><span class="label">备注</span><span class="value">${esc(t.note)}</span></div>` : ''}
        ${dnsRowHtml(t)}
      </div>
    </details>
    ${logOpen ? logPanelHtml() : ''}
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

/* One line above the list when any running tunnel's origin is unreachable. It
   says the thing the cards cannot say individually: the tunnel is up, the
   service is not. A stopped tunnel with a stale log error is left to its own
   card -- nothing is being served, so there is nothing to warn about here.
   Deliberately count-only so it does not change on every poll. */
function renderOriginBanner(tunnels) {
  const box = $('#origin-banner');
  if (!box) return;
  const broken = tunnels.filter((t) => {
    const status = t.status || 'stopped';
    if (status !== 'running' && status !== 'starting') return false;
    return originStatus(t).level === 'bad';
  });
  if (!broken.length) {
    if (!box.classList.contains('hidden')) {
      box.classList.add('hidden');
      box.innerHTML = '';
    }
    return;
  }
  const names = broken.slice(0, 3).map((t) => esc(t.name)).join('、');
  const suffix = broken.length > 3 ? ` 等 ${broken.length} 个` : '';
  const html =
    '<span class="banner-icon" aria-hidden="true">!</span>' +
    '<span class="banner-text"><strong>' +
    `${broken.length} 个隧道的源站连不上</strong>` +
    `<span class="banner-sub">${names}${suffix} · 隧道与 Cloudflare 的连接可能正常，但访问域名会返回 502。</span></span>`;
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
  renderOriginBanner(all);

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
  } else {
    /* Unchanged markup: the boxes survived, so only their text needs
     * refreshing. */
    applyLogBoxes(shown, null);
  }
  applyDetailsState();
  applyLogNumbers();
  refreshRelTimes();
}

/* The "more information" fold is deliberately NOT part of the markup string:
   its open/closed state is something the reader changes, not something the
   poll reports. Putting the `open` attribute in the string would make every
   fold toggle change the string, and the next poll would then rebuild the whole
   list to "apply" a state the DOM already had. */
function applyDetailsState() {
  const list = $('#tunnel-list');
  if (!list || !list.querySelectorAll) return;
  for (const fold of list.querySelectorAll('details[data-details]')) {
    const want = !!state.openDetails[fold.dataset.details];
    if (fold.open !== want) fold.open = want;
  }
}

/* Write every relative stamp in place. This is the only thing that changes on
   a quiet poll, and it touches textContent of a leaf span -- no innerHTML, no
   rebuild, no reflow of anything but that span's line box. */
export function refreshRelTimes() {
  const list = $('#tunnel-list');
  if (!list || !list.querySelectorAll) return;
  for (const el of list.querySelectorAll('.rel')) {
    const text = relTime(el.dataset ? el.dataset.since : '');
    if (el.textContent !== text) el.textContent = text;
  }
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

/* ── log boxes ────────────────────────────────────────────────────────── */

/* The failures worth the loudest colour regardless of how cloudflared labelled
   them: the origin is unreachable, or the name does not resolve. Checked before
   the client-cancel rule so a line mentioning both stays an error. */
const LOG_CRITICAL_RE = /Unable to reach the origin|connection refused|no such host/i;

/* cloudflared logs a client that walked away mid-request as ERR, but nothing is
   broken: the origin answered and then the browser (or a slow API caller) closed
   the connection. "error code 0" is an explicit "no error" cancel. Painting
   these the same red as a dead origin teaches the reader to ignore the colour
   that does matter, so they get their own muted style. */
const LOG_CANCEL_RE = /context canceled|canceled by remote with error code 0/i;

/* A transfer that stopped mid-flight. Large uploads through a tunnel fail this
   way -- the edge's 100MB request limit, an origin body limit, or a reset
   connection -- and cloudflared logs them as ordinary ERR lines, so they used
   to be indistinguishable from a dead origin. */
const LOG_STREAM_RE = /Request Entity Too Large|413|stream (?:error|closed|reset)|unexpected EOF|connection reset by peer|broken pipe/i;

/* cloudflared's own level token. Matched case-sensitively on purpose: every
   structured line carries an uppercase ERR / WRN, while the prose "error=" key
   appears in nearly all of them. Matching that case-insensitively painted the
   whole log red. */
const LOG_ERR_RE = /\b(?:ERR|ERROR|FATAL)\b/;
const LOG_WARN_RE = /\b(?:WRN|WARN)\b/;

/* The text last painted into a box. A WeakMap because boxes are recreated by
   the next rebuild and the entry should disappear with them. */
const lastLogText = new WeakMap();

/* Paint log lines into a box in place.
 *
 * The lines are elements rather than one text node so an error line can be
 * highlighted and so CSS counters can number them. That costs a rebuild of the
 * line elements whenever the text changes, which is why the joined text is
 * compared first: a poll that brought no new lines does nothing at all. */
export function paintLog(box, lines) {
  if (!box) return;
  const list = Array.isArray(lines) ? lines : [];
  const text = list.length ? list.join('\n') : '暂无日志';
  if (lastLogText.get(box) === text) return;

  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
  const prevTop = box.scrollTop;

  box.textContent = '';
  if (!list.length) {
    box.textContent = '暂无日志';
  } else {
    const frag = document.createDocumentFragment();
    for (const raw of list) {
      const line = String(raw == null ? '' : raw);
      const el = document.createElement('span');
      let cls = 'log-line';
      if (LOG_CRITICAL_RE.test(line)) cls += ' is-err';
      else if (LOG_CANCEL_RE.test(line)) cls += ' is-cancel';
      else if (LOG_STREAM_RE.test(line)) cls += ' is-stream';
      else if (LOG_ERR_RE.test(line)) cls += ' is-err';
      else if (LOG_WARN_RE.test(line)) cls += ' is-warn';
      el.className = cls;
      /* An empty line still has to occupy a row. */
      el.textContent = line || ' ';
      frag.appendChild(el);
    }
    box.appendChild(frag);
  }
  lastLogText.set(box, text);

  if (atBottom) box.scrollTop = box.scrollHeight;
  else box.scrollTop = prevTop;
}

/* Kept as the name the rest of this module (and the tests) use. */
export function updateLogBox(box, lines) {
  paintLog(box, lines);
}

/* Line numbers are a CSS counter, so switching them on is one class per box
   rather than a repaint of every line. */
export function applyLogNumbers() {
  const on = !!state.logNumbers;
  for (const box of document.querySelectorAll('.logbox')) {
    box.classList.toggle('linenos', on);
  }
  const appBtn = $('#btn-applog-linenos');
  if (appBtn) {
    appBtn.classList.toggle('is-active', on);
    appBtn.setAttribute('aria-pressed', String(on));
  }
  const list = $('#tunnel-list');
  if (list) {
    for (const btn of list.querySelectorAll('[data-act="linenos"]')) {
      btn.classList.toggle('is-active', on);
      btn.setAttribute('aria-pressed', String(on));
    }
  }
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

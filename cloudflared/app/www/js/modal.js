/*
 * The tunnel form and the modal shell it lives in.
 *
 * The modal is reused for two jobs: editing a tunnel, and asking for a single
 * line of input (see openPromptModal). Both go through #modal-backdrop, so the
 * page behind the backdrop is never interactable while one is open.
 */

import { state, $, toast } from './core.js';
import { api } from './api.js';
import { esc, refreshTunnels } from './render.js';

const TYPE_HELP = {
  token: '在 Cloudflare Zero Trust → Networks → Tunnels 创建隧道，复制 Token 粘贴到下面。最省事的方式。',
  quick: '无需账户，启动后得到一个随机的 trycloudflare.com 临时域名，适合临时演示。',
  local: '需要先登录 Cloudflare 账户，并创建一条命名隧道。可以自动写入 DNS 路由。',
  config: '直接使用你自己写的 config.yml，应用只负责拉起进程。',
};

export function splitService(service) {
  const m = /^(https?):\/\/([^/:]+)(?::(\d+))?/i.exec(String(service || ''));
  if (!m) return { protocol: 'http', host: '', port: '' };
  return { protocol: m[1].toLowerCase(), host: m[2], port: m[3] || '' };
}

function ingressRowHtml(rule) {
  const r = rule || { hostname: '', service: '', noTLSVerify: false, path: '' };
  const svc = splitService(r.service);
  return `
<div class="rule" data-rule>
  <label class="field">
    <span>公网域名</span>
    <input type="text" data-k="hostname" value="${esc(r.hostname)}" placeholder="nas.example.com">
  </label>
  <label class="field">
    <span>本地服务</span>
    <span class="service-row">
      <select data-k="protocol" aria-label="协议">
        <option value="http" ${svc.protocol === 'http' ? 'selected' : ''}>http://</option>
        <option value="https" ${svc.protocol === 'https' ? 'selected' : ''}>https://</option>
      </select>
      <input type="text" data-k="host" value="${esc(svc.host)}" placeholder="127.0.0.1" aria-label="地址">
      <input type="text" data-k="port" value="${esc(svc.port)}" placeholder="80" inputmode="numeric" aria-label="端口">
    </span>
  </label>
  <label class="field switch">
    <input type="checkbox" data-k="noTLSVerify" ${r.noTLSVerify ? 'checked' : ''}>
    <span>跳过 TLS 校验</span>
  </label>
  <button class="btn btn-sm btn-danger" data-remove-rule type="button" aria-label="移除这条入站规则">移除</button>
</div>`;
}

function tunnelFormHtml(t) {
  const tunnel = t || { type: 'token', name: '', autoStart: true, ingress: [], catchAll: 'http_status:404' };
  const accountOptions = state.accountTunnels
    .map((at) => `<option value="${esc(at.name)}" data-id="${esc(at.id || '')}" ${at.name === tunnel.tunnelName ? 'selected' : ''}>${esc(at.name)}</option>`)
    .join('');

  return `
<div class="form-grid">
  <div class="form-section">
    <p class="section-title">基本信息</p>
    <div class="form-row">
      <label class="field">
        <span>名称</span>
        <input type="text" id="f-name" value="${esc(tunnel.name)}" placeholder="例如 我的 NAS">
        <small class="field-help">只是给你自己看的标识。</small>
      </label>
      <label class="field">
        <span>隧道类型</span>
        <select id="f-type">
          <option value="token" ${tunnel.type === 'token' ? 'selected' : ''}>Token 隧道（Cloudflare 控制台管理）</option>
          <option value="quick" ${tunnel.type === 'quick' ? 'selected' : ''}>快速隧道（TryCloudflare 临时域名）</option>
          <option value="local" ${tunnel.type === 'local' ? 'selected' : ''}>本地管理隧道（需登录账户）</option>
          <option value="config" ${tunnel.type === 'config' ? 'selected' : ''}>自定义配置文件</option>
        </select>
        <small class="field-help" id="f-type-help">${esc(TYPE_HELP[tunnel.type] || '')}</small>
      </label>
    </div>
  </div>

  <div class="form-section" data-type-block="token">
    <p class="section-title">Token</p>
    <label class="field">
      <span>隧道 Token ${tunnel.hasToken ? `（已保存：${esc(tunnel.tokenPreview)}，留空表示不修改）` : ''}</span>
      <textarea id="f-token" placeholder="eyJhIjoi... （在 Cloudflare Zero Trust → Networks → Tunnels 中复制）"></textarea>
      <small class="field-help">Token 只保存在飞牛本机（$TRIM_PKGVAR/config.json，权限 0600），不会回传到浏览器。</small>
    </label>
  </div>

  <div class="form-section" data-type-block="quick">
    <p class="section-title">本地服务</p>
    <label class="field">
      <span>转发到</span>
      <span class="service-row" id="f-target-row">
        <select data-target-k="protocol" aria-label="协议">
          <option value="http" ${splitService(tunnel.target).protocol === 'http' ? 'selected' : ''}>http://</option>
          <option value="https" ${splitService(tunnel.target).protocol === 'https' ? 'selected' : ''}>https://</option>
        </select>
        <input type="text" data-target-k="host" value="${esc(splitService(tunnel.target).host)}" placeholder="127.0.0.1" aria-label="地址">
        <input type="text" data-target-k="port" value="${esc(splitService(tunnel.target).port)}" placeholder="80" inputmode="numeric" aria-label="端口">
      </span>
      <small class="field-help">端口留空时按协议自动补 80 / 443。</small>
    </label>
    <label class="field">
      <span>自定义主机名（可选，需账户）</span>
      <input type="text" id="f-quick-hostname" value="${esc(tunnel.quickHostname)}" placeholder="留空则使用随机 trycloudflare.com 域名">
    </label>
  </div>

  <div class="form-section" data-type-block="local">
    <p class="section-title">隧道</p>
    <div class="form-row">
      <label class="field">
        <span>选择已创建的隧道</span>
        <select id="f-tunnel-name">
          <option value="">— 请先在「账户」页创建 —</option>
          ${accountOptions}
        </select>
      </label>
      <label class="field">
        <span>或手动填写隧道名称 / ID</span>
        <input type="text" id="f-tunnel-name-manual" value="${esc(tunnel.tunnelName)}" placeholder="my-nas">
      </label>
    </div>

    <p class="section-title">入站规则（ingress）</p>
    <div class="rule-list" id="rule-list">${(tunnel.ingress || []).map(ingressRowHtml).join('')}</div>
    <div>
      <button class="btn btn-sm" id="btn-add-rule" type="button">+ 添加规则</button>
    </div>

    <div class="form-row">
      <label class="field">
        <span>兜底规则</span>
        <input type="text" id="f-catchall" value="${esc(tunnel.catchAll || 'http_status:404')}">
        <small class="field-help">没有匹配到上面任何域名时返回的内容。</small>
      </label>
    </div>

    <label class="field switch">
      <input type="checkbox" id="f-autodns" ${tunnel.autoDns !== false ? 'checked' : ''}>
      <span>
        启动时自动配置 DNS 路由（CNAME 指向隧道）
        <small class="field-help">开启后每次启动都会检查域名是否已 CNAME 到本隧道，不对就自动执行
          cloudflared tunnel route dns。少了这一步域名会返回 Cloudflare 边缘错误 1033，
          而隧道本身仍显示「运行中」，很难自查。</small>
      </span>
    </label>
  </div>

  <div class="form-section" data-type-block="config">
    <p class="section-title">配置文件</p>
    <label class="field">
      <span>config.yml 绝对路径</span>
      <input type="text" id="f-configfile" value="${esc(tunnel.configFile)}" placeholder="/vol1/@appdata/cloudflared/my-config.yml">
    </label>
  </div>

  <div class="form-section">
    <p class="section-title">其它</p>
    <div class="form-row">
      <label class="field switch">
        <input type="checkbox" id="f-autostart" ${tunnel.autoStart !== false ? 'checked' : ''}>
        <span>应用启动时自动启动此隧道</span>
      </label>
      <label class="field">
        <span>备注（可选）</span>
        <input type="text" id="f-note" value="${esc(tunnel.note)}" placeholder="">
      </label>
    </div>
    <label class="field">
      <span>附加参数（仅此隧道生效，高级）</span>
      <input type="text" id="f-extra" value="${esc(tunnel.extraArgs)}" placeholder="例如 --retries 5">
    </label>
  </div>
</div>`;
}

function syncTypeBlocks() {
  const type = $('#f-type').value;
  document.querySelectorAll('[data-type-block]').forEach((el) => {
    el.classList.toggle('hidden', el.dataset.typeBlock !== type);
  });
  const help = $('#f-type-help');
  if (help) help.textContent = TYPE_HELP[type] || '';
}

/* Focus is returned to whatever opened the modal, so keyboard users are not
   dropped back at the top of the page after saving. */
let lastFocused = null;

/* 应用内输入弹窗复用同一层外壳，保存回调暂存在这里。 */
let pendingModalSave = null;

export function openTunnelModal(tunnel, prefill) {
  state.editing = tunnel || null;
  const form = tunnel || prefill || null;
  lastFocused = document.activeElement || null;
  $('#modal-title').textContent = tunnel ? '编辑隧道' : '新建隧道';
  $('#modal-body').innerHTML = tunnelFormHtml(form);
  $('#modal-backdrop').classList.remove('hidden');

  $('#f-type').addEventListener('change', syncTypeBlocks);
  syncTypeBlocks();

  const addRule = $('#btn-add-rule');
  if (addRule) {
    addRule.addEventListener('click', () => {
      const list = $('#rule-list');
      if (list) list.insertAdjacentHTML('beforeend', ingressRowHtml(null));
    });
  }
  const ruleList = $('#rule-list');
  if (ruleList) {
    ruleList.addEventListener('click', (ev) => {
      const btn = ev.target.closest('[data-remove-rule]');
      if (btn) btn.closest('[data-rule]').remove();
    });
  }
  $('#f-name').focus();
}

export function closeTunnelModal() {
  $('#modal-backdrop').classList.add('hidden');
  $('#modal-body').innerHTML = '';
  state.editing = null;
  pendingModalSave = null;
  const restore = lastFocused;
  lastFocused = null;
  if (restore && typeof restore.focus === 'function') {
    try {
      restore.focus();
    } catch (err) {
      /* the element may be gone after a re-render */
    }
  }
}

/* 应用内输入弹窗：复用隧道弹窗的外壳。
   面板跑在 fnOS 的 iframe 里，浏览器原生 prompt 常被拦截（要么不弹、
   要么看不见输入框），所以凡是需要用户输入的场景都走这个。 */
export function openPromptModal(opts) {
  const o = opts || {};
  pendingModalSave = o.onSave || null;
  lastFocused = document.activeElement || null;
  $('#modal-title').textContent = o.title || '请输入';
  $('#modal-body').innerHTML = `
    <label class="field">
      <span>${esc(o.label || '')}</span>
      <input type="text" id="modal-prompt-input" value="${esc(o.value || '')}"
             placeholder="${esc(o.placeholder || '')}" autocomplete="off">
    </label>
    ${o.hint ? `<p class="hint">${esc(o.hint)}</p>` : ''}`;
  $('#modal-backdrop').classList.remove('hidden');
  const input = $('#modal-prompt-input');
  if (input) {
    input.focus();
    input.select();
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') {
        ev.preventDefault();
        $('#modal-save').click();
      }
    });
  }
}

function readPromptValue() {
  const input = $('#modal-prompt-input');
  return input ? input.value : '';
}

export function collectTunnelForm() {
  const type = $('#f-type').value;
  const payload = {
    name: $('#f-name').value.trim(),
    type,
    autoStart: $('#f-autostart').checked,
    note: $('#f-note').value.trim(),
    extraArgs: $('#f-extra').value.trim(),
  };
  if (type === 'token') {
    const token = $('#f-token').value.trim();
    if (token) payload.token = token;
  } else if (type === 'quick') {
    const tp = (document.querySelector('#f-target-row [data-target-k="protocol"]') || {}).value || 'http';
    const th = ((document.querySelector('#f-target-row [data-target-k="host"]') || {}).value || '').trim();
    const tport = ((document.querySelector('#f-target-row [data-target-k="port"]') || {}).value || '').trim();
    payload.target = th ? `${tp}://${th}:${tport || (tp === 'https' ? '443' : '80')}` : '';
    payload.quickHostname = $('#f-quick-hostname').value.trim();
  } else if (type === 'local') {
    const manual = $('#f-tunnel-name-manual').value.trim();
    const sel = $('#f-tunnel-name');
    const picked = sel && sel.selectedOptions ? sel.selectedOptions[0] : null;
    const pickedName = picked ? picked.value : '';
    const pickedId = picked ? picked.dataset.id || '' : '';
    // 手动输入优先；若手动输入与下拉所选同名，则沿用下拉项携带的隧道 ID，
    // 这样 cloudflared 能用 <ID>.json 精确定位凭据文件，避免多隧道时选错。
    const sameAsPick = !!manual && manual === pickedName;
    payload.tunnelName = manual || pickedName;
    payload.tunnelId = !manual || sameAsPick ? pickedId : '';
    const autoDnsBox = $('#f-autodns');
    payload.autoDns = autoDnsBox ? autoDnsBox.checked : true;
    payload.catchAll = $('#f-catchall').value.trim() || 'http_status:404';
    payload.ingress = Array.from(document.querySelectorAll('#rule-list [data-rule]')).map((row) => {
      const protocol = (row.querySelector('[data-k="protocol"]') || {}).value || 'http';
      const host = ((row.querySelector('[data-k="host"]') || {}).value || '').trim();
      const port = ((row.querySelector('[data-k="port"]') || {}).value || '').trim();
      // 端口留空时按协议补默认值，避免手填还要记 80/443。
      const service = host ? `${protocol}://${host}:${port || (protocol === 'https' ? '443' : '80')}` : '';
      return {
        hostname: ((row.querySelector('[data-k="hostname"]') || {}).value || '').trim(),
        service,
        noTLSVerify: !!(row.querySelector('[data-k="noTLSVerify"]') || {}).checked,
      };
    }).filter((r) => r.hostname || r.service);
  } else if (type === 'config') {
    payload.configFile = $('#f-configfile').value.trim();
  }
  return payload;
}

export async function saveTunnelModal() {
  const payload = collectTunnelForm();
  if (!payload.name) return toast('请填写名称', 'err');
  if (payload.type === 'token' && !payload.token && !(state.editing && state.editing.hasToken)) {
    return toast('请填写隧道 Token', 'err');
  }
  const btn = $('#modal-save');
  btn.disabled = true;
  btn.textContent = '保存中…';
  try {
    if (state.editing) {
      await api('PUT', `/tunnels/${encodeURIComponent(state.editing.id)}`, payload);
      toast('已保存');
    } else {
      await api('POST', '/tunnels', payload);
      toast('已创建');
    }
    closeTunnelModal();
    await refreshTunnels();
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = '保存';
  }
}

/* The footer button serves both the tunnel form and the one-line prompt: a
   pending prompt callback wins, because it is only set while a prompt is open. */
export function handleModalSave() {
  if (typeof pendingModalSave === 'function') {
    const fn = pendingModalSave;
    pendingModalSave = null;
    fn(readPromptValue());
    return;
  }
  saveTunnelModal();
}

/* Keep Tab inside the dialog while it is open: without this, tabbing walks into
   the page behind the backdrop, which is invisible and unreachable by mouse. */
export function trapModalFocus(ev) {
  if (ev.key !== 'Tab') return;
  const focusables = document.querySelectorAll(
    '#modal-backdrop button, #modal-backdrop input, #modal-backdrop select, #modal-backdrop textarea, #modal-backdrop a[href]',
  );
  if (!focusables.length) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (ev.shiftKey && document.activeElement === first) {
    ev.preventDefault();
    last.focus();
  } else if (!ev.shiftKey && document.activeElement === last) {
    ev.preventDefault();
    first.focus();
  }
}

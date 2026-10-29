'use strict';
// OpenDesign 账号池管理台。所有渲染都经 esc()，不使用内联脚本或样式属性（CSP）。

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
let csrf = '';
let accountsCache = [];
let settingsCache = null;
let currentPage = 'overview';
let refreshTimer = null;
// the pool tab on top: every list, overview and log shows one pool only
const POOLS = ['opendesign', 'river', 'ollama'];
let pool = POOLS.includes(localStorage.getItem('odb-pool')) ? localStorage.getItem('odb-pool') : 'opendesign';
const POOL_LABEL = { opendesign: 'OpenDesign', river: 'River', ollama: 'Ollama' };
// the default group of each pool, and what its keys look like
const POOL_GROUP = { opendesign: 'default', river: 'river', ollama: 'ollama' };
const defaultGroup = () => POOL_GROUP[pool] || 'default';
const isOllama = () => pool === 'ollama';
const RT_HINT = { opendesign: 'sk-amr-…', river: 'rv_…', ollama: 'ollama.com/settings/keys 签发的 key 原样粘贴' };
const withPool = (path) => path + (path.includes('?') ? '&' : '?') + 'provider=' + pool;

const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const num = (v) => (v === null || v === undefined || Number.isNaN(v)) ? '—' : Number(v).toLocaleString('en-US');
const pct = (v) => (v === null || v === undefined) ? '—' : (v * 100).toFixed(1) + '%';
const usd = (s) => { const f = parseFloat(s); return Number.isFinite(f) ? '$' + f.toFixed(2) : '—'; };
const TZ = { timeZone: 'Asia/Shanghai', hour12: false };
function fmtTime(iso, short) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  if (short) return d.toLocaleString('zh-CN', { ...TZ, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  return d.toLocaleString('zh-CN', { ...TZ, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
function fmtIn(iso) {
  const ms = new Date(iso).getTime() - Date.now();
  if (!Number.isFinite(ms)) return '';
  const abs = Math.abs(ms), m = Math.round(abs / 60000);
  const txt = m < 60 ? m + ' 分钟' : m < 2880 ? Math.round(m / 60) + ' 小时' : Math.round(m / 1440) + ' 天';
  return ms >= 0 ? txt + '后' : txt + '前';
}
function fmtDur(sec) {
  if (sec % 86400 === 0) return sec / 86400 + ' 天';
  if (sec % 3600 === 0) return sec / 3600 + ' 小时';
  return Math.round(sec / 60) + ' 分钟';
}
// navigator.clipboard only exists on https/localhost; the admin is often served over plain http.
function copyText(text) {
  const ok = () => toast('已复制');
  const legacy = () => {
    const ta = document.createElement('textarea');
    ta.value = text; ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta); ta.select();
    let done = false;
    try { done = document.execCommand('copy'); } catch (e) { /* fall through */ }
    ta.remove();
    done ? ok() : toast('复制失败，请手动选择密钥');
  };
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(ok, legacy); else legacy();
}

function toast(msg) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.remove('hidden');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => t.classList.add('hidden'), 2600);
}
function applyWidths(root) {
  $$('[data-w]', root).forEach((el) => { el.style.width = Math.max(0, Math.min(100, parseFloat(el.dataset.w))) + '%'; });
  $$('[data-h]', root).forEach((el) => { el.style.height = Math.max(0, Math.min(100, parseFloat(el.dataset.h))) + '%'; });
}

async function api(method, path, body) {
  const opt = { method, headers: { Accept: 'application/json' }, credentials: 'same-origin' };
  if (method !== 'GET') opt.headers['X-CSRF-Token'] = csrf;
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  let data = null;
  try { data = await res.json(); } catch { data = null; }
  if (res.status === 401 && !path.startsWith('/api/login') && !path.startsWith('/api/password')) { showAuth(); throw new Error('请先登录'); }
  if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
  return data;
}

// ---- modal ----
function modal({ title, body, buttons = [{ label: '关闭', value: 'close' }], wide = false, onOpen }) {
  const dlg = $('#modal');
  dlg.classList.toggle('wide', wide);
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = body;
  $('#modal-error').textContent = '';
  const actions = $('#modal-actions');
  actions.innerHTML = buttons.map((b, i) => `<button class="btn ${b.primary ? 'primary' : ''} ${b.danger ? 'danger' : ''}" value="${esc(b.value)}" data-i="${i}">${esc(b.label)}</button>`).join('');
  applyWidths(dlg);
  return new Promise((resolve) => {
    const handler = async (ev) => {
      const btn = ev.target.closest('button[data-i]');
      if (!btn) return;
      ev.preventDefault();
      const b = buttons[Number(btn.dataset.i)];
      if (b.action) {
        btn.disabled = true;
        try {
          const r = await b.action();
          if (r === false) { btn.disabled = false; return; }
        } catch (e) {
          $('#modal-error').textContent = e.message;
          btn.disabled = false;
          return;
        }
      }
      cleanup();
      dlg.close();
      resolve(b.value);
    };
    const onClose = () => { cleanup(); resolve('close'); };
    function cleanup() { actions.removeEventListener('click', handler); dlg.removeEventListener('close', onClose); if (dlg._onClose) dlg._onClose(); dlg._onClose = null; }
    actions.addEventListener('click', handler);
    dlg.addEventListener('close', onClose);
    dlg.showModal();
    if (onOpen) onOpen(dlg);
  });
}
$('#modal-form').addEventListener('submit', (e) => e.preventDefault());
const modalErr = (msg) => { $('#modal-error').textContent = msg; };
function showJSON(title, obj) { modal({ title, body: `<pre>${esc(JSON.stringify(obj, null, 2))}</pre>`, wide: true }); }

// ---- auth ----
let authMode = 'login';
async function boot() {
  let s;
  try { s = await api('GET', '/api/session'); } catch (e) { toast(e.message); return; }
  if (s.authenticated) { csrf = s.csrf; showApp(); return; }
  authMode = s.initialized ? 'login' : 'setup';
  showAuth();
}
function showAuth() {
  $('#app').classList.add('hidden');
  $('#auth').classList.remove('hidden');
  const setup = authMode === 'setup';
  $('#auth-hint').textContent = setup ? '首次使用：设置管理员密码（至少 8 位）。' : '请输入管理员密码。';
  $('#auth-password2').classList.toggle('hidden', !setup);
  $('#auth-password2').required = setup;
  $('#auth-password').autocomplete = setup ? 'new-password' : 'current-password';
  $('#auth-submit').textContent = setup ? '设置并进入' : '登录';
  $('#auth-password').focus();
}
$('#auth-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  $('#auth-error').textContent = '';
  const pw = $('#auth-password').value;
  if (authMode === 'setup' && pw !== $('#auth-password2').value) { $('#auth-error').textContent = '两次输入不一致'; return; }
  try {
    const r = await api('POST', authMode === 'setup' ? '/api/setup' : '/api/login', { password: pw });
    csrf = r.csrf;
    $('#auth-password').value = $('#auth-password2').value = '';
    showApp();
  } catch (e) { $('#auth-error').textContent = e.message; }
});
$('#btn-logout').addEventListener('click', async () => { await api('POST', '/api/logout').catch(() => {}); csrf = ''; authMode = 'login'; showAuth(); });
$('#btn-password').addEventListener('click', () => {
  modal({
    title: '修改管理员密码',
    body: `<div class="form"><label>原密码</label><input id="pw-old" type="password"><label>新密码</label><input id="pw-new" type="password" minlength="8"></div>`,
    buttons: [{ label: '取消', value: 'c' }, { label: '保存', value: 'ok', primary: true, action: async () => {
      const r = await api('POST', '/api/password', { old: $('#pw-old').value, new: $('#pw-new').value });
      csrf = r.csrf; toast('密码已修改');
    } }],
  });
});

function showApp() {
  $('#auth').classList.add('hidden');
  $('#app').classList.remove('hidden');
  $('#base-url').textContent = location.origin;
  renderPoolTabs();
  go(location.hash.slice(1) || 'overview');
}

// ---- navigation ----
const pages = { overview: loadOverview, accounts: loadAccounts, keys: loadKeys, routing: loadSettings, requests: () => loadLogs('requests', true), errors: () => loadLogs('errors', true), details: () => loadLogs('details', true) };
function go(page) {
  if (!pages[page]) page = 'overview';
  currentPage = page;
  history.replaceState(null, '', '#' + page);
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === page));
  $$('.page').forEach((p) => p.classList.toggle('hidden', p.id !== 'page-' + page));
  clearInterval(refreshTimer);
  pages[page]().catch((e) => toast(e.message));
  if (page === 'overview' || page === 'accounts') {
    refreshTimer = setInterval(() => { if (!document.hidden && !$('#modal').open) pages[currentPage]().catch(() => {}); }, 15000);
  }
}
function renderPoolTabs() {
  $$('#pool-tabs .pool-tab').forEach((b) => b.classList.toggle('active', b.dataset.pool === pool));
  document.title = POOL_LABEL[pool] + ' 账号池';
  document.body.dataset.pool = pool;
  $('#btn-add-device').classList.toggle('hidden', pool !== 'opendesign');
  // the last two columns of the account table mean different things per pool
  const heads = { windows: { opendesign: 'Design Plan 窗口', river: '额度', ollama: '额度（/api/balance）' },
    wallet: { opendesign: '钱包 / 套餐模型额度', river: '24h 用量', ollama: '已购额度 / 24h 用量' } };
  $$('#acc-table th[data-head]').forEach((th) => { th.textContent = heads[th.dataset.head][pool] || ''; });
  $('#acc-empty').textContent = {
    river: '还没有 River 账号。点「批量导入」粘贴 rv_ 开头的 key，每行一个。',
    ollama: '还没有 Ollama 账号。点「批量导入」一行一个 key（ollama.com/settings/keys 签发，本池的 key 不要求前缀），或「手动添加」。',
  }[pool] || '还没有账号。点「设备授权添加」，在已登录该账号的浏览器里批准即可。';
}
$('#pool-tabs').addEventListener('click', (ev) => {
  const b = ev.target.closest('.pool-tab');
  if (!b || b.dataset.pool === pool) return;
  pool = b.dataset.pool;
  localStorage.setItem('odb-pool', pool);
  accountsCache = [];
  logState.requests = { cursor: '', items: [] }; logState.errors = { cursor: '', items: [] }; logState.details = { cursor: '', items: [] };
  $('#acc-group-filter').value = '';
  renderPoolTabs();
  go(currentPage);
});
$('#nav').addEventListener('click', (ev) => { const a = ev.target.closest('a[data-page]'); if (a) go(a.dataset.page); });

// ---- overview ----
async function loadOverview() {
  const o = await api('GET', withPool('/api/overview'));
  const st = o.stats.total;
  const okRate = st.r ? (st.s / Math.max(1, st.r - st.c)) : null;
  const ok = o.status.ok || 0;
  const cards = [
    ['账号', `${ok} / ${o.accounts}`, `可调度 / 总数${o.authFailed ? ` · ${o.authFailed} 个控制密钥失效` : ''}`],
    ...(pool !== 'opendesign' ? [] : o.windows).map((w) => [`${fmtDur(w.durationSeconds)}窗口剩余`, num(w.remaining), `共 ${num(w.limit)} · ${w.workspaces} 个工作区 · 已用 ${w.limit ? (w.used / w.limit * 100).toFixed(1) : 0}%`]),
    ...(pool !== 'opendesign' ? [] : [['钱包合计', '$' + o.walletUsd.toFixed(2), `${o.walletAccounts} 个工作区`]]),
    ...(pool === 'ollama' && o.ollamaAccounts ? [['Ollama 额度合计', '$' + o.ollamaCredits.toFixed(2), `${o.ollamaAccounts} 个账号（含赠送额度 + 已购额度，同一团队的 key 会重复计）`]] : []),
    ['24h 请求', num(st.r), `成功率 ${okRate === null ? '—' : pct(okRate)} · 近 1h ${num(o.stats.lastHour.r)}`],
    ['24h 费用', costSum(st), '按模型价格估算，未配价格的不计'],
    ['24h 缓存命中率', hitRate(st) === null ? '—' : pct(hitRate(st)), `缓存 ${num(st.k)} / 提示 ${num(st.p)} token`],
    ['24h Token', num(st.i + st.o), `输入 ${num(st.i)} · 输出 ${num(st.o)} · 缓存 ${num(st.k)}`],
    ['会话绑定', num(o.bindings), `策略 ${o.strategy}`],
  ];
  $('#ov-cards').innerHTML = cards.map(([l, v, s]) => `<div class="card"><div class="label">${esc(l)}</div><div class="value">${esc(v)}</div><div class="sub">${esc(s)}</div></div>`).join('');
  const models = Object.entries(o.stats.models || {}).sort((a, b) => b[1].r - a[1].r);
  $('#ov-models tbody').innerHTML = models.map(([name, b]) => {
    const done = b.r - b.c;
    const hr = hitRate(b);
    return `<tr><td>${esc(name)}</td><td class="num">${num(b.r)}</td><td class="num">${done > 0 ? pct(b.s / done) : '—'}</td><td class="num">${num(b.i)}</td><td class="num">${num(b.o)}</td><td class="num">${hr === null ? '—' : pct(hr)}</td><td class="num">${esc(costSum(b))}</td></tr>`;
  }).join('') || '<tr><td colspan="7" class="empty">暂无请求</td></tr>';
  const proxies = Object.entries(o.stats.proxies || {}).sort((a, b) => b[1].r - a[1].r);
  $('#ov-proxies tbody').innerHTML = proxies.map(([name, b]) => {
    const done = b.r - b.c;
    return `<tr><td>${name === 'direct' ? '直连' : esc(name)}</td><td class="num">${num(b.r)}</td><td class="num">${done > 0 ? pct(b.s / done) : '—'}</td><td class="num">${num(b.s)}</td><td class="num">${num(b.f)}</td><td class="num">${num(b.c)}</td></tr>`;
  }).join('') || '<tr><td colspan="6" class="empty">暂无请求</td></tr>';
  const hourly = o.stats.hourly;
  const peak = Math.max(1, ...hourly.map((b) => b.r));
  $('#ov-chart').innerHTML = hourly.map((b) => {
    const label = new Date(b.m * 60000).toLocaleString('zh-CN', { ...TZ, hour: '2-digit', minute: '2-digit' });
    return `<div class="col" title="${esc(label)} 请求 ${b.r}，失败 ${b.f}，取消 ${b.c}"><div class="seg ok" data-h="${b.s / peak * 100}"></div><div class="seg bad" data-h="${b.f / peak * 100}"></div><div class="seg cancel" data-h="${b.c / peak * 100}"></div></div>`;
  }).join('');
  applyWidths($('#ov-chart'));
  $('#ov-windows').innerHTML = o.windows.length ? o.windows.map((w) => {
    const used = w.limit ? w.used / w.limit * 100 : 0;
    return `<div class="win"><div class="row"><b>${esc(fmtDur(w.durationSeconds))}</b><span class="muted">剩余 ${num(w.remaining)} / ${num(w.limit)} credits</span></div><div class="bar ${used >= 98 ? 'bad' : used >= 90 ? 'warn' : ''}"><i data-w="${used}"></i></div></div>`;
  }).join('') : `<p class="muted">${pool === 'river' ? 'River 没有额度接口，额度用完靠上游 402 / 429 错误推断冷却。' : pool === 'ollama' ? '暂无余额数据：Ollama 的 key 直接读 /api/balance，30 秒内不会重复读（该接口每个用户每分钟限 10 次）。' : '暂无额度数据（需要账号配置 vela_ctrl_ 控制密钥）。'}</p>`;
  applyWidths($('#ov-windows'));
  $('#ov-status').innerHTML = Object.entries(o.status).map(([k, v]) => `<span class="badge ${statusClass(k)}">${esc(statusLabel(k))} ${v}</span>`).join('') || '<span class="muted">无账号</span>';
  $('#ov-updated').textContent = '更新于 ' + fmtTime(o.stats.generated);
}

// ---- accounts ----
const STATUS = { ok: ['可用', 'ok'], disabled: ['已禁用', ''], cooldown: ['冷却中', 'warn'], quarantined: ['已隔离', 'bad'], 'quota-exhausted': ['额度耗尽', 'bad'], 'model-cooldown': ['模型冷却', 'warn'], 'no-key': ['无密钥', 'bad'], 'not-entitled': ['套餐不含该模型', 'warn'], 'funding-cooldown': ['额度耗尽（推断）', 'warn'] };
const statusLabel = (s) => (STATUS[s] || [s])[0];
const statusClass = (s) => (STATUS[s] || ['', ''])[1];

// ollamaQuotaHTML shows an Ollama account's credits: the included allowance
// of the current period (or a legacy plan's session/weekly percentages) and
// the purchased balance next to it.
function ollamaQuotaHTML(a) {
  const q = a.quota;
  const u = a.usage24h || {};
  const usage = `<div class="acc-sub">24h：${num(u.r || 0)} 次 · ${num((u.i || 0) + (u.o || 0))} tokens</div>`;
  if (!q || !q.ollama) return '<span class="muted small">等待首次读取…</span>' + usage;
  const b = q.ollama;
  const parts = [];
  if (q.authFailed) parts.push('<span class="badge bad">API Key 失效，需换 key</span>');
  else if (q.lastError) parts.push(`<div class="acc-sub" title="${esc(q.lastError)}">读取出错：${esc(q.lastError)}</div>`);
  const bar = (used) => `<div class="bar ${used >= 98 ? 'bad' : used >= 90 ? 'warn' : ''}"><i data-w="${used}"></i></div>`;
  if (b.includedUsd !== undefined && b.includedUsd !== '') {
    const left = parseFloat(b.includedUsd), all = parseFloat(b.allowanceUsd);
    const used = Number.isFinite(left) && Number.isFinite(all) && all > 0 ? (all - left) / all * 100 : 0;
    parts.push(`<div class="win"><div class="acc-sub">赠送额度 · 剩余 <b>${usd(b.includedUsd)}</b> / ${usd(b.allowanceUsd)}</div>${bar(used)}`
      + (b.periodUntil ? `<div class="acc-sub">周期至 ${esc(fmtTime(b.periodUntil, true))}（${esc(fmtIn(b.periodUntil))}）</div>` : '') + `</div>`);
  }
  for (const [label, pctLeft, reset] of [['会话额度', b.sessionPercent, b.sessionResetsAt], ['周额度', b.weeklyPercent, b.weeklyResetsAt]]) {
    if (pctLeft === undefined || pctLeft < 0) continue;
    parts.push(`<div class="win"><div class="acc-sub">${label} · 剩余 <b>${num(pctLeft)}%</b></div>${bar(100 - pctLeft)}`
      + (reset ? `<div class="acc-sub">重置 ${esc(fmtTime(reset, true))}（${esc(fmtIn(reset))}）</div>` : '') + `</div>`);
  }
  parts.push(`<div class="acc-sub">已购额度 ${usd(b.purchasedUsd)}</div>`);
  if (!a.quotaFresh) parts.push('<span class="badge warn">数据过期</span>');
  parts.push(usage);
  return parts.join('');
}

function windowsHTML(a) {
  if (a.provider === 'ollama') return ollamaQuotaHTML(a);
  const q = a.quota;
  if (!a.hasControlKey) {
    const u = a.usage24h || {};
    return `<span class="badge">仅 API Key</span><div class="acc-sub" title="没有控制密钥读不到剩余额度；额度用完时按上游的余额不足 / 窗口用尽错误冷却，冷却逐次翻倍，成功一次清零。可用「重新设备授权」补上控制密钥">额度未知，按错误推断</div>` +
      `<div class="acc-sub">24h：${num(u.r || 0)} 次 · ${num((u.i || 0) + (u.o || 0))} tokens</div>`;
  }
  if (!q || (!q.planAt && !q.lastError)) return '<span class="muted small">等待首次读取…</span>';
  const parts = [];
  if (q.authFailed) parts.push('<span class="badge bad">控制密钥失效，需重新授权</span>');
  else if (q.lastError) parts.push(`<div class="acc-sub" title="${esc(q.lastError)}">读取出错：${esc(q.lastError)}</div>`);
  if (q.planAt) {
    if (!q.eligible || !q.windows.length) parts.push('<span class="badge">无生效套餐，按钱包计</span>');
    for (const w of q.windows) {
      const passed = w.resetsAt && new Date(w.resetsAt) <= new Date();
      const rem = passed ? w.limit : w.remaining;
      const used = w.limit ? (w.limit - rem) / w.limit * 100 : 100;
      parts.push(`<div class="win"><div class="acc-sub">${esc(fmtDur(w.durationSeconds))} · 剩余 <b>${num(rem)}</b> / ${num(w.limit)}</div><div class="bar ${used >= 98 ? 'bad' : used >= 90 ? 'warn' : ''}"><i data-w="${used}"></i></div><div class="acc-sub">重置 ${esc(fmtTime(w.resetsAt, true))}（${esc(fmtIn(w.resetsAt))}）</div></div>`);
    }
    if (!a.quotaFresh) parts.push('<span class="badge warn">数据过期</span>');
  }
  return parts.join('');
}

const effLimit = (own, learned) => { const v = [own, learned].filter((x) => x > 0); return v.length ? Math.min(...v) : '∞'; };

function accountRow(a) {
  const r = a.runtime;
  const st = r.state || {};
  const models = Object.entries(st.models || {});
  let status = `<span class="badge ${statusClass(r.status)}">${esc(statusLabel(r.status))}</span>`;
  if (st.cooldownUntil) status += `<div class="acc-sub">至 ${esc(fmtTime(st.cooldownUntil, true))}</div>`;
  if (st.ruleId) status += `<div class="acc-sub">规则 ${esc(st.ruleId)}${st.statusCode ? ' · ' + st.statusCode : ''}</div>`;
  if (st.learnedConcurrency || st.learnedRpm) status += `<div class="acc-sub" title="网关 tier_limit_exceeded 告知的套餐上限；点「恢复」清除">学到上限：${st.learnedConcurrency ? '并发 ' + st.learnedConcurrency : ''}${st.learnedRpm ? ' RPM ' + st.learnedRpm : ''}</div>`;
  const classes = Object.entries(st.classes || {});
  if (classes.length) status += classes.map(([c, x]) => `<div class="acc-sub" title="${esc(x.ruleId)}">${c === 'package' ? '套餐模型' : esc(c)}额度耗尽，至 ${esc(fmtTime(x.cooldownUntil, true))}</div>`).join('');
  const strikes = Object.entries(st.strikes || {}).filter(([, n]) => n > 1);
  if (strikes.length) status += `<div class="acc-sub" title="连续余额不足次数，冷却按次数翻倍，成功一次清零">连续失败：${strikes.map(([k, n]) => esc(k) + '×' + n).join(' ')}</div>`;
  if (models.length) status += `<div class="acc-sub" title="${esc(models.map(([m, s]) => m + ' 至 ' + fmtTime(s.cooldownUntil, true) + ' (' + s.ruleId + ')').join('\n'))}">${models.length} 个模型冷却中</div>`;
  const q = a.quota;
  const credits = (v) => { const f = parseFloat(v); return Number.isFinite(f) ? num(f) : '—'; };
  // Ollama keeps its own three amounts: included, purchased, and what this
  // pool sent in the last 24h
  const ollama = a.provider === 'ollama';
  const ou = a.usage24h || {};
  const wallet = ollama
    ? (q && q.ollama && q.ollama.purchasedUsd !== undefined && q.ollama.purchasedUsd !== ''
      ? usd(q.ollama.purchasedUsd) + `<div class="acc-sub">24h ${num(ou.r || 0)} 次 · ${num((ou.i || 0) + (ou.o || 0))} tokens</div>` : '—')
    : (q && q.walletUsd !== undefined && q.walletUsd !== '' ? usd(q.walletUsd) + (q.walletSource === 'preflight' ? '<div class="acc-sub">来源 preflight</div>' : '') : '—');
  const pkg = !ollama && q && q.summary ? `<div class="acc-sub" title="套餐模型额度：Design Plan 窗口用尽后、以及调用套餐模型（Claude Sonnet、GPT-6 Sol、Gemini 等）时扣这里，按计费周期重置不结转">套餐模型 ${credits(q.summary.subscriptionCredits)}</div><div class="acc-sub">充值 ${credits(q.summary.rechargeCredits)}</div>` : '';
  const cov = !ollama && q && q.coverage ? Object.entries(q.coverage) : [];
  const cat = q && q.catalog ? Object.entries(q.catalog) : [];
  const catOn = cat.filter(([, en]) => en).map(([m]) => m).sort();
  const catTitle = ollama ? '该 key 的 /v1/models（Ollama 云端目录对所有 key 相同）：\n' : '该 API Key 的模型列表（/v1/models），决定套餐能调用哪些模型：\n';
  const catHTML = cat.length ? `<div class="acc-sub" title="${esc(catTitle + catOn.join('\n'))}">可用模型 ${catOn.length}${cat.length > catOn.length ? ' / ' + cat.length : ''} 个</div>` : (q && q.catalogError ? `<div class="acc-sub">模型列表读取失败：${esc(q.catalogError)}</div>` : '');
  status += catHTML;
  const covHTML = cov.length ? `<div class="acc-sub" title="${esc(cov.map(([m, c]) => m + '：' + (c.covered === true ? 'Design Plan 模型' : c.covered === false ? '套餐模型' : '未知')).join('\n'))}">已识别 ${cov.length} 个模型的类别</div>` : '';
  const sum = !ollama && q && q.summary ? `<div class="acc-sub" title="累计消费 ${esc(q.summary.totalConsumedUsd)} · 累计充值 ${esc(q.summary.totalRechargedUsd)} · 调用 ${esc(q.summary.usageCount)} 次">今日 ${esc(usd(q.summary.todayConsumedUsd))}</div>` : '';
  const tier = q && (q.tier || q.membershipTier) ? `<span class="badge accent">${esc(q.tier || q.membershipTier)}</span>` : '';
  const shared = a.sharedWorkspaceWith && a.sharedWorkspaceWith.length ? `<span class="badge warn" title="与其他账号同一工作区，额度共享">共享工作区</span>` : '';
  return `<tr data-id="${esc(a.id)}">
    <td><div class="acc-name">${esc(a.name)}</div><div class="acc-sub">${esc(a.email || a.id)}</div>
      <div class="chips"><span class="badge">${esc(a.group)}</span>${tier}${a.hasProxy ? '<span class="badge" title="' + esc(a.proxyUrl) + '">代理</span>' : ''}${a.priority !== 50 ? '<span class="badge">优先级 ' + a.priority + '</span>' : ''}${shared}</div></td>
    <td>${status}</td>
    <td>${windowsHTML(a)}</td>
    <td class="num">${wallet}${pkg}${sum}${covHTML}</td>
    <td class="num">${r.active} / ${effLimit(a.maxConcurrent, st.learnedConcurrency)}</td>
    <td class="num">${r.rpm} / ${effLimit(a.maxRpm, st.learnedRpm)}</td>
    <td class="num">${r.health === null || r.health === undefined ? '<span class="muted">—</span>' : pct(r.health)}<div class="acc-sub">${r.samples} 样本</div></td>
    <td class="num">${num(r.selections)} / ${num(r.bindings)}</td>
    <td><div class="menu"><button class="btn small" data-menu>操作 ▾</button></div></td>
  </tr>`;
}

async function loadAccounts() {
  const d = await api('GET', withPool('/api/accounts'));
  accountsCache = d.accounts;
  const groups = [...new Set(d.accounts.map((a) => a.group))].sort();
  const sel = $('#acc-group-filter');
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部分组</option>' + groups.map((g) => `<option ${g === cur ? 'selected' : ''}>${esc(g)}</option>`).join('');
  const rows = d.accounts.filter((a) => !cur || a.group === cur);
  $('#acc-table tbody').innerHTML = rows.map(accountRow).join('');
  $('#acc-empty').classList.toggle('hidden', d.accounts.length > 0);
  applyWidths($('#acc-table'));
  await loadStaging();
}

// ---- staging: open-endpoint imports that collide with an account ----
const REASON = { runtimeKey: '运行密钥相同', controlKey: '控制密钥相同', name: '名称相同' };
async function loadStaging() {
  const d = await api('GET', withPool('/api/staging'));
  const items = d.items || [];
  const cnt = $('#stg-count');
  cnt.textContent = items.length + ' 条待处理';
  cnt.classList.toggle('hidden', items.length === 0);
  $('#stg-empty').classList.toggle('hidden', items.length > 0);
  $('#stg-table tbody').innerHTML = items.map((it) => {
    const confs = it.conflicts.length ? it.conflicts.map((c) => `<div class="conf"><b>${esc(c.name)}</b> <span class="badge">${esc(c.group)}</span> <span class="muted small">${esc(REASON[c.reason] || c.reason)}</span></div>`).join('') : '<span class="muted">现已无重复</span>';
    return `<tr data-id="${esc(it.id)}">
      <td><div class="acc-name">${esc(it.name || '（未命名）')}</div><div class="chips"><span class="badge">${esc(it.group)}</span>${it.proxyUrl ? '<span class="badge" title="' + esc(it.proxyUrl) + '">代理</span>' : ''}</div>${it.note ? '<div class="acc-sub">' + esc(it.note) + '</div>' : ''}</td>
      <td><code>${esc(it.runtimeKey)}</code>${it.controlKey ? '<div class="acc-sub"><code>' + esc(it.controlKey) + '</code></div>' : '<div class="acc-sub">无控制密钥</div>'}</td>
      <td class="num">${esc(fmtTime(it.importedAt))}<div class="acc-sub">密钥 ${esc(it.importKey)}</div>${it.updatedAt !== it.importedAt ? '<div class="acc-sub">再次提交 ' + esc(fmtTime(it.updatedAt, true)) + '</div>' : ''}</td>
      <td>${confs}</td>
      <td class="actions">${it.conflicts.length ? '<button class="btn small primary" data-s="overwrite">覆盖</button>' : ''}${it.canAdd ? '<button class="btn small" data-s="add">作为新账号添加</button>' : ''}<button class="btn small danger" data-s="discard">丢弃</button></td>
    </tr>`;
  }).join('');
  $$('#stg-table [data-s]').forEach((b) => b.addEventListener('click', () => stagingAction(items.find((x) => x.id === b.closest('tr').dataset.id), b.dataset.s)));
}
async function stagingAction(it, act) {
  if (!it) return;
  try {
    if (act === 'discard') {
      if (await modal({ title: '丢弃暂存账号', body: `<p>丢弃 <b>${esc(it.name || it.runtimeKey)}</b>？它的密钥不会写入任何账号。</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '丢弃', value: 'd', danger: true }] }) !== 'd') return;
      await api('DELETE', `/api/staging/${it.id}`); toast('已丢弃');
    } else if (act === 'add') {
      await api('POST', `/api/staging/${it.id}/apply`, { action: 'add' }); toast('已作为新账号添加');
    } else {
      const opts = it.conflicts.map((c, i) => `<option value="${esc(c.accountId)}" ${i === 0 ? 'selected' : ''}>${esc(c.name)}（${esc(c.group)}，${esc(REASON[c.reason] || c.reason)}）</option>`).join('');
      const r = await modal({ title: '覆盖现有账号',
        body: `<p>把 <b>${esc(it.name || '（未命名）')}</b> 的运行密钥${it.controlKey ? '、控制密钥' : ''}${it.name ? '和名称' : ''}写入所选账号，该账号的冷却/隔离会被清除，绑定到它的会话继续有效。</p>
          <div class="form"><label>覆盖账号</label><select id="stg-target">${opts}</select></div>`,
        buttons: [{ label: '取消', value: 'c' }, { label: '覆盖', value: 'ok', primary: true, action: () => api('POST', `/api/staging/${it.id}/apply`, { action: 'overwrite', target: $('#stg-target').value }) }] });
      if (r !== 'ok') return;
      toast('已覆盖');
    }
    await loadAccounts();
  } catch (e) { toast(e.message); }
}
$('#acc-group-filter').addEventListener('change', loadAccounts);
$('#btn-refresh-acc').addEventListener('click', () => loadAccounts().catch((e) => toast(e.message)));

document.addEventListener('click', (ev) => {
  const open = $('.menu-list');
  if (open && !ev.target.closest('.menu-list')) open.remove();
  if (open && ev.target.closest('[data-menu]')) return; // a second click on 操作 just closes it
  const btn = ev.target.closest('[data-menu]');
  if (!btn) return;
  const id = btn.closest('tr').dataset.id;
  const a = accountsCache.find((x) => x.id === id);
  if (!a) return;
  const items = [['edit', '编辑'], ['test', '测试连通'], ['refresh', a.provider === 'ollama' ? '刷新余额' : '刷新额度'], ['recover', '恢复（清除冷却/隔离）'], ['reset-health', '重置健康分'], ...(a.provider === 'opendesign' ? [['reauth', '重新设备授权']] : []), ['toggle', a.enabled ? '禁用' : '启用'], ['delete', '删除', 'danger']];
  const list = document.createElement('div');
  list.className = 'menu-list';
  list.innerHTML = items.map(([k, l, c]) => `<button data-act="${k}" class="${c || ''}">${esc(l)}</button>`).join('');
  // fixed to the viewport so the table's scroll container can't clip it
  document.body.appendChild(list);
  const rect = btn.getBoundingClientRect();
  list.style.right = Math.max(8, window.innerWidth - rect.right) + 'px';
  const below = rect.bottom + 4;
  list.style.top = (below + list.offsetHeight > window.innerHeight ? Math.max(8, rect.top - list.offsetHeight - 4) : below) + 'px';
  list.addEventListener('click', (e) => { const b = e.target.closest('[data-act]'); if (b) { list.remove(); accountAction(a, b.dataset.act); } });
});

window.addEventListener('scroll', () => { const m = $('.menu-list'); if (m) m.remove(); }, true);

async function accountAction(a, act) {
  try {
    switch (act) {
      case 'edit': return editAccount(a);
      case 'test': return testAccount(a);
      case 'refresh': await api('POST', `/api/accounts/${a.id}/refresh-quota`); toast('额度已刷新'); break;
      case 'recover': await api('POST', `/api/accounts/${a.id}/recover`); toast('已恢复'); break;
      case 'reset-health': await api('POST', `/api/accounts/${a.id}/reset-health`); toast('健康分已重置'); break;
      case 'reauth': return deviceAuth({ accountId: a.id });
      case 'toggle': await api('PUT', `/api/accounts/${a.id}`, { enabled: !a.enabled }); break;
      case 'delete':
        if (await modal({ title: '删除账号', body: `<p>确定删除 <b>${esc(a.name)}</b>？它的密钥会从配置中移除，绑定到它的会话会改派。</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '删除', value: 'del', danger: true }] }) !== 'del') return;
        await api('DELETE', `/api/accounts/${a.id}`); toast('已删除'); break;
    }
    await loadAccounts();
  } catch (e) { toast(e.message); }
}

function accountForm(a) {
  const v = (k, d = '') => esc(a ? (a[k] ?? d) : d);
  const defGroup = defaultGroup();
  return `<div class="form">
    <label>名称</label><input id="f-name" value="${v('name')}" maxlength="50">
    <label>邮箱</label><input id="f-email" value="${v('email')}">
    <label>分组</label><input id="f-group" value="${v('group', defGroup)}" placeholder="${defGroup}">
    <span class="hint">下游密钥只调度同分组的账号</span>
    <label>运行密钥</label><input id="f-rt" placeholder="${a ? esc(a.runtimeKey) + '（留空不变）' : RT_HINT[pool] || RT_HINT.opendesign}" autocomplete="off">
    ${isOllama() ? `<span></span><span class="hint">Ollama 的 key 直接读 /api/balance，没有单独的控制密钥；自建服务填任意值（如 <code>ollama</code>）即可</span>`
      : `<label>控制密钥</label><input id="f-ctrl" placeholder="${a && a.hasControlKey ? esc(a.controlKey) + '（留空不变）' : 'vela_ctrl_…（可选，用于读额度）'}" autocomplete="off">
    ${a && a.hasControlKey ? '<span></span><label class="check"><input type="checkbox" id="f-clear-ctrl">移除控制密钥</label>' : ''}`}
    ${a ? `<span></span><span class="keycopy"><button type="button" class="btn small" data-copy="runtimeKey">复制运行密钥</button>${a.hasControlKey ? '<button type="button" class="btn small" data-copy="controlKey">复制控制密钥</button>' : ''}</span>` : ''}
    ${!a ? `<span></span><span class="hint">新账号进 ${POOL_LABEL[pool]} 号池${isOllama() ? '（provider=ollama）' : ''}</span>` : ''}
    <label>最大并发</label><input id="f-conc" type="number" min="0" value="${v('maxConcurrent', 0)}">
    <span class="hint">0 表示不限</span>
    <label>每分钟请求</label><input id="f-rpm" type="number" min="0" value="${v('maxRpm', 0)}">
    <label>权重</label><input id="f-weight" type="number" min="1" max="100" value="${v('weight', 1)}">
    <label>优先级</label><input id="f-prio" type="number" min="1" max="100" value="${v('priority', 50)}">
    <span class="hint">数字越小越优先，同优先级内按策略排序</span>
    <label>出站代理</label><input id="f-proxy" placeholder="${a && a.hasProxy ? esc(a.proxyUrl) + '（留空不变）' : 'http:// 或 socks5://（可选）'}">
    ${a && a.hasProxy ? '<span></span><label class="check"><input type="checkbox" id="f-clear-proxy">移除代理（直连）</label>' : ''}
    <label>备注</label><input id="f-note" value="${v('note')}" maxlength="500">
    <label>启用</label><label class="check"><input type="checkbox" id="f-enabled" ${!a || a.enabled ? 'checked' : ''}>参与调度</label>
  </div>`;
}
function readAccountForm() {
  const n = (id) => Number($(id).value || 0);
  const out = { name: $('#f-name').value, email: $('#f-email').value, group: $('#f-group').value, note: $('#f-note').value,
    maxConcurrent: n('#f-conc'), maxRpm: n('#f-rpm'), weight: n('#f-weight'), priority: n('#f-prio'), enabled: $('#f-enabled').checked };
  if ($('#f-rt').value.trim()) out.runtimeKey = $('#f-rt').value.trim();
  if ($('#f-ctrl') && $('#f-ctrl').value.trim()) out.controlKey = $('#f-ctrl').value.trim();
  if (isOllama()) out.provider = 'ollama'; // an Ollama key has no prefix to read this from
  if ($('#f-proxy').value.trim()) out.proxyUrl = $('#f-proxy').value.trim();
  if ($('#f-clear-ctrl') && $('#f-clear-ctrl').checked) out.clearControlKey = true;
  if ($('#f-clear-proxy') && $('#f-clear-proxy').checked) out.clearProxy = true;
  return out;
}
let editingAccountId = null;
document.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-copy]');
  if (!b || !editingAccountId) return;
  try { copyText((await api('GET', `/api/accounts/${editingAccountId}/keys`))[b.dataset.copy]); } catch (err) { toast(err.message); }
});
async function editAccount(a) {
  editingAccountId = a.id;
  await modal({ title: '编辑账号', body: accountForm(a), buttons: [{ label: '取消', value: 'c' }, { label: '保存', value: 'ok', primary: true, action: async () => {
    await api('PUT', `/api/accounts/${a.id}`, readAccountForm()); toast('已保存');
  } }] });
  editingAccountId = null;
  loadAccounts().catch(() => {});
}
$('#btn-add-manual').addEventListener('click', async () => {
  await modal({ title: '手动添加账号', body: accountForm(null), buttons: [{ label: '取消', value: 'c' }, { label: '添加', value: 'ok', primary: true, action: async () => {
    const f = readAccountForm();
    if (!f.runtimeKey) throw new Error('运行密钥必填');
    await api('POST', '/api/accounts', f); toast('已添加');
  } }] });
  loadAccounts().catch(() => {});
});
$('#btn-import').addEventListener('click', async () => {
  const help = isOllama()
    ? `<p class="muted small">每行一个账号：<code>运行密钥[,名称[,代理URL]]</code>。Ollama 的 key 没有固定前缀，导入时整行按本池默认的 <code>provider=ollama</code> 处理；也可以在行首写 <code>provider:ollama</code> 明确指定。逗号、空格或制表符分隔，<code>#</code> 开头为注释。</p>`
    : `<p class="muted small">每行一个账号：<code>运行密钥[,控制密钥[,名称[,代理URL]]]</code>（<code>rv_</code> 开头的自动识别为 River 账号，无控制密钥），逗号、空格或制表符分隔，<code>#</code> 开头为注释。行首可写 <code>provider:ollama</code> 指定 Ollama 号池。</p>`;
  const ph = { opendesign: 'sk-amr-xxx,vela_ctrl_xxx,账号A\nrv_xxx,River-1', river: 'rv_xxx,River-1', ollama: 'xxxxxxxxxxxxxxxxxxxxxxxx,ollama-1' }[pool];
  await modal({ title: '批量导入', wide: true,
    body: `${help}
      <textarea id="imp-text" class="code" rows="10" placeholder="${esc(ph)}"></textarea>
      <div class="row"><label class="muted">分组</label><input id="imp-group" placeholder="${defaultGroup()}"></div>`,
    buttons: [{ label: '取消', value: 'c' }, { label: '导入', value: 'ok', primary: true, action: async () => {
      const r = await api('POST', '/api/accounts/import', { text: $('#imp-text').value, group: $('#imp-group').value, provider: isOllama() ? 'ollama' : '' });
      toast(`已导入 ${r.added} 个账号`);
    } }] });
  loadAccounts().catch(() => {});
});

async function testAccount(a) {
  const run = async (inference) => {
    $('#test-out').textContent = '测试中…';
    const r = await api('POST', `/api/accounts/${a.id}/test`, { inference, model: $('#test-model').value.trim() });
    $('#test-out').textContent = JSON.stringify(r, null, 2);
  };
  await modal({ title: '测试 ' + a.name, wide: true,
    body: `<p class="muted small">「免费检查」只读取${a.provider === 'ollama' ? '模型列表和 /api/balance 余额' : '模型列表和工作区'}，不消耗额度。「真实请求」会发一条 max_tokens=1 的推理，消耗少量 credits。</p>
      <div class="row"><input id="test-model" placeholder="真实请求模型，默认 ${esc(a.provider === 'ollama' ? 'gpt-oss:20b' : a.provider === 'river' ? 'deepseek-ai/DeepSeek-V4.1-Flash' : 'deepseek-v4.1-flash')}" size="34"></div>
      <pre id="test-out" class="mt">点击下方按钮开始</pre>`,
    buttons: [{ label: '关闭', value: 'c' }, { label: '真实请求', value: 'inf', action: async () => { await run(true); return false; } },
      { label: '免费检查', value: 'free', primary: true, action: async () => { await run(false); return false; } }] });
}

// ---- device authorization ----
async function deviceAuth(opts = {}) {
  let flow = null, timer = null;
  const groups = [...new Set(accountsCache.map((a) => a.group).concat([defaultGroup()]))].sort();
  const step1 = !opts.accountId ? `<div class="form"><label>分组</label><select id="da-group">${groups.map((g) => `<option>${esc(g)}</option>`).join('')}</select>
      <label>出站代理</label><input id="da-proxy" placeholder="可选，签发与后续请求都走它"></div>` : '';
  const target = opts.accountId ? accountsCache.find((a) => a.id === opts.accountId) : null;
  const render = () => {
    if (!flow) return;
    const st = { pending: '等待批准…', approved: '已批准', expired: '已过期', failed: '失败', cancelled: '已取消' }[flow.status] || flow.status;
    $('#da-flow').innerHTML = `<p>在<b>已登录目标账号</b>的浏览器中打开激活页，确认验证码后批准：</p>
      <div class="code-big">${esc(flow.userCode)}</div>
      <p><a href="${esc(flow.activationUrl)}" target="_blank" rel="noopener noreferrer" class="btn primary">打开激活页</a>
      <button type="button" class="btn ghost small" id="da-copy">复制链接</button></p>
      <p>状态：<span class="badge ${flow.status === 'approved' ? 'ok' : flow.status === 'pending' ? 'accent' : 'bad'}">${esc(st)}</span>
      ${flow.email ? ' · ' + esc(flow.email) : ''}${flow.error ? ' · ' + esc(flow.error) : ''}</p>
      <p class="muted small">有效期至 ${esc(fmtTime(flow.expiresAt))}。批准使用的是该浏览器会话，请确认登录的是正确账号。</p>`;
    const copy = $('#da-copy');
    if (copy) copy.onclick = () => navigator.clipboard.writeText(flow.activationUrl).then(() => toast('已复制'));
  };
  const poll = async () => {
    try {
      flow = await api('GET', `/api/device-auth/${flow.id}`);
      render();
      if (flow.status !== 'pending') {
        clearInterval(timer);
        if (flow.status === 'approved') { toast(flow.updated ? '已更新账号密钥：' + flow.email : '已添加账号：' + flow.email); loadAccounts().catch(() => {}); }
      }
    } catch (e) { modalErr(e.message); }
  };
  await modal({
    title: target ? '重新授权 ' + target.name : '设备授权添加账号',
    body: `${step1}<div id="da-flow" class="mt"><p class="muted">点「开始」向 OpenDesign 申请一个设备授权码。</p></div>`,
    onOpen: (dlg) => { dlg._onClose = () => { clearInterval(timer); if (flow && flow.status === 'pending') api('DELETE', `/api/device-auth/${flow.id}`).catch(() => {}); }; },
    buttons: [{ label: '关闭', value: 'c' }, { label: '开始', value: 'start', primary: true, action: async () => {
      if (flow && flow.status === 'pending') return false;
      const body = opts.accountId ? { accountId: opts.accountId } : { group: $('#da-group').value, proxyUrl: $('#da-proxy').value.trim() };
      flow = await api('POST', '/api/device-auth', body);
      render();
      clearInterval(timer);
      timer = setInterval(poll, 2000);
      return false;
    } }],
  });
}
$('#btn-add-device').addEventListener('click', () => deviceAuth());

// ---- client keys ----
async function loadKeys() {
  const d = await api('GET', withPool('/api/client-keys'));
  $('#key-table tbody').innerHTML = d.keys.map((k) => `<tr data-id="${esc(k.id)}">
    <td>${esc(k.name || '—')}</td><td><span class="badge">${esc(k.group)}</span></td><td><code>${esc(k.keyPrefix)}</code></td>
    <td><span class="badge ${k.enabled ? 'ok' : ''}">${k.enabled ? '启用' : '停用'}</span></td><td class="num">${esc(fmtTime(k.createdAt, true))}</td>
    <td class="actions"><button class="btn small" data-k="copy"${k.key ? '' : ' disabled title="该密钥创建于支持复制之前，无法找回，请新建"'}>复制</button><button class="btn small" data-k="toggle">${k.enabled ? '停用' : '启用'}</button><button class="btn small" data-k="edit">编辑</button><button class="btn small danger" data-k="del">删除</button></td></tr>`).join('');
  $('#key-empty').classList.toggle('hidden', d.keys.length > 0);
  await loadImportKeys();
  $$('#key-table [data-k]').forEach((b) => b.addEventListener('click', async () => {
    const id = b.closest('tr').dataset.id;
    const k = d.keys.find((x) => x.id === id);
    try {
      if (b.dataset.k === 'copy') return copyText(k.key);
      if (b.dataset.k === 'toggle') await api('PUT', `/api/client-keys/${id}`, { enabled: !k.enabled });
      if (b.dataset.k === 'del') {
        if (await modal({ title: '删除下游密钥', body: `<p>删除后使用 <code>${esc(k.keyPrefix)}</code> 的调用方会立即 401。</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '删除', value: 'd', danger: true }] }) !== 'd') return;
        await api('DELETE', `/api/client-keys/${id}`);
      }
      if (b.dataset.k === 'edit') {
        await modal({ title: '编辑密钥', body: `<div class="form"><label>名称</label><input id="k-name" value="${esc(k.name)}"><label>分组</label><input id="k-group" value="${esc(k.group)}"></div>`,
          buttons: [{ label: '取消', value: 'c' }, { label: '保存', value: 'ok', primary: true, action: () => api('PUT', `/api/client-keys/${id}`, { name: $('#k-name').value, group: $('#k-group').value }) }] });
      }
      loadKeys();
    } catch (e) { toast(e.message); }
  }));
}
$('#btn-new-key').addEventListener('click', async () => {
  let created = null;
  await modal({ title: '新建下游密钥',
    body: `<div class="form"><label>名称</label><input id="k-name" placeholder="new-api"><label>分组</label><input id="k-group" placeholder="${defaultGroup()}" value="${defaultGroup()}"><span class="hint">只调度该分组的账号；这把密钥归 ${POOL_LABEL[pool]} 号池，不会调度其他池的账号</span></div><div id="k-out"></div>`,
    buttons: [{ label: '关闭', value: 'c' }, { label: '创建', value: 'ok', primary: true, action: async () => {
      if (created) return false;
      created = await api('POST', '/api/client-keys', { name: $('#k-name').value, group: $('#k-group').value, provider: pool });
      $('#k-out').innerHTML = `<p class="mt"><b>密钥已创建，可随时在列表中复制：</b></p><div class="keybox"><code>${esc(created.key)}</code><button type="button" class="btn small" id="k-copy">复制</button></div>`;
      $('#k-copy').onclick = () => copyText(created.key);
      return false;
    } }] });
  loadKeys().catch(() => {});
});

// ---- import keys (open account-import endpoint) ----
async function loadImportKeys() {
  const d = await api('GET', withPool('/api/import-keys'));
  $('#import-url').textContent = location.origin + '/open/v1/accounts/import';
  $('#import-curl').textContent = `curl -X POST ${location.origin}/open/v1/accounts/import \\\n  -H 'Authorization: Bearer sk-odbimp-…' -H 'Content-Type: application/json' \\\n  -d '{"runtimeKey":"sk-amr-…","controlKey":"vela_ctrl_…","name":"账号A"}'`;
  $('#ik-table tbody').innerHTML = d.keys.map((k) => `<tr data-id="${esc(k.id)}">
    <td>${esc(k.name || '—')}</td><td><span class="badge">${esc(k.group)}</span></td><td><code>${esc(k.keyPrefix)}</code></td>
    <td><span class="badge ${k.enabled ? 'ok' : ''}">${k.enabled ? '启用' : '停用'}</span></td><td class="num">${esc(fmtTime(k.createdAt, true))}</td>
    <td class="actions"><button class="btn small" data-k="copy"${k.key ? '' : ' disabled title="该密钥创建于支持复制之前，无法找回，请新建"'}>复制</button><button class="btn small" data-k="toggle">${k.enabled ? '停用' : '启用'}</button><button class="btn small" data-k="edit">编辑</button><button class="btn small danger" data-k="del">删除</button></td></tr>`).join('');
  $('#ik-empty').classList.toggle('hidden', d.keys.length > 0);
  $$('#ik-table [data-k]').forEach((b) => b.addEventListener('click', async () => {
    const id = b.closest('tr').dataset.id;
    const k = d.keys.find((x) => x.id === id);
    try {
      if (b.dataset.k === 'copy') return copyText(k.key);
      if (b.dataset.k === 'toggle') await api('PUT', `/api/import-keys/${id}`, { enabled: !k.enabled });
      if (b.dataset.k === 'del') {
        if (await modal({ title: '删除导入密钥', body: `<p>删除后使用 <code>${esc(k.keyPrefix)}</code> 的自动上号脚本会立即 401。已导入的账号不受影响。</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '删除', value: 'd', danger: true }] }) !== 'd') return;
        await api('DELETE', `/api/import-keys/${id}`);
      }
      if (b.dataset.k === 'edit') {
        await modal({ title: '编辑导入密钥', body: `<div class="form"><label>名称</label><input id="ik-name" value="${esc(k.name)}"><label>默认分组</label><input id="ik-group" value="${esc(k.group)}"></div>`,
          buttons: [{ label: '取消', value: 'c' }, { label: '保存', value: 'ok', primary: true, action: () => api('PUT', `/api/import-keys/${id}`, { name: $('#ik-name').value, group: $('#ik-group').value }) }] });
      }
      loadImportKeys();
    } catch (e) { toast(e.message); }
  }));
  await loadImportLog();
}
async function loadImportLog() {
  // the console shows one pool at a time: each tab lists only its own imports
  const d = await api('GET', '/api/import-log?provider=' + encodeURIComponent(pool));
  const items = d.items || [];
  const n = (v) => v ? num(v) : '<span class="muted">0</span>';
  $('#ilog-table tbody').innerHTML = items.map((it) => `<tr>
    <td class="num">${esc(fmtTime(it.at))}</td>
    <td>${it.source === 'console' ? '管理台批量导入' : '开放接口'}${it.importKey ? '<div class="acc-sub">密钥 ' + esc(it.importKey) + '</div>' : ''}${it.group ? '<div class="acc-sub">分组 ' + esc(it.group) + '</div>' : ''}</td>
    <td><code>${esc(it.ip)}</code>${it.peer ? '<div class="acc-sub" title="TCP 对端（代理）">经 ' + esc(it.peer) + '</div>' : ''}</td>
    <td class="num">${n(it.total)}</td><td class="num">${n(it.added)}</td><td class="num">${n(it.staged)}</td><td class="num">${n(it.unchanged)}</td><td class="num">${n(it.failed)}</td>
    <td><span class="badge ${it.status === 200 ? 'ok' : it.status < 300 ? 'warn' : 'bad'}">${esc(it.status)}</span>${it.error ? '<div class="acc-sub">' + esc(it.error) + '</div>' : ''}</td>
  </tr>`).join('');
  $('#ilog-empty').classList.toggle('hidden', items.length > 0);
}
$('#btn-refresh-ilog').addEventListener('click', () => loadImportLog().catch((e) => toast(e.message)));
$('#btn-new-import-key').addEventListener('click', async () => {
  let created = null;
  await modal({ title: '新建导入密钥',
    body: `<div class="form"><label>名称</label><input id="ik-name" placeholder="signup-bot"><span class="hint">会写进导入账号的备注，便于追溯来源</span><label>默认分组</label><input id="ik-group" placeholder="${defaultGroup()}" value="${defaultGroup()}"><span class="hint">请求不带 group、provider 时导入到该分组，并进 ${POOL_LABEL[pool]} 号池</span></div><div id="ik-out"></div>`,
    buttons: [{ label: '关闭', value: 'c' }, { label: '创建', value: 'ok', primary: true, action: async () => {
      if (created) return false;
      created = await api('POST', '/api/import-keys', { name: $('#ik-name').value, group: $('#ik-group').value, provider: pool });
      $('#ik-out').innerHTML = `<p class="mt"><b>密钥已创建，可随时在列表中复制：</b></p><div class="keybox"><code>${esc(created.key)}</code><button type="button" class="btn small" id="ik-copy">复制</button></div>`;
      $('#ik-copy').onclick = () => copyText(created.key);
      return false;
    } }] });
  loadImportKeys().catch(() => {});
});

// ---- settings ----
const STRATEGY_HINTS = {
  smart: '智能（默认，参考 magpie Smart）：余量充足的账号里，窗口最早重置的先用，避免额度在重置时浪费；余量低于阈值的排后，钱包付费的最后',
  pace: '周额度节奏：剩余比例 ÷ 距重置小时数，越高越先用',
  'least-used': '用量最少优先：按窗口已用比例，其次按近期 token',
  weighted: '按权重平滑轮询（NGINX 平滑加权）',
  roundrobin: '轮询：最久未被选中者优先',
  'least-selections': '选中次数最少优先，平局按在途数',
  'least-connections': '在途请求最少优先',
  ordered: '按配置顺序：前一个用尽或失败才用下一个',
};
const ROUTING_FIELDS = [
  ['strategy', '选号策略', 'strategy'],
  ['sticky', '会话粘性', 'bool', '同一会话固定到首次应答的账号，命中上游缓存'],
  ['onBindingBusy', '绑定账号忙时', 'select', ['overflow', 'wait-overflow', 'reject'], 'overflow 临时借用其他账号但不改绑；wait-overflow 先等待再借；reject 直接 429'],
  ['explicitTtlSec', '显式会话 TTL（秒）', 'int', '有会话头/字段时，滑动过期'],
  ['fallbackHash', '消息哈希弱粘性', 'bool', '没有会话标识时，按首条 system 与 user 消息生成指纹'],
  ['fallbackTtlSec', '弱粘性 TTL（秒）', 'int'],
  ['sessionHeaders', '额外会话头', 'list', '逗号分隔，优先于内置会话头，例如 X-User-Session'],
  ['concurrencyWaitMs', '满载等待（毫秒）', 'int', '所有账号并发/RPM 满时最多等待，超时返回 429'],
  ['maxAccountsPerRequest', '单请求最多账号', 'int', '仅在首包前、账号被冷却/隔离/401 时换号'],
  ['walletFallback', '允许钱包付费', 'bool', '付费顺序：Design Plan 窗口（仅 Design Plan 模型）→ 套餐模型额度 → 钱包/充值。关闭后只剩钱包能付的账号不调度'],
  ['ignoreCatalog', '忽略套餐模型权限', 'bool', '默认按每把 API Key 自己的 /v1/models 判断套餐能调哪些模型，套餐外的模型不派给该账号；开启后不做此检查'],
  ['lowShare', '已用多少 % 开始靠后', 'float', '按已用百分比比较（不是剩余）：账号用得最满的额度窗口已用达到该值，就排到余量充足的账号之后。默认 90'],
  ['spentShare', '已用多少 % 视为耗尽', 'float', '已用达到该值，再往后排（仍可被选中，不会被排除）。须不小于上一项，默认 98'],
  ['healthFilter', '健康过滤', 'bool', '24h 成功率低于阈值的账号不参与新会话（样本少于 20 视为健康）'],
  ['minimumHealth', '最低健康分', 'float'],
  ['firstEventTimeoutSec', '流式首包超时（秒）', 'int'],
  ['idleTimeoutSec', '流式空闲超时（秒）', 'int'],
  ['heartbeatSec', '心跳间隔（秒）', 'int'],
  ['requestTimeoutSec', '非流式超时（秒）', 'int'],
  ['maxBodyMB', '请求体上限（MB）', 'int'],
  ['modelsCacheSec', '模型列表缓存（秒）', 'int'],
];
const QUOTA_FIELDS = [
  ['enabled', '启用额度轮询', 'bool'],
  ['intervalSec', '轮询间隔（秒）', 'int', '建议 30–60 秒'],
  ['concurrency', '并发读取数', 'int'],
  ['staleSec', '快照过期（秒）', 'int', '过期后按未知额度处理'],
];
const LOG_FIELDS = [
  ['retentionDays', '保留天数', 'int'], ['maxRequests', '请求日志条数', 'int'], ['maxErrors', '错误日志条数', 'int'], ['maxTotalMB', '总容量（MB）', 'int'],
  ['detail', '详细日志', 'bool', '记录请求头/体、响应头/体（凭据已脱敏），排查完请关闭'], ['maxDetails', '详细日志条数', 'int'], ['detailBodyKB', '单个体上限（KB）', 'int', '超出时保留头尾各一半、省略中间；内存中的详细日志总量上限 256MB，超出自动淘汰最旧的'],
];
function renderForm(el, fields, obj, prefix) {
  el.innerHTML = fields.map(([k, label, type, extra, hint]) => {
    const id = `${prefix}-${k}`;
    const v = obj[k];
    let input;
    if (type === 'bool') input = `<label class="check"><input type="checkbox" id="${id}" ${v ? 'checked' : ''}>开启</label>`;
    else if (type === 'strategy') input = `<select id="${id}">${settingsCache.strategies.map((s) => `<option ${s === v ? 'selected' : ''}>${esc(s)}</option>`).join('')}</select>`;
    else if (type === 'select') input = `<select id="${id}">${extra.map((s) => `<option ${s === v ? 'selected' : ''}>${esc(s)}</option>`).join('')}</select>`;
    else if (type === 'list') input = `<input id="${id}" value="${esc((v || []).join(', '))}">`;
    else input = `<input id="${id}" type="number" step="${type === 'float' ? 'any' : '1'}" value="${esc(v)}">`;
    const h = type === 'select' ? hint : extra;
    const hintHTML = type === 'strategy' ? `<span class="hint" id="${id}-hint">${esc(STRATEGY_HINTS[v] || '')}</span>` : (h && typeof h === 'string' ? `<span class="hint">${esc(h)}</span>` : '');
    return `<label for="${id}">${esc(label)}</label>${input}${hintHTML}`;
  }).join('');
}
function readForm(fields, prefix, base) {
  const out = { ...base };
  for (const [k, , type] of fields) {
    const el = $(`#${prefix}-${k}`);
    if (type === 'bool') out[k] = el.checked;
    else if (type === 'int') out[k] = parseInt(el.value || '0', 10);
    else if (type === 'float') out[k] = parseFloat(el.value || '0');
    else if (type === 'list') out[k] = el.value.split(',').map((s) => s.trim()).filter(Boolean);
    else out[k] = el.value;
  }
  return out;
}
// ---- error / retry rules (visual editor) ----
const clone = (v) => JSON.parse(JSON.stringify(v));
let errRules = [], retRules = [];
const SCOPES = { account: '整个账号', model: '账号 × 模型', funding: '同一付费来源的所有模型' };
const ACTIONS = { ignore: '忽略（不扣分、不换号）', degrade: '仅降级（扣健康分）', cooldown: '冷却', 'hard-quarantine': '隔离（需人工处理）' };
const RESET_FORMATS = { '': '不读响应头', 'retry-after': 'Retry-After（秒数或日期）', 'unix-seconds': 'Unix 秒时间戳', 'unix-milliseconds': 'Unix 毫秒时间戳', duration: '时长（如 30s）' };
const lines = (t) => String(t || '').split('\n').map((x) => x.trim()).filter(Boolean);
const matchSummary = (w) => {
  const parts = [];
  if ((w.statuses || []).length) parts.push(`状态码 ${w.statuses.join(' / ')}`);
  if ((w.bodyContains || []).length) parts.push(`正文含 ${w.bodyContains.map((x) => '“' + x + '”').join(' 或 ')}`);
  Object.entries(w.headers || {}).forEach(([k, v]) => parts.push(v ? `响应头 ${k} 含 “${v}”` : `带响应头 ${k}`));
  return parts.length ? parts.map((x) => `<div>${esc(x)}</div>`).join('') : '<span class="muted">（无条件，不会命中）</span>';
};
function errActionHTML(r) {
  const act = { ignore: ['忽略', ''], degrade: ['仅降级', 'warn'], cooldown: ['冷却', 'warn'], 'hard-quarantine': ['隔离', 'bad'] }[r.action] || [r.action, ''];
  let sub = SCOPES[r.scope] || r.scope;
  if (r.action === 'cooldown' && r.reset) sub += `，默认 ${r.reset.fallback}${r.reset.max ? '，最长 ' + r.reset.max : ''}${r.reset.escalate ? '，连续命中翻倍' : ''}`;
  if (r.refreshQuota) sub += '，并刷新额度';
  if (r.downstreamStatus) sub += `，回给下游 ${r.downstreamStatus}`;
  return `<span class="badge ${act[1]}">${esc(act[0])}</span><div class="acc-sub">${esc(sub)}</div>`;
}
function renderRules() {
  const btns = (i, n) => `<button class="btn small" data-r="up" ${i === 0 ? 'disabled' : ''} title="上移">↑</button><button class="btn small" data-r="down" ${i === n - 1 ? 'disabled' : ''} title="下移">↓</button><button class="btn small" data-r="toggle">`;
  $('#rules-error tbody').innerHTML = errRules.map((r, i) => `<tr data-i="${i}" class="${r.disabled ? 'off' : ''}">
    <td class="num">${i + 1}</td><td><b>${esc(r.id)}</b>${r.disabled ? ' <span class="badge">已停用</span>' : ''}<div class="acc-sub">${esc(r.note || '')}</div></td>
    <td>${matchSummary(r.when || {})}</td><td>${errActionHTML(r)}</td>
    <td class="actions">${btns(i, errRules.length)}${r.disabled ? '启用' : '停用'}</button><button class="btn small" data-r="edit">编辑</button><button class="btn small danger" data-r="del">删除</button></td></tr>`).join('');
  $('#rules-retry tbody').innerHTML = retRules.map((r, i) => `<tr data-i="${i}" class="${r.disabled ? 'off' : ''}">
    <td class="num">${i + 1}</td><td><b>${esc(r.id)}</b>${r.disabled ? ' <span class="badge">已停用</span>' : ''}<div class="acc-sub">${esc(r.note || '')}</div></td>
    <td>${matchSummary(r.when || {})}</td>
    <td class="actions">${btns(i, retRules.length)}${r.disabled ? '启用' : '停用'}</button><button class="btn small" data-r="edit">编辑</button><button class="btn small danger" data-r="del">删除</button></td></tr>`).join('');
  $('#rules-error-empty').classList.toggle('hidden', errRules.length > 0);
  $('#rules-retry-empty').classList.toggle('hidden', retRules.length > 0);
}
function bindRuleTable(sel, list, kind) {
  $(sel).addEventListener('click', async (ev) => {
    const b = ev.target.closest('button[data-r]');
    if (!b) return;
    const arr = list();
    const i = Number(b.closest('tr').dataset.i);
    const act = b.dataset.r;
    if (act === 'up' || act === 'down') {
      const j = act === 'up' ? i - 1 : i + 1;
      [arr[i], arr[j]] = [arr[j], arr[i]];
    } else if (act === 'toggle') {
      arr[i].disabled = !arr[i].disabled || undefined;
    } else if (act === 'del') {
      if (await modal({ title: '删除规则', body: `<p>删除规则 <b>${esc(arr[i].id)}</b>？保存设置后生效。</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '删除', value: 'd', danger: true }] }) !== 'd') return;
      arr.splice(i, 1);
    } else if (act === 'edit') {
      await editRule(kind, arr, i);
    }
    renderRules();
  });
}
bindRuleTable('#rules-error', () => errRules, 'error');
bindRuleTable('#rules-retry', () => retRules, 'retry');
$('#btn-add-error-rule').addEventListener('click', async () => { await editRule('error', errRules, -1); renderRules(); });
$('#btn-add-retry-rule').addEventListener('click', async () => { await editRule('retry', retRules, -1); renderRules(); });

async function editRule(kind, arr, idx) {
  const isErr = kind === 'error';
  const r = idx >= 0 ? clone(arr[idx]) : (isErr ? { id: '', scope: 'account', action: 'cooldown', when: {}, reset: { fallback: '30s' } } : { id: '', decision: 'stop', when: {} });
  const w = r.when || {};
  const rs = r.reset || {};
  const opts = (m, v) => Object.entries(m).map(([k, t]) => `<option value="${esc(k)}" ${k === v ? 'selected' : ''}>${esc(t)}</option>`).join('');
  const hdrs = Object.entries(w.headers || {}).map(([k, v]) => v ? `${k}: ${v}` : k).join('\n');
  const body = `<div class="form">
    <label>规则名称</label><input id="r-id" value="${esc(r.id)}" placeholder="如 rate-limited（字母数字 _ . -）">
    <label>备注</label><input id="r-note" value="${esc(r.note || '')}">
    <div class="sect">命中条件（多项同时满足；每项内部任一即可）</div>
    <label>状态码</label><input id="r-st" value="${esc((w.statuses || []).join(', '))}" placeholder="如 429, 503，逗号分隔"><span class="hint">留空表示不限状态码</span>
    <label>正文包含</label><textarea id="r-body" rows="3" placeholder="每行一个关键字，不区分大小写">${esc((w.bodyContains || []).join('\n'))}</textarea><span class="hint">响应正文含其中任意一个即命中</span>
    <label>响应头</label><textarea id="r-hdr" rows="2" placeholder="每行一个：Header: 子串（子串可省略）">${esc(hdrs)}</textarea>
    ${isErr ? `<div class="sect">处理方式</div>
    <label>作用范围</label><select id="r-scope">${opts(SCOPES, r.scope)}</select>
    <label>动作</label><select id="r-action">${opts(ACTIONS, r.action)}</select>
    <div id="r-reset" class="form" style="grid-column:1/-1;grid-template-columns:120px 1fr">
      <label>默认冷却</label><input id="r-fb" value="${esc(rs.fallback || '')}" placeholder="如 30s、10m、24h"><span class="hint">读不到恢复时间时使用</span>
      <label>最长冷却</label><input id="r-max" value="${esc(rs.max || '')}" placeholder="如 8d，可留空"><span class="hint">支持 s / m / h / d</span>
      <label>读取响应头</label><input id="r-rh" value="${esc(rs.header || '')}" placeholder="如 Retry-After，可留空">
      <label>响应头格式</label><select id="r-rf">${opts(RESET_FORMATS, rs.format || '')}</select>
      <label>正文时间前缀</label><textarea id="r-ba" rows="2" placeholder="每行一个，如 try again after">${esc((rs.bodyAfter || []).join('\n'))}</textarea><span class="hint">正文中这些词后面跟的 RFC3339 时间作为恢复时间</span>
      <span></span><label class="check"><input type="checkbox" id="r-esc" ${rs.escalate ? 'checked' : ''}>连续命中时冷却翻倍（成功一次清零）</label>
    </div>
    <span></span><label class="check"><input type="checkbox" id="r-rq" ${r.refreshQuota ? 'checked' : ''}>同时立即刷新该账号额度</label>
    <label>回给下游状态码</label><input id="r-ds" type="number" value="${r.downstreamStatus || ''}" placeholder="默认"><span class="hint">最后一次失败时返回给下游的状态码（400–599），留空用默认映射</span>` : ''}
    <span></span><label class="check"><input type="checkbox" id="r-off" ${r.disabled ? 'checked' : ''}>停用此规则</label>
  </div>`;
  await modal({ title: (idx >= 0 ? '编辑' : '添加') + (isErr ? '错误规则' : '重试规则'), body, wide: true,
    onOpen: isErr ? () => { const sync = () => { $('#r-reset').style.display = $('#r-action').value === 'cooldown' ? '' : 'none'; }; $('#r-action').addEventListener('change', sync); sync(); } : undefined,
    buttons: [{ label: '取消', value: 'c' }, { label: '确定', value: 'ok', primary: true, action: () => {
      const out = { ...r, id: $('#r-id').value.trim(), note: $('#r-note').value.trim() || undefined, disabled: $('#r-off').checked || undefined };
      if (!out.id) throw new Error('规则名称必填');
      if (arr.some((x, j) => j !== idx && x.id === out.id)) throw new Error('规则名称重复');
      const statuses = $('#r-st').value.split(/[,，\s]+/).filter(Boolean).map(Number);
      if (statuses.some((n) => !Number.isInteger(n) || n < 100 || n > 599)) throw new Error('状态码必须是 100–599 的整数');
      const headers = {};
      lines($('#r-hdr').value).forEach((l) => { const k = l.indexOf(':'); if (k < 0) headers[l] = ''; else headers[l.slice(0, k).trim()] = l.slice(k + 1).trim(); });
      out.when = { statuses: statuses.length ? statuses : undefined, bodyContains: lines($('#r-body').value).length ? lines($('#r-body').value) : undefined, headers: Object.keys(headers).length ? headers : undefined };
      if (!statuses.length && !out.when.bodyContains && !out.when.headers) throw new Error('至少设置一个命中条件');
      if (isErr) {
        out.scope = $('#r-scope').value; out.action = $('#r-action').value;
        out.refreshQuota = $('#r-rq').checked || undefined;
        const ds = parseInt($('#r-ds').value || '0', 10);
        if (ds && (ds < 400 || ds > 599)) throw new Error('下游状态码必须在 400–599');
        out.downstreamStatus = ds || undefined;
        if (out.action === 'cooldown') {
          if (!$('#r-fb').value.trim()) throw new Error('冷却动作必须填默认冷却时长');
          const rh = $('#r-rh').value.trim(), rf = $('#r-rf').value;
          if (rh && !rf) throw new Error('填了响应头就要选响应头格式');
          out.reset = { header: rh || undefined, format: rf || undefined, bodyAfter: lines($('#r-ba').value).length ? lines($('#r-ba').value) : undefined,
            fallback: $('#r-fb').value.trim(), max: $('#r-max').value.trim() || undefined, escalate: $('#r-esc').checked || undefined };
        } else delete out.reset;
      }
      if (idx >= 0) arr[idx] = out; else arr.push(out);
    } }] });
}

// ---- model prices ----
let priceRows = [];
const PRICE_COLS = [['model', 'text', 22], ['currency', 'cur'], ['input', 'n'], ['cachedInput', 'n'], ['output', 'n'], ['peakMultiplier', 'n'], ['from', 'text', 10],
  ['longThreshold', 'n'], ['longInput', 'n'], ['longCachedInput', 'n'], ['longOutput', 'n'], ['note', 'text', 24]];
function renderPrices() {
  $('#price-table tbody').innerHTML = priceRows.map((e, i) => '<tr>' + PRICE_COLS.map(([k, t, w]) => {
    const v = e[k] === undefined || e[k] === 0 && t === 'n' ? '' : e[k];
    if (t === 'cur') return `<td><select data-i="${i}" data-f="${k}">${['CNY', 'USD'].map((c) => `<option ${c === e[k] ? 'selected' : ''}>${c}</option>`).join('')}</select></td>`;
    return `<td><input data-i="${i}" data-f="${k}" value="${esc(v)}" ${t === 'n' ? 'type="number" step="any" min="0" size="7"' : `size="${w}"`}></td>`;
  }).join('') + `<td><button class="btn ghost small" data-del="${i}">删除</button></td></tr>`).join('');
}
function readPricing() {
  const num0 = (v) => (v === undefined || v === '' ? 0 : Number(v));
  const models = priceRows.map((e) => {
    const o = { model: String(e.model || '').trim(), currency: e.currency || 'USD' };
    for (const k of ['input', 'cachedInput', 'output', 'peakMultiplier', 'longThreshold', 'longInput', 'longCachedInput', 'longOutput']) o[k] = num0(e[k]);
    if (e.from) o.from = String(e.from).trim();
    if (e.note) o.note = e.note;
    return o;
  });
  const holidays = $('#price-holidays').value.split(/[\s,，]+/).filter(Boolean);
  return { models, holidays };
}
$('#price-table').addEventListener('input', (ev) => { const t = ev.target; if (t.dataset.f) priceRows[+t.dataset.i][t.dataset.f] = t.value; });
$('#price-table').addEventListener('click', (ev) => { const b = ev.target.closest('button[data-del]'); if (b) { priceRows.splice(+b.dataset.del, 1); renderPrices(); } });
$('#btn-add-price').addEventListener('click', () => { priceRows.unshift({ model: '', currency: 'USD', input: 0, cachedInput: 0, output: 0 }); renderPrices(); });
$('#btn-default-pricing').addEventListener('click', () => { priceRows = clone(settingsCache.defaults.pricing); renderPrices(); toast('已填入默认价格，保存后生效'); });

async function loadSettings() {
  settingsCache = await api('GET', withPool('/api/settings'));
  const s = settingsCache.settings;
  renderForm($('#form-routing'), ROUTING_FIELDS, s.routing, 'rt');
  renderForm($('#form-quota'), QUOTA_FIELDS, s.quota, 'qt');
  renderForm($('#form-logs'), LOG_FIELDS, s.logs, 'lg');
  $('#rt-strategy').addEventListener('change', (e) => { $('#rt-strategy-hint').textContent = STRATEGY_HINTS[e.target.value] || ''; });
  errRules = clone(s.errorRules || []);
  retRules = clone(s.retryRules || []);
  $('#rules-pool').textContent = POOL_LABEL[pool] + ' 号池';
  renderRules();
  priceRows = clone(s.pricing.models || []);
  $('#price-holidays').value = (s.pricing.holidays || []).join(', ');
  renderPrices();
  $('#set-rev').textContent = '配置版本 ' + s.revision;
  const ov = await api('GET', withPool('/api/overview'));
  $('#pv-group').innerHTML = (ov.groups.length ? ov.groups : ['default']).map((g) => `<option>${esc(g)}</option>`).join('');
}
$('#btn-save-settings').addEventListener('click', async () => {
  const s = settingsCache.settings;
  const errorRules = errRules, retryRules = retRules;
  const body = { revision: s.revision, upstream: s.upstream, routing: readForm(ROUTING_FIELDS, 'rt', s.routing), quota: readForm(QUOTA_FIELDS, 'qt', s.quota), logs: readForm(LOG_FIELDS, 'lg', s.logs), pricing: readPricing(), errorRules, retryRules };
  try { await api('PUT', withPool('/api/settings'), body); toast('已保存'); loadSettings(); } catch (e) { toast(e.message); }
});
$('#btn-default-rules').addEventListener('click', () => {
  errRules = clone(settingsCache.defaults.errorRules);
  retRules = clone(settingsCache.defaults.retryRules);
  renderRules();
  toast('已填入默认规则，保存后生效');
});
$('#btn-preview').addEventListener('click', async () => {
  try {
    const r = await api('POST', '/api/workflow/preview', { group: $('#pv-group').value, model: $('#pv-model').value.trim() });
    const accs = await api('GET', withPool('/api/accounts'));
    const name = (id) => (accs.accounts.find((a) => a.id === id) || { name: id }).name;
    const ex = Object.entries(r.excluded);
    $('#pv-result').innerHTML = `<div><b>候选顺序：</b>${r.order.length ? r.order.map((id, i) => `${i + 1}. ${esc(name(id))}`).join('　') : '无'}</div>` +
      (ex.length ? `<div class="mt"><b>被排除：</b>${ex.map(([id, why]) => `${esc(name(id))}（${esc(statusLabel(why))}）`).join('、')}</div>` : '') +
      '<div class="mt">绑定命中的会话不走此顺序。预览不占用并发、不改计数。</div>';
  } catch (e) { toast(e.message); }
});
$('#btn-reset-counts').addEventListener('click', async () => { try { await api('POST', '/api/workflow/reset-counts'); toast('已清零'); } catch (e) { toast(e.message); } });

// ---- logs ----
const logState = { requests: { cursor: '', items: [] }, errors: { cursor: '', items: [] }, details: { cursor: '', items: [] } };
const LOG_PREFIX = { requests: 'rq', errors: 'er', details: 'dt' };
function accountOptions(sel) {
  const cur = sel.value;
  sel.innerHTML = '<option value="">全部账号</option>' + accountsCache.map((a) => `<option value="${esc(a.id)}" ${a.id === cur ? 'selected' : ''}>${esc(a.name)}</option>`).join('');
}
function statusBadge(st, result) {
  const cls = result === 'success' || (st >= 200 && st < 300) ? 'ok' : result === 'client_cancelled' || st === 499 ? '' : st === 429 ? 'warn' : 'bad';
  return `<span class="badge ${cls}">${esc(st)}</span>`;
}
const BIND = { hit: '命中', miss: '新绑定', provisional: '待确认', invalidated: '失效改派', 'temporary-overflow': '临时溢出', 'not-applicable': '—' };
function cacheRateCell(r) {
  let v = r.cacheRate;
  if (v === undefined && r.inputTokens != null && r.cachedTokens != null) { // logged before cacheRate existed
    const total = r.inputTokens + (r.inboundProtocol === 'anthropic' ? r.cachedTokens : 0);
    v = total > 0 ? Math.min(r.cachedTokens / total, 1) : null;
  }
  if (v === null || v === undefined) return '<span class="acc-sub">—</span>';
  return `<span class="badge ${v >= 0.8 ? 'ok' : v >= 0.3 ? '' : 'warn'}">${(v * 100).toFixed(1)}%</span>`;
}
const CUR = { CNY: '¥', USD: '$' };
function money(v, cur) { return (CUR[cur] || '') + (v >= 1 ? v.toFixed(2) : v >= 0.01 ? v.toFixed(4) : v.toFixed(6)); }
function costSum(b) { return [b.cc ? money(b.cc, 'CNY') : '', b.cu ? money(b.cu, 'USD') : ''].filter(Boolean).join(' + ') || '—'; }
// cached share of the prompt (b.p, backfilled from input for old buckets on load)
function hitRate(b) { return b.p ? Math.min(b.k / b.p, 1) : null; }
function costCell(r) { return r.cost === undefined || r.cost === null ? '—' : esc(money(r.cost, r.currency)); }
function keyCell(r) { return esc(r.clientKeyName || r.clientKeyId || '—'); }
function requestRow(r) {
  const tok = [r.inputTokens, r.outputTokens, r.cachedTokens].map((v) => v === null || v === undefined ? '—' : num(v)).join(' / ');
  return `<tr data-rid="${esc(r.requestId)}"><td class="num">${esc(fmtTime(r.ts))}</td><td><span class="acc-sub">${esc(r.protocol || r.inboundProtocol || '—')}</span></td><td>${esc(r.model)}${r.stream ? ' <span class="badge">流</span>' : ''}</td>
    <td>${keyCell(r)}</td>
    <td>${esc(r.accountName || '—')}${r.switched ? ' <span class="badge warn">换号</span>' : ''}${r.overflow ? ' <span class="badge warn">溢出</span>' : ''}</td>
    <td>${statusBadge(r.status, r.result)}${r.upstreamStatus && r.upstreamStatus !== r.status ? `<div class="acc-sub">上游 ${esc(r.upstreamStatus)}</div>` : ''}${r.errorCategory ? `<div class="acc-sub">${esc(r.errorCategory)}${r.blockedBy ? '/' + esc(r.blockedBy) : ''}</div>` : ''}</td>
    <td><span class="acc-sub">${esc(BIND[r.bindingResult] || r.bindingResult)}</span><div class="acc-sub">${esc(r.affinityKeyType)}</div></td>
    <td class="num">${r.ttftMs === null || r.ttftMs === undefined ? '—' : num(r.ttftMs) + 'ms'}</td><td class="num">${num(r.durationMs)}ms<div class="acc-sub">${costCell(r)}</div></td>
    <td class="num">${tok}<div>${cacheRateCell(r)}</div></td><td class="num">${r.attempts ? r.attempts.length : 0}</td></tr>`;
}
function detailRow(d) {
  const sz = (n, cut) => num(n) + ' B' + (cut ? ' <span class="badge warn">截断</span>' : '');
  return `<tr><td class="num">${esc(fmtTime(d.ts))}</td><td><span class="acc-sub">${esc(d.protocol)}</span></td><td>${esc(d.model)}</td><td>${keyCell(d)}</td><td>${statusBadge(d.status)}</td>
    <td class="num">${sz(d.requestBodyBytes, d.requestBodyTruncated)}</td><td class="num">${sz(d.responseBodyBytes, d.responseBodyTruncated)}</td><td class="small">${esc(d.requestId)}</td></tr>`;
}
function showDetail(d) {
  const pretty = (t) => { try { return JSON.stringify(JSON.parse(t), null, 2); } catch { return t; } };
  const sec = (title, text) => `<h4>${esc(title)}</h4><pre>${esc(text)}</pre>`;
  modal({ title: `${d.method} ${d.path} → ${d.status}${d.clientKeyName || d.clientKeyId ? '（密钥 ' + (d.clientKeyName || d.clientKeyId) + '）' : ''}`, wide: true, body:
    sec('请求头', JSON.stringify(d.requestHeaders, null, 2)) + sec('请求体' + (d.requestBodyTruncated ? '（已截断）' : ''), pretty(d.requestBody)) +
    sec('响应头', JSON.stringify(d.responseHeaders, null, 2)) + sec('响应体' + (d.responseBodyTruncated ? '（已截断）' : ''), pretty(d.responseBody)) });
}
function errorRow(e) {
  return `<tr><td class="num">${esc(fmtTime(e.ts))}</td><td>${keyCell(e)}</td><td>${esc(e.accountName)}</td><td>${esc(e.model)}</td>
    <td>${statusBadge(e.status)}${e.upstreamStatus && e.upstreamStatus !== e.status ? `<div class="acc-sub">上游 ${esc(e.upstreamStatus)}</div>` : ''}</td>
    <td>${esc(e.category)}<div class="acc-sub">${esc(e.terminal)}</div></td>
    <td>${e.ruleId ? `${esc(e.ruleId)}<div class="acc-sub">${esc(e.ruleAction)}${e.switched ? ' · 换号' : ''}</div>` : `<span class="muted">${esc(e.matchedBy)}</span>`}${e.retryRuleId ? `<div class="acc-sub">停止重试：${esc(e.retryRuleId)}</div>` : ''}</td>
    <td class="small">${esc(String(e.reason).slice(0, 160))}</td></tr>`;
}
async function loadLogs(kind, reset) {
  if (!accountsCache.length) { try { accountsCache = (await api('GET', withPool('/api/accounts'))).accounts; } catch { /* ignore */ } }
  const p = LOG_PREFIX[kind];
  if (kind !== 'details') accountOptions($(`#${p}-account`));
  const st = logState[kind];
  if (reset) { st.cursor = ''; st.items = []; }
  const q = new URLSearchParams({ limit: '50', provider: pool });
  if (st.cursor) q.set('cursor', st.cursor);
  const add = (k, v) => { if (v) q.set(k, v); };
  add('accountId', $(`#${p}-account`).value);
  add('status', $(`#${p}-status`).value.trim());
  if (kind === 'requests') { add('result', $('#rq-result').value); add('protocol', $('#rq-protocol').value);
    const af = $('#rq-affinity').value;
    if (af.startsWith('c:')) add('affinityConfidence', af.slice(2)); else add('affinityKeyType', af);
    add('model', $('#rq-model').value.trim()); } else if (kind === 'details') add('requestId', $('#dt-rid').value.trim()); else add('ruleId', $('#er-rule').value.trim());
  const d = await api('GET', `/api/logs/${kind}?${q}`);
  st.items = st.items.concat(d.items);
  st.cursor = d.nextCursor || '';
  $(`#${p}-table tbody`).innerHTML = st.items.map({ requests: requestRow, errors: errorRow, details: detailRow }[kind]).join('') || `<tr><td colspan="9" class="empty">暂无记录</td></tr>`;
  $(`#${p}-more`).classList.toggle('hidden', !st.cursor);
}
for (const [kind, p] of Object.entries(LOG_PREFIX)) {
  $(`#btn-${p}-search`).addEventListener('click', () => loadLogs(kind, true).catch((e) => toast(e.message)));
  $(`#${p}-more`).addEventListener('click', () => loadLogs(kind, false).catch((e) => toast(e.message)));
  $(`#btn-${p}-clear`).addEventListener('click', async () => {
    if (await modal({ title: '清空日志', body: `<p>确定清空全部${{ requests: '请求', errors: '错误', details: '详细' }[kind]}日志？</p>`, buttons: [{ label: '取消', value: 'c' }, { label: '清空', value: 'x', danger: true }] }) !== 'x') return;
    try { await api('DELETE', `/api/logs/${kind}`); loadLogs(kind, true); } catch (e) { toast(e.message); }
  });
  $(`#${p}-table tbody`).addEventListener('click', (ev) => {
    const tr = ev.target.closest('tr');
    if (!tr || !tr.parentElement) return;
    const idx = Array.from(tr.parentElement.children).indexOf(tr);
    const item = logState[kind].items[idx];
    if (item && kind === 'details') showDetail(item);
    else if (item) showJSON(kind === 'requests' ? '请求 ' + item.requestId : '错误 ' + item.requestId + ' #' + item.attemptIndex, item);
  });
}

boot();

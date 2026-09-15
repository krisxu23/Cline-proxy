package app

// adminHTML 分段 4/4: 全部脚本(JS): 加载/渲染/操作逻辑。
// 由 admin_html.go 拆分而来(P2-21): 单文件近 2900 行的原始字符串难以评审,
// 按面板边界切成多段常量, 拼接结果与拆分前逐字节一致; 原始字符串内
// 仍然禁止出现反引号(会终止字符串)。
const adminHTMLPart4 = `<script>
const API = '/admin/api';

// ========== 主题切换 ==========
function getTheme() {
  return localStorage.getItem('theme') || 'dark';
}
function applyTheme(t) {
  if (t === 'dark') {
    document.documentElement.setAttribute('data-theme', 'dark');
    const ic = document.getElementById('themeIcon');
    if (ic) ic.textContent = '🌙';
  } else {
    document.documentElement.setAttribute('data-theme', 'light');
    const ic = document.getElementById('themeIcon');
    if (ic) ic.textContent = '☀️';
  }
}
function toggleTheme() {
  const cur = getTheme();
  const next = cur === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', next);
  applyTheme(next);
}
applyTheme(getTheme());

const _ = id => document.getElementById(id);
// 与后端 providerIDRe 同义(^[a-z][a-z0-9_-]*$): 保存前先在 JS 侧拦一道,
// 避免非法 id 经 escJs 拼进 onclick 属性的字符串字面量里。
const providerIDRe = /^[a-z][a-z0-9_-]*$/;
const esc = s => { const d=document.createElement('div'); d.textContent=s||''; return d.innerHTML; };
// 属性值转义: 用于 value="..." / data-x="..." / class="..." 这类双引号属性。
const escAttr = s => esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
// 用于 onclick="fn('...')" 这类「JS 字符串嵌在 HTML 属性里」的场景。
// 顺序很关键: 先做 HTML 实体化(注意 & 必须最先), 再做 JS 反斜杠与引号转义。
const escJs = s => String(s == null ? '' : s)
  .replace(/&/g, '&amp;')
  .replace(/"/g, '&quot;')
  .replace(/</g, '&lt;')
  .replace(/>/g, '&gt;')
  .replace(/\\/g, '\\\\')
  .replace(/'/g, "\\'");
const fmtNum = n => (n || 0).toLocaleString('zh-CN');
const fmtTokens = n => {
  n = n || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(2).replace(/\.?0+$/, '') + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'K';
  return String(n);
};
if (window.location.host) _('footerApiAddr').textContent = 'http://' + window.location.host;

function toast(msg, t, duration) {
  const el = _('toast');
  el.textContent = msg;
  el.style.whiteSpace = 'pre-line';
  el.className = 'toast ' + (t || 'info') + ' show';
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.classList.remove('show'), duration || 3500);
}

// ========== 导航 ==========
// 统一委托给 switchTab: 此前这里内联了一份面板切换逻辑且漏了 router 分支,
// 导致点「自动路由」只切了面板、loadRouter 永远不执行, 页面一直停在"加载中"。
document.querySelectorAll('.nav-item').forEach(el => {
  el.addEventListener('click', () => {
    if (el.classList.contains('active')) return;
    switchTab(el.dataset.tab);
  });
});

function switchTab(name) {
  document.querySelectorAll('.nav-item').forEach(e => {
    e.classList.toggle('active', e.dataset.tab === name);
  });
  document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
  _('tab-' + name).style.display = 'block';
  if (name === 'dashboard') { loadStats(); loadOcStats(); loadConfig(); loadOcConfig(); loadHealth(); }
  if (name === 'accounts') { loadAccounts(); loadConfig(); }
  if (name === 'models') { loadModelIndex(); }
  if (name === 'router') { loadRouter(); }
  if (name === 'settings') { loadKeys(); loadConfig(); loadOcConfig(); }
  if (name === 'logs') loadLogs();
}

// 导入子标签
document.querySelectorAll('#importTabs .tab').forEach(el => {
  el.addEventListener('click', () => {
    document.querySelectorAll('#importTabs .tab').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('#import-oauth,#import-token,#import-batch').forEach(e => e.classList.remove('active'));
    _('import-' + el.dataset.tab).classList.add('active');
  });
});

// ========== API 请求 ==========
async function api(method, path, body, timeoutMs) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs || 30000);
  const opts = { method, headers: {}, signal: ctl.signal };
  if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  try {
    const res = await fetch(API + path, opts);
    if (!res.ok) {
      throw new Error(res.status === 502 || res.status === 503 || res.status === 504
        ? 'HTTP ' + res.status + '：后端忙或已退出'
        : 'HTTP ' + res.status);
    }
    const data = await res.json();
    if (!data.success) throw new Error(data.error || '请求失败');
    return data;
  } catch (e) {
    if (e.name === 'AbortError') throw new Error('请求超时：后端正忙（目录刷新/节点探测占用出口），稍后重试');
    if (/Failed to fetch|NetworkError|load failed/i.test(e.message || '')) {
      throw new Error('无法连接后端：网关进程可能已退出，检查 data/cline-proxy.log');
    }
    throw e;
  } finally {
    clearTimeout(timer);
  }
}

// fail 统一的加载失败占位: 显示真实原因并给一个重试按钮,
// 而不是只留一句"加载失败"让人无从下手。
function fail(e, retryExpr) {
  const msg = (e && e.message) ? e.message : '加载失败';
  return '<div class="empty" style="padding:14px;line-height:1.9">⚠️ ' + esc(msg)
    + (retryExpr ? '<br><button class="btn btn-sm" style="margin-top:var(--sp-2)" onclick="' + escAttr(retryExpr) + '">重试</button>' : '')
    + '</div>';
}

// ========== 仪表盘 ==========
async function loadStats() {
  try {
    const d = await api('GET', '/stats');
    const s = d.data;
    _('statTotal').textContent = s.total;
    _('statActive').textContent = s.active;
    _('statCooldown').textContent = s.cooldown;
    _('statExpired').textContent = s.expired;
  } catch (e) { console.warn('仪表盘统计加载失败(计数保持上一值):', e && e.message); }
}

// ========== 账号管理 ==========
async function loadAccounts() {
  try {
    const d = await api('GET', '/accounts');
    const list = d.data.accounts;
    const tbody = _('accountTableBody');
    if (!list || list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty">暂无账号，可在下方「导入账号」区域添加</td></tr>';
      return;
    }
    const sn = { active: '活跃', cooldown: '冷却', expired: '已过期' };
    tbody.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('zh-CN') : '-';
      const cr = a.createdAt ? new Date(a.createdAt).toLocaleString('zh-CN') : '-';
      // 冷却标签：展示预计恢复时间
      let statusExtra = '';
      if (a.status === 'cooldown') {
        const until = a.cooldownUntil ? new Date(a.cooldownUntil).toLocaleString('zh-CN') : '';
        statusExtra = until ? '<div style="font-size:var(--fs-xs);color:var(--text3);margin-top:2px">预计 ' + esc(until) + ' 恢复</div>' : '';
      }
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td><span class="status ' + escAttr(a.status) + '"><span class="status-dot ' + escAttr(a.status) + '"></span>' + esc(sn[a.status] || a.status) + '</span>' + statusExtra + '</td>' +
          '<td title="今日 ' + fmtNum(a.tokensToday) + ' / 累计 ' + fmtNum(a.tokensTotal) + ' tokens（上游返回 usage 时精确，否则为估算值）">' + fmtTokens(a.tokensToday) + ' / ' + fmtTokens(a.tokensTotal) + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + lu + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          '<button class="btn btn-sm" onclick="testAccount(\'' + escJs(a.accountId) + '\', this)" title="测试账号是否可用（成功会清除冷却/过期状态）">⚡</button> ' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + escJs(a.accountId) + '\', this)" title="检测限流并解除：探测上游，若仍限流则保持冷却并提示恢复时间">↻</button> ' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + escJs(a.accountId) + '\')" title="删除">✕</button>' +
        '</td></tr>';
    }).join('');
  } catch (e) { toast('加载账号失败: ' + e.message, 'error'); }
}

async function testAccount(id, btn) {
  const original = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>测试中'; }
  try {
    const d = await api('POST', '/accounts/test', { accountId: id });
    const r = d.data || {};
    const statusMap = { active: '可用', cooldown: '冷却', expired: '已失效', error: '错误' };
    const label = statusMap[r.status] || r.status;
    const prevMap = { active: '活跃', cooldown: '冷却', expired: '已过期', '': '' };
    let msg = '账号 ' + esc(r.email || '') + ' — ' + label;
    if (r.prevStatus && r.prevStatus !== r.status) msg += '（原状态: ' + (prevMap[r.prevStatus] || r.prevStatus) + '）';
    if (r.cooldownUntil) msg += '\n预计恢复: ' + esc(r.cooldownUntil);
    if (r.remaining) msg += '（剩余 ' + esc(r.remaining) + '）';
    if (r.reason) msg += '\n原因: ' + esc(r.reason);
    if (r.httpStatus) msg += '\nHTTP: ' + r.httpStatus;
    const type = r.status === 'active' ? 'success' : (r.status === 'cooldown' ? 'warning' : 'error');
    toast(msg, type, 6000);
    loadAccounts(); loadStats();
  } catch (e) {
    toast('测试失败: ' + e.message, 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = original; }
  }
}

async function deleteAccount(id) {
  if (!confirm('确定删除此账号？')) return;
  try {
    await api('POST', '/accounts/delete', { accountId: id });
    toast('账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function resetAccount(id, btn) {
  const original = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>检测中'; }
  try {
    const d = await api('POST', '/accounts/reset', { accountId: id });
    const r = d.data || {};
    const type = d.success ? 'success' : (r.status === 'cooldown' ? 'warning' : 'error');
    let msg = d.message || '检测完成';
    if (r.remaining && r.status !== 'active') msg += '（剩余 ' + esc(r.remaining) + '）';
    toast(msg, type, 6000);
    loadAccounts(); loadStats();
  } catch (e) {
    toast('检测失败: ' + e.message, 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = original; }
  }
}

async function deleteAllAccounts() {
  if (!confirm('⚠️ 确定删除所有账号？不可撤销！')) return;
  try {
    await api('POST', '/accounts/delete-all', {});
    toast('全部账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function refreshAllTokens() {
  try {
    await api('POST', '/accounts/refresh-all', {});
    toast('全部 Token 已刷新', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

// ========== OAuth 登录 ==========
async function startOAuth() {
  const btn = _('oauthBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="loading"></span> 启动中...';
  _('oauthProgress').style.display = 'block';
  _('oauthResult').style.display = 'none';
  _('oauthStatus').textContent = '正在连接 WorkOS...';
  try {
    const d = await api('POST', '/oauth/start');
    const s = d.data;
    _('oauthStatus').textContent = '请在浏览器中打开链接并输入代码';
    const u = _('oauthUrl');
    u.textContent = s.verificationUri;
    u.href = s.verificationUri;
    _('oauthUserCode').textContent = s.userCode;
    const poll = setInterval(async () => {
      try {
        const r = await api('GET', '/oauth/status?sessionId=' + s.sessionId);
        if (r.data.done) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = '🚀 开始 OAuth 登录';
          if (r.data.success) {
            _('oauthProgress').style.display = 'none';
            _('oauthResult').innerHTML = '<div style="color:var(--accent2);font-weight:600;font-size:var(--fs-md)">✓ 账号添加成功: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('账号添加成功！', 'success');
          } else {
            _('oauthStatus').textContent = '失败: ' + (r.data.error || '未知错误');
            toast('OAuth 失败', 'error');
          }
        }
      } catch(e) { /* 轮询内的瞬时错误可忽略: 2s 后下一轮自动重试, 不弹 toast 以免刷屏 */ }
    }, 2000);
  } catch (e) {
    btn.disabled = false;
    btn.innerHTML = '🚀 开始 OAuth 登录';
    _('oauthStatus').textContent = '错误: ' + e.message;
    toast('OAuth 失败: ' + e.message, 'error');
  }
}

// ========== Token 导入 ==========
async function addByToken() {
  const token = _('tokenInput').value.trim();
  if (!token) { toast('请输入 refreshToken', 'error'); return; }
  const email = _('tokenEmail').value.trim();
  try {
    const d = await api('POST', '/accounts/add', { refreshToken: token, email: email || undefined });
    toast('账号添加成功: ' + (d.data.email || ''), 'success');
    _('tokenInput').value = '';
    _('tokenEmail').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('添加失败: ' + e.message, 'error'); }
}

// ========== 批量导入 ==========
async function batchImport() {
  const raw = _('batchInput').value.trim();
  if (!raw) { toast('请输入账号数据', 'error'); return; }
  let tokens;
  try { tokens = JSON.parse(raw); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = raw.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入完成', 'success');
    _('batchInput').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

async function handleFileImport(event) {
  const file = event.target.files[0];
  if (!file) return;
  const text = await file.text();
  let tokens;
  try { tokens = JSON.parse(text); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = text.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入了 ' + tokens.length + ' 个账号', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
  event.target.value = '';
}

// ========== API 密钥管理 ==========
async function loadKeys() {
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys;
    const el = _('keysList');
    if (!keys || keys.length === 0) {
      el.innerHTML = '<div class="empty-state"><span class="icon">🔑</span>暂无 API 密钥</div>';
      return;
    }
    el.innerHTML = keys.map(k =>
      '<div class="flex" style="margin-bottom:var(--sp-2)">' +
        '<span class="key-display" style="flex:1" onclick="copyText(\'' + escJs(k) + '\')" title="点击复制">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + escJs(k) + '\')">✕</button>' +
      '</div>'
    ).join('');
  } catch (e) { _('keysList').innerHTML = fail(e, 'loadKeys()'); }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:rgba(52,211,153,.08);border:1px solid rgba(52,211,153,.4);border-radius:var(--radius-sm);padding:var(--sp-3)">' +
        '<div style="color:var(--accent2);font-weight:600;margin-bottom:var(--sp-2)">✓ 新密钥已生成（点击复制）</div>' +
        '<div class="key-display" onclick="copyText(\'' + escJs(key) + '\')">' + esc(key) + '</div>' +
      '</div>';
    loadKeys();
    toast('密钥已生成', 'success');
    setTimeout(() => _('keyGenResult').innerHTML = '', 8000);
  } catch (e) { toast('生成失败: ' + e.message, 'error'); }
}

async function deleteKey(key) {
  if (!confirm('确定删除此密钥？')) return;
  try {
    await api('POST', '/keys/delete', { key });
    toast('密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function deleteAllKeys() {
  if (!confirm('确定删除所有 API 密钥？')) return;
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys || [];
    for (const k of keys) await api('POST', '/keys/delete', { key: k });
    toast('全部密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

// ========== 请求日志 ==========
const ROUTE_LABEL = { zen: 'opencode', cline: 'cline 池', admin: '管理', meta: '元信息', other: '其他' };
const STATUS_CLASS = s => s >= 500 ? 'color:var(--danger)' : (s >= 400 ? 'color:var(--amber)' : 'color:var(--accent2)');

let logPage = 1;
let logLastTotal = 0;
let logLastPageSize = 50;
let logLastList = [];

function logPagePrev() { if (logPage > 1) { logPage--; loadLogs(); } }
function logPageNext() {
  if (logPage * logLastPageSize < logLastTotal) { logPage++; loadLogs(); }
}

async function loadLogs() {
  const tbody = _('logsTableBody');
  try {
    const qs = new URLSearchParams({
      page: String(logPage), pageSize: String(logLastPageSize),
    });
    if (_('logQ').value.trim()) qs.set('q', _('logQ').value.trim());
    if (_('logModel').value.trim()) qs.set('model', _('logModel').value.trim());
    if (_('logUpstream').value) qs.set('upstream', _('logUpstream').value);
    if (_('logStatus').value) qs.set('status', _('logStatus').value);
    const d = await api('GET', '/logs?' + qs.toString());
    const logs = d.data.logs || [];
    logLastTotal = d.data.total || 0;
    logLastPageSize = d.data.pageSize || 50;
    logLastList = logs;
    const from = logLastTotal === 0 ? 0 : (logPage - 1) * logLastPageSize + 1;
    const to = Math.min(logPage * logLastPageSize, logLastTotal);
    _('logsPaging').textContent = '共 ' + logLastTotal + ' 条 · 显示 ' + from + '-' + to;
    if (!logs.length) { tbody.innerHTML = '<tr><td colspan="10" class="empty">没有匹配的请求记录</td></tr>'; return; }
    const html = logs.map((l, i) => {
      const t = l.time ? new Date(l.time).toLocaleString('zh-CN') : '-';
      const route = (ROUTE_LABEL[l.route] || l.route || '-');
      const upstream = l.upstream || route;
      const exit = l.exit ? ' · ' + l.exit : '';
      const st = l.status || 0;
      const modelPair = (l.model || '-') + (l.resolvedModel && l.resolvedModel !== l.model ? ' → ' + l.resolvedModel : '');
      const toks = (l.promptTokens || l.completionTokens)
        ? (l.promptTokens || 0) + '/' + (l.completionTokens || 0) + (l.reasoningTokens ? '+' + l.reasoningTokens + 'r' : '') + (l.cacheTokens ? '+' + l.cacheTokens + 'c' : '')
        : (l.usageReported ? '0/0' : '-');
      const dur = (l.durationMs != null ? l.durationMs + 'ms' : '-') + (l.ttftMs ? ' / ' + l.ttftMs + 'ms' : '');
      const note = l.note || '';
      return '<tr style="cursor:pointer" onclick="showLogDetail(' + i + ')" title="点击查看详情">' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + t + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + esc(l.client || '-') + '</td>' +
        '<td>' + esc(l.method || '-') + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs);max-width:140px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(l.path || '-') + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs);max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(modelPair) + '">' + esc(modelPair) + '</td>' +
        '<td><span class="model-tag">' + esc(upstream + exit) + '</span></td>' +
        '<td style="font-weight:600;color:' + STATUS_CLASS(st) + '">' + st + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + dur + '</td>' +
        '<td class="mono" style="font-size:var(--fs-xs)">' + toks + '</td>' +
        '<td style="font-size:var(--fs-xs);max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:' + (l.status >= 400 ? 'var(--amber)' : 'var(--text3)') + '" title="' + esc(note) + '">' + esc(note || '-') + '</td>' +
      '</tr>';
    }).join('');
    tbody.innerHTML = html;
  } catch (e) { tbody.innerHTML = '<tr><td colspan="10">' + fail(e, 'loadLogs()') + '</td></tr>'; }
}

// showLogDetail 日志详情抽屉: 完整记录字段 + 路由决策轨迹(为什么选了这站/跳过其它)。
async function showLogDetail(i) {
  const l = logLastList[i];
  if (!l) return;
  const panel = _('logDetailPanel');
  const row = (k, v) => '<div style="display:flex;gap:10px;padding:3px 0;border-bottom:1px solid rgba(148,163,184,.08)">' +
    '<span style="flex:none;width:120px;color:var(--text3);font-size:var(--fs-xs)">' + k + '</span>' +
    '<span class="mono" style="flex:1;font-size:var(--fs-xs);word-break:break-all">' + esc(v == null || v === '' ? '-' : String(v)) + '</span></div>';
  let html = '<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:var(--sp-3)">' +
    '<h3 style="margin:0">请求详情</h3>' +
    '<button class="btn btn-sm" onclick="closeLogDetail()">关闭 ✕</button></div>';
  html += '<div style="margin-bottom:var(--sp-3)">' +
    row('请求 id', l.id) +
    row('时间', l.time ? new Date(l.time).toLocaleString('zh-CN') : '-') +
    row('客户端', l.client) + row('方法/路径', (l.method || '-') + ' ' + (l.path || '-')) +
    row('请求模型', l.model) + row('实际模型', l.resolvedModel) +
    row('上游', l.upstream) + row('路由/出口', (l.route || '-') + ' · ' + (l.exit || '-')) +
    row('协议/流式', (l.protocol || '-') + (l.stream ? ' · 流式' : '')) +
    row('状态', l.status) +
    row('总耗时/TTFT', (l.durationMs || 0) + 'ms / ' + (l.ttftMs || 0) + 'ms') +
    row('尝试/跳过', (l.attempts || 0) + ' 次尝试, 跳过 ' + ((l.skipped && l.skipped.length) || 0) + ' 站') +
    row('tokens', '入 ' + (l.promptTokens || 0) + ' · 出 ' + (l.completionTokens || 0) +
      ' · 推理 ' + (l.reasoningTokens || 0) + ' · 缓存 ' + (l.cacheTokens || 0) +
      (l.usageReported ? '' : ' (上游未上报)')) +
    row('错误类别', l.errClass) + row('错误消息', l.errMsg) +
    row('摘要', l.note) + '</div>';
  if (l.skipped && l.skipped.length) {
    html += '<div style="margin-bottom:var(--sp-3)"><div style="color:var(--text3);font-size:var(--fs-xs);margin-bottom:4px">被跳过的候选</div>' +
      l.skipped.map(s => '<div class="model-tag" style="display:block;margin-bottom:4px;font-size:var(--fs-xs)">' + esc(s) + '</div>').join('') + '</div>';
  }
  html += '<div id="logTraceBox" style="color:var(--text3);font-size:var(--fs-xs)">决策轨迹加载中…</div>';
  panel.innerHTML = html;
  panel.style.display = 'block';
  // 拉路由决策轨迹(30 分钟内的请求才有)
  if (l.id) {
    try {
      const td = await api('GET', '/logs/trace?id=' + encodeURIComponent(l.id));
      const tr = td.data.trace;
      if (tr && tr.candidates && tr.candidates.length) {
        const mark = c => c.decision === 'tried'
          ? (c.status >= 400 ? '🔴' : '🟢')
          : '<span style="color:var(--text3)">⏭</span>';
        html = '<div style="color:var(--text3);font-size:var(--fs-xs);margin-bottom:4px">路由决策轨迹' +
          (tr.winner ? ' · 命中 ' + esc(tr.winner) : '') + '</div>' +
          tr.candidates.map(c =>
            '<div style="padding:3px 0;border-bottom:1px solid rgba(148,163,184,.08)">' +
            mark(c) + ' <span class="mono">' + esc(c.candidate) + '</span>' +
            (c.reason ? ' <span style="color:var(--text3)">' + esc(c.reason) + '</span>' : '') +
            (c.status ? ' <span class="mono" style="color:var(--text3)">HTTP ' + c.status + '</span>' : '') +
            '</div>').join('');
      } else {
        html = '<div style="color:var(--text3)">该请求没有候选链轨迹(可能直连单一上游)</div>';
      }
    } catch (e) {
      html = '<div style="color:var(--text3)">决策轨迹不可用: ' + esc(String(e && e.message || e)) + '</div>';
    }
    const box = _('logTraceBox');
    if (box) box.innerHTML = html;
  }
}

function closeLogDetail() { _('logDetailPanel').style.display = 'none'; }

// ========== 网关健康 (P2-20) ==========

const HEALTH_STATE_LABEL = { ok: '🟢 健康', warn: '🟡 需要注意', critical: '🔴 异常' };

async function loadHealth() {
  try {
    const d = await api('GET', '/health');
    const h = d.data || {};
    _('healthState').textContent = HEALTH_STATE_LABEL[h.state] || h.state;
    _('healthState').style.color = h.state === 'ok' ? 'var(--accent2)' : (h.state === 'warn' ? 'var(--amber)' : 'var(--danger)');
    _('healthScore').textContent = '评分 ' + (h.score != null ? h.score : '-') + ' / 100';
    const sig = h.signals || {};
    const chip = (label, okFlag, extra) =>
      '<span class="model-tag" style="color:' + (okFlag ? 'var(--accent2)' : 'var(--amber)') + '">' +
      (okFlag ? '✔' : '⚠') + ' ' + esc(label) + (extra ? ' · ' + esc(extra) : '') + '</span>';
    _('healthSignals').innerHTML =
      chip('zen', sig.zenReady) +
      chip('cline 池', sig.clinePoolReady) +
      chip('clinepass', sig.clinePassReady) +
      chip('冷却', true, sig.cooling + ' 个') +
      chip('探针中', true, sig.probing + ' 个') +
      chip('永久剔除', true, sig.permanentRemoved + ' 个') +
      chip('决策轨迹', true, sig.decisionTraces + ' 条');
    const issues = h.issues || [];
    if (!issues.length) { _('healthIssues').innerHTML = '<div class="hint">没有发现问题。</div>'; return; }
    const sevColor = i => i === 'critical' ? 'var(--danger)' : (i === 'warn' ? 'var(--amber)' : 'var(--text3)');
    _('healthIssues').innerHTML = issues.map(i =>
      '<div style="padding:6px 0;border-bottom:1px solid rgba(148,163,184,.08)">' +
      '<span style="color:' + sevColor(i.severity) + ';font-weight:600">[' + esc(i.severity) + ']</span> ' +
      '<b>' + esc(i.component) + '</b> — ' + esc(i.problem) +
      '<div style="font-size:var(--fs-xs);color:var(--text3);margin-top:2px">建议: ' + esc(i.recommendation) +
      ' · 证据: <span class="mono">' + esc(i.evidence) + '</span></div></div>').join('');
  } catch (e) {
    _('healthState').textContent = '加载失败';
    _('healthIssues').innerHTML = '<div class="hint">' + fail(e, 'loadHealth()') + '</div>';
  }
}

async function previewRoute(name) {
  const box = _('previewResult');
  const model = name || _('previewModel').value.trim();
  if (!model) { box.innerHTML = '<span style="color:var(--amber)">请先填模型名</span>'; return; }
  try {
    const d = await api('GET', '/router/preview?model=' + encodeURIComponent(model));
    const r = d.data || {};
    if (!r.matched) {
      box.innerHTML = '<div style="color:var(--amber)">无法解析模型名 ' + esc(model) + (r.error ? ': ' + esc(r.error) : '') + '</div>';
      return;
    }
    const mark = h => {
      if (h.decision === 'skipped') return '<span style="color:var(--text3)">⏭ 跳过</span>';
      if (h.decision === 'first_tried') return '<span style="color:var(--accent2)">🥇 首选</span>';
      return '<span style="color:var(--text3)">↩️ 兜底</span>';
    };
    box.innerHTML = '<div style="margin-bottom:4px">按顺序尝试 ' + (r.hops || []).length + ' 站' +
      (r.winner ? ' · 首选 <span class="mono">' + esc(r.winner) + '</span>' : '') + '</div>' +
      (r.hops || []).map(h =>
        '<div style="padding:3px 0;border-bottom:1px solid rgba(148,163,184,.08)">' +
        mark(h) + ' <span class="mono">' + esc(h.upstream + ':' + h.model) + '</span>' +
        (h.reason ? ' <span style="color:var(--amber)">' + esc(h.reason) + '</span>' : '') +
        (h.context ? ' <span style="color:var(--text3)">ctx ' + h.context + '</span>' : '') +
        '</div>').join('');
  } catch (e) { box.innerHTML = '<span style="color:var(--danger)">' + esc(String(e && e.message || e)) + '</span>'; }
}

// ========== 导出账号 ==========
async function exportAccounts() {
  try {
    // 走后端统一的 apiResponse 信封(见 handleAccountsExport), 这样能复用 api() 的
    // 超时/中断处理与"人话"错误文案, 不再自己裸写 fetch。
    const r = await api('GET', '/accounts/export');
    const items = r.data || [];
    const blob = new Blob([JSON.stringify(items, null, 2)], { type: 'application/json;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = 'cline-accounts-export.json';
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast('账号已导出（JSON，' + items.length + ' 条）', 'success');
  } catch (e) { toast('导出失败: ' + e.message, 'error'); }
}

// ========== 配置管理 ==========
// savePoolStrategy Cline 账号池的轮询策略（原「设置 → 代理配置」，现在账号管理页）。
async function savePoolStrategy() {
  const strategy = _('acctStrategy').value;
  try {
    await api('POST', '/config/update', { strategy });
    toast('账号轮询策略已更新为: ' + strategy, 'success');
  } catch (e) { toast('更新失败: ' + e.message, 'error'); }
}

// saveHeaderAuto 切换请求头的版本对齐方式。
async function saveHeaderAuto() {
  const auto = _('hdrAutoMode').value === 'true';
  try {
    await api('POST', '/config/update', { headersAuto: auto });
    toast(auto ? '已开启自动对齐（每 12 小时，并在启动时对齐一次）' : '已切换为手动维护请求头', 'success');
    if (auto) await syncHeadersNow(true);
    else loadConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// syncHeadersNow 立即向官方发行渠道对齐一次版本类请求头。
async function syncHeadersNow(quiet) {
  if (!quiet) toast('正在查询官方版本…', 'info');
  try {
    const d = await api('POST', '/config/headers/sync', {});
    const r = d.data || {};
    if (!quiet) toast('已对齐 ' + (r.version || '官方版本') + (r.source ? '（来源 ' + r.source + '）' : ''), 'success', 7000);
    loadConfig();
    return r;
  } catch (e) {
    if (!quiet) toast('对齐失败: ' + e.message, 'error', 8000);
    loadConfig();
    return null;
  }
}

function addHeaderRow() {
  const tbody = _('headersTableBody');
  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:var(--fs-xs);font-family:var(--font-mono)"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:var(--fs-xs);font-family:var(--font-mono)"></td>' +
    '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>';
  tbody.appendChild(tr);
}

async function saveHeaders() {
  const tbody = _('headersTableBody');
  const rows = tbody.querySelectorAll('tr');
  const headers = {};
  let hasEmpty = false;
  rows.forEach(tr => {
    const keyInput = tr.querySelector('.header-key');
    const valInput = tr.querySelector('.header-val');
    if (keyInput && valInput) {
      const k = keyInput.value.trim();
      const v = valInput.value.trim();
      if (k) { headers[k] = v; }
      else if (v) { hasEmpty = true; }
    }
  });
  if (hasEmpty) { toast('存在有值无键的行，已忽略', 'info'); }
  try {
    const d = await api('POST', '/config/update', { headers });
    toast('请求头已保存', 'success');
    _('headerSaveResult').innerHTML =
      '<div style="color:var(--accent2);font-size:var(--fs-xs)">✓ 已保存 ' + Object.keys(d.data.headers).length + ' 个请求头</div>';
    setTimeout(() => _('headerSaveResult').innerHTML = '', 5000);
    loadConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

const MODEL_STYLE = {
  active:  { label: '可用', css: 'color:var(--accent2);border:1px solid rgba(52,211,153,.5);background:rgba(52,211,153,.08)' },
  empty:   { label: '响应为空', css: 'color:var(--amber);border:1px solid rgba(245,158,11,.5);background:rgba(245,158,11,.08)' },
  pass:    { label: '需订阅', css: 'color:var(--amber);border:1px solid rgba(245,158,11,.5);background:rgba(245,158,11,.08)' },
  removed: { label: '已下架', css: 'color:var(--text3);border:1px solid var(--border)' },
  error:   { label: '异常', css: 'color:var(--danger);border:1px solid rgba(248,113,113,.5);background:rgba(248,113,113,.08)' },
  unknown: { label: '未探测', css: 'color:var(--text3);border:1px dashed var(--border-strong)' }
};
const COST_LABEL = { free: '免费', pass: '订阅', quota: '消耗额度' };

function copyText(t) {
  const done = () => toast('已复制: ' + t, 'success');
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(t).then(done).catch(() => { fallbackCopy(t); done(); });
  } else { fallbackCopy(t); done(); }
}
function fallbackCopy(t) {
  const i = document.createElement('textarea');
  i.value = t; document.body.appendChild(i); i.select();
  document.execCommand('copy'); i.remove();
}

// cline 模型清单由 GET /providers 的 cline 卡片渲染(带状态/费用/同步时间),
// 这里只负责触发官方推荐清单的同步。
async function refreshModels() {
  toast('正在同步 Cline 官方推荐清单…', 'info');
  try {
    await api('POST', '/models/refresh');
    setTimeout(loadModelIndex, 3000);
    setTimeout(loadModelIndex, 12000);
  } catch (e) { toast('同步失败: ' + e.message, 'error'); }
}

async function loadModelOptions() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    const sel = _('settingDefModel');
    if (!sel) return;
    sel.innerHTML = models.map(m => {
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      return '<option value="' + escAttr(m.id) + '">' + esc(m.id) + ' (' + st.label + ')</option>';
    }).join('');
    const c = await api('GET', '/config');
    if (c.data.defaultModel) sel.value = c.data.defaultModel;
    if (!sel.value && models.length) sel.value = models[0].id;
  } catch (e) { console.warn('默认模型选项加载失败:', e && e.message); }
}

async function saveDefaultModel() {
  const v = _('settingDefModel').value;
  if (!v) { toast('请选择模型', 'error'); return; }
  try {
    const d = await api('POST', '/config/update', { defaultModel: v });
    toast('默认模型已保存: ' + d.data.defaultModel, 'success');
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 配置加载 ==========
async function loadConfig() {
  try {
    const d = await api('GET', '/config');
    const c = d.data;
    // 入口信息展示在仪表盘; 账号池信息展示在账号管理页
    if (_('dashApiBase')) _('dashApiBase').value = c.apiBase || ('http://' + (c.address || ''));
    if (_('dashListenAddr')) _('dashListenAddr').value = c.listenAddr || c.address || '';
    if (_('dashVersion')) _('dashVersion').value = c.version || '';
    if (_('acctPoolPath')) _('acctPoolPath').value = c.poolPath || '';
    if (_('acctStrategy') && c.strategy) _('acctStrategy').value = c.strategy;
    if (_('hdrAutoMode')) _('hdrAutoMode').value = String(!!c.headersAuto);
    renderHeaderSyncInfo(c.headersSync || {}, !!c.headersAuto);
    loadModelOptions();
    if (c.headers) {
      const tbody = _('headersTableBody');
      tbody.innerHTML = Object.entries(c.headers).map(([k, v]) =>
        '<tr>' +
          '<td><input type="text" class="header-key" value="' + escAttr(k) + '" style="font-size:var(--fs-xs);font-family:var(--font-mono);width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + escAttr(v) + '" style="font-size:var(--fs-xs);font-family:var(--font-mono);width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { console.warn('配置加载失败(表单可能为空):', e && e.message); }
}

// renderHeaderSyncInfo 展示最近一次自动对齐的结果。
function renderHeaderSyncInfo(sync, auto) {
  const el = _('hdrSyncInfo');
  if (!el) return;
  const parts = [];
  if (sync.version) parts.push(sync.version);
  if (sync.syncedAt) parts.push('对齐于 ' + new Date(sync.syncedAt).toLocaleString('zh-CN'));
  if (!parts.length) parts.push(auto ? '尚未对齐，启动后会自动执行一次' : '手动维护中');
  el.value = parts.join(' · ');
}

// ========== opencode 免费模型 ==========
async function loadOcConfig() {
  try {
    const d = await api('GET', '/opencode/config');
    const c = d.data;
    _('ocEnabled').value = String(c.enabled);
    _('ocKey').value = c.key || 'public';
    _('ocBaseURLs').value = (c.baseURLs && c.baseURLs.length ? c.baseURLs : (c.baseURL ? [c.baseURL] : [])).join('\n');
    _('ocProxies').value = (c.proxies || []).join('\n');
    ocSubsArr = (c.subs || []).slice();
    renderOcSubs();
    _('ocExitMode').value = c.exitMode === 'direct' ? 'direct' : 'proxy';
    _('ocSticky').value = c.stickySessions ? 'true' : 'false';
    _('ocNodeExclude').value = (c.nodeExcludeKeywords || []).join('\n');
    if (_('ocDnsMode')) _('ocDnsMode').value = c.dnsMode || 'doh-ali';
    if (_('ocDnsCustom')) _('ocDnsCustom').value = c.dnsCustom || '';
    if (_('ocRescue')) _('ocRescue').value = (c.rescueDirect === false) ? 'false' : 'true';
    _('ocSubRefresh').value = c.subsRefreshMins || 30;
    if (_('dashExitMode')) _('dashExitMode').value = (c.exitMode === 'direct') ? '直连（不走节点）' : '节点出口（按所选地区）';
    if (Array.isArray(c.enabledRegions)) ocRegions = c.enabledRegions.slice();
    ocRegionStats = c.regionSummary || [];
    renderExitRegions();
    _('ocStrategy').value = c.proxyStrategy || 'round_robin';
    _('ocMaxConc').value = c.maxConcurrency || 8;
    _('ocRetries').value = c.retries || 3;
    _('ocFailover').value = String(c.failover);
    _('ocFailoverCount').value = c.failoverCount || 3;
    _('ocFailoverMinutes').value = c.failoverMinutes || 5;
    _('ocCompactAuto').value = String(c.compaction ? c.compaction.auto : true);
    _('ocCompactBuffer').value = c.compaction ? c.compaction.buffer : 20000;
    _('ocKeepTokens').value = c.compaction ? c.compaction.keepTokens : 8000;
    _('ocSummaryModel').value = c.compaction ? (c.compaction.summaryModel || '') : '';
    _('ocMaxSummary').value = c.compaction ? c.compaction.maxSummary : 4096;
    const rt = c.runtime || {};
    _('ocFailoverInfo').innerHTML = rt.failoverActive
      ? '<span style="color:var(--danger)">🔴 故障转移中 (opencode 不可用, 请求走 cline 池)</span>'
      : '<span style="color:var(--accent2)">🟢 正常</span>';
    renderCooldowns(rt.proxyCooldowns || {}, c.exitMode === 'direct');
    const ss = rt.subsStatus || {};
    const sk = Object.keys(ss);
    ocSubsStatus = ss;
    renderOcSubs();
    _('ocSubsInfo').textContent = sk.length
      ? ''
      : '订阅尚未抓取';
  } catch (e) { console.warn('opencode 配置加载失败:', e && e.message); }
}

// renderCooldowns 节点列表下方的冷却框: 只列出真正处于冷却期的出口。
function renderCooldowns(cd, direct) {
  const el = _('ocCooldownBox');
  if (!el) return;
  const keys = Object.keys(cd);
  if (direct) {
    el.innerHTML = '<div style="padding:9px 12px;font-size:var(--fs-xs);color:var(--text3)">当前为直连模式，节点不参与出口，冷却不适用</div>';
    return;
  }
  if (!keys.length) {
    el.innerHTML = '<div style="padding:9px 12px;font-size:var(--fs-xs);color:var(--text3)">暂无冷却中的节点</div>';
    return;
  }
  el.innerHTML = keys.map(k =>
    '<div style="display:flex;align-items:center;gap:9px;padding:6px 12px;font-size:var(--fs-sm);border-bottom:1px solid rgba(148,163,184,.07)">' +
    '<span style="flex:none">🧊</span>' +
    '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(k) + '</span>' +
    '<span style="flex:none;font-size:var(--fs-xs);color:var(--text3)">冷却至 ' + esc(cd[k]) + '</span></div>'
  ).join('');
}

let ocSubsArr = [];
let ocSubsStatus = {};
// 订阅区整体折叠: 订阅多时平铺太长, 整块收起只留一行摘要; 单条链接仍是平铺样式
// (完整地址 + 抓取状态 + 删除), 不做逐条折叠 —— 展开后与以前的显示一致。
let ocSubsAreaOpen = false;

function toggleOcSubsArea() {
  ocSubsAreaOpen = !ocSubsAreaOpen;
  renderOcSubs();
}

function renderOcSubs() {
  const list = _('ocSubsList');
  const area = _('ocSubsArea');
  const btn = _('ocSubsToggle');
  const sum = _('ocSubsSummary');
  const n = ocSubsArr.length;
  if (btn) btn.textContent = ocSubsAreaOpen ? '收起' : '展开';
  if (sum) {
    // 收起时把"有没有抓取过"也带进摘要, 否则折叠状态会把未抓取这件事藏起来。
    const anyStatus = ocSubsArr.some(u => ocSubsStatus[u]);
    sum.textContent = n ? (n + ' 条订阅' + (anyStatus ? '' : ' · 未抓取')) : '暂无订阅';
  }
  if (area && area.style) area.style.display = ocSubsAreaOpen ? '' : 'none';
  if (!list) return;
  if (!ocSubsAreaOpen) {
    // 收起时不渲染长列表: 区域里只剩标签行的摘要, 页面高度与订阅条数无关。
    list.innerHTML = '';
    return;
  }
  if (!n) {
    list.innerHTML = '<div style="font-size:var(--fs-xs);color:var(--text3);padding:2px 0">暂无订阅, 在下方添加; 保存后自动抓取并按设定的刷新间隔更新, 支持 sing-box JSON / Clash YAML / base64 节点列表</div>';
    return;
  }
  list.innerHTML = ocSubsArr.map((u, i) => {
    const st = ocSubsStatus[u] || '';
    return '<div style="display:flex;align-items:center;gap:10px;padding:var(--sp-2) var(--sp-3);background:rgba(148,163,184,.06);border:1px solid var(--border);border-radius:var(--radius-sm)">' +
      '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:var(--fs-base)">' + esc(u) + '</span>' +
      (st ? '<span style="flex:none;font-size:var(--fs-xs);color:var(--text3);white-space:nowrap">' + esc(st) + '</span>' : '') +
      '<button type="button" class="btn" style="flex:none;padding:4px 10px;font-size:var(--fs-xs)" onclick="delOcSub(' + i + ')">删除</button></div>';
  }).join('');
}
function addOcSub() {
  const u = _('ocSubNew').value.trim();
  if (!/^https?:\/\//.test(u)) { toast('订阅需以 http(s):// 开头', 'error'); return; }
  if (ocSubsArr.includes(u)) { toast('订阅已存在', 'error'); return; }
  ocSubsArr.push(u);
  _('ocSubNew').value = '';
  renderOcSubs();
}
function delOcSub(i) {
  ocSubsArr.splice(i, 1);
  renderOcSubs();
}

// ============ 出口地区勾选 ============
//
// 设置页不再逐个罗列节点: 后端按实测出口国家把出口归到 7 个地区, 这里只渲染
// 地区级勾选。勾选后全网关(zen / cline 池 / 通用 Provider / 订阅抓取)的出站
// 只走所选地区的出口; 不勾选 = 不限制。
const exitRegionDefs = [
  { id: 'us', label: '美国' },
  { id: 'jp', label: '日本' },
  { id: 'tw', label: '台湾' },
  { id: 'hk', label: '香港' },
  { id: 'sg', label: '新加坡' },
  { id: 'eu', label: '欧洲' },
  { id: 'other', label: '其他地区' }
];
let ocRegions = [];      // 已勾选地区 ID
let ocRegionStats = [];  // 后端统计: [{id,label,total,ok}]
let ocRegionActive = false; // 后端检测进行中标记(仅用于提示文案)

function renderExitRegions() {
  const box = _('ocRegionBox');
  if (!box) return;
  const stats = {};
  ocRegionStats.forEach(s => { stats[s.id] = s; });
  const rows = exitRegionDefs.map(def => {
    const s = stats[def.id] || { total: 0, ok: 0 };
    const on = ocRegions.indexOf(def.id) >= 0;
    return '<label style="display:flex;align-items:center;gap:10px;padding:7px 12px;border-bottom:1px solid rgba(148,163,184,.07);cursor:pointer">' +
      '<input type="checkbox" style="flex:none" ' + (on ? 'checked' : '') +
        ' onchange="toggleExitRegion(\'' + def.id + '\', this.checked)" />' +
      '<span style="flex:1">' + def.label + '</span>' +
      '<span style="flex:none;font-size:var(--fs-xs);color:var(--text3)">可用 ' + (s.ok || 0) + ' / 共 ' + (s.total || 0) + '</span>' +
      '</label>';
  }).join('');
  const chosen = exitRegionDefs.filter(d => ocRegions.indexOf(d.id) >= 0).map(d => d.label);
  let chosenTotal = 0, chosenOk = 0;
  ocRegions.forEach(id => { const s = stats[id]; if (s) { chosenTotal += s.total || 0; chosenOk += s.ok || 0; } });
  const tail = chosen.length
    ? '<div style="padding:7px 12px;font-size:var(--fs-xs);display:flex;align-items:center;gap:8px">' +
        '<span style="flex:1;color:var(--accent2)">仅使用: ' + esc(chosen.join('、')) + '（' + chosenOk + ' 可用 / ' + chosenTotal + ' 个出口）</span>' +
        '<button type="button" class="btn" style="flex:none;padding:2px 9px;font-size:var(--fs-xs)" onclick="toggleExitRegion(\'\', false)">清除限制</button></div>'
    : '<div style="padding:7px 12px;font-size:var(--fs-xs);color:var(--text3)">未勾选 = 使用全部地区出口（不限制）</div>';
  // 勾了地区但一个出口都没有: 明确告警 —— 后端此时会临时回退全部出口以保证可用,
  // 不提示的话用户会以为限制生效了。
  const emptyWarn = (chosen.length && chosenTotal === 0)
    ? '<div style="padding:7px 12px 0;font-size:var(--fs-xs);color:var(--danger)">所选地区当前没有出口：请点「连通检测」获取各出口的国家（结果会记住，重启不丢），或先取消勾选</div>'
    : '';
  box.innerHTML = rows + tail + emptyWarn +
    '<div style="padding:0 12px 9px;font-size:var(--fs-xs);color:var(--text3);line-height:1.6">' +
    '地区按连通检测实测的出口国家归类；<b>未检测或无法判定国家的出口归入「其他地区」</b>，手填的代理同理。' +
    '勾选后请点下方「💾 保存出口配置」生效' + (ocChecking ? '；连通检测进行中，计数会自动刷新…' : '') +
    '</div>';
}

// toggleExitRegion 勾选/取消一个地区; id 为空表示"清除限制"。
function toggleExitRegion(id, checked) {
  if (!id) {
    ocRegions = [];
  } else if (checked) {
    if (ocRegions.indexOf(id) < 0) ocRegions.push(id);
  } else {
    ocRegions = ocRegions.filter(r => r !== id);
  }
  ocRegions = exitRegionDefs.filter(d => ocRegions.indexOf(d.id) >= 0).map(d => d.id);
  renderExitRegions();
}

// loadOcRegions 只刷新地区统计(不再拉取全部节点, 设置页因此更轻)
async function loadOcRegions() {
  try {
    const d = await api('GET', '/opencode/config');
    const c = d.data || {};
    if (Array.isArray(c.enabledRegions)) ocRegions = c.enabledRegions.slice();
    ocRegionStats = c.regionSummary || [];
    renderExitRegions();
  } catch (e) { console.warn('地区统计加载失败:', e && e.message); }
}

let ocChecking = false;
async function refreshOcNodes() {
  if (ocChecking) return;
  ocChecking = true;
  const btn = _('ocCheckBtn');
  const old = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '检测中…'; }
  try { await api('POST', '/opencode/nodes/check'); toast('连通检测已启动, 结果将在 1~2 分钟内陆续刷新', 'success'); }
  catch (e) { toast('连通检测启动失败: ' + e.message, 'error'); }
  renderExitRegions();
  await loadOcRegions();
  [15, 35, 60, 90].forEach(sec => setTimeout(loadOcRegions, sec * 1000));
  setTimeout(() => {
    ocChecking = false;
    if (btn) { btn.disabled = false; btn.textContent = old || '连通检测'; }
    loadOcRegions();
  }, 95 * 1000);
}

async function saveOcConfig() {
  const proxies = _('ocProxies').value.split('\n').map(s => s.trim()).filter(Boolean);
  const PROXY_RE = /^(https?|socks5h?):\/\/[^\s]+:\d+/;
  const NODE_RE = /^(vmess|vless|trojan|ss|hy2|hysteria2|tuic|hysteria|anytls|ssh|shadowtls|snell|sbox):\/\//;
  const bad = proxies.find(p => !(PROXY_RE.test(p) || NODE_RE.test(p)));
  if (bad) { toast('代理格式无效: ' + bad.slice(0, 60) + '（支持 http/socks5 代理或 vmess/vless/trojan/ss/hy2/tuic 等节点链接）', 'error'); return; }
  const refresh = parseInt(_('ocSubRefresh').value) || 30;
  if (refresh < 1 || refresh > 43200) { toast('刷新间隔需在 1~43200 分钟之间', 'error'); return; }
  const body = {
    enabled: _('ocEnabled').value === 'true',
    key: _('ocKey').value.trim(),
    baseURLs: _('ocBaseURLs').value.split('\n').map(s => s.trim()).filter(Boolean),
    proxies: proxies,
    subs: ocSubsArr,
    exitMode: _('ocExitMode').value,
    stickySessions: _('ocSticky').value === 'true',
    nodeExcludeKeywords: _('ocNodeExclude').value.split('\n').map(s => s.trim()).filter(Boolean),
    enabledRegions: ocRegions,
    dnsMode: _('ocDnsMode') ? _('ocDnsMode').value : 'doh-ali',
    dnsCustom: _('ocDnsCustom') ? _('ocDnsCustom').value.trim() : '',
    rescueDirect: _('ocRescue') ? _('ocRescue').value === 'true' : true,
    subsRefreshMins: refresh,
    proxyStrategy: _('ocStrategy').value,
    maxConcurrency: parseInt(_('ocMaxConc').value) || 8,
    retries: parseInt(_('ocRetries').value) || 3,
    failover: _('ocFailover').value === 'true',
    failoverCount: parseInt(_('ocFailoverCount').value) || 3,
    failoverMinutes: parseInt(_('ocFailoverMinutes').value) || 5,
    compaction: {
      auto: _('ocCompactAuto').value === 'true',
      buffer: parseInt(_('ocCompactBuffer').value) || 20000,
      keepTokens: parseInt(_('ocKeepTokens').value) || 8000,
      summaryModel: _('ocSummaryModel').value.trim(),
      maxSummary: parseInt(_('ocMaxSummary').value) || 4096
    }
  };
  try {
    const d = await api('POST', '/opencode/config/update', body);
    toast('opencode 配置已保存', 'success');
    loadOcConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 供应商与模型 ==========
// 参考 ai-gateway 的供应商卡片: 内置(opencode/cline)与通用 Provider 同一个口径
// 从 GET /providers 拿, 每个供应商一张可折叠卡片, 展开即见它的全部模型。
let pvData = {};
// 记住哪些卡片是展开的: 保存/刷新目录/切换勾选会重绘, 展开状态不能丢。
const pvOpenSet = {};
// 请求序号: loadModelIndex 可能被多个 setTimeout(loadModelIndex, 3000/12000) 同时挂着,
// 慢响应覆盖快响应的竞态靠它消掉 —— 只有最新一次请求的回调才允许写 pvData / 渲染。
let mIdxSeq = 0;
// 上一次整表渲染的数据签名。保存/勾选后会安排多个延迟刷新(3s/8s/10s), 目录内容
// 没变时重建 DOM 只会重播展开动画、闪一下并把滚动位置打回顶部 —— 所以签名相同
// 就跳过重建。出错占位时必须把它清空(见 loadModelIndex 的 catch), 否则错误页会
// 挡住随后的同数据正常渲染。
let pvRenderSig = '';

async function loadModelIndex() {
  const s = ++mIdxSeq;
  try {
    const d = await api('GET', '/providers');
    if (s !== mIdxSeq) return;   // 已有更新的请求在途, 丢弃这次(可能更慢的)响应
    pvData = (d.data && d.data.providers) || {};
    renderModelIndex();
  } catch (e) {
    if (s !== mIdxSeq) return;   // 同样丢弃过期的错误占位, 不让旧失败覆盖新成功
    pvRenderSig = '';            // DOM 已被错误占位替换, 下次成功必须真正重建
    _('modelIndex').innerHTML = fail(e, 'loadModelIndex()');
  }
}

// providerModels 把三种口径(catalogModels / modelEntries / models)归一成
// [{id, on, context, output, status, cost, syncedAt}], 顺序保持稳定。
// 勾选态以 catalogModels / modelEntries 为准(面板读写的源), models 只补元数据。
function providerModels(p) {
  const meta = {};
  (p.models || []).forEach(m => {
    meta[m.model || String(m.id).split(':').slice(1).join(':')] = m;
  });
  let base = [];
  if (p.catalogModels && p.catalogModels.length) {
    base = p.catalogModels.map(m => ({ id: m.id, on: m.disabled === false }));
  } else if (p.modelEntries && p.modelEntries.length) {
    base = p.modelEntries.map(e => ({ id: e.id, on: !!e.enabled }));
  }
  if (!base.length) base = Object.keys(meta).map(id => ({ id: id, on: true }));
  // 防止「用户手工填、不在上游目录里的模型」从卡片消失(报告 §6 的诚实盲区):
  // 通用 Provider 的 p.models 是用户手工清单, 上游目录里没有的私有模型只存在于此。
  // catalog 优先但必须把它补回来; 内置(cline/opencode)的 p.models 是池元数据, 不并入以免炸开整表。
  if (!p.builtin) {
    const have = new Set(base.map(b => b.id));
    Object.keys(meta).forEach(id => {
      if (!have.has(id)) base.push({ id: id, on: meta[id].enabled !== false });
    });
  }
  return base.map(b => {
    const m = meta[b.id] || {};
    return {
      id: b.id, on: b.on !== false,
      context: m.context, output: m.output,
      status: m.status, cost: m.cost, syncedAt: m.syncedAt,
      // requiresStream 必须透传: 行渲染靠它画「流式」标记, 漏掉就永远不显示
      // (proxy.go 里同名字段是真实决定走不走流式的依据, 两边口径要一致)。
      requiresStream: m.requiresStream
    };
  });
}

// providerStat 卡片右上角的状态口径(就绪 / 未就绪 / 出错)。
function providerStat(n, p) {
  const rt = p.runtime || {};
  const ms = providerModels(p);
  const count = ms.filter(m => m.on).length;
  if (p.builtin) {
    if (n === 'cline') {
      const off = ms.length - count;
      return { count: count, ok: true, label: off > 0 ? ('就绪 · ' + off + ' 个已下架') : '账号池就绪' };
    }
    return { count: count, ok: !!p.keys, label: p.keys ? 'zen key 已配置' : '未配置 zen key' };
  }
  if (!rt.configured) return { count: count, ok: false, label: '未配置 key' };
  if (rt.error) return { count: count, ok: false, label: String(rt.error).slice(0, 60) };
  const extra = p.catalog ? ('目录 ' + (rt.catalogSize || 0) + ' · 可聊 ' + (rt.chatCount || 0)) : ('手动 ' + ms.length);
  return { count: count, ok: true, label: extra };
}

function renderModelIndex() {
  const box = _('modelIndex');
  const names = Object.keys(pvData).sort((a, b) => {
    const pa = pvData[a] || {}, pb = pvData[b] || {};
    if (!!pa.builtin !== !!pb.builtin) return pa.builtin ? -1 : 1;   // 内置钉在最前
    return a.localeCompare(b);
  });
  if (!names.length) {
    pvRenderSig = '';
    box.innerHTML = '<div class="empty" style="padding:14px">暂无供应商, 在下方添加第一个通用 Provider</div>';
    _('modelIndexSummary').textContent = '0 个供应商';
    return;
  }
  // 数据签名相同 → 跳过重建, 只把全局搜索的显隐重新套一遍(保持与搜索框一致)。
  // 签名必须同时含数据与展开态: 只比数据的话, 外部改了 pvOpenSet 后再重绘会被
  // 误判为"没变化"而跳过, 卡片的 open 类就丢渲染(渲染测试 [9] 锁的就是这个)。
  const sig = JSON.stringify([names.map(n => pvData[n] || {}), pvOpenSet]);
  if (sig === pvRenderSig) {
    filterModelIndex(_('modelSearchBox') ? _('modelSearchBox').value : '');
    return;
  }
  pvRenderSig = sig;
  let totalModels = 0, totalOn = 0, readyN = 0;
  names.forEach(n => {
    const p = pvData[n] || {};
    const s = providerStat(n, p);
    totalModels += providerModels(p).length;
    totalOn += s.count;
    if (s.ok) readyN++;
  });
  box.innerHTML = names.map(n => providerCard(n, pvData[n] || {})).join('');
  _('modelIndexSummary').textContent = names.length + ' 个供应商 · ' + totalOn + '/' + totalModels + ' 模型可用 · ' + readyN + ' 个已就绪';
  filterModelIndex(_('modelSearchBox') ? _('modelSearchBox').value : '');
}

function providerCard(n, p) {
  const stat = providerStat(n, p);
  const ms = providerModels(p);
  const disp = p.display || n;
  const kind = p.builtin ? '内置路由' : (p.google ? 'Google Gemini' : (p.apiType === 'anthropic' ? 'Anthropic' : 'OpenAI'));
  const meta = ['<code>' + esc(n) + '</code>', esc(kind),
    (p.keys || 0) > 0 ? (p.keys + ' key') : null, (ms.length + ' 模型')
  ].filter(Boolean).join('');
  // title 是双引号属性, 必须走 escAttr —— esc 只转义 & < >, 不转义引号。
  // stat.label 的出错分支取的是上游返回文本(rt.error), 含 " 即可闭合属性注入。
  const badge = '<span class="pbadge ' + (stat.ok ? 'pb-on' : 'pb-off') +
    '" title="' + escAttr(stat.label) + '">● ' + esc(stat.label) + '</span>';
  // 卡片头可点击折叠: 补键盘可达性 —— role/tabindex/aria-expanded/aria-controls,
  // 并用 onkeydown 支持 Enter/Space 展开(空格要 preventDefault 防页面滚动)。
  // aria-controls 指向展开区 id="dt-<n>", 与下方 .pd 的 id 对应。
  const head = '<div class="ps" role="button" tabindex="0" aria-expanded="' + (pvOpenSet[n] ? 'true' : 'false') +
    '" aria-controls="dt-' + escAttr(n) + '" onclick="togCard(\'' + escJs(n) + '\')" onkeydown="psKey(event,\'' + escJs(n) + '\')">' +
    '<div class="pl">' +
      '<span class="pchev">▶</span>' +
      '<span class="pav' + (p.builtin ? ' builtin' : '') + '">' + esc(disp.charAt(0).toUpperCase() || 'A') + '</span>' +
      '<div><h3 title="' + escAttr(disp) + '">' + esc(disp) + '</h3><div class="pmeta">' + meta + '</div></div>' +
    '</div>' + badge +
  '</div>';
  const search = escAttr((disp + ' ' + n + ' ' + ms.map(m => m.id).join(' ')).toLowerCase());
  return '<article class="pi' + (pvOpenSet[n] ? ' open' : '') + '" data-id="' + escAttr(n) + '" data-search="' + search + '">' +
    head + '<div class="pd" id="dt-' + escAttr(n) + '">' + providerCardBody(n, p, stat) + '</div></article>';
}

// 只切 .open 类: 模型表 / 搜索框 / 勾选态都在 DOM 里, 不整表重绘。
function togCard(n) {
  pvOpenSet[n] = !pvOpenSet[n];
  document.querySelectorAll('#modelIndex .pi').forEach(a => {
    if (a.dataset.id === n) {
      a.classList.toggle('open', !!pvOpenSet[n]);
      const ps = a.querySelector('.ps');
      if (ps) ps.setAttribute('aria-expanded', String(!!pvOpenSet[n]));
    }
  });
}

// 卡片头键盘可达: Enter / Space 触发折叠, 空格要 preventDefault 防页面滚动。
function psKey(e, n) {
  if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
    e.preventDefault();
    togCard(n);
  }
}

// 只重绘指定 provider 的一张卡片(替换对应 <article>), 不动其余卡片与 pvOpenSet。
// 用于 toggleProviderModel 的乐观更新, 避免整表重建和全局搜索展开逻辑重入冲突。
// 找不到对应节点(首次渲染还没建好)时退回整表重建, 保证不丢渲染。
function renderOneCard(n) {
  const p = pvData[n] || {};
  // data-id 在 HTML 里用 escAttr 写入, 经 innerHTML 解析后实际值就是裸 n; CSS 选择器必须用 CSS.escape,
  // 用 escAttr 会把 " 转成 &quot; 字面量, 导致含 " 的 provider id 查询失败。
  const card = document.querySelector ? document.querySelector('#modelIndex .pi[data-id="' + CSS.escape(n) + '"]') : null;
  if (card) {
    card.outerHTML = providerCard(n, p);
  } else {
    renderModelIndex();
  }
  filterModelIndex(_('modelSearchBox') ? _('modelSearchBox').value : '');
}

function providerCardBody(n, p, stat) {
  const builtin = !!p.builtin;
  const ms = providerModels(p);
  const rows = ms.map(m => {
    if (n === 'cline') {
      const disp = 'cline/' + m.id;
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      const cost = COST_LABEL[m.cost] || m.cost || '';
      const synced = m.syncedAt ? new Date(m.syncedAt).toLocaleTimeString('zh-CN') : '-';
      return '<tr data-mrow data-row-search="' + escAttr(disp) + '"><td class="mrow-label" style="text-align:left;font-family:var(--font-mono)">' + esc(disp) + '</td>' +
        '<td>' + (cost ? '<span class="model-tag' + (m.cost === 'free' ? ' free' : '') + '">' + esc(cost) + '</span>' : '<span style="color:var(--text3)">-</span>') + '</td>' +
        '<td><span class="model-tag" style="' + st.css + '">' + st.label + '</span>' +
          (m.requiresStream ? '<span class="model-tag" title="该模型需要流式响应">流式</span>' : '') + '</td>' +
        '<td style="font-size:var(--fs-xs);color:var(--text3);min-width:66px;text-align:right">' + synced + '</td>' +
        '<td><button type="button" class="copy-icon" aria-label="复制 ' + escAttr(disp) + '" onclick="copyText(\'' + escJs(disp) + '\')">📋</button></td></tr>';
    }
    if (n === 'opencode') {
      const disp = 'zen/' + m.id;
      return '<tr data-mrow data-row-search="' + escAttr(disp) + '"><td class="mrow-label" style="text-align:left;font-family:var(--font-mono)">' + esc(disp) + '</td>' +
        '<td style="font-size:var(--fs-sm)">' + fmtNum(m.context || 0) + '</td>' +
        '<td style="font-size:var(--fs-sm)">' + fmtNum(m.output || 0) + '</td>' +
        '<td><button type="button" class="copy-icon" aria-label="复制 ' + escAttr(disp) + '" onclick="copyText(\'' + escJs(disp) + '\')">📋</button></td></tr>';
    }
    const disp = n + ':' + m.id;
    return '<tr data-mrow data-row-search="' + escAttr(disp) + '"><td><input type="checkbox" data-pv="' + escAttr(n) + '" data-model="' + escAttr(m.id) + '"' + (m.on ? ' checked' : '') + ' onchange="toggleProviderModel(this)"></td>' +
      '<td class="mrow-label" style="text-align:left;font-family:var(--font-mono)">' + esc(disp) + '</td>' +
      '<td><button type="button" class="copy-icon" aria-label="复制 ' + escAttr(disp) + '" onclick="copyText(\'' + escJs(disp) + '\')">📋</button></td></tr>';
  }).join('');

  let head, note;
  if (n === 'cline') {
    head = '<tr><th style="text-align:left">模型 ID</th><th>费用</th><th>状态</th><th style="text-align:right">同步时间</th><th style="width:44px"></th></tr>';
    const ls = p.lastSync ? new Date(p.lastSync).toLocaleString('zh-CN') : '-';
    note = '池内自动选账号与模型, 无需逐个启用。状态来自官方推荐清单(上次同步 ' + ls + ')。';
  } else if (n === 'opencode') {
    head = '<tr><th style="text-align:left">模型 ID</th><th>上下文</th><th>最大输出</th><th style="width:44px"></th></tr>';
    note = 'zen 免费目录, 每 10 分钟自动同步; 用 zen/ 前缀调用。';
  } else {
    head = '<tr><th style="width:44px">启用</th><th style="text-align:left">模型 ID' +
      '<button type="button" class="btn btn-sm" style="margin-left:10px;padding:2px 10px;font-size:var(--fs-xs)" onclick="toggleAllProviderModels(\'' + escJs(n) + '\',true)">全选</button>' +
      '<button type="button" class="btn btn-sm" style="margin-left:6px;padding:2px 10px;font-size:var(--fs-xs)" onclick="toggleAllProviderModels(\'' + escJs(n) + '\',false)">全不选</button>' +
      '</th><th style="width:44px"></th></tr>';
    note = '用 ' + n + ':模型名 调用; 勾选决定它是否对网关发布。';
  }

  const acts = [];
  if (n === 'cline') acts.push('<button class="btn btn-sm" onclick="refreshModels()">🔄 同步官方清单</button>');
  if (n === 'opencode') acts.push('<button class="btn btn-sm" onclick="refreshOcModels()">🔄 同步 zen 目录</button>');
  if (!builtin) {
    acts.push('<button class="btn btn-sm" onclick="testProviderByName(\'' + escJs(n) + '\')">🔍 连通测试</button>');
    acts.push('<button class="btn btn-sm" onclick="refreshOneCatalog(\'' + escJs(n) + '\')">🔄 刷新目录</button>');
    acts.push('<button class="btn btn-sm" onclick="editProvider(\'' + escJs(n) + '\')">✏️ 编辑</button>');
    acts.push('<button class="btn btn-sm btn-danger" onclick="delProvider(\'' + escJs(n) + '\')">🗑 删除</button>');
  }

  // colspan 必须等于表头实际列数, 否则空态行错位: cline 5 列 / opencode 4 列 / 通用 3 列。
  // 从 head 直接数 <th> —— 表头改动后这里自动跟着对, 不留硬编码。
  const cols = (head.match(/<th/g) || []).length;
  const emptyRow = ms.length ? '' :
    '<tr><td colspan="' + cols + '" style="text-align:center;color:var(--text3);font-size:var(--fs-sm);padding:var(--sp-4)">该供应商暂无模型 — ' +
    (builtin ? '等待上游同步' : '配置 API Key 后刷新目录, 或在下方添加表单里手填模型') + '</td></tr>';

  return '<div class="pd-head"><p>' + note + '</p><div class="pacts">' + acts.join('') + '</div></div>' +
    '<input type="text" placeholder="在 ' + esc(n) + ' 内搜索模型" oninput="filterCardModels(\'' + escJs(n) + '\',this)" style="max-width:280px;margin-bottom:9px">' +
    '<div class="table-wrap"><table><thead>' + head + '</thead><tbody>' + rows + emptyRow + '</tbody></table></div>' +
    (ms.length ? '<div class="hint" style="margin-top:9px">' + stat.count + '/' + ms.length + ' 个已启用' +
      (p.google ? ' · Google 的端点与鉴权约定已由程序内置补齐' : '') + '</div>' : '');
}

// 全局搜索: 只切卡片显隐 —— 输入框焦点与展开状态都保住。
// 搜索时强制展开命中的卡片, 清空搜索恢复用户自己的展开选择。
function filterModelIndex(q) {
  q = String(q || '').trim().toLowerCase();
  let shown = 0;
  document.querySelectorAll('#modelIndex .pi').forEach(a => {
    const hit = !q || (a.getAttribute('data-search') || '').indexOf(q) >= 0;
    a.style.display = hit ? '' : 'none';
    if (hit) {
      if (q) {
        a.classList.add('open');
        // 高亮供应商名里命中的子串 —— 文本先 esc 再插 <mark>, 绝不拼接未转义输入(防 XSS)
        const h3 = a.querySelector ? a.querySelector('h3') : null;
        if (h3 && h3.dataset) {
          if (!h3.dataset.nameLabel && h3.textContent != null) h3.dataset.nameLabel = h3.textContent;
          const label = h3.dataset.nameLabel || h3.textContent || '';
          const i = label.toLowerCase().indexOf(q);
          h3.innerHTML = (i >= 0)
            ? esc(label.slice(0, i)) + '<mark>' + esc(label.slice(i, i + q.length)) + '</mark>' + esc(label.slice(i + q.length))
            : esc(label);
        }
      } else {
        a.classList.toggle('open', !!pvOpenSet[a.dataset.id]);
        // 清空搜索时必须在这里显式还原标题。q 为空时 hit 恒为 true(见上面 hit 的取值逻辑),
        // 所有卡片都走这个分支, 下方那个"未命中"分支里的清除逻辑永远不会执行 ——
        // 于是上一轮搜索留下的 mark 高亮会一直挂在标题上, 直到整表重绘。
        // 注意: 本文件是 Go 原始字符串, 注释里不要出现反引号, 否则会提前终止字符串。
        const h3 = a.querySelector ? a.querySelector('h3') : null;
        if (h3 && h3.dataset && h3.dataset.nameLabel != null) h3.innerHTML = esc(h3.dataset.nameLabel);
      }
      shown++;
    } else {
      a.classList.remove('open');
      // 清空上一轮搜索留下来的高亮
      const h3 = a.querySelector ? a.querySelector('h3') : null;
      if (h3 && h3.dataset && h3.dataset.nameLabel != null) h3.innerHTML = esc(h3.dataset.nameLabel);
    }
  });
  const empty = _('modelSearchEmpty');
  if (empty) empty.style.display = (shown === 0 && q) ? '' : 'none';
}

// 卡片内搜索: 只切行显隐, 不重绘, 勾选态不丢。
function filterCardModels(n, el) {
  const q = String(el.value || '').trim().toLowerCase();
  const card = el.closest ? el.closest('.pi') : null;
  if (!card) return;
  let hit = 0;
  card.querySelectorAll('[data-mrow]').forEach(tr => {
    const ok = !q || (tr.getAttribute('data-row-search') || '').toLowerCase().indexOf(q) >= 0;
    tr.style.display = ok ? '' : 'none';
    if (ok) hit++;
    // 高亮命中子串: 仅搜索词非空时, 把模型 id 单元格里命中的文本用 <mark> 包起来。
    // 文本先 esc 再插 <mark>, 绝不拼接未转义内容 —— 防 XSS。
    const labelTd = tr.querySelector ? tr.querySelector('.mrow-label') : null;
    if (labelTd && labelTd.dataset) {
      if (!labelTd.dataset.rowLabel && labelTd.textContent != null) labelTd.dataset.rowLabel = labelTd.textContent;
      const label = labelTd.dataset.rowLabel || labelTd.textContent || '';
      if (q) {
        const i = label.toLowerCase().indexOf(q);
        labelTd.innerHTML = (i >= 0)
          ? esc(label.slice(0, i)) + '<mark>' + esc(label.slice(i, i + q.length)) + '</mark>' + esc(label.slice(i + q.length))
          : esc(label);
      } else if (labelTd.innerHTML.indexOf('<mark>') >= 0) {
        labelTd.innerHTML = esc(label);   // 清除上一轮搜索残留的高亮
      }
    }
  });
  // 全无命中时给一个空态占位, 否则用户搜错词只看到一片空白。
  // 占位节点只建一次, 之后随命中数显隐; 用可选链防御无 querySelector 的环境。
  const ph = card.querySelector ? card.querySelector('.card-models-empty') : null;
  if (ph) { ph.style.display = hit ? 'none' : ''; return; }
  if (!hit && card.querySelector) {
    const tb = card.querySelector('.table-wrap');
    if (tb && tb.appendChild) {
      const t = document.createElement('div');
      t.className = 'card-models-empty empty';
      t.style.cssText = 'padding:var(--sp-4);text-align:center;color:var(--text3);font-size:var(--fs-sm)';
      t.textContent = '没有匹配的模型';
      tb.appendChild(t);
    }
  }
}

async function refreshOneCatalog(n) {
  try {
    await api('POST', '/providers/refresh', { name: n });
    toast('已刷新 ' + n + ' 的模型目录', 'success');
    setTimeout(loadModelIndex, 3000);
    setTimeout(loadModelIndex, 12000);
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

// 按 provider 名拉全量目录(表单按钮, name 留空则全部)。
async function refreshProviderCatalog() {
  const name = _('pvName').value.trim();
  try {
    await api('POST', '/providers/refresh', name ? { name } : {});
    toast('目录刷新已启动', 'success');
    setTimeout(loadModelIndex, 3000);
    setTimeout(loadModelIndex, 12000);
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

async function toggleProviderModel(box) {
  const name = box.getAttribute('data-pv'), id = box.getAttribute('data-model');
  const p = pvData[name] || {};
  const entries = new Map((p.modelEntries || []).map(e => [e.id, !!e.enabled]));
  // 预迁移回退: 用目录勾选态/旧白名单补齐条目, 否则一次切换会丢数据
  (p.catalogModels || []).forEach(m => { if (!entries.has(m.id)) entries.set(m.id, !m.disabled); });
  (p.freeModels || []).forEach(mid => { if (!entries.has(mid)) entries.set(mid, true); });
  (p.disabledModels || []).forEach(mid => { if (!entries.has(mid)) entries.set(mid, false); });
  entries.set(id, box.checked);
  // 只保留上游返回的完整 provider(含任何后端新增字段), 仅剥离运行时态 runtime,
  // 再覆盖本轮操作拥有的字段(models / migrated)。不再硬编码 8 字段删除名单 —— 那样
  // 后端每加一个字段都得同步改这里, 否则新字段会被「漏删」而被动丢失(原黑名单的坑)。
  const existing = Object.assign({}, p);
  delete existing.runtime;
  existing.models = Array.from(entries, ([mid, enabled]) => ({ id: mid, enabled }));
  existing.migrated = true;
  try {
    await api('POST', '/providers/update', { name, provider: existing });
    toast((box.checked ? '已启用 ' : '已剔除 ') + name + ':' + id, 'success');
    // 乐观更新本地态后只 patch 这一张卡片, 不整表重建 —— 避免和 filterModelIndex 的
    // 「搜索时强制展开」/ pvOpenSet 还原逻辑产生重入冲突(整表重建会把展开态打回)。
    // pvOpenSet 是模块级、本次不动它, patch 后再套一遍全局搜索过滤保持与搜索态一致。
    const merged = Array.from(entries, ([mid, enabled]) => ({ id: mid, enabled }));
    p.modelEntries = merged;
    (p.catalogModels || []).forEach(m => { if (m.id === id) m.disabled = !box.checked; });
    renderOneCard(name);
    try { await api('POST', '/providers/refresh', { name }); setTimeout(loadModelIndex, 8000); } catch (e2) { /* 目录回来后自动对齐 */ }
  } catch (e) { toast('保存失败: ' + e.message, 'error'); box.checked = !box.checked; }
}

// toggleAllProviderModels 一键全选/全不选当前 provider 的全部模型。
// 条目归并口径与 toggleProviderModel 完全一致(modelEntries + catalogModels /
// freeModels / disabledModels 预迁移回退), 区别只在把全部 id 置为同一状态,
// 并**一次 POST** 保存 —— 绝不能逐个模型发 N 次请求。
// 只对通用 Provider 生效: 内置 cline/opencode 的行没有勾选框, 表头也不渲染按钮。
async function toggleAllProviderModels(name, checked) {
  const p = pvData[name] || {};
  const entries = new Map((p.modelEntries || []).map(e => [e.id, !!e.enabled]));
  (p.catalogModels || []).forEach(m => { if (!entries.has(m.id)) entries.set(m.id, !m.disabled); });
  (p.freeModels || []).forEach(mid => { if (!entries.has(mid)) entries.set(mid, true); });
  (p.disabledModels || []).forEach(mid => { if (!entries.has(mid)) entries.set(mid, false); });
  if (!entries.size) { toast(name + ' 没有可勾选的模型', 'error'); return; }
  entries.forEach((v, k) => entries.set(k, checked));
  // 与 toggleProviderModel 相同: 整体回传 provider, 只剥离运行时态 runtime,
  // 覆盖 models / migrated, 其余字段原样保留以免后端加字段时被漏删。
  const existing = Object.assign({}, p);
  delete existing.runtime;
  existing.models = Array.from(entries, ([mid, enabled]) => ({ id: mid, enabled }));
  existing.migrated = true;
  try {
    await api('POST', '/providers/update', { name, provider: existing });
    toast((checked ? '已全选 ' : '已全不选 ') + name + ' 的 ' + entries.size + ' 个模型', 'success');
    p.modelEntries = Array.from(entries, ([mid, enabled]) => ({ id: mid, enabled }));
    (p.catalogModels || []).forEach(m => { m.disabled = !checked; });
    renderOneCard(name);
    try { await api('POST', '/providers/refresh', { name }); setTimeout(loadModelIndex, 8000); } catch (e2) { /* 目录回来后自动对齐 */ }
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

function editProvider(n) {
  const p = pvData[n] || {};
  _('pvName').value = n;
  _('pvBaseUrl').value = p.baseUrl || '';
  _('pvKey').value = p.apiKey || '';
  if (_('pvCatalog')) _('pvCatalog').checked = p.catalog !== false;
  const entries = p.modelEntries || [];
  const enabled = entries.length ? entries.filter(e => e.enabled).map(e => e.id) : (p.freeModels || []);
  _('pvModels').value = enabled.join('\n');
  toast('已载入 ' + n + ', 修改后点保存', 'success');
}

function resetProviderForm() {
  _('pvName').value = '';
  _('pvBaseUrl').value = '';
  _('pvKey').value = '';
  if (_('pvCatalog')) _('pvCatalog').checked = true;
  _('pvModels').value = '';
  _('pvTestModel').value = '';
  _('pvResult').innerHTML = '';
}

async function saveProvider() {
  const name = _('pvName').value.trim();
  const baseUrl = _('pvBaseUrl').value.trim();
  const apiKey = _('pvKey').value.trim();
  if (!name) { toast('请填写 Provider 名', 'error'); return; }
  // JS 侧先按与后端 providerIDRe 同义的规则拦一道: 非法 id 会经 escJs 拼进 onclick
  // 字符串字面量, 仅靠后端校验不够(用户已看到过一次注入风险)。不合法就直接拒, 不发请求。
  if (!providerIDRe.test(name)) {
    toast('Provider 名只能以小写字母开头，仅含小写字母、数字、连字符、下划线', 'error');
    return;
  }
  if (!baseUrl) { toast('请填写 API 地址', 'error'); return; }
  if (!apiKey) { toast('请填写 API Key', 'error'); return; }
  // 后端按整体替换处理 provider: 表单未编辑的字段(headers 等)
  // 必须原样回传, 否则保存会把它们清掉, 已配好的 provider 会静默失真。
  const existing = Object.assign({}, pvData[name] || {});
  delete existing.runtime;
  delete existing.models;
  delete existing.catalogModels;
  delete existing.google;
  delete existing.chatEndpoint;
  delete existing.catalogEndpoint;
  delete existing.modelEntries;
  // 文本框里的行 = 显式启用; 之前已勾掉的不在框里, 要原样保留禁用态
  const want = new Set(_('pvModels').value.split('\n').map(s => s.trim()).filter(Boolean));
  const prev = new Map(((pvData[name] || {}).modelEntries || []).map(e => [e.id, !!e.enabled]));
  const models = [];
  want.forEach(id => models.push({ id, enabled: true }));
  const keepDisabled = id => { if (!want.has(id) && !models.some(m => m.id === id)) models.push({ id, enabled: false }); };
  prev.forEach((en, id) => { if (!en) keepDisabled(id); });
  ((pvData[name] || {}).disabledModels || []).forEach(keepDisabled);
  const body = {
    name,
    provider: Object.assign(existing, {
      baseUrl,
      apiKey,
      catalog: _('pvCatalog').checked,
      models,
      migrated: true,
      // 旧白名单/剔除已折叠进 models, 不再保留
      freeModels: [],
      disabledModels: [],
    }),
  };
  try {
    await api('POST', '/providers/update', body);
    toast('已保存 ' + name + '，正在拉取模型目录…', 'success');
    // 保存成功即清空表单: 输入框里留着已保存的内容会让人分不清"还没保存"和"已保存"，
    // 也容易在改名后误再点一次保存出第二条记录。清空后要填就是一次全新的添加。
    resetProviderForm();
    pvOpenSet[name] = true;   // 保存后直接展开这张卡片, 用户当场看到结果
    loadModelIndex();
    // 保存后自动拉一次目录, 用户不需要再点「刷新目录」
    try {
      await api('POST', '/providers/refresh', { name });
      setTimeout(loadModelIndex, 3000);
      setTimeout(loadModelIndex, 10000);
    } catch (e2) { /* 目录拉取失败会显示在 provider 行上 */ }
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 自动路由 ==========

// fmtRemain 冷却剩余时间的可读形式。
function fmtRemain(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s >= 3600) return Math.floor(s / 3600) + 'h' + Math.floor((s % 3600) / 60) + 'm';
  if (s >= 60) return Math.floor(s / 60) + 'm' + (s % 60) + 's';
  return s + 's';
}

// routerData 最近一次 GET /router 的结果, 保存时回传未编辑的字段要靠它。
let routerData = null;
let routerProviders = new Set(); // 勾选的供应商名
let routerModels = new Set();    // 勾选的 "provider:model"

async function loadRouter() {
  try {
    const d = await api('GET', '/router');
    routerData = d.data || {};
  } catch (e) {
    // 静默 return 会让页面永远停在"加载中"，看不出是后端问题还是没数据
    const msg = fail(e, 'loadRouter()');
    ['arProviderList', 'arModelList'].forEach(id => { if (_(id)) _(id).innerHTML = msg; });
    if (_('arChainBody')) _('arChainBody').innerHTML = '<tr><td colspan="2">' + msg + '</td></tr>';
    return;
  }
  // 用服务端返回的勾选状态初始化本地选择
  routerProviders = new Set();
  routerModels = new Set();
  (routerData.providers || []).forEach(p => {
    if (p.selected) routerProviders.add(p.name);
    (p.models || []).forEach(m => {
      if (m.selected) routerModels.add(p.name + ':' + m.id);
    });
  });
  renderRouter();
}

function renderRouter() {
  const d = routerData || {};
  if (_('arAlias')) _('arAlias').value = d.alias || d.defaultAlias || 'auto-router';
  renderRouterExample();

  // ---- 供应商勾选 ----
  const pl = _('arProviderList');
  if (pl) {
    const provs = d.providers || [];
    if (!provs.length) {
      pl.innerHTML = '<div class="empty" style="padding:var(--sp-3)">没有可用的上游</div>';
    } else {
      pl.innerHTML = provs.map(p => {
        const models = p.models || [];
        const chosen = routerProviders.has(p.name);
        const picked = models.filter(m => routerModels.has(p.name + ':' + m.id)).length;
        const bad = [];
        if (p.name === 'cline' && !p.configured) bad.push('无可用账号');
        if (p.name !== 'cline' && !p.configured) bad.push('缺 API Key');
        const meta = [];
        if (p.builtin) meta.push('内置');
        if (p.google) meta.push('Google');
        if (p.name === 'cline') meta.push('按账号轮询');
        meta.push(models.length + ' 个可用模型');
        if (chosen) meta.push('已选 ' + picked + ' 个');
        return '<label style="display:flex;align-items:center;gap:10px;padding:9px 12px;'
          + 'border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--inset);cursor:pointer">'
          + '<input type="checkbox" data-prov="' + escAttr(p.name) + '"' + (chosen ? ' checked' : '') + '>'
          + '<span style="flex:1;display:flex;align-items:center;gap:10px;min-width:0;flex-wrap:wrap">'
          + '<strong>' + esc(p.display || p.name) + '</strong>'
          + (p.builtin ? '<span class="model-tag" style="opacity:.8">内置上游</span>' : '')
          + '<span style="color:var(--text3);font-size:var(--fs-xs)">' + esc(meta.join(' · ')) + '</span>'
          + (bad.length ? '<span style="color:var(--danger);font-size:var(--fs-xs)">' + esc(bad.join('，')) + '</span>' : '')
          + '</span></label>';
      }).join('');
    }
  }

  // ---- 模型勾选: 只列已勾选供应商的模型 ----
  const ml = _('arModelList');
  if (ml) {
    const chosen = (d.providers || []).filter(p => routerProviders.has(p.name));
    if (!chosen.length) {
      ml.innerHTML = '<div class="empty" style="padding:var(--sp-3)">先在上面勾选供应商，这里会列出它们的模型</div>';
    } else {
      ml.innerHTML = chosen.map(p => {
        const models = p.models || [];
        const picked = models.filter(m => routerModels.has(p.name + ':' + m.id)).length;
        const rows = models.map(m => {
          const key = p.name + ':' + m.id;
          const label = m.id === '*' ? '账号池自动选模型' : m.id;
          const ctx = m.context ? (' · ' + Math.round(m.context / 1000) + 'k 上下文') : '';
          return '<label style="display:flex;align-items:center;gap:var(--sp-2);padding:5px 10px;font-size:var(--fs-sm);cursor:pointer">'
            + '<input type="checkbox" data-key="' + escAttr(key) + '"' + (routerModels.has(key) ? ' checked' : '') + '>'
            + '<code>' + esc(label) + '</code><span style="color:var(--text3)">' + esc(ctx) + '</span></label>';
        }).join('') || '<div style="padding:8px 10px;color:var(--text3);font-size:var(--fs-xs)">该供应商暂无可用模型（先点上面的「刷新全部目录」）</div>';
        const pname = p.display || p.name;
        return '<div style="margin-bottom:10px;border:1px solid var(--border);border-radius:var(--radius-sm);overflow:hidden">'
          + '<div style="padding:var(--sp-2) var(--sp-3);display:flex;align-items:center;gap:var(--sp-2);background:rgba(148,163,184,.05);font-size:var(--fs-base)">'
          + '<strong>' + esc(pname) + '</strong>'
          + '<span style="color:var(--text3);font-weight:normal">已选 ' + picked + ' / ' + models.length + '</span>'
          + '<span style="margin-left:auto;display:flex;gap:6px">'
          + '<button type="button" class="btn btn-sm" data-prov-all="' + escAttr(p.name) + '">全选</button>'
          + '<button type="button" class="btn btn-sm" data-prov-none="' + escAttr(p.name) + '">全不选</button>'
          + '</span></div><div style="padding:4px 6px">' + rows + '</div></div>';
      }).join('');
    }
  }
  if (_('arSelectionWarn')) {
    _('arSelectionWarn').textContent = routerModels.size
      ? '已选 ' + routerModels.size + ' 个模型参与自动路由'
      : '未勾选任何模型：保存后自动路由会回落为「全部供应商的全部已启用模型」';
  }

  renderRouterChain(d);
  renderRouterUsage(d);
  renderRouterCooling(d);
}

// 勾选交互统一走事件委托: 供应商名与模型 id 里可能带 / : . 等字符,
// 拼进内联 onclick 很容易被引号打断, 用 data-* 属性 + 委托最稳妥。
document.addEventListener('change', e => {
  const el = e.target;
  if (!el || el.tagName !== 'INPUT' || el.type !== 'checkbox') return;
  if (el.dataset && el.dataset.prov) toggleProvider(el.dataset.prov, el.checked);
  else if (el.dataset && el.dataset.key) toggleModel(el.dataset.key, el.checked);
});
document.addEventListener('click', e => {
  const el = e.target;
  if (!el || !el.dataset) return;
  if (el.dataset.provAll) routerSelectProviderModels(el.dataset.provAll, true);
  else if (el.dataset.provNone) routerSelectProviderModels(el.dataset.provNone, false);
});

function toggleProvider(name, on) {
  if (on) {
    routerProviders.add(name);
  } else {
    routerProviders.delete(name);
    // 取消供应商时一并取消它名下已选的模型, 否则会出现"勾了模型却没勾供应商"的矛盾状态
    const p = ((routerData.providers) || []).find(x => x.name === name);
    ((p && p.models) || []).forEach(m => routerModels.delete(name + ':' + m.id));
  }
  renderRouter();
}

function toggleModel(key, on) {
  if (on) routerModels.add(key); else routerModels.delete(key);
  renderRouter();
}

function routerSelectProviderModels(name, on) {
  const p = ((routerData.providers) || []).find(x => x.name === name);
  ((p && p.models) || []).forEach(m => {
    const key = name + ':' + m.id;
    if (on) routerModels.add(key); else routerModels.delete(key);
  });
  renderRouter();
}

function routerSelectAll(on) {
  routerProviders = new Set();
  routerModels = new Set();
  if (on) {
    (routerData.providers || []).forEach(p => {
      if (!p.configured) return; // 缺 key 的选了也只会被跳过
      routerProviders.add(p.name);
      (p.models || []).forEach(m => routerModels.add(p.name + ':' + m.id));
    });
  }
  renderRouter();
}

function renderRouterExample() {
  if (!_('arExample')) return;
  const alias = (_('arAlias').value || '').trim() || 'auto-router';
  const base = (_('dashApiBase') && _('dashApiBase').value) || window.location.origin;
  _('arExample').value = base + '/v1/chat/completions  ·  "model": "' + alias + '"';
}

function routerSelectionBody() {
  return {
    alias: (_('arAlias').value || '').trim(),
    providers: Array.from(routerProviders),
    models: Array.from(routerModels),
  };
}

async function saveRouter() {
  try {
    const d = await api('POST', '/router/save', routerSelectionBody());
    const r = d.data || {};
    const probs = r.problems || [];
    showRouterResult(probs.length ? 'warn' : 'ok',
      '已保存：模型名 ' + (r.alias || '') + '，参与模型 ' + (r.models || 0) + ' 个', probs);
    toast('已保存自动路由设置', 'success');
    loadRouter();
  } catch (e) { showRouterResult('error', '保存失败：' + e.message, []); }
}

async function validateRouter() {
  try {
    const d = await api('POST', '/router/validate', routerSelectionBody());
    const r = d.data || {};
    const probs = r.problems || [];
    showRouterResult(probs.length ? 'warn' : 'ok',
      probs.length ? '校验发现 ' + probs.length + ' 个问题' : ('校验通过：' + (r.models || 0) + ' 个模型都会参与自动路由'),
      probs);
  } catch (e) { showRouterResult('error', '校验失败：' + e.message, []); }
}

function showRouterResult(kind, msg, problems) {
  const el = _('arResult');
  if (!el) return;
  const color = kind === 'ok' ? 'var(--accent2)' : (kind === 'warn' ? 'var(--amber)' : 'var(--danger)');
  el.innerHTML = '<div style="padding:10px 12px;border-radius:var(--radius-sm);border:1px solid ' + color
    + ';background:var(--inset);font-size:var(--fs-base);color:' + color + '">' + esc(msg) + '</div>'
    + (problems && problems.length
      ? '<ul style="margin:8px 0 0 18px;font-size:var(--fs-sm);color:var(--text2)">'
        + problems.map(p => '<li>' + esc(p) + '</li>').join('') + '</ul>'
      : '');
}

async function refreshRouterCatalogs() {
  try {
    await api('POST', '/router/refresh', {});
    toast('已触发目录刷新，稍后自动重载', 'success');
    setTimeout(loadRouter, 4000);
    setTimeout(loadRouter, 10000);
  } catch (e) { toast('刷新失败：' + e.message, 'error'); }
}

async function routerMaintenance(kind) {
  const body = kind === 'cooling' ? { clearCooling: true } : { clearPermanent: true };
  if (kind === 'permanent' && !confirm('确认清空永久剔除列表？之前被判死的候选会重新参与自动路由。')) return;
  try {
    await api('POST', '/router/maintenance', body);
    toast(kind === 'cooling' ? '已解除全部冷却' : '已清空永久剔除', 'success');
    loadRouter();
  } catch (e) { toast('操作失败：' + e.message, 'error'); }
}

// ---- 诊断区块 ----

function renderRouterChain(d) {
  const body = _('arChainBody');
  if (!body) return;
  const routes = d.routes || [];
  if (!routes.length) {
    body.innerHTML = '<tr><td colspan="2" class="empty">还没有保存过候选链 —— 在上面勾选模型并点「保存设置」后，这里会显示实际执行顺序</td></tr>';
    return;
  }
  body.innerHTML = routes.map(r => {
    if (r.error) {
      return '<tr><td><code>' + esc(r.alias || '') + '</code></td><td style="color:var(--text3)">' + esc(r.error) + '</td></tr>';
    }
    const hops = (r.hops || []).map((h, i) => {
      const label = (i + 1) + '. <code>' + esc(h.upstream) + ':' + esc(h.model) + '</code>';
      return h.skip
        ? '<span class="model-tag" style="opacity:.55;text-decoration:line-through" title="' + escAttr(h.skip) + '">' + label + '</span>'
        : '<span class="model-tag">' + label + '</span>';
    }).join(' <span style="color:var(--text3)">→</span> ');
    return '<tr><td><code>' + esc(r.alias || '') + '</code></td><td>'
      + (hops || '<span style="color:var(--text3)">无候选</span>') + '</td></tr>';
  }).join('');
}

function renderRouterUsage(d) {
  const body = _('arUsageBody');
  const usage = d.usage || {};
  const rows = usage.rows || [];
  if (body) {
    body.innerHTML = rows.length
      ? rows.map(r => {
          const limit = r.limit ? (r.limit + '（剩 ' + r.remaining + '）') : '<span style="color:var(--text3)">不限</span>';
          return '<tr><td><code>' + esc(r.key) + '</code></td><td>' + r.req + '</td><td>' + r.ok
            + '</td><td>' + (r.fail ? '<span style="color:var(--danger)">' + r.fail + '</span>' : '0') + '</td><td>' + limit + '</td></tr>';
        }).join('')
      : '<tr><td colspan="5" class="empty">今天还没有调用记录</td></tr>';
  }
  if (_('arUsageInfo')) {
    _('arUsageInfo').textContent = '日界时区 ' + (usage.timezone || '') + '；保留 ' + (usage.retention || 7)
      + ' 天；账本文件 ' + (d.usagePath || '');
  }
}

function renderRouterCooling(d) {
  const box = _('arCoolingBox');
  if (box) {
    const cool = d.cooling || [];
    box.innerHTML = cool.length
      ? cool.map(c => '<div style="padding:6px 10px;border-bottom:1px solid var(--border);font-size:var(--fs-xs)">'
        + '<code>' + esc(c.key) + '</code> · ' + esc(c.class) + ' · 剩 ' + fmtRemain(c.remainMs) + '</div>').join('')
      : '<div style="padding:8px 10px;font-size:var(--fs-xs);color:var(--text3)">暂无冷却中的候选</div>';
  }
  const pb = _('arPermBox');
  if (pb) {
    const perm = d.permanent || [];
    pb.innerHTML = perm.length
      ? perm.map(p => '<div style="padding:6px 10px;border-bottom:1px solid var(--border);font-size:var(--fs-xs)">'
        + '<code>' + esc(p.key) + '</code><div style="color:var(--text3);margin-top:2px">' + esc(p.reason) + '</div></div>').join('')
      : '<div style="padding:8px 10px;font-size:var(--fs-xs);color:var(--text3)">暂无永久剔除</div>';
  }
}

async function delProvider(n) {
  if (!confirm('确认删除 provider ' + n + '?')) return;
  try {
    await api('POST', '/providers/update', { name: n, remove: true });
    delete pvOpenSet[n];
    toast('已删除 ' + n, 'success');
    loadModelIndex();
  }
  catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

// pvTest 抽取一次连通测试的展示文案, 表单按钮与列表按钮共用。
function pvTestReport(name, r) {
  const r2 = r || {};
  const detail = r2.error ? String(r2.error).slice(0, 200) : String(r2.body || '').slice(0, 200);
  const ok = r2.status === 200;
  return { ok, text: name + ' · HTTP ' + (r2.status || '?') + ' · ' + detail };
}

async function testProvider() {
  const name = _('pvName').value.trim();
  if (!name) { toast('请先填写 Provider 名', 'error'); return; }
  const box = _('pvResult');
  if (box) box.innerHTML = '<div class="hint" style="margin:0">连通测试中…（请求会按当前出口模式发出）</div>';
  try {
    const d = await api('POST', '/providers/test', { name, model: _('pvTestModel').value.trim() });
    const rep = pvTestReport(name, d.data);
    if (box) box.innerHTML = '<div class="hint" style="margin:0;color:' + (rep.ok ? 'var(--accent2)' : 'var(--danger)') + '">' + esc(rep.text) + '</div>';
    toast(rep.text, rep.ok ? 'success' : 'error', 8000);
  } catch (e) {
    if (box) box.innerHTML = '<div class="hint" style="margin:0;color:var(--danger)">' + esc(e.message) + '</div>';
    toast('测试失败: ' + e.message, 'error');
  }
}

async function testProviderByName(name) {
  toast('连通测试中…', 'info');
  try {
    const d = await api('POST', '/providers/test', { name });
    const rep = pvTestReport(name, d.data);
    toast(rep.text, rep.ok ? 'success' : 'error', 9000);
  } catch (e) { toast('测试失败: ' + e.message, 'error'); }
}

async function refreshOcModels() {
  toast('正在同步 zen 免费目录…', 'info');
  try {
    const d = await api('POST', '/opencode/models/refresh');
    toast(d.message || '同步完成', 'success');
    loadModelIndex();
  } catch (e) { toast('同步失败: ' + e.message, 'error'); }
}

// 统计表渲染: 三个框分别展示 总量 / 按上游 / 按模型, 口径都是全部上游。
const STAT_HEAD = '<table><thead><tr><th style="text-align:left">口径</th><th>请求数</th><th>输入 tokens</th><th>输出 tokens</th><th>合计 tokens</th><th>压缩消耗</th><th>限流命中</th></tr></thead><tbody>';
function statRow(label, e) {
  const o = e || {};
  const pt = o.promptTokens || 0, ct = o.completionTokens || 0;
  return '<tr><td style="text-align:left">' + esc(label) + '</td><td>' + fmtNum(o.requests || 0) + '</td><td>' + fmtNum(pt) +
    '</td><td>' + fmtNum(ct) + '</td><td><strong>' + fmtNum(pt + ct) + '</strong></td><td>' + fmtNum(o.compaction || 0) +
    '</td><td>' + fmtNum(o.rateLimited || 0) + '</td></tr>';
}
function statBreakdown(byKey, note) {
  const keys = Object.keys(byKey || {});
  if (!keys.length) return '<div class="empty" style="padding:var(--sp-3)">暂无数据</div>';
  // 按合计 token 降序: 谁消耗多谁在前面
  keys.sort((a, b) => {
    const ea = byKey[a] || {}, eb = byKey[b] || {};
    return ((eb.promptTokens || 0) + (eb.completionTokens || 0)) - ((ea.promptTokens || 0) + (ea.completionTokens || 0));
  });
  const rows = keys.map(k => {
    const e = byKey[k] || {};
    const pt = e.promptTokens || 0, ct = e.completionTokens || 0;
    return '<tr><td style="text-align:left;font-family:var(--font-mono)">' + esc(k) + '</td><td>' + fmtNum(e.requests || 0) +
      '</td><td>' + fmtNum(pt) + '</td><td>' + fmtNum(ct) + '</td><td><strong>' + fmtNum(pt + ct) + '</strong></td></tr>';
  }).join('');
  return '<table><thead><tr><th style="text-align:left">' + esc(note || '名称') + '</th><th>请求数</th><th>输入 tokens</th><th>输出 tokens</th><th>合计</th></tr></thead><tbody>' +
    rows + '</tbody></table>';
}

async function loadOcStats() {
  try {
    const d = await api('GET', '/opencode/stats');
    const t = d.data.today || {}, s = d.data.total || {};
    _('statTotalsBox').innerHTML = STAT_HEAD +
      statRow('今日', t) + statRow('累计', s) + '</tbody></table>' +
      '<div class="hint" style="margin-top:var(--sp-2)">覆盖全部上游: Cline 账号池 / opencode / ClinePass / 通用 Provider。上游返回 usage 时精确，否则按请求体估算。</div>';
    _('statUpstreamBox').innerHTML = statBreakdown(t.byUpstream, '上游');
    _('statModelBox').innerHTML = statBreakdown(t.byModel, '模型');
  } catch (e) { console.warn('opencode 统计加载失败:', e && e.message); }
}

// ========== 初始化 ==========
loadStats();
loadAccounts();
loadKeys();
loadModelIndex();
loadConfig();
loadOcConfig();
setInterval(() => { loadStats(); }, 10000);
setInterval(() => { loadOcStats(); }, 15000);
setInterval(() => { if (_('tab-logs').style.display !== 'none') loadLogs(); }, 8000);
</script>
</body>
</html>`

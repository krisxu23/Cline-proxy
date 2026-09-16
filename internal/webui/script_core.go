package webui

// 面板脚本 scriptCore —— 核心工具: 主题、转义、格式化、toast、tab 导航、API 请求封装。
// 由 page_script.go 按分节横幅切开(2026-09-16): 单个 1900+ 行原始字符串难以评审,
// 切片后每片聚焦一个面板域。**拼接顺序在 webui.go 的 HTML 常量里**;
// 各片单独看都不是完整 JS(跨片引用是常态), 不要调整顺序。
// 注意: 原始字符串内禁止出现反引号(会提前终止字符串)。
const scriptCore = `<script>
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

`

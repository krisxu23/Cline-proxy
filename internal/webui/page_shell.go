package webui

// adminHTML 分段 1/4: 框架/样式/导航 + 仪表盘 + 供应商管理页。
// 由 admin_html.go 拆分而来(P2-21): 单文件近 2900 行的原始字符串难以评审,
// 按面板边界切成多段常量, 拼接结果与拆分前逐字节一致; 原始字符串内
// 仍然禁止出现反引号(会终止字符串)。
const htmlShell = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline 代理管理面板</title>
<style>
:root{
  --bg:#0b0e17;--bg2:#111527;--bg3:#1a2038;--panel:rgba(148,163,184,.055);--inset:rgba(2,6,23,.35);
  --border:rgba(148,163,184,.14);--border-strong:rgba(148,163,184,.28);
  --text:#e6edf6;--text2:#8b98b4;--text3:#5b6b89;
  --accent:#22d3ee;--accent2:#34d399;--amber:#f59e0b;--danger:#f87171;
  --accent-grad:linear-gradient(135deg,#22d3ee,#34d399);
  --glow:0 0 0 1px rgba(34,211,238,.25),0 0 24px rgba(34,211,238,.12);
  --status-active-bg:rgba(52,211,153,.14);--status-cooldown-bg:rgba(245,158,11,.14);
  --status-expired-bg:rgba(248,113,113,.14);
  --btn-primary-bg:linear-gradient(135deg,#0ea5e9,#22d3ee);--btn-primary-hover:linear-gradient(135deg,#0284c7,#0ea5e9);
  --btn-success-bg:linear-gradient(135deg,#059669,#34d399);--btn-success-hover:linear-gradient(135deg,#047857,#059669);
  --fs-xs:12px;--fs-sm:12.5px;--fs-base:13px;--fs-md:14px;--fs-lg:15px;--fs-title:21px;--fs-num:30px;--sp-1:4px;--sp-2:8px;--sp-3:12px;--sp-4:16px;--sp-5:24px;--radius:14px;--radius-sm:9px;--radius-pill:999px;--shadow-panel:0 10px 30px rgba(2,6,23,.35);--font-mono:var(--font-mono);
}
[data-theme="light"]{
  --bg:#f3f5fa;--bg2:#ffffff;--bg3:#eef1f7;--panel:rgba(255,255,255,.7);--inset:rgba(15,23,42,.05);
  --border:rgba(15,23,42,.12);--border-strong:rgba(15,23,42,.26);
  --text:#0f172a;--text2:#57617a;--text3:#67718a;
  --accent:#0891b2;--accent2:#059669;--amber:#b45309;--danger:#dc2626;
  --accent-grad:linear-gradient(135deg,#0891b2,#059669);
  --glow:0 0 0 1px rgba(8,145,178,.22),0 6px 24px rgba(8,145,178,.10);
  --status-active-bg:rgba(5,150,105,.12);--status-cooldown-bg:rgba(180,83,9,.12);
  --status-expired-bg:rgba(220,38,38,.10);
  --btn-primary-bg:linear-gradient(135deg,#0284c7,#06b6d4);--btn-primary-hover:linear-gradient(135deg,#0369a1,#0284c7);
  --btn-success-bg:linear-gradient(135deg,#059669,#10b981);--btn-success-hover:linear-gradient(135deg,#047857,#059669);
  --shadow-panel:0 10px 30px rgba(15,23,42,.12);
}
*{margin:0;padding:0;box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{font-family:'Inter','Segoe UI','PingFang SC','Microsoft YaHei',system-ui,sans-serif;background:var(--bg);color:var(--text);font-size:var(--fs-md);line-height:1.55;min-height:100vh}
body::before{content:'';position:fixed;inset:0;z-index:-1;background:
  radial-gradient(900px 500px at 85% -10%,rgba(34,211,238,.09),transparent 60%),
  radial-gradient(800px 500px at -10% 110%,rgba(52,211,153,.07),transparent 60%),
  var(--bg);pointer-events:none}
.mono,code{font-family:var(--font-mono);font-size:var(--fs-xs)}

/* ===== 布局 ===== */
.layout{display:flex;min-height:100vh}
.sidebar{width:236px;background:var(--panel);backdrop-filter:blur(14px);border-right:1px solid var(--border);padding:18px 10px;flex-shrink:0;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
.sidebar h1{font-size:var(--fs-lg);font-weight:700;padding:2px 10px 16px;border-bottom:1px solid var(--border);margin-bottom:10px;display:flex;align-items:center;gap:var(--sp-2);letter-spacing:.02em}
.sidebar h1 .logo{width:28px;height:28px;border-radius:8px;background:var(--accent-grad);display:inline-flex;align-items:center;justify-content:center;font-size:var(--fs-md);color:#04121a;box-shadow:var(--glow)}
.sidebar h1 .brand-name{background:var(--accent-grad);-webkit-background-clip:text;background-clip:text;-webkit-text-fill-color:transparent}
.sidebar h1 .theme-toggle{margin-left:auto;padding:var(--sp-1) var(--sp-2)}
.sidebar h1 span{color:var(--accent)}
.nav-item{display:flex;align-items:center;gap:10px;padding:9px 12px;border-radius:var(--radius-sm);cursor:pointer;color:var(--text2);transition:.18s;font-size:var(--fs-base);margin-bottom:2px;position:relative}
.nav-item .nav-ico{width:18px;text-align:center;font-size:var(--fs-lg);filter:saturate(.8)}
.nav-item:hover{color:var(--text);background:rgba(148,163,184,.09)}
.nav-item.active{color:var(--text);background:linear-gradient(90deg,rgba(34,211,238,.16),rgba(34,211,238,.05));font-weight:600}
.nav-item.active::before{content:'';position:absolute;left:-10px;top:20%;bottom:20%;width:3px;border-radius:3px;background:var(--accent-grad);box-shadow:0 0 12px rgba(34,211,238,.6)}
.sidebar-footer{margin-top:auto;padding:var(--sp-3) var(--sp-2) var(--sp-1);font-size:var(--fs-xs);color:var(--text3);border-top:1px solid var(--border)}
.sidebar-footer a{color:var(--accent);text-decoration:none}
.main{flex:1;padding:26px 34px 60px;min-width:0;max-width:1500px;margin:0 auto;width:100%}
h2{font-size:var(--fs-title);margin-bottom:18px;font-weight:700;letter-spacing:.01em}

/* ===== 卡片 ===== */
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:14px;margin-bottom:26px}
.card{background:var(--panel);backdrop-filter:blur(10px);border:1px solid var(--border);border-radius:var(--radius);padding:18px;position:relative;overflow:hidden;transition:.2s}
.card::after{content:'';position:absolute;inset:0;background:radial-gradient(220px 80px at 85% -10%,rgba(34,211,238,.12),transparent);pointer-events:none}
.card:hover{transform:translateY(-2px);border-color:var(--border-strong);box-shadow:var(--shadow-panel)}
.card .num{font-size:var(--fs-num);font-weight:700;font-variant-numeric:tabular-nums;letter-spacing:-.02em;color:var(--text)}
.card .label{font-size:var(--fs-xs);color:var(--text2);margin-top:5px;display:flex;align-items:center;gap:6px}
.card .label::before{content:'';width:7px;height:7px;border-radius:50%;background:var(--lab-c,var(--accent));box-shadow:0 0 10px var(--lab-c,var(--accent))}
.card .num.green{color:var(--accent2);--lab-c:var(--accent2)}
.card .num.red{color:var(--danger);--lab-c:var(--danger)}
.card .num.yellow{color:var(--amber);--lab-c:var(--amber)}
.card .num.blue{color:var(--accent);--lab-c:var(--accent)}
.cards .card{animation:rise .45s ease both}
.cards .card:nth-child(1){animation-delay:.02s}
.cards .card:nth-child(2){animation-delay:.08s}
.cards .card:nth-child(3){animation-delay:.14s}
.cards .card:nth-child(4){animation-delay:.2s}
@keyframes rise{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:none}}

/* ===== 区块 ===== */
.section{background:var(--panel);backdrop-filter:blur(10px);border:1px solid var(--border);border-radius:var(--radius);margin-bottom:22px;overflow:hidden;animation:rise .4s ease both}
.section-title{padding:13px 18px;border-bottom:1px solid var(--border);font-weight:600;font-size:var(--fs-md);display:flex;align-items:center;gap:var(--sp-2);flex-wrap:wrap;background:rgba(148,163,184,.03)}
.section-body{padding:18px}
.tabs{display:flex;border-bottom:1px solid var(--border);padding:0 var(--sp-2);gap:var(--sp-1);overflow-x:auto}
.tab{padding:11px 18px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:var(--fs-base);white-space:nowrap;transition:.15s;border-radius:8px 8px 0 0}
.tab:hover{color:var(--text);background:rgba(148,163,184,.07)}
.tab.active{color:var(--accent);border-bottom-color:var(--accent);font-weight:600}
.tab-content{display:none;padding:18px}
.tab-content.active{display:block;animation:rise .25s ease both}

/* ===== 表格 ===== */
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--border);font-size:var(--fs-base);white-space:nowrap}
th{color:var(--text2);font-weight:600;font-size:var(--fs-xs);text-transform:uppercase;letter-spacing:.06em}
tbody tr{transition:.12s}
tbody tr:hover{background:rgba(148,163,184,.06)}
tbody tr:last-child td{border-bottom:none}

/* ===== 状态徽章 ===== */
.status{display:inline-flex;align-items:center;gap:6px;padding:3px 10px;border-radius:var(--radius-pill);font-size:var(--fs-xs);font-weight:600}
.status.active{background:var(--status-active-bg);color:var(--accent2)}
.status.cooldown{background:var(--status-cooldown-bg);color:var(--amber)}
.status.expired{background:var(--status-expired-bg);color:var(--danger)}
.status-dot{width:7px;height:7px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--accent2);box-shadow:0 0 8px var(--accent2)}
.status-dot.cooldown{background:var(--amber);box-shadow:0 0 8px var(--amber)}
.status-dot.expired{background:var(--danger);box-shadow:0 0 8px var(--danger)}

/* ===== 按钮 ===== */
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;padding:7px 15px;border:1px solid var(--border);border-radius:var(--radius-sm);background:rgba(148,163,184,.08);color:var(--text);cursor:pointer;font-size:var(--fs-base);transition:.18s;text-decoration:none;font-family:inherit;white-space:nowrap}
.btn:hover{background:rgba(148,163,184,.16);border-color:var(--border-strong);transform:translateY(-1px)}
.btn:active{transform:none}
.btn-primary{background:var(--btn-primary-bg);border-color:transparent;color:#04121a;font-weight:600;box-shadow:0 4px 14px rgba(14,165,233,.28)}
.btn-primary:hover{background:var(--btn-primary-hover);box-shadow:0 6px 18px rgba(14,165,233,.38)}
.btn-success{background:var(--btn-success-bg);border-color:transparent;color:#04231a;font-weight:600;box-shadow:0 4px 14px rgba(5,150,105,.28)}
.btn-success:hover{background:var(--btn-success-hover)}
.btn-danger{border-color:rgba(248,113,113,.4);color:var(--danger);background:transparent}
.btn-danger:hover{background:rgba(248,113,113,.12);border-color:var(--danger)}
.btn-sm{padding:3px 10px;font-size:var(--fs-xs);border-radius:7px}

/* ===== 表单 ===== */
input,textarea,select{width:100%;padding:9px 13px;background:rgba(2,6,23,.4);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);font-size:var(--fs-base);font-family:inherit;transition:.15s}
/* 勾选框是原生小方块, 不吃上面的输入框宽度/背景 —— 否则会被拉满整行变成一大片灰 */
input[type=checkbox],input[type=radio]{appearance:auto;-webkit-appearance:checkbox;width:15px;height:15px;min-width:0;padding:0;margin:0 2px 0 0;border:0;background:none;box-shadow:none;flex:none;accent-color:var(--accent);cursor:pointer;vertical-align:middle}
[data-theme="light"] input,[data-theme="light"] textarea,[data-theme="light"] select{background:rgba(15,23,42,.03)}
input::placeholder,textarea::placeholder{color:var(--text3)}
input:focus,textarea:focus,select:focus{border-color:var(--accent);box-shadow:0 0 0 3px rgba(34,211,238,.15)}
/* 键盘聚焦给统一可见反馈; 输入框已有 box-shadow 指示, 这里再补一层 outline 兜底 */
:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
textarea{resize:vertical;min-height:84px;font-family:var(--font-mono);font-size:var(--fs-xs)}
select{cursor:pointer;appearance:none;background-image:linear-gradient(45deg,transparent 50%,var(--text2) 50%),linear-gradient(135deg,var(--text2) 50%,transparent 50%);background-position:calc(100% - 18px) 55%,calc(100% - 13px) 55%;background-size:5px 5px;background-repeat:no-repeat;padding-right:32px}
.form-row{display:flex;gap:14px;align-items:flex-end;margin-bottom:14px;flex-wrap:wrap}
.form-row .field{flex:1;min-width:180px}
.form-row .field label{display:block;font-size:var(--fs-xs);color:var(--text2);margin-bottom:6px;font-weight:500}
.form-actions{display:flex;gap:10px;margin-top:14px;flex-wrap:wrap}
.flex{display:flex;align-items:center;gap:var(--sp-2)}
.gap-4{gap:var(--sp-1)}
.text-right{text-align:right}
.mt-8{margin-top:var(--sp-2)}
.inline-flex{display:inline-flex;align-items:center;gap:6px}
.justify-between{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}
.hint{font-size:var(--fs-xs);color:var(--text2);margin-top:var(--sp-2);line-height:1.6}
.hint strong{color:var(--text)}

/* ===== Toast ===== */
.toast{position:fixed;top:22px;right:22px;padding:12px 20px;border-radius:var(--radius);color:#fff;z-index:9999;opacity:0;transform:translateY(-12px) scale(.97);transition:.3s cubic-bezier(.2,.9,.3,1.2);font-size:var(--fs-base);max-width:420px;backdrop-filter:blur(12px);border:1px solid rgba(255,255,255,.14);box-shadow:0 12px 40px rgba(2,6,23,.5);white-space:pre-line}
.toast.show{opacity:1;transform:none}
.toast.success{background:rgba(5,150,105,.92)}
.toast.error{background:rgba(220,38,38,.92)}
.toast.info{background:rgba(14,165,233,.92)}
.toast.warning{background:rgba(180,83,9,.92)}

/* ===== 杂项 ===== */
.loading{display:inline-block;width:14px;height:14px;border:2px solid var(--text3);border-top-color:var(--accent);border-radius:50%;animation:spin .7s linear infinite;vertical-align:-2px}
@keyframes spin{to{transform:rotate(360deg)}}
.empty{padding:30px;text-align:center;color:var(--text2)}
.empty-state{padding:44px 20px;text-align:center;color:var(--text2)}
.empty-state .icon{font-size:40px;margin-bottom:10px;display:block;opacity:.8}
.key-display{background:var(--inset);padding:9px 13px;border-radius:var(--radius-sm);border:1px solid var(--border);font-family:var(--font-mono);font-size:var(--fs-xs);word-break:break-all;cursor:pointer;transition:.15s}
.key-display:hover{background:rgba(34,211,238,.08);border-color:var(--accent)}
/* .copy-icon 现在渲染成 <button>, 需要清掉原生按钮样式, 视觉上保持原 span 观感 */
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px;border:0;background:none;font:inherit;line-height:1}
.copy-icon:hover{color:var(--text);background:var(--bg3)}
.model-tag{display:inline-block;padding:3px 10px;border-radius:6px;font-size:var(--fs-xs);background:rgba(148,163,184,.1);color:var(--text2);margin:2px;letter-spacing:.02em}
.model-tag.free{border:1px solid rgba(52,211,153,.5);color:var(--accent2);background:rgba(52,211,153,.08)}
.model-tag.pass{border:1px solid rgba(245,158,11,.5);color:var(--amber);background:rgba(245,158,11,.08)}
/* 搜索命中高亮: 文本先 esc 再插 <mark>, 不会引入 XSS */
mark{background:rgba(250,204,21,.4);color:inherit;border-radius:3px;padding:0 2px}

/* ===== 供应商模型卡片(可折叠) ===== */
.pi{border:1px solid var(--border);border-radius:var(--radius);background:var(--panel);backdrop-filter:blur(10px);margin-bottom:var(--sp-3);overflow:hidden;transition:border-color .18s,box-shadow .18s}
.pi:last-child{margin-bottom:0}
.pi:hover{border-color:var(--border-strong)}
.pi.open{border-color:var(--border-strong);box-shadow:var(--shadow-panel)}
.ps{min-height:54px;padding:10px 16px;display:flex;align-items:center;justify-content:space-between;gap:var(--sp-3);cursor:pointer;transition:background .15s}
.ps:hover{background:rgba(148,163,184,.07)}
.ps .pl{min-width:0;display:flex;align-items:center;gap:11px}
.ps .pl>div{min-width:0}
.pchev{width:14px;flex:none;text-align:center;color:var(--text3);font-size:9px;line-height:1;transition:transform .2s cubic-bezier(.4,0,.2,1)}
.pi.open .pchev{transform:rotate(90deg)}
.pav{width:36px;height:36px;flex:none;display:grid;place-items:center;border-radius:var(--radius-sm);border:1px solid var(--border);background:rgba(148,163,184,.09);font-weight:700;font-size:var(--fs-lg);font-family:'Inter',system-ui,sans-serif}
.pav.builtin{background:rgba(34,211,238,.12);border-color:rgba(34,211,238,.32);color:var(--accent)}
.ps h3{font-size:var(--fs-lg);font-weight:600;letter-spacing:.01em;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pmeta{margin-top:3px;display:flex;flex-wrap:wrap;align-items:center;font-size:var(--fs-xs);color:var(--text3);font-family:var(--font-mono);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pmeta>*{display:inline-flex;align-items:center;flex:none}
.pmeta>*:not(:last-child)::after{content:'·';margin:0 7px;color:var(--border-strong)}
.pbadge{display:inline-flex;align-items:center;gap:6px;padding:3px 10px;border-radius:var(--radius-pill);font-size:var(--fs-xs);font-weight:600;white-space:nowrap;font-family:inherit;max-width:260px;overflow:hidden;text-overflow:ellipsis}
.pbadge.pb-on{background:rgba(52,211,153,.13);color:var(--accent2);border:1px solid rgba(52,211,153,.35)}
.pbadge.pb-off{background:rgba(248,113,113,.12);color:var(--danger);border:1px solid rgba(248,113,113,.35)}
.pbadge.pb-info{background:rgba(34,211,238,.12);color:var(--accent);border:1px solid rgba(34,211,238,.32)}
.pd{display:none;padding:14px 16px 16px;border-top:1px solid var(--border);background:var(--inset)}
.pi.open .pd{display:block}
.pd-head{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:11px;flex-wrap:wrap}
.pd-head p{font-size:var(--fs-xs);color:var(--text2);margin:0}
.pacts{display:flex;gap:7px;flex-wrap:wrap}
.theme-toggle{display:inline-flex;align-items:center;gap:5px;padding:4px 9px;border:1px solid var(--border);border-radius:8px;background:rgba(148,163,184,.08);color:var(--text2);cursor:pointer;font-size:var(--fs-xs);transition:.15s;font-family:inherit}
.theme-toggle:hover{color:var(--text);background:rgba(148,163,184,.16)}
[data-theme="dark"] .theme-toggle .light-label{display:none}
body:not([data-theme="dark"]) .theme-toggle .dark-label{display:none}
.probe-pill{font-size:var(--fs-xs);color:var(--text3)}
.oauth-card{border:1px solid var(--border);border-radius:var(--radius);padding:var(--sp-4);background:rgba(148,163,184,.05);margin-top:14px}
.stat-mini{font-family:var(--font-mono);font-size:var(--fs-xs);color:var(--text2)}

/* ===== 响应式 ===== */
@media (max-width:980px){
  .layout{flex-direction:column}
  .sidebar{width:100%;height:auto;position:sticky;top:0;flex-direction:row;align-items:center;padding:10px 12px;overflow-x:auto;gap:var(--sp-1);z-index:50}
  .sidebar h1{display:flex;align-items:center;border-bottom:none;margin:0;padding:0 6px 0 0;gap:6px;font-size:var(--fs-base);white-space:nowrap}
  .sidebar h1 .brand-name{display:none}
  .sidebar .nav-item{padding:7px 11px;white-space:nowrap}
  .sidebar .nav-item.active::before{display:none}
  .sidebar .theme-toggle{margin-left:var(--sp-1)}
  .sidebar-footer{display:none}
  .main{padding:20px 16px 48px}
}
@media (max-width:560px){
  .cards{grid-template-columns:repeat(2,1fr)}
  h2{font-size:18px}
  .section-title{padding:11px 14px}
  .section-body{padding:14px}
  .form-row .field{min-width:100%}
}
</style>
</head>
<body>
<div class="layout">
<div class="sidebar">
<h1><span class="logo">⚡</span><span class="brand-name">Cline 代理</span><button class="theme-toggle" onclick="toggleTheme()" title="切换主题"><span class="icon" id="themeIcon">🌙</span><span class="light-label">浅色</span><span class="dark-label">深色</span></button></h1>
<div class="nav-item active" data-tab="dashboard"><span class="nav-ico">📊</span> 仪表盘</div>
<div class="nav-item" data-tab="accounts"><span class="nav-ico">👤</span> 账号管理</div>
<div class="nav-item" data-tab="models"><span class="nav-ico">🧠</span> 模型列表</div>
<div class="nav-item" data-tab="router"><span class="nav-ico">🔀</span> 自动路由</div>
<div class="nav-item" data-tab="settings"><span class="nav-ico">⚙️</span> 设置</div>
<div class="nav-item" data-tab="logs"><span class="nav-ico">📜</span> 请求日志</div>
<div class="sidebar-footer">
  <div>管理面板: <a href="/admin/">/admin/</a></div>
  <div>API 地址: <span id="footerApiAddr">http://127.0.0.1:3457</span></div>
</div>
</div>

<div class="main">

<div id="tab-dashboard" class="tab-panel">
<h2>📊 仪表盘</h2>

<div class="section">
  <div class="section-title">🏥 网关健康 <button class="btn btn-sm" style="margin-left:8px" onclick="loadHealth()">刷新</button></div>
  <div class="section-body">
    <div style="display:flex;align-items:center;gap:var(--sp-3);margin-bottom:8px">
      <div id="healthState" style="font-weight:700;font-size:var(--fs-lg)">加载中…</div>
      <div id="healthScore" class="mono" style="color:var(--text3);font-size:var(--fs-sm)"></div>
    </div>
    <div id="healthSignals" style="display:flex;gap:var(--sp-2);flex-wrap:wrap;margin-bottom:8px;font-size:var(--fs-xs)"></div>
    <div id="healthIssues"></div>
  </div>
</div>

<div class="cards">
  <div class="card"><div class="num blue" id="statTotal">-</div><div class="label">账号总数</div></div>
  <div class="card"><div class="num green" id="statActive">-</div><div class="label">活跃</div></div>
  <div class="card"><div class="num yellow" id="statCooldown">-</div><div class="label">冷却</div></div>
  <div class="card"><div class="num red" id="statExpired">-</div><div class="label">已过期</div></div>
</div>
<div class="section">
  <div class="section-title">🔗 网关入口</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>客户端 API 地址（Base URL）</label>
        <div style="display:flex;gap:6px">
          <input type="text" id="dashApiBase" readonly onclick="this.select()" style="font-family:var(--font-mono)">
          <button class="btn btn-sm" onclick="copyText(_('dashApiBase').value)" style="flex:none">📋</button>
        </div>
      </div>
      <div class="field"><label>监听地址</label><input type="text" id="dashListenAddr" disabled></div>
    </div>
    <div class="form-row">
      <div class="field"><label>引擎版本</label><input type="text" id="dashVersion" disabled></div>
      <div class="field"><label>出口模式</label><input type="text" id="dashExitMode" disabled></div>
    </div>
    <div class="hint">客户端把 Base URL 指向上面的地址；访问密钥在「设置 → 🔑 API 密钥管理」里生成，出口模式在「设置 → 🌐 出口代理与节点」切换。</div>
  </div>
</div>
<div class="section">
  <div class="section-title">📋 快捷操作</div>
  <div class="section-body" style="display:flex;gap:10px;flex-wrap:wrap">
    <button class="btn btn-primary" onclick="switchTab('accounts')">➕ 添加账号</button>
    <button class="btn" onclick="refreshAllTokens()">🔄 刷新全部 Token</button>
    <button class="btn" onclick="document.getElementById('fileInput').click()">📄 从文件导入</button>
    <input type="file" id="fileInput" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    <button class="btn" onclick="switchTab('models')">🧠 模型列表</button>
    <button class="btn" onclick="switchTab('settings')">⚙️ 设置</button>
  </div>
</div>
<div class="section">
  <div class="section-title">📈 Token 统计（全部上游）</div>
  <div class="section-body"><div class="table-wrap"><div id="statTotalsBox"></div></div></div>
</div>
<div class="section">
  <div class="section-title">🌐 按上游分布</div>
  <div class="section-body"><div class="table-wrap"><div id="statUpstreamBox"></div></div></div>
</div>
<div class="section">
  <div class="section-title">🧠 按模型分布</div>
  <div class="section-body"><div class="table-wrap"><div id="statModelBox"></div></div></div>
</div>
</div>

<div id="tab-accounts" class="tab-panel" style="display:none">
  <div class="flex justify-between" style="margin-bottom:var(--sp-4)">
  <h2>👤 账号管理</h2>
  <div style="display:flex;gap:var(--sp-2)">
    <button class="btn btn-sm" onclick="exportAccounts()">📤 导出账号</button>
    <button class="btn btn-primary btn-sm" onclick="switchTab('accounts')">➕ 添加</button>
    <button class="btn btn-sm" onclick="loadAccounts()">🔄 刷新</button>
  </div>
</div>
<div class="hint" style="margin:-var(--sp-1) 0 var(--sp-4);padding:11px 14px;border:1px solid var(--border);border-radius:var(--radius-sm);background:rgba(148,163,184,.05)">
  ℹ️ <strong style="color:var(--text)">Tokens</strong>为本代理本地统计（输入+输出，上游返回 usage 时精确，否则按请求体估算），用于估算离官方限流还有多远；⚡ 测试按钮发起真实探测请求；↻ 重置按钮会<strong style="color:var(--text)">探测上游限流状态</strong>：若上游仍限流则保持冷却并提示恢复时间，探测通过才解除冷却并重置今日统计。
</div>
<div class="section">
  <div class="section-body" style="padding:6px">
    <div class="table-wrap">
    <table>
      <thead>
        <tr><th>邮箱</th><th>状态</th><th title="本代理本地统计，不代表官方免费额度">今日/累计 Tokens</th><th>最后使用</th><th>创建时间</th><th>操作</th></tr>
      </thead>
      <tbody id="accountTableBody">
        <tr><td colspan="6" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    </div>
  </div>
</div>

<div class="section" style="margin-top:26px">
  <div class="section-title">🔵 Cline 账号池</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>账号文件保存地址</label>
        <div style="display:flex;gap:6px">
          <input type="text" id="acctPoolPath" readonly onclick="this.select()" style="font-family:var(--font-mono)">
          <button class="btn btn-sm" onclick="copyText(_('acctPoolPath').value)" style="flex:none">📋</button>
        </div>
      </div>
      <div class="field"><label>账号轮询策略</label>
        <select id="acctStrategy" onchange="savePoolStrategy()">
          <option value="round_robin">轮询 (round_robin)</option>
          <option value="fill">填满 (fill)</option>
          <option value="random">随机 (random)</option>
        </select>
      </div>
    </div>
    <div class="hint" id="acctPoolHint">账号池按上面的策略在可用账号之间轮换；文件为 JSON 格式，可随时代备份或迁移。</div>
  </div>
</div>

<h2 style="margin-top:26px">📥 导入账号</h2>
<div class="section">
  <div class="tabs" id="importTabs">
    <div class="tab active" data-tab="oauth">🔑 OAuth 浏览器登录</div>
    <div class="tab" data-tab="token">✏️ 手动输入 Token</div>
    <div class="tab" data-tab="batch">📦 批量导入</div>
  </div>

  <div id="import-oauth" class="tab-content active">
    <p class="hint">通过浏览器完成 OAuth 认证，支持 Google/GitHub/邮箱登录，自动获取 refreshToken。</p>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="startOAuth()" id="oauthBtn">🚀 开始 OAuth 登录</button>
    </div>
    <div id="oauthProgress" style="display:none;margin-top:14px" class="oauth-card">
      <div style="display:flex;align-items:center;gap:14px">
        <div class="loading"></div>
        <div>
          <div style="font-weight:600" id="oauthStatus">等待浏览器授权...</div>
          <div class="hint">
            打开 <a href="#" id="oauthUrl" target="_blank" style="color:var(--accent)"></a>
            并输入代码: <strong style="color:var(--accent);font-size:var(--fs-lg);letter-spacing:2px" id="oauthUserCode"></strong>
          </div>
        </div>
      </div>
    </div>
    <div id="oauthResult" style="display:none;margin-top:14px"></div>
  </div>

  <div id="import-token" class="tab-content">
    <p class="hint">输入已有的 Cline refreshToken，系统会自动验证并加入池。</p>
    <div class="form-row">
      <div class="field">
        <label>Refresh Token *</label>
        <input type="text" id="tokenInput" placeholder="粘贴 refreshToken" style="font-family:var(--font-mono)">
      </div>
      <div class="field">
        <label>邮箱（可选，留空自动生成）</label>
        <input type="text" id="tokenEmail" placeholder="user@example.com">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="addByToken()">➕ 添加账号</button>
    </div>
    <div id="tokenResult" style="margin-top:var(--sp-2)"></div>
  </div>

  <div id="import-batch" class="tab-content">
    <p class="hint">批量导入多个账号。支持 JSON 数组或每行一个 token。</p>
    <div class="form-row">
      <div class="field">
        <label>JSON 数组格式：[{"refreshToken":"...","email":"..."}]</label>
        <textarea id="batchInput" placeholder='[{"refreshToken":"xxx","email":"u1@x.com"},{"refreshToken":"yyy","email":"u2@x.com"}]'></textarea>
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="batchImport()">📦 导入全部</button>
      <button class="btn" onclick="document.getElementById('fileInput2').click()">📄 选择文件</button>
      <input type="file" id="fileInput2" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    </div>
    <div id="batchResult" style="margin-top:var(--sp-2)"></div>
  </div>
</div>

<div class="section" style="margin-top:26px">
  <div class="section-title">🌐 opencode 上游配置</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>启用 opencode 上游</label>
        <select id="ocEnabled"><option value="true">开启</option><option value="false">关闭</option></select>
      </div>
      <div class="field"><label>API Key</label><input type="text" id="ocKey" placeholder="public"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>API 端点（每行一个，第一个为主端点，其余为 CDN 镜像，重试自动轮换）</label>
        <textarea id="ocBaseURLs" rows="4" placeholder="https://opencode.ai/zen/v1"></textarea>
      </div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存上游配置</button></div>
  </div>
</div>
</div>

<div id="tab-models" class="tab-panel" style="display:none">
<h2>🧠 模型列表</h2>
<div class="hint" style="margin:-var(--sp-1) 0 var(--sp-4);padding:11px 14px;border:1px solid var(--border);border-radius:var(--radius-sm);background:rgba(148,163,184,.05)">
  ℹ️ 前缀仅用于区分来源: <strong style="color:var(--text)">cline/</strong> 走 Cline 账号池, <strong style="color:var(--text)">zen/</strong> 走 opencode 上游, <strong style="color:var(--text)">provider名:</strong> 走对应的通用 Provider。请求时携带带前缀的名称, 网关会自动还原为上游原始模型名。点击供应商卡片即可展开/收起它的全部模型。
</div>
<div class="section">
  <div class="section-title">⚙️ 默认模型
    <span class="probe-pill" style="font-weight:normal;margin-left:auto" id="defModelHint">未指定模型时使用</span>
  </div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Cline 池默认模型</label>
        <div style="display:flex;gap:6px;align-items:center">
          <select id="settingDefModel" style="flex:1;font-family:var(--font-mono)"></select>
          <button class="btn btn-sm btn-primary" onclick="saveDefaultModel()">💾 保存</button>
        </div>
      </div>
    </div>
  </div>
</div>
<div class="section">
  <div class="section-title">🔌 供应商与模型
    <span class="probe-pill" style="font-weight:normal" id="modelIndexSummary">加载中...</span>
    <span style="margin-left:auto;display:flex;align-items:center;gap:var(--sp-2);flex-wrap:wrap">
      <input type="text" id="modelSearchBox" placeholder="搜索供应商或模型" oninput="filterModelIndex(this.value)" style="width:212px">
      <button class="btn btn-sm" onclick="loadModelIndex()">🔄 刷新</button>
    </span>
  </div>
  <div class="section-body" style="padding:14px">
    <div id="modelIndex">加载中...</div>
    <div id="modelSearchEmpty" style="display:none;padding:28px 14px;text-align:center;color:var(--text3);font-size:var(--fs-base)">🔍 没有匹配的供应商或模型，换个关键词试试</div>
  </div>
</div>
<div class="section">
  <div class="section-title">➕ 添加通用 Provider（OpenAI 兼容上游）</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 14px">只需要 <strong style="color:var(--text)">Provider 名 + API 地址 + API Key</strong>，保存后会自动拉取模型目录。Google Gemini 只需填 <code>https://generativelanguage.googleapis.com</code>，端点后缀与鉴权方言由程序自动补齐。</p>
    <div class="form-row">
      <div class="field"><label>Provider 名 *</label><input type="text" id="pvName" placeholder="openrouter / gemini / tokenrouter / bai"></div>
      <div class="field"><label>API 地址 *</label><input type="text" id="pvBaseUrl" placeholder="https://openrouter.ai/api/v1"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>API Key *</label><input type="password" id="pvKey" placeholder="sk-..."></div>
      <div class="field"><label>模型目录</label>
        <label style="display:flex;align-items:center;gap:var(--sp-2);font-weight:normal"><input type="checkbox" id="pvCatalog" checked style="width:auto;min-width:0"> 拉取 /models 目录（关闭则只用下方手填模型）</label>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>模型（每行一个，保存为显式启用；目录拉取后也可在下方列表逐个勾选）</label>
        <textarea id="pvModels" rows="3" placeholder="glm-5.3-flash"></textarea></div>
      <div class="field"><label>连通测试模型（留空用第一个可用模型）</label>
        <input type="text" id="pvTestModel" placeholder="glm-5.3-flash"></div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="saveProvider()">💾 保存并拉取模型</button>
      <button class="btn" onclick="testProvider()">🔍 连通测试</button>
      <button class="btn" onclick="refreshProviderCatalog()">🔄 刷新目录</button>
      <button class="btn" onclick="resetProviderForm()">✖ 清空表单</button>
    </div>
    <div id="pvResult" style="margin-top:10px"></div>
  </div>
</div>
</div>
`

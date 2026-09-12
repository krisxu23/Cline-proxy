package app

const adminHTML = `<!DOCTYPE html>
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
  --radius:14px;--radius-sm:9px;
}
[data-theme="light"]{
  --bg:#f3f5fa;--bg2:#ffffff;--bg3:#eef1f7;--panel:rgba(255,255,255,.7);--inset:rgba(15,23,42,.05);
  --border:rgba(15,23,42,.12);--border-strong:rgba(15,23,42,.26);
  --text:#0f172a;--text2:#57617a;--text3:#8a94ab;
  --accent:#0891b2;--accent2:#059669;--amber:#b45309;--danger:#dc2626;
  --accent-grad:linear-gradient(135deg,#0891b2,#059669);
  --glow:0 0 0 1px rgba(8,145,178,.22),0 6px 24px rgba(8,145,178,.10);
  --status-active-bg:rgba(5,150,105,.12);--status-cooldown-bg:rgba(180,83,9,.12);
  --status-expired-bg:rgba(220,38,38,.10);
  --btn-primary-bg:linear-gradient(135deg,#0284c7,#06b6d4);--btn-primary-hover:linear-gradient(135deg,#0369a1,#0284c7);
  --btn-success-bg:linear-gradient(135deg,#059669,#10b981);--btn-success-hover:linear-gradient(135deg,#047857,#059669);
}
*{margin:0;padding:0;box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{font-family:'Inter','Segoe UI','PingFang SC','Microsoft YaHei',system-ui,sans-serif;background:var(--bg);color:var(--text);font-size:14px;line-height:1.55;min-height:100vh}
body::before{content:'';position:fixed;inset:0;z-index:-1;background:
  radial-gradient(900px 500px at 85% -10%,rgba(34,211,238,.09),transparent 60%),
  radial-gradient(800px 500px at -10% 110%,rgba(52,211,153,.07),transparent 60%),
  var(--bg);pointer-events:none}
.mono,code{font-family:'JetBrains Mono','Cascadia Code','Fira Code',Consolas,monospace;font-size:12px}

/* ===== 布局 ===== */
.layout{display:flex;min-height:100vh}
.sidebar{width:236px;background:var(--panel);backdrop-filter:blur(14px);border-right:1px solid var(--border);padding:18px 10px;flex-shrink:0;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
.sidebar h1{font-size:15px;font-weight:700;padding:2px 10px 16px;border-bottom:1px solid var(--border);margin-bottom:10px;display:flex;align-items:center;gap:8px;letter-spacing:.02em}
.sidebar h1 .logo{width:28px;height:28px;border-radius:8px;background:var(--accent-grad);display:inline-flex;align-items:center;justify-content:center;font-size:14px;color:#04121a;box-shadow:var(--glow)}
.sidebar h1 .brand-name{background:var(--accent-grad);-webkit-background-clip:text;background-clip:text;-webkit-text-fill-color:transparent}
.sidebar h1 .theme-toggle{margin-left:auto;padding:4px 8px}
.sidebar h1 span{color:var(--accent)}
.nav-item{display:flex;align-items:center;gap:10px;padding:9px 12px;border-radius:10px;cursor:pointer;color:var(--text2);transition:.18s;font-size:13.5px;margin-bottom:2px;position:relative}
.nav-item .nav-ico{width:18px;text-align:center;font-size:15px;filter:saturate(.8)}
.nav-item:hover{color:var(--text);background:rgba(148,163,184,.09)}
.nav-item.active{color:var(--text);background:linear-gradient(90deg,rgba(34,211,238,.16),rgba(34,211,238,.05));font-weight:600}
.nav-item.active::before{content:'';position:absolute;left:-10px;top:20%;bottom:20%;width:3px;border-radius:3px;background:var(--accent-grad);box-shadow:0 0 12px rgba(34,211,238,.6)}
.sidebar-footer{margin-top:auto;padding:12px 8px 4px;font-size:11.5px;color:var(--text3);border-top:1px solid var(--border)}
.sidebar-footer a{color:var(--accent);text-decoration:none}
.main{flex:1;padding:26px 34px 60px;min-width:0;max-width:1500px;margin:0 auto;width:100%}
h2{font-size:21px;margin-bottom:18px;font-weight:700;letter-spacing:.01em}

/* ===== 卡片 ===== */
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:14px;margin-bottom:26px}
.card{background:var(--panel);backdrop-filter:blur(10px);border:1px solid var(--border);border-radius:var(--radius);padding:18px;position:relative;overflow:hidden;transition:.2s}
.card::after{content:'';position:absolute;inset:0;background:radial-gradient(220px 80px at 85% -10%,rgba(34,211,238,.12),transparent);pointer-events:none}
.card:hover{transform:translateY(-2px);border-color:var(--border-strong);box-shadow:0 10px 30px rgba(2,6,23,.35)}
.card .num{font-size:30px;font-weight:700;font-variant-numeric:tabular-nums;letter-spacing:-.02em;color:var(--text)}
.card .label{font-size:12px;color:var(--text2);margin-top:5px;display:flex;align-items:center;gap:6px}
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
.section-title{padding:13px 18px;border-bottom:1px solid var(--border);font-weight:600;font-size:14px;display:flex;align-items:center;gap:8px;background:rgba(148,163,184,.03)}
.section-body{padding:18px}
.tabs{display:flex;border-bottom:1px solid var(--border);padding:0 8px;gap:4px;overflow-x:auto}
.tab{padding:11px 18px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:13px;white-space:nowrap;transition:.15s;border-radius:8px 8px 0 0}
.tab:hover{color:var(--text);background:rgba(148,163,184,.07)}
.tab.active{color:var(--accent);border-bottom-color:var(--accent);font-weight:600}
.tab-content{display:none;padding:18px}
.tab-content.active{display:block;animation:rise .25s ease both}

/* ===== 表格 ===== */
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--border);font-size:13px;white-space:nowrap}
th{color:var(--text2);font-weight:600;font-size:11.5px;text-transform:uppercase;letter-spacing:.06em}
tbody tr{transition:.12s}
tbody tr:hover{background:rgba(148,163,184,.06)}
tbody tr:last-child td{border-bottom:none}

/* ===== 状态徽章 ===== */
.status{display:inline-flex;align-items:center;gap:6px;padding:3px 10px;border-radius:999px;font-size:12px;font-weight:600}
.status.active{background:var(--status-active-bg);color:var(--accent2)}
.status.cooldown{background:var(--status-cooldown-bg);color:var(--amber)}
.status.expired{background:var(--status-expired-bg);color:var(--danger)}
.status-dot{width:7px;height:7px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--accent2);box-shadow:0 0 8px var(--accent2)}
.status-dot.cooldown{background:var(--amber);box-shadow:0 0 8px var(--amber)}
.status-dot.expired{background:var(--danger);box-shadow:0 0 8px var(--danger)}

/* ===== 按钮 ===== */
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;padding:7px 15px;border:1px solid var(--border);border-radius:var(--radius-sm);background:rgba(148,163,184,.08);color:var(--text);cursor:pointer;font-size:13px;transition:.18s;text-decoration:none;font-family:inherit;white-space:nowrap}
.btn:hover{background:rgba(148,163,184,.16);border-color:var(--border-strong);transform:translateY(-1px)}
.btn:active{transform:none}
.btn-primary{background:var(--btn-primary-bg);border-color:transparent;color:#04121a;font-weight:600;box-shadow:0 4px 14px rgba(14,165,233,.28)}
.btn-primary:hover{background:var(--btn-primary-hover);box-shadow:0 6px 18px rgba(14,165,233,.38)}
.btn-success{background:var(--btn-success-bg);border-color:transparent;color:#04231a;font-weight:600;box-shadow:0 4px 14px rgba(5,150,105,.28)}
.btn-success:hover{background:var(--btn-success-hover)}
.btn-danger{border-color:rgba(248,113,113,.4);color:var(--danger);background:transparent}
.btn-danger:hover{background:rgba(248,113,113,.12);border-color:var(--danger)}
.btn-sm{padding:3px 10px;font-size:12px;border-radius:7px}

/* ===== 表单 ===== */
input,textarea,select{width:100%;padding:9px 13px;background:rgba(2,6,23,.4);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);font-size:13px;font-family:inherit;transition:.15s}
/* 勾选框是原生小方块, 不吃上面的输入框宽度/背景 —— 否则会被拉满整行变成一大片灰 */
input[type=checkbox],input[type=radio]{appearance:auto;-webkit-appearance:checkbox;width:15px;height:15px;min-width:0;padding:0;margin:0 2px 0 0;border:0;background:none;box-shadow:none;flex:none;accent-color:var(--accent);cursor:pointer;vertical-align:middle}
[data-theme="light"] input,[data-theme="light"] textarea,[data-theme="light"] select{background:rgba(15,23,42,.03)}
input::placeholder,textarea::placeholder{color:var(--text3)}
input:focus,textarea:focus,select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(34,211,238,.15)}
textarea{resize:vertical;min-height:84px;font-family:'JetBrains Mono','Cascadia Code',Consolas,monospace;font-size:12px}
select{cursor:pointer;appearance:none;background-image:linear-gradient(45deg,transparent 50%,var(--text2) 50%),linear-gradient(135deg,var(--text2) 50%,transparent 50%);background-position:calc(100% - 18px) 55%,calc(100% - 13px) 55%;background-size:5px 5px;background-repeat:no-repeat;padding-right:32px}
.form-row{display:flex;gap:14px;align-items:flex-end;margin-bottom:14px;flex-wrap:wrap}
.form-row .field{flex:1;min-width:180px}
.form-row .field label{display:block;font-size:12px;color:var(--text2);margin-bottom:6px;font-weight:500}
.form-actions{display:flex;gap:10px;margin-top:14px;flex-wrap:wrap}
.flex{display:flex;align-items:center;gap:8px}
.gap-4{gap:4px}
.text-right{text-align:right}
.mt-8{margin-top:8px}
.inline-flex{display:inline-flex;align-items:center;gap:6px}
.justify-between{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}
.hint{font-size:12px;color:var(--text2);margin-top:8px;line-height:1.6}
.hint strong{color:var(--text)}

/* ===== Toast ===== */
.toast{position:fixed;top:22px;right:22px;padding:12px 20px;border-radius:12px;color:#fff;z-index:9999;opacity:0;transform:translateY(-12px) scale(.97);transition:.3s cubic-bezier(.2,.9,.3,1.2);font-size:13px;max-width:420px;backdrop-filter:blur(12px);border:1px solid rgba(255,255,255,.14);box-shadow:0 12px 40px rgba(2,6,23,.5);white-space:pre-line}
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
.key-display{background:var(--inset);padding:9px 13px;border-radius:var(--radius-sm);border:1px solid var(--border);font-family:'JetBrains Mono','Cascadia Code',Consolas,monospace;font-size:12px;word-break:break-all;cursor:pointer;transition:.15s}
.key-display:hover{background:rgba(34,211,238,.08);border-color:var(--accent)}
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px}
.copy-icon:hover{color:var(--text);background:var(--bg3)}
.model-tag{display:inline-block;padding:2px 9px;border-radius:6px;font-size:11px;background:rgba(148,163,184,.1);color:var(--text2);margin:2px;letter-spacing:.02em}
.model-tag.free{border:1px solid rgba(52,211,153,.5);color:var(--accent2);background:rgba(52,211,153,.08)}
.model-tag.pass{border:1px solid rgba(245,158,11,.5);color:var(--amber);background:rgba(245,158,11,.08)}
.theme-toggle{display:inline-flex;align-items:center;gap:5px;padding:4px 9px;border:1px solid var(--border);border-radius:8px;background:rgba(148,163,184,.08);color:var(--text2);cursor:pointer;font-size:12px;transition:.15s;font-family:inherit}
.theme-toggle:hover{color:var(--text);background:rgba(148,163,184,.16)}
[data-theme="dark"] .theme-toggle .light-label{display:none}
body:not([data-theme="dark"]) .theme-toggle .dark-label{display:none}
.probe-pill{font-size:11px;color:var(--text3)}
.oauth-card{border:1px solid var(--border);border-radius:12px;padding:16px;background:rgba(148,163,184,.05);margin-top:14px}
.stat-mini{font-family:'JetBrains Mono',Consolas,monospace;font-size:12px;color:var(--text2)}

/* ===== 响应式 ===== */
@media (max-width:980px){
  .layout{flex-direction:column}
  .sidebar{width:100%;height:auto;position:sticky;top:0;flex-direction:row;align-items:center;padding:10px 12px;overflow-x:auto;gap:4px;z-index:50}
  .sidebar h1{display:flex;align-items:center;border-bottom:none;margin:0;padding:0 6px 0 0;gap:6px;font-size:13px;white-space:nowrap}
  .sidebar h1 .brand-name{display:none}
  .sidebar .nav-item{padding:7px 11px;white-space:nowrap}
  .sidebar .nav-item.active::before{display:none}
  .sidebar .theme-toggle{margin-left:4px}
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
          <input type="text" id="dashApiBase" readonly onclick="this.select()" style="font-family:'JetBrains Mono',Consolas,monospace">
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
  <div class="flex justify-between" style="margin-bottom:16px">
  <h2>👤 账号管理</h2>
  <div style="display:flex;gap:8px">
    <button class="btn btn-sm" onclick="exportAccounts()">📤 导出账号</button>
    <button class="btn btn-primary btn-sm" onclick="switchTab('accounts')">➕ 添加</button>
    <button class="btn btn-sm" onclick="loadAccounts()">🔄 刷新</button>
  </div>
</div>
<div class="hint" style="margin:-4px 0 16px;padding:11px 14px;border:1px solid var(--border);border-radius:10px;background:rgba(148,163,184,.05)">
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
          <input type="text" id="acctPoolPath" readonly onclick="this.select()" style="font-family:'JetBrains Mono',Consolas,monospace">
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
            并输入代码: <strong style="color:var(--accent);font-size:15px;letter-spacing:2px" id="oauthUserCode"></strong>
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
        <input type="text" id="tokenInput" placeholder="粘贴 refreshToken" style="font-family:'JetBrains Mono',Consolas,monospace">
      </div>
      <div class="field">
        <label>邮箱（可选，留空自动生成）</label>
        <input type="text" id="tokenEmail" placeholder="user@example.com">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="addByToken()">➕ 添加账号</button>
    </div>
    <div id="tokenResult" style="margin-top:8px"></div>
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
    <div id="batchResult" style="margin-top:8px"></div>
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
<div class="hint" style="margin:-4px 0 16px;padding:11px 14px;border:1px solid var(--border);border-radius:10px;background:rgba(148,163,184,.05)">
  ℹ️ 前缀仅用于区分来源: <strong style="color:var(--text)">cline/</strong> 走 Cline 账号池, <strong style="color:var(--text)">zen/</strong> 走 opencode 上游, <strong style="color:var(--text)">provider名:</strong> 走对应的通用 Provider。请求时携带带前缀的名称, 网关会自动还原为上游原始模型名。
</div>
<div class="section">
  <div class="section-title">⚙️ 默认模型
    <span class="probe-pill" style="font-weight:normal;margin-left:auto" id="defModelHint">未指定模型时使用</span>
  </div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Cline 池默认模型</label>
        <div style="display:flex;gap:6px;align-items:center">
          <select id="settingDefModel" style="flex:1;font-family:'JetBrains Mono',Consolas,monospace"></select>
          <button class="btn btn-sm btn-primary" onclick="saveDefaultModel()">💾 保存</button>
        </div>
      </div>
    </div>
  </div>
</div>
<div class="section">
  <div class="section-title">🟣 Cline 模型 <span id="modelsProbeInfo" class="probe-pill" style="font-weight:normal"></span>
    <button class="btn btn-sm" onclick="refreshModels()" style="margin-left:auto">🔄 同步</button>
  </div>
  <div class="section-body" style="padding:6px"><div id="modelsList" style="padding:12px">加载中...</div></div>
</div>
<div class="section">
  <div class="section-title">🌐 opencode 模型
    <button class="btn btn-sm" onclick="refreshOcModels()" style="margin-left:auto">🔄 同步</button>
  </div>
  <div class="section-body" style="padding:6px"><div id="ocModelsList" style="padding:12px">加载中...</div></div>
</div>
<div class="section">
  <div class="section-title">🔌 通用 Provider 模型
    <button class="btn btn-sm" onclick="loadProviderModels()" style="margin-left:auto">🔄 刷新</button>
  </div>
  <div class="section-body" style="padding:6px"><div id="pvModelsList" style="padding:12px">加载中...</div></div>
</div>
</div>

<div id="tab-router" class="tab-panel" style="display:none">
<h2>🔀 自动路由</h2>

<div class="section">
  <div class="section-title">🏷️ 自动路由模型名</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 14px">
      客户端把 <strong style="color:var(--text)">模型名</strong>填成下面这个名字，网关就会从你勾选的模型里按顺序逐个尝试：
      某一站失败就按错误类型冷却它、换下一站，全链失败才返回最后一站的错误。
    </p>
    <div class="form-row">
      <div class="field" style="flex:1;min-width:260px"><label>模型名（可自定义）</label>
        <div style="display:flex;gap:8px">
          <input id="arAlias" placeholder="auto-router" oninput="renderRouterExample()">
          <button type="button" class="btn btn-primary" style="width:auto;white-space:nowrap" onclick="saveRouter()">💾 保存</button>
        </div>
      </div>
      <div class="field" style="flex:1.4;min-width:300px"><label>客户端调用示例</label>
        <input id="arExample" readonly onclick="this.select()" style="font-family:'JetBrains Mono',Consolas,monospace">
      </div>
    </div>
    <p class="hint" style="margin:0">改名后点「保存」即可：旧名字下的勾选会自动迁移到新名字，已发布给客户端的旧模型名仍然可用（兼容保留）。</p>
  </div>
</div>

<div class="section">
  <div class="section-title">🏭 网关已加入的供应商
    <span style="margin-left:auto;display:flex;gap:8px">
      <button type="button" class="btn btn-sm" onclick="routerSelectAll(true)">全选</button>
      <button type="button" class="btn btn-sm" onclick="routerSelectAll(false)">全不选</button>
      <button type="button" class="btn btn-sm" onclick="refreshRouterCatalogs()">🔄 刷新全部目录</button>
    </span>
  </div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 12px">
      下面是网关探测到的全部供应商。勾选要参与自动路由的供应商，其模型会出现在下一节供逐项勾选。
    </p>
    <div id="arProviderList" style="display:flex;flex-direction:column;gap:8px">加载中...</div>
  </div>
</div>

<div class="section">
  <div class="section-title">🧩 参与自动路由的模型</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 12px">
      只有勾选的模型会参与。若一个都不勾，自动路由会回落到「全部供应商的全部免费模型」。
    </p>
    <div id="arModelList">加载中...</div>
    <div class="hint" id="arSelectionWarn" style="margin-top:8px;font-size:12px;color:var(--text3)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">💾 保存与校验</div>
  <div class="section-body">
    <div class="form-actions">
      <button class="btn btn-primary" onclick="saveRouter()">💾 保存设置</button>
      <button class="btn" onclick="validateRouter()">🔍 校验勾选</button>
      <button class="btn" onclick="loadRouter()">↺ 放弃改动</button>
    </div>
    <div id="arResult" style="margin-top:10px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">📈 当前实际顺序</div>
  <div class="section-body">
    <div class="table-wrap">
      <table>
        <thead><tr><th style="width:180px">路由别名</th><th>当前实际顺序</th></tr></thead>
        <tbody id="arChainBody"><tr><td colspan="2" class="empty">加载中...</td></tr></tbody>
      </table>
    </div>
    <div class="hint" style="margin-top:6px;font-size:12px;color:var(--text3)">
      带删除线的站当前不可用（鼠标悬停看原因），会被自动跳过。
    </div>
  </div>
</div>

<div class="section">
  <div class="section-title">📊 今日用量</div>
  <div class="section-body">
    <div class="table-wrap">
      <table>
        <thead><tr><th style="width:220px">候选</th><th style="width:90px">请求</th><th style="width:70px">成功</th><th style="width:70px">失败</th><th>限额</th></tr></thead>
        <tbody id="arUsageBody"><tr><td colspan="5" class="empty">加载中...</td></tr></tbody>
      </table>
    </div>
    <div class="hint" id="arUsageInfo" style="margin-top:6px;font-size:12px;color:var(--text3)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🌱 自动发现免费模型
    <span style="margin-left:auto;display:flex;gap:8px">
      <button type="button" class="btn btn-sm" onclick="routerMaintenance('cooling')">解除全部冷却</button>
      <button type="button" class="btn btn-sm" onclick="routerMaintenance('permanent')">清空永久剔除</button>
    </span>
  </div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>自动发现</label>
        <select id="discEnabled" onchange="saveDiscovery()">
          <option value="false">关闭</option>
          <option value="true">开启（定期拉取并试跑）</option>
        </select>
      </div>
      <div class="field"><label>发现源（已加入的供应商）</label>
        <select id="discProvider" onchange="saveDiscovery()"></select>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>间隔（小时）</label>
        <input id="discIntervalH" type="number" min="1" placeholder="48" onchange="saveDiscovery()">
      </div>
      <div class="field"><label>每轮最多试跑</label>
        <input id="discMaxPerRun" type="number" min="1" placeholder="8" onchange="saveDiscovery()">
      </div>
    </div>
    <div class="hint" id="discInfo" style="font-size:12px;color:var(--text3)"></div>

    <div class="form-row" style="margin-top:14px">
      <div class="field"><label>冷却中的候选</label>
        <div id="arCoolingBox" style="border:1px solid var(--border);border-radius:10px;background:var(--inset);min-height:42px"></div>
      </div>
      <div class="field"><label>永久剔除的候选</label>
        <div id="arPermBox" style="border:1px solid var(--border);border-radius:10px;background:var(--inset);min-height:42px;max-height:220px;overflow-y:auto"></div>
      </div>
    </div>
  </div>
</div>
</div>

<div id="tab-settings" class="tab-panel" style="display:none">
<h2>⚙️ 设置</h2>

<div class="section">
  <div class="section-title">🔑 API 密钥管理</div>
  <div class="section-body">
    <p class="hint">生成的密钥可用于客户端访问代理 API（作为 x-api-key 或 Authorization 头）。</p>
    <div class="form-actions" style="margin-bottom:14px">
      <button class="btn btn-success" onclick="generateKey()">➕ 生成新密钥</button>
    </div>
    <div id="keysList"></div>
    <div id="keyGenResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">📨 请求头配置（模拟 Cline CLI 发出）</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>版本对齐方式</label>
        <div style="display:flex;gap:8px;align-items:center">
          <select id="hdrAutoMode" style="flex:1" onchange="saveHeaderAuto()">
            <option value="false">手动（下方表格自行维护）</option>
            <option value="true">自动对齐官方（推荐）</option>
          </select>
          <button class="btn btn-sm" type="button" onclick="syncHeadersNow()" style="flex:none">🔄 立即对齐</button>
        </div>
      </div>
      <div class="field"><label>官方版本</label>
        <input type="text" id="hdrSyncInfo" disabled>
      </div>
    </div>
    <div class="hint" id="hdrSyncHint">自动对齐会从官方发行渠道读取当前 Cline CLI 与核心版本，改写 User-Agent、X-CLIENT-VERSION、X-PLATFORM-VERSION、X-CORE-VERSION；你自己添加的其他请求头不会被改动。</div>
    <div class="table-wrap">
    <table>
      <thead><tr><th style="width:220px">请求头</th><th>值</th><th style="width:40px"></th></tr></thead>
      <tbody id="headersTableBody">
        <tr><td colspan="3" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    </div>
    <div class="form-actions">
      <button class="btn btn-sm" onclick="addHeaderRow()">➕ 添加请求头</button>
      <button class="btn btn-sm btn-primary" onclick="saveHeaders()">💾 保存请求头</button>
    </div>
    <div class="hint">这些请求头会附加到所有转发给 Cline API 的请求中，以模拟官方客户端行为。保存为整表替换：删掉的行不会残留。</div>
    <div id="headerSaveResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🌐 出口代理与节点</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 14px">出口模式作用于整个网关：<strong style="color:var(--text)">所有上游、所有模型</strong>（Cline 账号池 / opencode / 通用 Provider / 订阅抓取）共用同一套出口决策。</p>
    <div class="form-row">
      <div class="field"><label>代理策略</label>
        <select id="ocStrategy"><option value="round_robin">轮询 round_robin</option><option value="random">随机 random</option><option value="fill">固定 fill</option></select>
      </div>
      <div class="field"><label>出口模式</label>
        <select id="ocExitMode">
          <option value="proxy">节点出口（全部走下面节点列表）</option>
          <option value="direct">直连（不走任何节点）</option>
        </select>
      </div>
      <div class="field"><label>节点全挂时</label>
        <select id="ocRescue">
          <option value="true">允许直连兜底（推荐，经 sing-box 的 direct 出站）</option>
          <option value="false">严格走节点（一个可用节点都没有就直接失败）</option>
        </select>
      </div>
    </div>
    <p class="hint" style="margin:0 0 14px;font-size:12px;color:var(--text3)">
      出口模式作用于整个网关：<strong style="color:var(--text)">直连模式下流量依然经过 sing-box</strong>（走它的 direct 出站），
      因此所有联网行为都统一在 sing-box 里 —— 只有 sing-box 实例起不来时才会回退 Go 原生拨号保命（日志会标注）。
    </p>
    <div class="form-row">
      <div class="field"><label>DNS 解析</label>
        <select id="ocDnsMode">
          <option value="doh-ali">DoH 阿里（推荐，节点域名与直连目标用 https://dns.alidns.com）</option>
          <option value="doh-cf">DoH Cloudflare（1.1.1.1）</option>
          <option value="custom">自定义 DoH 地址</option>
          <option value="system">系统解析器（用本机 DNS）</option>
        </select>
      </div>
      <div class="field"><label>自定义 DoH 地址</label>
        <input id="ocDnsCustom" placeholder="https://dns.alidns.com/dns-query">
      </div>
    </div>
    <p class="hint" style="margin:0 0 14px;font-size:12px;color:var(--text3)">
      用于解析 <strong style="color:var(--text)">节点服务器域名</strong> 与直连目标域名；代理请求的目标域名仍由节点侧解析（本地不解析，无污染）。
      默认不用明文 8.8.8.8 —— 它在国内常被污染，表现为"节点时通时不通"。
    </p>
    <div class="form-row">
      <div class="field"><label>代理列表</label>
        <textarea id="ocProxies" rows="4" placeholder="每行一个: http://user:pass@host:port / socks5://host:port&#10;或节点链接: vmess:// vless:// trojan:// ss:// hy2:// tuic:// hysteria:// anytls:// ssh:// shadowtls:// snell://"></textarea>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>订阅链接</label>
        <div id="ocSubsList" style="display:flex;flex-direction:column;gap:6px;margin-bottom:8px"></div>
        <div style="display:flex;gap:8px">
          <input id="ocSubNew" placeholder="https://订阅地址" style="flex:1" />
          <input id="ocSubRefresh" type="number" min="1" max="43200" title="自动刷新间隔（分钟）" placeholder="30" style="width:96px;flex:none" />
          <span style="align-self:center;font-size:12px;color:var(--text3);flex:none">分钟刷新</span>
          <button type="button" onclick="addOcSub()" style="flex:none;padding:9px 14px">添加</button>
        </div>
        <div class="hint" id="ocSubsInfo" style="margin-top:6px;font-size:12px;color:var(--text3);white-space:pre-wrap"></div>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label style="display:flex;align-items:center;justify-content:space-between">节点列表
        <button type="button" id="ocCheckBtn" class="btn" style="padding:3px 10px;font-size:12px" onclick="refreshOcNodes()">连通检测</button></label>
        <div id="ocNodesBox" style="max-height:190px;overflow-y:auto;border:1px solid var(--border);border-radius:10px;background:var(--inset)"></div>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>代理冷却</label>
        <div id="ocCooldownBox" style="max-height:190px;overflow-y:auto;border:1px solid var(--border);border-radius:10px;background:var(--inset);min-height:42px"></div>
      </div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存出口配置</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🔌 通用 Provider（OpenAI 兼容上游）</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 14px">只需要 <strong style="color:var(--text)">Provider 名 + API 地址 + API Key</strong>，保存后会自动拉取模型目录。Google Gemini 只需填 <code>https://generativelanguage.googleapis.com</code>，端点后缀与鉴权方言由程序自动补齐。</p>
    <div class="form-row">
      <div class="field"><label>Provider 名 *</label><input type="text" id="pvName" placeholder="openrouter / gemini / tokenrouter / bai"></div>
      <div class="field"><label>API 地址 *</label><input type="text" id="pvBaseUrl" placeholder="https://openrouter.ai/api/v1"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>API Key *</label><input type="password" id="pvKey" placeholder="sk-..."></div>
      <div class="field"><label>可用模型范围</label>
        <select id="pvMode">
          <option value="all">目录中的全部模型（通用默认）</option>
          <option value="pricing">按目录价格（仅 0 元模型）</option>
          <option value="whitelist">仅白名单（不拉目录）</option>
        </select>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>白名单（每行一个，仅「仅白名单」模式使用）</label>
        <textarea id="pvFree" rows="3" placeholder="glm-5.3-flash"></textarea></div>
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
    <div id="pvList" style="margin-top:10px;border:1px solid var(--border);border-radius:10px;background:var(--inset)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🛡️ 限流防御</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>最大并发</label><input type="text" id="ocMaxConc" placeholder="8"></div>
      <div class="field"><label>限流重试</label><input type="text" id="ocRetries" placeholder="3"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>故障转移</label>
        <select id="ocFailover"><option value="true">开启(切 cline 池)</option><option value="false">关闭</option></select>
      </div>
      <div class="field"><label>失败阈值</label><input type="text" id="ocFailoverCount" placeholder="3"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>转移窗口(分钟)</label><input type="text" id="ocFailoverMinutes" placeholder="5"></div>
      <div class="field"><label>当前状态</label><span id="ocFailoverInfo" class="stat-mini" style="align-self:center">-</span></div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存限流配置</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🗜️ 上下文压缩（opencode 官方机制）</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>自动压缩</label>
        <select id="ocCompactAuto"><option value="true">开启</option><option value="false">关闭</option></select>
      </div>
      <div class="field"><label>预留缓冲</label><input type="text" id="ocCompactBuffer" placeholder="20000"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>尾部保留</label><input type="text" id="ocKeepTokens" placeholder="8000"></div>
      <div class="field"><label>摘要模型</label><input type="text" id="ocSummaryModel" placeholder="留空=同请求模型"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>摘要上限</label><input type="text" id="ocMaxSummary" placeholder="4096"></div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存压缩配置</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🗑️ 危险操作</div>
  <div class="section-body">
    <div style="display:flex;gap:10px;flex-wrap:wrap">
      <button class="btn btn-danger" onclick="deleteAllAccounts()">🗑️ 删除全部账号</button>
      <button class="btn btn-danger" onclick="deleteAllKeys()">🗑️ 删除全部密钥</button>
    </div>
  </div>
</div>
</div>

<div id="tab-logs" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:16px">
  <h2>📜 请求日志 <span class="probe-pill" style="font-weight:normal">最近 500 条，落盘 data/requests.jsonl</span></h2>
  <div style="display:flex;gap:8px">
    <button class="btn btn-sm" onclick="loadLogs()">🔄 刷新</button>
  </div>
</div>
<div class="section">
  <div class="section-body" style="padding:6px">
    <div class="table-wrap">
    <table>
      <thead>
        <tr><th>时间</th><th>来源</th><th>方法</th><th>路径</th><th>模型</th><th>路由</th><th>状态</th><th>耗时</th></tr>
      </thead>
      <tbody id="logsTableBody">
        <tr><td colspan="8" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    </div>
  </div>
</div>
</div>

<div id="toast" class="toast"></div>

<script>
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
const esc = s => { const d=document.createElement('div'); d.textContent=s||''; return d.innerHTML; };
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
  if (name === 'dashboard') { loadStats(); loadOcStats(); loadConfig(); loadOcConfig(); }
  if (name === 'accounts') { loadAccounts(); loadConfig(); }
  if (name === 'models') { loadModels(); loadOcModels(); loadProviders(); }
  if (name === 'router') { loadRouter(); }
  if (name === 'settings') { loadKeys(); loadConfig(); loadOcConfig(); loadOcNodes(); loadProviders(); }
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
    if (!data.success && data.error) throw new Error(data.error);
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
    + (retryExpr ? '<br><button class="btn btn-sm" style="margin-top:8px" onclick="' + retryExpr + '">重试</button>' : '')
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
  } catch (e) { /* ignore */ }
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
        statusExtra = until ? '<div style="font-size:10px;color:var(--text3);margin-top:2px">预计 ' + esc(until) + ' 恢复</div>' : '';
      }
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td><span class="status ' + a.status + '"><span class="status-dot ' + a.status + '"></span>' + (sn[a.status] || a.status) + '</span>' + statusExtra + '</td>' +
          '<td title="今日 ' + fmtNum(a.tokensToday) + ' / 累计 ' + fmtNum(a.tokensTotal) + ' tokens（上游返回 usage 时精确，否则为估算值）">' + fmtTokens(a.tokensToday) + ' / ' + fmtTokens(a.tokensTotal) + '</td>' +
        '<td class="mono" style="font-size:11px">' + lu + '</td>' +
        '<td class="mono" style="font-size:11px">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          '<button class="btn btn-sm" onclick="testAccount(\'' + a.accountId + '\', this)" title="测试账号是否可用（成功会清除冷却/过期状态）">⚡</button> ' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + a.accountId + '\', this)" title="检测限流并解除：探测上游，若仍限流则保持冷却并提示恢复时间">↻</button> ' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.accountId + '\')" title="删除">✕</button>' +
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
            _('oauthResult').innerHTML = '<div style="color:var(--accent2);font-weight:600;font-size:14px">✓ 账号添加成功: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('账号添加成功！', 'success');
          } else {
            _('oauthStatus').textContent = '失败: ' + (r.data.error || '未知错误');
            toast('OAuth 失败', 'error');
          }
        }
      } catch(e) {}
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
      '<div class="flex" style="margin-bottom:8px">' +
        '<span class="key-display" style="flex:1" onclick="copyText(\'' + k + '\')" title="点击复制">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + k + '\')">✕</button>' +
      '</div>'
    ).join('');
  } catch (e) { _('keysList').innerHTML = fail(e, 'loadKeys()'); }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:rgba(52,211,153,.08);border:1px solid rgba(52,211,153,.4);border-radius:10px;padding:12px">' +
        '<div style="color:var(--accent2);font-weight:600;margin-bottom:8px">✓ 新密钥已生成（点击复制）</div>' +
        '<div class="key-display" onclick="copyText(\'' + key + '\')">' + esc(key) + '</div>' +
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

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('已复制到剪贴板', 'success')).catch(() => {
    const ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta);
    toast('已复制到剪贴板', 'success');
  });
}

// ========== 请求日志 ==========
const ROUTE_LABEL = { zen: 'opencode', cline: 'cline 池', admin: '管理', meta: '元信息', other: '其他' };
const STATUS_CLASS = s => s >= 500 ? 'color:var(--danger)' : (s >= 400 ? 'color:var(--amber)' : 'color:var(--accent2)');

async function loadLogs() {
  try {
    const d = await api('GET', '/logs');
    const logs = d.data.logs || [];
    const tbody = _('logsTableBody');
    if (!logs.length) { tbody.innerHTML = '<tr><td colspan="8" class="empty">暂无请求记录</td></tr>'; return; }
    const html = logs.map(l => {
      const t = l.time ? new Date(l.time).toLocaleString('zh-CN') : '-';
      const route = (ROUTE_LABEL[l.route] || l.route || '-') + (l.exit ? ' · ' + l.exit : '');
      const st = l.status || 0;
      return '<tr>' +
        '<td class="mono" style="font-size:11px">' + t + '</td>' +
        '<td class="mono" style="font-size:11px">' + esc(l.client || '-') + '</td>' +
        '<td>' + esc(l.method || '-') + '</td>' +
        '<td class="mono" style="font-size:11px">' + esc(l.path || '-') + '</td>' +
        '<td class="mono" style="font-size:12px">' + esc(l.model || '-') + '</td>' +
        '<td><span class="model-tag">' + esc(route) + '</span></td>' +
        '<td style="font-weight:600;color:' + STATUS_CLASS(st) + '">' + st + '</td>' +
        '<td class="mono" style="font-size:11px">' + (l.durationMs != null ? l.durationMs + ' ms' : '-') + '</td>' +
      '</tr>';
    }).join('');
    tbody.innerHTML = html;
  } catch (e) { tbody.innerHTML = '<tr><td colspan="8">' + fail(e, 'loadLogs()') + '</td></tr>'; }
}

// ========== 导出账号 ==========
async function exportAccounts() {
  try {
    const res = await fetch(API + '/accounts/export');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = 'cline-accounts-export.json';
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast('账号已导出（JSON）', 'success');
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
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:12px;font-family:monospace"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:12px;font-family:monospace"></td>' +
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
      '<div style="color:var(--accent2);font-size:12px">✓ 已保存 ' + Object.keys(d.data.headers).length + ' 个请求头</div>';
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

async function loadModels() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    let info = '';
    if (d.data.lastSync) info += '· 官方清单: ' + new Date(d.data.lastSync).toLocaleTimeString('zh-CN');
    _('modelsProbeInfo').textContent = info;
    if (!models.length) { _('modelsList').innerHTML = '<div class="empty">暂无模型</div>'; return; }
    _('modelsList').innerHTML = models.map(m => {
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      const cost = COST_LABEL[m.cost] || m.cost || '';
      const synced = m.syncedAt ? new Date(m.syncedAt).toLocaleTimeString('zh-CN') : '-';
      const disp = 'cline/' + m.id;
      return '<div style="display:flex;align-items:center;gap:10px;padding:8px 12px;margin:5px 0;background:rgba(148,163,184,.06);border:1px solid var(--border);border-radius:10px;transition:.15s">' +
        '<span style="font-family:\'JetBrains Mono\',monospace;font-size:13px;flex:1">' + esc(disp) + '</span>' +
        '<span class="copy-icon" title="复制" onclick="copyText(\'' + esc(disp).replace(/'/g, "\\'") + '\')">📋</span>' +
        (m.cost === 'free' ? '<span style="font-size:11px;color:var(--accent2)">不扣费</span>' : '') +
        (cost ? '<span class="model-tag">' + esc(cost) + '</span>' : '') +
        '<span class="model-tag" style="' + st.css + '">' + st.label + '</span>' +
        '<span style="font-size:11px;color:var(--text3);min-width:60px;text-align:right">' + synced + '</span>' +
        '</div>';
    }).join('');
  } catch (e) { _('modelsList').innerHTML = fail(e, 'loadModels()'); }
}

async function refreshModels() {
  try {
    _('modelsProbeInfo').textContent = '· 同步中...';
    const d = await api('POST', '/models/refresh');
    toast(d.data.message || '同步已开始', 'info');
    setTimeout(loadModels, 3000);
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); _('modelsProbeInfo').textContent = ''; }
}

async function loadModelOptions() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    const sel = _('settingDefModel');
    if (!sel) return;
    sel.innerHTML = models.map(m => {
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      return '<option value="' + esc(m.id) + '">' + esc(m.id) + ' (' + st.label + ')</option>';
    }).join('');
    const c = await api('GET', '/config');
    if (c.data.defaultModel) sel.value = c.data.defaultModel;
    if (!sel.value && models.length) sel.value = models[0].id;
  } catch (e) { /* ignore */ }
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
          '<td><input type="text" class="header-key" value="' + esc(k) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + esc(v) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { /* ignore */ }
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
    if (_('ocDnsMode')) _('ocDnsMode').value = c.dnsMode || 'doh-ali';
    if (_('ocDnsCustom')) _('ocDnsCustom').value = c.dnsCustom || '';
    if (_('ocRescue')) _('ocRescue').value = (c.rescueDirect === false) ? 'false' : 'true';
    _('ocSubRefresh').value = c.subsRefreshMins || 30;
    if (_('dashExitMode')) _('dashExitMode').value = (c.exitMode === 'direct') ? '直连（不走节点）' : '节点出口（走节点列表）';
    loadOcNodes();
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
    _('ocSubsInfo').textContent = sk.length
      ? sk.map(k => k + ' → ' + ss[k]).join('\n')
      : '订阅尚未抓取';
  } catch (e) { /* ignore */ }
}

// renderCooldowns 节点列表下方的冷却框: 只列出真正处于冷却期的出口。
function renderCooldowns(cd, direct) {
  const el = _('ocCooldownBox');
  if (!el) return;
  const keys = Object.keys(cd);
  if (direct) {
    el.innerHTML = '<div style="padding:9px 12px;font-size:12px;color:var(--text3)">当前为直连模式，节点不参与出口，冷却不适用</div>';
    return;
  }
  if (!keys.length) {
    el.innerHTML = '<div style="padding:9px 12px;font-size:12px;color:var(--text3)">暂无冷却中的节点</div>';
    return;
  }
  el.innerHTML = keys.map(k =>
    '<div style="display:flex;align-items:center;gap:9px;padding:6px 12px;font-size:12.5px;border-bottom:1px solid rgba(148,163,184,.07)">' +
    '<span style="flex:none">🧊</span>' +
    '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(k) + '</span>' +
    '<span style="flex:none;font-size:11px;color:var(--text3)">冷却至 ' + esc(cd[k]) + '</span></div>'
  ).join('');
}

let ocSubsArr = [];
function renderOcSubs() {
  const el = _('ocSubsList');
  if (!ocSubsArr.length) {
    el.innerHTML = '<div style="font-size:12px;color:var(--text3);padding:2px 0">暂无订阅, 在下方添加; 保存后自动抓取并按设定的刷新间隔更新, 支持 sing-box JSON / Clash YAML / base64 节点列表</div>';
    return;
  }
  el.innerHTML = ocSubsArr.map((u, i) =>
    '<div style="display:flex;align-items:center;gap:10px;padding:8px 12px;background:rgba(148,163,184,.06);border:1px solid var(--border);border-radius:10px">' +
    '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px">' + u.replace(/</g, '&lt;') + '</span>' +
    '<button type="button" class="btn" style="flex:none;padding:4px 10px;font-size:12px" onclick="delOcSub(' + i + ')">删除</button></div>'
  ).join('');
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

async function loadOcNodes() {
  try {
    const d = await api('GET', '/opencode/nodes');
    const list = d.data || [];
    if (!list.length) {
      _('ocNodesBox').innerHTML = '<div style="padding:10px 12px;font-size:12px;color:var(--text3)">'
        + (ocChecking ? '连通检测进行中, 完成后列表自动恢复…' : '暂无出口节点, 在上方添加代理/节点链接或订阅') + '</div>';
      return;
    }
    const okN = list.filter(n => n.health === 'ok').length;
    const failN = list.filter(n => n.health === 'fail').length;
    const unkN = list.length - okN - failN;
    const icon = n => n.health === 'ok' ? '🟢' : (n.health === 'fail' ? '🔴' : '⚪');
    const regionN = list.filter(n => (n.regions || []).length).length;
    _('ocNodesBox').innerHTML = list.map(n => {
      const regions = (n.regions || []).map(r => r.replace(/-free$/, ''));
      const ups = n.upstreams || {};
      const upNames = Object.keys(ups);
      const upOk = upNames.filter(u => ups[u]);
      const upBad = upNames.filter(u => !ups[u]);
      return '<div style="display:flex;align-items:center;gap:9px;padding:5px 12px;font-size:12.5px;border-bottom:1px solid rgba(148,163,184,.07)">' +
      '<span style="flex:none">' + icon(n) + '</span>' +
      '<span style="flex:none;min-width:58px;color:var(--text3);font-family:monospace">' + esc(n.type) + '</span>' +
      '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(n.name) + '</span>' +
      (upOk.length ? '<span style="flex:none;font-size:10.5px;color:#16a34a;border:1px solid currentColor;border-radius:4px;padding:0 5px" title="已探测: 该出口到这些上游可达">✓ ' + esc(upOk.join(',')) + '</span>' : '') +
      (upBad.length ? '<span style="flex:none;font-size:10.5px;color:#f87171;border:1px solid currentColor;border-radius:4px;padding:0 5px" title="已探测: 该出口到这些上游不通(选节点时会跳过)">✕ ' + esc(upBad.join(',')) + '</span>' : '') +
      (regions.length ? '<span style="flex:none;font-size:10.5px;color:#16a34a;border:1px solid currentColor;border-radius:4px;padding:0 5px" title="该出口已验证可用于地区受限模型">🌍 ' + esc(regions.join(',')) + '</span>' : '') +
      '<span style="flex:none;font-size:11px;color:var(--text3)">' + n.source + '</span></div>';
    }).join('') +
    '<div style="padding:6px 12px;font-size:11px;color:var(--text3)">共 ' + list.length + ' 个出口 · 🟢 可达 ' + okN + ' · 🔴 不可达 ' + failN + ' · ⚪ 未检测 ' + unkN + ' · 🌍 可用于地区受限模型 ' + regionN + '<br>✓/✕ 是该出口到各上游的可达性(逐节点 TLS 握手探测, 按上游名); ✕ 的节点在请求该上游时会被自动跳过'
      + (ocChecking ? '<br>🔍 连通检测进行中, 图标与计数将在检测完成后更新…' : '') + '</div>';
  } catch (e) { _('ocNodesBox').innerHTML = fail(e, 'loadOcNodes()'); }
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
  await loadOcNodes();
  [15, 35, 60, 90].forEach(sec => setTimeout(loadOcNodes, sec * 1000));
  setTimeout(() => {
    ocChecking = false;
    if (btn) { btn.disabled = false; btn.textContent = old || '连通检测'; }
    loadOcNodes();
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

let pvData = {};
async function loadProviders() {
  try {
    const d = await api('GET', '/providers');
    pvData = (d.data && d.data.providers) || {};
    renderProviderList();
    renderProviderModels();
  } catch (e) { _('pvList').innerHTML = fail(e, 'loadProviders()'); }
}

// providerMode 把三个开关还原成面板上的单一选择。
function providerMode(p) {
  if (p.catalog && p.allModels) return 'all';
  if (p.catalog && p.pricing) return 'pricing';
  return 'whitelist';
}

function renderProviderList() {
  const names = Object.keys(pvData);
  if (!names.length) {
    _('pvList').innerHTML = '<div style="padding:10px 12px;font-size:12px;color:var(--text3)">暂无 provider, 填上方表单添加</div>';
    return;
  }
  _('pvList').innerHTML = names.map(n => {
    const p = pvData[n] || {}, rt = p.runtime || {};
    let st;
    if (!rt.configured) { st = '未配置 key'; }
    else if (rt.error) { st = '❌ ' + esc(String(rt.error).slice(0, 90)); }
    else if (p.catalog) { st = '目录 ' + (rt.catalogSize || 0) + ' · 可聊 ' + (rt.chatCount || 0) + ' · 可用 ' + (p.models || []).length; }
    else { st = '白名单 ' + ((p.freeModels || []).length) + ' 个'; }
    if (rt.rejected) { st += ' · 剔除 ' + rt.rejected; }
    const badge = p.google ? '<span style="flex:none;font-size:10.5px;color:#4285f4;border:1px solid currentColor;border-radius:4px;padding:0 5px" title="Google 特殊约定已内置">Google</span>' : '';
    const open = (window.pvOpen && window.pvOpen[n]) ? true : false;
    let block = '<div style="display:flex;align-items:center;gap:9px;padding:6px 12px;font-size:12.5px;border-bottom:1px solid rgba(148,163,184,.07)">' +
      '<span style="flex:none;min-width:92px;font-family:monospace;color:var(--text3)">' + esc(n) + '</span>' +
      badge +
      '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(p.baseUrl || '') + '</span>' +
      '<span style="flex:none;font-size:11px;color:var(--text3);max-width:44%">' + st + '</span>' +
      '<button type="button" class="btn" style="padding:2px 8px;font-size:11px" onclick="togglePvModels(\'' + esc(n).replace(/'/g, "\\'") + '\')">模型' + (open ? '▲' : '▼') + '</button>' +
      '<button type="button" class="btn" style="padding:2px 8px;font-size:11px" onclick="editProvider(\'' + esc(n).replace(/'/g, "\\'") + '\')">编辑</button>' +
      '<button type="button" class="btn" style="padding:2px 8px;font-size:11px" onclick="testProviderByName(\'' + esc(n).replace(/'/g, "\\'") + '\')">测试</button>' +
      '<button type="button" class="btn" style="padding:2px 8px;font-size:11px;color:#f87171" onclick="delProvider(\'' + esc(n).replace(/'/g, "\\'") + '\')">删除</button></div>';
    if (open) block += renderPvModelBlock(n, p);
    return block;
  }).join('');
}

// renderProviderModels 模型列表页的「通用 Provider 模型」分组。
function renderProviderModels() {
  const el = _('pvModelsList');
  if (!el) return;
  const names = Object.keys(pvData);
  const q = (window.pvModelFilter || '').toLowerCase();
  const search = '<div style="margin-bottom:10px"><input type="text" id="pvModelSearch" placeholder="搜索模型（几百个时快速定位）" value="' + esc(window.pvModelFilter || '') + '" oninput="window.pvModelFilter=this.value;renderProviderModels()" style="max-width:320px"></div>';
  const blocks = names.map(n => {
    const p = pvData[n] || {}, rt = p.runtime || {};
    const models = (p.models || []).filter(m => !q || String(m.id).toLowerCase().indexOf(q) >= 0);
    let note = '';
    if (!rt.configured) note = '未配置 API Key';
    else if (!(p.models || []).length) note = rt.error ? ('拉取失败：' + String(rt.error).slice(0, 160)) : '目录为空，点「刷新目录」重试';
    const rows = models.map(m => {
      const disp = m.id;
      return '<tr>' +
        '<td style="text-align:left;font-family:monospace">' + esc(disp) + '</td>' +
        '<td><span class="copy-icon" title="复制" onclick="copyText(\'' + esc(disp).replace(/'/g, "\\'") + '\')">📋</span></td></tr>';
    }).join('');
    return '<div style="margin-bottom:14px">' +
      '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px">' +
      '<span style="font-weight:600;font-family:monospace">' + esc(n) + '</span>' +
      '<span class="model-tag">' + (p.models || []).length + ' 个可用模型</span>' +
      (p.catalog ? '<span class="model-tag">目录已拉取</span>' : '<span class="model-tag">白名单模式</span>') +
      '<span style="font-size:11px;color:var(--text3)">启用/剔除去「设置 → 🔌 通用 Provider」点行内「模型▼」</span>' +
      '</div>' +
      (note ? '<div class="hint" style="margin:0">' + esc(note) + '</div>'
        : '<div class="table-wrap"><table><thead><tr><th style="text-align:left">模型 ID</th><th style="width:44px"></th></tr></thead><tbody>' + rows + '</tbody></table></div>') +
      '</div>';
  }).join('');
  el.innerHTML = search + (blocks || '<div class="empty">暂无通用 Provider，在「设置 → 🔌 通用 Provider」里添加</div>');
  const si = document.getElementById('pvModelSearch');
  if (si) { si.focus(); si.setSelectionRange(si.value.length, si.value.length); }
}

function togglePvModels(n) {
  window.pvOpen = window.pvOpen || {};
  window.pvOpen[n] = !window.pvOpen[n];
  renderProviderList();
}

function pvSearchInput(n, el) {
  window.pvModelQ = window.pvModelQ || {};
  window.pvModelQ[n] = el.value;
  const pos = el.selectionStart;
  renderProviderList();
  const si = document.querySelector('input[data-pvq="' + n + '"]');
  if (si) { si.focus(); try { si.setSelectionRange(pos, pos); } catch (e) { /* ignore */ } }
}

// 行内模型管理(ai-gateway 式: 加/管/看在同一处): 搜索 + 逐个启用勾。
function renderPvModelBlock(n, p) {
  const q = ((window.pvModelQ || {})[n] || '').toLowerCase();
  const all = p.catalogModels && p.catalogModels.length ? p.catalogModels : (p.models || []).map(m => ({ id: m.model || String(m.id).split(':').slice(1).join(':'), disabled: false }));
  const models = all.filter(m => !q || String(m.id).toLowerCase().indexOf(q) >= 0);
  const rows = models.map(m => {
    const disp = n + ':' + m.id;
    const checked = m.disabled ? '' : ' checked';
    return '<tr>' +
      '<td><input type="checkbox" data-pv="' + esc(n).replace(/'/g, "\\'") + '" data-model="' + esc(m.id).replace(/'/g, "\\'") + '"' + checked + ' onchange="toggleProviderModel(this)" style="width:auto;min-width:0"></td>' +
      '<td style="text-align:left;font-family:monospace">' + esc(disp) + '</td>' +
      '<td><span class="copy-icon" title="复制" onclick="copyText(\'' + esc(disp).replace(/'/g, "\\'") + '\')">📋</span></td></tr>';
  }).join('');
  return '<div style="padding:6px 12px 10px 24px;border-bottom:1px solid rgba(148,163,184,.07)">' +
    '<input type="text" placeholder="搜索模型" data-pvq="' + esc(n).replace(/'/g, "\\'") + '" value="' + esc((window.pvModelQ || {})[n] || '') + '" oninput="pvSearchInput(\'' + esc(n).replace(/'/g, "\\'") + '\',this)" style="max-width:280px;margin-bottom:6px">' +
    '<div class="table-wrap"><table><thead><tr><th style="width:40px">启用</th><th style="text-align:left">模型 ID</th><th style="width:44px"></th></tr></thead><tbody>' + rows + '</tbody></table></div></div>';
}

async function toggleProviderModel(box) {
  const name = box.getAttribute('data-pv'), id = box.getAttribute('data-model');
  const p = pvData[name] || {};
  const cur = new Set(p.disabledModels || []);
  if (box.checked) cur.delete(id); else cur.add(id);
  const existing = Object.assign({}, p);
  delete existing.runtime;
  delete existing.models;
  delete existing.catalogModels;
  delete existing.google;
  delete existing.chatEndpoint;
  delete existing.catalogEndpoint;
  existing.disabledModels = Array.from(cur);
  try {
    await api('POST', '/providers/update', { name, provider: existing });
    toast((box.checked ? '已启用 ' : '已剔除 ') + name + ':' + id, 'success');
    loadProviders();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); box.checked = !box.checked; }
}

function loadProviderModels() { loadProviders(); }

function editProvider(n) {
  const p = pvData[n] || {};
  _('pvName').value = n;
  _('pvBaseUrl').value = p.baseUrl || '';
  _('pvKey').value = p.apiKey || '';
  _('pvMode').value = providerMode(p);
  _('pvFree').value = (p.freeModels || []).join('\n');
  toast('已载入 ' + n + ', 修改后点保存', 'success');
}

function resetProviderForm() {
  _('pvName').value = '';
  _('pvBaseUrl').value = '';
  _('pvKey').value = '';
  _('pvMode').value = 'all';
  _('pvFree').value = '';
  _('pvTestModel').value = '';
  _('pvResult').innerHTML = '';
}

async function saveProvider() {
  const name = _('pvName').value.trim();
  const baseUrl = _('pvBaseUrl').value.trim();
  const apiKey = _('pvKey').value.trim();
  if (!name) { toast('请填写 Provider 名', 'error'); return; }
  if (!baseUrl) { toast('请填写 API 地址', 'error'); return; }
  if (!apiKey) { toast('请填写 API Key', 'error'); return; }
  // 后端按整体替换处理 provider: 表单未编辑的字段(headers、chatPath 等)
  // 必须原样回传, 否则保存会把它们清掉, 已配好的 provider 会静默失真。
  const existing = Object.assign({}, pvData[name] || {});
  delete existing.runtime;
  delete existing.models;
  delete existing.catalogModels;
  delete existing.google;
  delete existing.chatEndpoint;
  delete existing.catalogEndpoint;
  const mode = _('pvMode').value;
  const body = {
    name,
    provider: Object.assign(existing, {
      baseUrl,
      apiKey,
      catalog: mode !== 'whitelist',
      pricing: mode === 'pricing',
      allModels: mode === 'all',
      freeModels: _('pvFree').value.split('\n').map(s => s.trim()).filter(Boolean),
      // 目录地址与鉴权方言由后端按上游推导, 面板不再暴露
      modelsUrl: '',
      modelsKeyHeader: '',
    }),
  };
  try {
    await api('POST', '/providers/update', body);
    toast('已保存 ' + name + '，正在拉取模型目录…', 'success');
    loadProviders();
    // 保存后自动拉一次目录, 用户不需要再点「刷新目录」
    try {
      await api('POST', '/providers/refresh', { name });
      setTimeout(loadProviders, 3000);
      setTimeout(loadProviders, 10000);
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
      pl.innerHTML = '<div class="empty" style="padding:12px">没有可用的上游</div>';
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
          + 'border:1px solid var(--border);border-radius:10px;background:var(--inset);cursor:pointer">'
          + '<input type="checkbox" data-prov="' + esc(p.name) + '"' + (chosen ? ' checked' : '') + '>'
          + '<span style="flex:1;display:flex;align-items:center;gap:10px;min-width:0;flex-wrap:wrap">'
          + '<strong>' + esc(p.display || p.name) + '</strong>'
          + (p.builtin ? '<span class="model-tag" style="opacity:.8">内置上游</span>' : '')
          + '<span style="color:var(--text3);font-size:12px">' + esc(meta.join(' · ')) + '</span>'
          + (bad.length ? '<span style="color:#f87171;font-size:12px">' + esc(bad.join('，')) + '</span>' : '')
          + '</span></label>';
      }).join('');
    }
  }

  // ---- 模型勾选: 只列已勾选供应商的模型 ----
  const ml = _('arModelList');
  if (ml) {
    const chosen = (d.providers || []).filter(p => routerProviders.has(p.name));
    if (!chosen.length) {
      ml.innerHTML = '<div class="empty" style="padding:12px">先在上面勾选供应商，这里会列出它们的模型</div>';
    } else {
      ml.innerHTML = chosen.map(p => {
        const models = p.models || [];
        const picked = models.filter(m => routerModels.has(p.name + ':' + m.id)).length;
        const rows = models.map(m => {
          const key = p.name + ':' + m.id;
          const label = m.id === '*' ? '账号池自动选模型' : m.id;
          const ctx = m.context ? (' · ' + Math.round(m.context / 1000) + 'k 上下文') : '';
          return '<label style="display:flex;align-items:center;gap:8px;padding:5px 10px;font-size:12.5px;cursor:pointer">'
            + '<input type="checkbox" data-key="' + esc(key) + '"' + (routerModels.has(key) ? ' checked' : '') + '>'
            + '<code>' + esc(label) + '</code><span style="color:var(--text3)">' + esc(ctx) + '</span></label>';
        }).join('') || '<div style="padding:8px 10px;color:var(--text3);font-size:12px">该供应商暂无可用模型（先点上面的「刷新全部目录」）</div>';
        const pname = p.display || p.name;
        return '<div style="margin-bottom:10px;border:1px solid var(--border);border-radius:10px;overflow:hidden">'
          + '<div style="padding:8px 12px;display:flex;align-items:center;gap:8px;background:rgba(148,163,184,.05);font-size:13px">'
          + '<strong>' + esc(pname) + '</strong>'
          + '<span style="color:var(--text3);font-weight:normal">已选 ' + picked + ' / ' + models.length + '</span>'
          + '<span style="margin-left:auto;display:flex;gap:6px">'
          + '<button type="button" class="btn btn-sm" data-prov-all="' + esc(p.name) + '">全选</button>'
          + '<button type="button" class="btn btn-sm" data-prov-none="' + esc(p.name) + '">全不选</button>'
          + '</span></div><div style="padding:4px 6px">' + rows + '</div></div>';
      }).join('');
    }
  }
  if (_('arSelectionWarn')) {
    _('arSelectionWarn').textContent = routerModels.size
      ? '已选 ' + routerModels.size + ' 个模型参与自动路由'
      : '未勾选任何模型：保存后自动路由会回落为「全部供应商的全部免费模型」';
  }

  renderRouterChain(d);
  renderRouterUsage(d);
  renderRouterCooling(d);
  renderRouterDiscovery(d);
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
  const color = kind === 'ok' ? '#4ade80' : (kind === 'warn' ? '#fbbf24' : '#f87171');
  el.innerHTML = '<div style="padding:10px 12px;border-radius:10px;border:1px solid ' + color
    + ';background:var(--inset);font-size:13px;color:' + color + '">' + esc(msg) + '</div>'
    + (problems && problems.length
      ? '<ul style="margin:8px 0 0 18px;font-size:12.5px;color:var(--text2)">'
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
        ? '<span class="model-tag" style="opacity:.55;text-decoration:line-through" title="' + esc(h.skip) + '">' + label + '</span>'
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
            + '</td><td>' + (r.fail ? '<span style="color:#f87171">' + r.fail + '</span>' : '0') + '</td><td>' + limit + '</td></tr>';
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
      ? cool.map(c => '<div style="padding:6px 10px;border-bottom:1px solid var(--border);font-size:12px">'
        + '<code>' + esc(c.key) + '</code> · ' + esc(c.class) + ' · 剩 ' + fmtRemain(c.remainMs) + '</div>').join('')
      : '<div style="padding:8px 10px;font-size:12px;color:var(--text3)">暂无冷却中的候选</div>';
  }
  const pb = _('arPermBox');
  if (pb) {
    const perm = d.permanent || [];
    pb.innerHTML = perm.length
      ? perm.map(p => '<div style="padding:6px 10px;border-bottom:1px solid var(--border);font-size:12px">'
        + '<code>' + esc(p.key) + '</code><div style="color:var(--text3);margin-top:2px">' + esc(p.reason) + '</div></div>').join('')
      : '<div style="padding:8px 10px;font-size:12px;color:var(--text3)">暂无永久剔除</div>';
  }
}

function renderRouterDiscovery(d) {
  const disc = d.discovery || {};
  const dc = disc.config || {};
  const provs = (d.providers || []).filter(p => !p.builtin);
  if (_('discProvider')) {
    // 发现源只能从"已加入的通用 Provider"里选(内置上游没有目录), 不允许手填
    const cur = dc.provider || '';
    _('discProvider').innerHTML = provs.map(p =>
      '<option value="' + esc(p.name) + '"' + (p.name === cur ? ' selected' : '') + '>' + esc(p.display || p.name) + '</option>').join('')
      || '<option value="">（还没有通用 Provider）</option>';
    if (cur && provs.findIndex(p => p.name === cur) < 0) _('discProvider').value = '';
  }
  if (_('discEnabled')) _('discEnabled').value = dc.enabled ? 'true' : 'false';
  if (_('discIntervalH')) {
    const h = Math.round((dc.intervalMs || 0) / 3600000);
    _('discIntervalH').value = h > 0 ? h : 48;
  }
  if (_('discMaxPerRun')) _('discMaxPerRun').value = dc.maxPerRun || 8;
  if (_('discInfo')) {
    _('discInfo').textContent = dc.enabled
      ? ('已收录 ' + (disc.discovered || 0) + ' 个自动发现的模型，会追加在自动路由的末尾；文件 ' + (disc.path || ''))
      : '当前为关闭状态：不会自动发现新模型，自动路由只用手动勾选的候选。';
  }
}

async function saveDiscovery() {
  const cfg = Object.assign({}, ((routerData && routerData.discovery) || {}).config || {});
  cfg.enabled = _('discEnabled').value === 'true';
  cfg.provider = _('discProvider').value || cfg.provider || '';
  const h = parseInt(_('discIntervalH').value, 10);
  cfg.intervalMs = (h > 0 ? h : 48) * 3600000;
  const n = parseInt(_('discMaxPerRun').value, 10);
  cfg.maxPerRun = n > 0 ? n : 8;
  try {
    await api('POST', '/router/discovery', cfg);
    toast('已保存自动发现设置', 'success');
    loadRouter();
  } catch (e) { toast('保存失败：' + e.message, 'error'); }
}

async function delProvider(n) {
  if (!confirm('确认删除 provider ' + n + '?')) return;
  try { await api('POST', '/providers/update', { name: n, remove: true }); toast('已删除 ' + n, 'success'); loadProviders(); }
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

async function refreshProviderCatalog() {
  const name = _('pvName').value.trim();
  try {
    await api('POST', '/providers/refresh', name ? { name } : {});
    toast('目录刷新已启动', 'success');
    setTimeout(loadProviders, 3000);
    setTimeout(loadProviders, 12000);
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

async function loadOcModels() {
  try {
    const d = await api('GET', '/opencode/models');
    const models = d.data.models || [];
    _('ocModelsList').innerHTML = '<div class="table-wrap"><table><thead><tr><th style="text-align:left">模型 ID</th><th style="width:44px"></th><th>上下文</th><th>输出</th><th>来源</th></tr></thead><tbody>' +
      models.map(m => {
        const disp = 'zen/' + m.id;
        return '<tr><td style="text-align:left;font-family:monospace">' + esc(disp) + '</td>' +
          '<td><span class="copy-icon" title="复制" onclick="copyText(\'' + esc(disp).replace(/'/g, "\\'") + '\')">📋</span></td>' +
          '<td>' + m.context + '</td><td>' + m.output + '</td><td>' + m.source + '</td></tr>';
      }).join('') +
      '</tbody></table></div><div class="hint">共 ' + models.length + ' 个免费模型（每 10 分钟自动同步）</div>';
  } catch (e) { _('ocModelsList').innerHTML = fail(e, 'loadOcModels()'); }
}

async function refreshOcModels() {
  try {
    const d = await api('POST', '/opencode/models/refresh');
    toast(d.message || '同步完成', 'success');
    loadOcModels();
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
  if (!keys.length) return '<div class="empty" style="padding:12px">暂无数据</div>';
  // 按合计 token 降序: 谁消耗多谁在前面
  keys.sort((a, b) => {
    const ea = byKey[a] || {}, eb = byKey[b] || {};
    return ((eb.promptTokens || 0) + (eb.completionTokens || 0)) - ((ea.promptTokens || 0) + (ea.completionTokens || 0));
  });
  const rows = keys.map(k => {
    const e = byKey[k] || {};
    const pt = e.promptTokens || 0, ct = e.completionTokens || 0;
    return '<tr><td style="text-align:left;font-family:monospace">' + esc(k) + '</td><td>' + fmtNum(e.requests || 0) +
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
      '<div class="hint" style="margin-top:8px">覆盖全部上游: Cline 账号池 / opencode / ClinePass / 通用 Provider。上游返回 usage 时精确，否则按请求体估算。</div>';
    _('statUpstreamBox').innerHTML = statBreakdown(t.byUpstream, '上游');
    _('statModelBox').innerHTML = statBreakdown(t.byModel, '模型');
  } catch (e) { /* ignore */ }
}

// ========== 初始化 ==========
loadStats();
loadAccounts();
loadKeys();
loadModels();
loadConfig();
loadOcConfig();
loadProviders();
setInterval(() => { loadStats(); }, 10000);
setInterval(() => { loadOcStats(); }, 15000);
setInterval(() => { if (_('tab-logs').style.display !== 'none') loadLogs(); }, 8000);
</script>
</body>
</html>`

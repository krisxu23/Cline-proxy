package webui

// 面板脚本 scriptConfig —— 配置管理(头同步/密钥/模型) + 配置加载 + opencode 免费模型 + 出口地区。
// 由 page_script.go 按分节横幅切开(2026-09-16): 单个 1900+ 行原始字符串难以评审,
// 切片后每片聚焦一个面板域。**拼接顺序在 webui.go 的 HTML 常量里**;
// 各片单独看都不是完整 JS(跨片引用是常态), 不要调整顺序。
// 注意: 原始字符串内禁止出现反引号(会提前终止字符串)。
const scriptConfig = `// ========== 配置管理 ==========
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

`

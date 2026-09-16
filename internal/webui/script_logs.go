package webui

// 面板脚本 scriptLogs —— 请求日志(分页/详情) + 网关健康 + 账号导出。
// 由 page_script.go 按分节横幅切开(2026-09-16): 单个 1900+ 行原始字符串难以评审,
// 切片后每片聚焦一个面板域。**拼接顺序在 webui.go 的 HTML 常量里**;
// 各片单独看都不是完整 JS(跨片引用是常态), 不要调整顺序。
// 注意: 原始字符串内禁止出现反引号(会提前终止字符串)。
const scriptLogs = `// ========== 请求日志 ==========
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

`

package webui

// 面板脚本 scriptRouter —— 自动路由(候选链/预演/用量/冷却) + 统计口径 + 初始化定时器与收尾标签。
// 由 page_script.go 按分节横幅切开(2026-09-16): 单个 1900+ 行原始字符串难以评审,
// 切片后每片聚焦一个面板域。**拼接顺序在 webui.go 的 HTML 常量里**;
// 各片单独看都不是完整 JS(跨片引用是常态), 不要调整顺序。
// 注意: 原始字符串内禁止出现反引号(会提前终止字符串)。
const scriptRouter = `
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

package webui

// 面板脚本 scriptProviders —— 供应商与模型: 卡片渲染、搜索过滤、编辑表单。
// 由 page_script.go 按分节横幅切开(2026-09-16): 单个 1900+ 行原始字符串难以评审,
// 切片后每片聚焦一个面板域。**拼接顺序在 webui.go 的 HTML 常量里**;
// 各片单独看都不是完整 JS(跨片引用是常态), 不要调整顺序。
// 注意: 原始字符串内禁止出现反引号(会提前终止字符串)。
const scriptProviders = `// ========== 供应商与模型 ==========
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
      requiresStream: m.requiresStream,
      // free 同样必须透传: opencode 行靠它区分「自动免费(锁定勾选)」与
      // 「未标注 -free 的测试模型(可勾选)」。漏掉时 m.free 为 undefined,
      // 旧判定 m.free !== false 会把 undefined 当 true → 全部渲染成灰框。
      free: m.free === true
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
      // 自动免费的模型(seed / -free 后缀)锁定勾选: 它们本来就免费, 关掉只会误导。
      // 其余的是 opencode 不定期放进、未标注 -free 的(免费)测试模型 —— 开关写入
      // cfg.EnabledModels, 后端 isZenFreeModel 放行后才对网关可用。
      // ★ 必须严格判 === true: providerModels 之外直接拼的对象可能没有 free 字段,
      //   undefined !== false 会误判成"免费"导致灰框。
      const free = m.free === true;
      const on = free || !!m.on;
      const box = free
        ? '<input type="checkbox" checked disabled title="自动免费模型, 无需手动启用">'
        : '<input type="checkbox" data-ocmodel="' + escAttr(m.id) + '"' + (on ? ' checked' : '') + ' onchange="toggleOcModel(this)">';
      const tag = free ? '' : '<span class="model-tag" title="opencode 免费但未标注 -free 的测试模型, 手动启用后才会对网关发布">测试</span>';
      return '<tr data-mrow data-row-search="' + escAttr(disp) + '"><td>' + box + '</td>' +
        '<td class="mrow-label" style="text-align:left;font-family:var(--font-mono)">' + esc(disp) + ' ' + tag + '</td>' +
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
    head = '<tr><th style="width:44px">启用</th><th style="text-align:left">模型 ID</th><th>上下文</th><th>最大输出</th><th style="width:44px"></th></tr>';
    note = 'zen 目录全量(每 10 分钟自动同步); 自动免费的模型锁定勾选, 标「测试」的是上游放进但未标注 -free 的模型, 手动勾选即启用。';
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
`

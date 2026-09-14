// 管理后台渲染冒烟测试
//
// admin_html.go 里嵌着一个 2300+ 行的 HTML/JS 字符串, 没有构建步骤、没有打包器,
// 浏览器里改坏一处很难定位。这个脚本把它抽出来在 Node 里真的跑一遍:
// 用样例 provider 数据调用 renderModelIndex(), 断言输出 HTML 的结构 / 前缀 / 权限边界。
//
// 用法:  node scripts/admin-render-test.js
// 退出码: 0 = 全过, 1 = 有失败, 2 = 测试自身搭不起来
//
// 只做两处无害改造, 不改源码:
//   1. 替换 esc 的实现 —— 它依赖 createElement().innerHTML 这条 DOM 链路, stub 里做不到。
//      ★ stub 与生产语义保持一致: 只转义 & < >, **不转义引号**。
//        如果 stub 比生产更严(多转引号), 「title 属性可被双引号闭合注入」这类
//        XSS 就会被掩盖 —— 测试全绿而生产是漏洞。
//   2. 把测试探针放进同一个 eval 作用域 —— 否则访问不到 let pvData / const pvOpenSet。
//
// ★ 覆盖边界(务必读): 本脚本无 DOM 解析器, document.querySelectorAll / .closest
//   在 render 之后是空跑, 所以「靠 DOM 遍历的事件处理器」(卡片内搜索、行勾选切换、
//   tab 切换、api 请求)不会通过真实 DOM 被执行。
//   - [13] 直接断言 providerModels 的 join 不变量(裸名 ↔ 裸名, 附加字段不丢);
//   - [14] 断言渲染出的**文本值**(费用 / 状态 / 同步时间 / 上下文 / 输出),
//          而不是只断言标签开闭 —— 专门堵「渲染成空壳但断言全绿」;
//   - [15] 断言 title 属性经 escAttr 转义, 堵 [12] 抓不到的属性上下文 XSS;
//   - [16] 用受控替身直接单测 filterCardModels 的显隐分支;
//   - [17] 把仍未覆盖的交互函数显式列出来, 不让 ALL PASS 被误读成全覆盖。
'use strict';
const fs = require('fs');
const path = require('path');

const ADMIN_GO = path.join(__dirname, '..', 'internal', 'app', 'admin_html.go');
const src = fs.readFileSync(ADMIN_GO, 'utf8');

// ---- 抽出 const adminHTML = `...` (Go 原始字符串, 无反义, 直接取) ----
const m = src.match(/const adminHTML\s*=\s*`([\s\S]*?)`/);
if (!m) {
  console.error('FATAL: 没找到 adminHTML 原始字符串');
  process.exit(2);
}
const script = (m[1].match(/<script>([\s\S]*?)<\/script>/) || [])[1];
if (!script) {
  console.error('FATAL: adminHTML 里没有 <script>');
  process.exit(2);
}

// ---- 极简 DOM stub ----
class El {
  constructor(id) {
    this.id = id;
    this._html = '';
    this._text = '';
    this.value = '';
    this.checked = false;
    this.style = {};
    this.dataset = {};
    this.parentElement = null;
    this.classList = { add: () => {}, remove: () => {}, toggle: () => {}, contains: () => false };
  }
  get innerHTML() { return this._html; }
  set innerHTML(v) { this._html = String(v); }
  get textContent() { return this._text; }
  set textContent(v) { this._text = String(v); }
  setAttribute() {}
  getAttribute() { return null; }
  removeAttribute() {}
  addEventListener() {}
  // 与浏览器一致: 沿父链向上找 class / id。
  // 用闭包记次数 —— 不能写 this.hits 字段, 字段初始化晚于方法表, 调用时会踩 TDZ。
  closest(sel) {
    const hits = [];
    let e = this;
    while (e) {
      hits.push(e);
      e = e.parentElement;
    }
    if (!sel) return null;
    const re = sel.charAt(0) === '.' ? new RegExp('(^|\\s)' + sel.slice(1) + '(\\s|$)') : null;
    for (const c of hits) {
      const cn = c.className || '';
      if (re) { if (re.test(cn)) return c; continue; }
      if (c.id === sel.slice(1)) return c;
    }
    return null;
  }
  // render 之后 document.querySelectorAll('#modelIndex .pi') 是空跑,
  // 这些分支没被执行 —— [17] 会显式列出未覆盖的交互函数。
  querySelectorAll() { return []; }
}
const els = {};
global.els = els;
global.document = {
  documentElement: new El('html'),
  body: new El('body'),
  getElementById: id => els[id] || (els[id] = new El(id)),
  querySelectorAll: () => [],
  querySelector: () => null,
  createElement: () => new El('dyn'),
  addEventListener: () => {},
};
global.location = { host: '127.0.0.1:3457', origin: 'http://127.0.0.1:3457' };
global.window = global;
// Node 22+ 里 global.navigator 是只读 getter, 必须用 defineProperty 覆盖
Object.defineProperty(global, 'navigator', { value: { clipboard: null }, configurable: true, writable: true });
global.fetch = async () => { throw new Error('network disabled in test'); };
global.AbortController = class { constructor() { this.signal = {}; } abort() {} };
global.confirm = () => true;
global.setTimeout = () => 0;
global.setInterval = () => 0;
global.localStorage = { getItem: () => 'dark', setItem: () => {} };
global.alert = () => {};
global.scrollTo = () => {};
global.requestAnimationFrame = () => 0;

// ---- 测试探针: 先声明, 稍后拼进同一个 eval 作用域 ----
const PROBE = `
const SAMPLE = {
  opencode: {
    builtin: true, display: 'opencode（zen 免费模型）', apiType: 'builtin',
    enabled: true, keys: 1, baseUrl: 'zen 免费目录', catalog: false,
    runtime: { configured: true },
    catalogModels: [
      { id: 'deepseek-v4-flash', disabled: false },
      { id: 'gemini-3.8-flash', disabled: false },
    ],
    models: [
      { id: 'opencode:deepseek-v4-flash', model: 'deepseek-v4-flash', context: 1000000, output: 65536 },
      { id: 'opencode:gemini-3.8-flash', model: 'gemini-3.8-flash', context: 1000000, output: 65536 },
    ],
  },
  cline: {
    builtin: true, display: 'Cline 账号池', apiType: 'builtin',
    enabled: true, keys: 0, baseUrl: 'Cline 账号池（自动选账号与模型）', catalog: false,
    runtime: { configured: true },
    catalogModels: [
      { id: 'deepseek/deepseek-v4-flash', disabled: false },
      { id: 'old-removed-model', disabled: true },
    ],
    models: [
      { id: 'cline:deepseek/deepseek-v4-flash', model: 'deepseek/deepseek-v4-flash', status: 'active', cost: 'free', syncedAt: '2026-09-13T08:00:00Z', requiresStream: true },
      { id: 'cline:old-removed-model', model: 'old-removed-model', status: 'removed', cost: 'free', syncedAt: '2026-09-13T08:00:00Z' },
    ],
    lastSync: '2026-09-13T08:00:00Z',
  },
  bai: {
    builtin: false, display: 'B.AI', apiType: 'openai',
    enabled: true, keys: 2, baseUrl: 'https://api.b.ai/v1', catalog: true,
    runtime: { configured: true, catalogSize: 120, chatCount: 88 },
    catalogModels: [
      { id: 'glm-5.3-flash', disabled: false },
      { id: 'glm-4-air', disabled: true },
    ],
    models: [{ id: 'bai:glm-5.3-flash', model: 'glm-5.3-flash' }],
    google: false,
  },
  gemini: {
    builtin: false, display: '', apiType: 'openai',
    enabled: true, keys: 1, baseUrl: 'https://generativelanguage.googleapis.com', catalog: true,
    runtime: { configured: true, catalogSize: 54, chatCount: 41 },
    catalogModels: [{ id: 'gemini-3.8-flash', disabled: false }],
    models: [{ id: 'gemini:gemini-3.8-flash', model: 'gemini-3.8-flash' }],
    google: true,
  },
  dead: {
    builtin: false, display: 'Dead 上游', apiType: 'anthropic',
    enabled: true, keys: 0, baseUrl: 'https://api.dead.example/v1', catalog: true,
    runtime: { configured: false },
    catalogModels: [], modelEntries: [], models: [], google: false,
  },
};

let FAILS = 0;
function check(label, cond, extra) {
  console.log((cond ? '  PASS  ' : '  FAIL  ') + label + (cond || extra == null ? '' : '  -> ' + extra));
  if (!cond) FAILS++;
}
const tagBal = (tag, s) => {
  const o = (s.match(new RegExp('<' + tag + '[ >]', 'g')) || []).length;
  const c = (s.match(new RegExp('</' + tag + '>', 'g')) || []).length;
  return o === c ? '平衡' : '不平衡(' + o + '/' + c + ')';
};
const cardHtml = (name) => {
  const h = els.modelIndex._html;
  const i = h.indexOf('data-id="' + name + '"');
  if (i < 0) return '';
  const j = h.indexOf('<article class="pi', i + 1);
  return h.slice(i, j < 0 ? undefined : j);
};

pvData = SAMPLE;
renderModelIndex();

const html = els.modelIndex._html;
console.log('\\n[1] 汇总行: ' + els.modelIndexSummary._text);
check('汇总行包含 5 个供应商', /5 个供应商/.test(els.modelIndexSummary._text));
check('汇总行包含模型计数', /模型可用/.test(els.modelIndexSummary._text));
check('汇总行包含已就绪计数', /已就绪/.test(els.modelIndexSummary._text));

console.log('\\n[2] 卡片数量与顺序(内置在前)');
const ids = (html.match(/data-id="([^"]+)"/g) || []).map(s => s.slice(9, -1));
check('共 5 张卡片', ids.length === 5, JSON.stringify(ids));
check('内置卡片钉在最前', ids[0] === 'cline' && ids[1] === 'opencode', JSON.stringify(ids));
check('通用卡片按字母序', ids[2] === 'bai' && ids[3] === 'dead' && ids[4] === 'gemini', JSON.stringify(ids));

console.log('\\n[3] 表结构开闭平衡');
['cline', 'opencode', 'bai', 'gemini', 'dead'].forEach(n => {
  const c = cardHtml(n);
  check(n + ': <td> 平衡', tagBal('td', c) === '平衡', tagBal('td', c));
  check(n + ': <tr> 平衡', tagBal('tr', c) === '平衡', tagBal('tr', c));
  check(n + ': <table> 平衡', tagBal('table', c) === '平衡', tagBal('table', c));
  check(n + ': <span> 平衡', tagBal('span', c) === '平衡', tagBal('span', c));
});
check('cline 展开 2 行模型(含已下架)', (cardHtml('cline').match(/<tr[ >]/g) || []).length === 3,
  (cardHtml('cline').match(/<tr[ >]/g) || []).length + ' 行');
check('dead 有「暂无模型」占位行', /该供应商暂无模型/.test(cardHtml('dead')));

console.log('\\n[4] 复制按钮的模型 ID 前缀');
const all = (html.match(/copyText\\('([^']*)'\\)/g) || []).map(s => s.slice(10, -2));
check('cline 用 cline/ 前缀', all.some(s => s === 'cline/deepseek/deepseek-v4-flash'),
  JSON.stringify(all.filter(s => s.startsWith('cline/'))));
check('opencode 用 zen/ 前缀', all.some(s => s === 'zen/deepseek-v4-flash'),
  JSON.stringify(all.filter(s => s.startsWith('zen/'))));
check('通用用 name: 前缀', all.some(s => s === 'bai:glm-5.3-flash'),
  JSON.stringify(all.filter(s => s.startsWith('bai:'))));
check('所有复制 ID 前缀格式正确', all.every(s => /^(cline\\/|zen\\/|[a-z0-9_-]+:)/.test(s)),
  JSON.stringify(all.filter(s => !/^(cline\\/|zen\\/|[a-z0-9_-]+:)/.test(s))));

console.log('\\n[5] 勾选框只对通用 Provider 出现');
const cb = n => (cardHtml(n).match(/type="checkbox"/g) || []).length;
check('cline 卡片 0 个勾选框', cb('cline') === 0, cb('cline') + ' 个');
check('opencode 卡片 0 个勾选框', cb('opencode') === 0, cb('opencode') + ' 个');
check('bai 卡片 2 个勾选框', cb('bai') === 2, cb('bai') + ' 个');
const row = id => (cardHtml('bai').match(new RegExp('<td><input[^>]*' + id.replace(/\\./g, '\\\\.') + '[^>]*></td>')) || ['未找到'])[0];
check('glm-5.3-flash 已勾选', /checked/.test(row('glm-5.3-flash')), row('glm-5.3-flash'));
check('glm-4-air 未勾选', !/checked/.test(row('glm-4-air')) && row('glm-4-air') !== '未找到', row('glm-4-air'));

console.log('\\n[6] 状态徽章口径');
const badge = n => (cardHtml(n).match(/<span class="pbadge[^>]*>[^<]*<\\//) || ['未找到'])[0];
check('bai 就绪态', /pbadge pb-on/.test(cardHtml('bai')) && /目录 120/.test(cardHtml('bai')), badge('bai'));
check('dead 未就绪态', /pbadge pb-off/.test(cardHtml('dead')) && /未配置 key/.test(cardHtml('dead')), badge('dead'));
check('cline 显示已下架计数', /就绪 · 1 个已下架/.test(cardHtml('cline')), badge('cline'));
check('opencode 显示 zen key 已配置', /zen key 已配置/.test(cardHtml('opencode')), badge('opencode'));
check('gemini 标注 Google Gemini', /Google Gemini/.test(cardHtml('gemini')));
check('dead 标注 Anthropic 方言', /Anthropic/.test(cardHtml('dead')));

console.log('\\n[7] 操作按钮路由');
const fns = (n, attr) => {
  const src = cardHtml(n);
  const re = attr ? new RegExp(attr + '="([A-Za-z]+)\\\\(', 'g') : /on(?:click|input|change)="([A-Za-z]+)\\(/g;
  return (src.match(re) || []).map(s => s.slice(attr ? attr.length + 2 : 9, -1));
};
check('bai 有连通测试', fns('bai').includes('testProviderByName'), JSON.stringify(fns('bai')));
check('bai 有刷新目录', fns('bai').includes('refreshOneCatalog'), JSON.stringify(fns('bai')));
check('bai 有编辑', fns('bai').includes('editProvider'));
check('bai 有删除', fns('bai').includes('delProvider'));
check('bai 有卡片内模型搜索(oninput)', fns('bai', 'oninput').includes('filterCardModels'), JSON.stringify(fns('bai', 'oninput')));
check('bai 行内可切换模型启用(onchange)', fns('bai', 'onchange').includes('toggleProviderModel'), JSON.stringify(fns('bai', 'onchange')));
check('bai 无同步官方清单按钮', !fns('bai').includes('refreshModels'));
check('cline 有同步官方清单', fns('cline').includes('refreshModels'), JSON.stringify(fns('cline')));
check('cline 无删除按钮(内置)', !fns('cline').includes('delProvider'));
check('cline 无连通测试按钮(内置)', !fns('cline').includes('testProviderByName'));
check('cline 无模型启用勾选(onchange)', fns('cline', 'onchange').indexOf('toggleProviderModel') < 0, JSON.stringify(fns('cline', 'onchange')));
check('opencode 有同步 zen 目录', fns('opencode').includes('refreshOcModels'), JSON.stringify(fns('opencode')));
check('opencode 无模型勾选切换', !fns('opencode', 'onchange').includes('toggleProviderModel'));
check('opencode 无连通测试按钮(内置)', !fns('opencode').includes('testProviderByName'));

console.log('\\n[8] onclick 引号闭合(回归)');
const bad = [];
html.split('onclick="').forEach(part => {
  const end = part.indexOf('">');
  if (end < 0) return;
  const body = part.slice(0, end);
  if ((body.match(/'/g) || []).length % 2 !== 0) bad.push(body);
});
check('所有 onclick 单引号成对', bad.length === 0, JSON.stringify(bad));

console.log('\\n[9] 展开/收起状态跨重绘保留');
pvOpenSet.bai = true;
renderModelIndex();
check('bai 重绘后仍是 open', /class="pi open" data-id="bai"/.test(els.modelIndex._html),
  els.modelIndex._html.slice(els.modelIndex._html.indexOf('data-id="bai"') - 30, els.modelIndex._html.indexOf('data-id="bai"') + 10));
check('未展开的 cline 不带 open', !/class="pi open" data-id="cline"/.test(els.modelIndex._html));

console.log('\\n[9b] 数据与展开态都未变时, 重绘不得重建 DOM(防整页闪烁)');
pvData = SAMPLE;
delete pvOpenSet.bai;
renderModelIndex();
// 在现有 DOM 内容上打标记: 若下一次 renderModelIndex 真的重建了 innerHTML,
// 标记会消失; 若按签名跳过重建, 标记保留。保存/勾选后会安排 3s/8s/10s 多次
// 延迟刷新, 重建会重播展开区动画(整页一闪一闪)并把滚动位置打回顶部。
els.modelIndex.innerHTML = els.modelIndex._html + '<!--SIG-SENTINEL-->';
renderModelIndex();
check('数据与展开态未变时跳过重建', els.modelIndex._html.indexOf('SIG-SENTINEL') >= 0,
  els.modelIndex._html.slice(-40));
pvOpenSet.bai = true;
renderModelIndex();
check('展开态变化时仍会重建并带 open',
  els.modelIndex._html.indexOf('SIG-SENTINEL') < 0 && /class="pi open" data-id="bai"/.test(els.modelIndex._html),
  els.modelIndex._html.slice(0, 90));
pvOpenSet.bai = false;   // 还原, 不影响后续用例

console.log('\\n[9c] 全选/全不选按钮: 只出现在有勾选框的通用 Provider 表头');
pvData = SAMPLE;
renderModelIndex();
const ah9c = els.modelIndex._html;
check('通用 Provider 表头带 全选/全不选',
  /onclick="toggleAllProviderModels\\('bai',true\\)"/.test(ah9c) &&
  /onclick="toggleAllProviderModels\\('bai',false\\)"/.test(ah9c), '');
check('内置 cline 表头不带批量按钮', ah9c.indexOf("toggleAllProviderModels('cline'") < 0, '');

console.log('\\n[10] 空数据兜底');
pvData = {};
renderModelIndex();
check('无供应商时给提示', /暂无供应商/.test(els.modelIndex._html), els.modelIndex._html.slice(0, 120));
check('无供应商时汇总归零', /0 个供应商/.test(els.modelIndexSummary._text), els.modelIndexSummary._text);

console.log('\\n[11] 错误路径: 加载失败给重试按钮');
pvData = SAMPLE;
renderModelIndex();
els.modelIndex.innerHTML = fail(new Error('boom'), 'loadModelIndex()');
check('失败占位含真实原因', /boom/.test(els.modelIndex._html));
check('失败占位含重试按钮', /重试/.test(els.modelIndex._html) && /loadModelIndex/.test(els.modelIndex._html));

console.log('\\n[12] HTML 转义(防注入)');
pvData = { evil: { builtin: false, display: '<img src=x onerror=1>', apiType: 'openai', enabled: true, keys: 1, baseUrl: 'u', catalog: false, runtime: { configured: true }, catalogModels: [], modelEntries: [{ id: 'a<b', enabled: true }], models: [], google: false } };
renderModelIndex();
const eh = els.modelIndex._html;
check('display 里的 < 被转义', !eh.includes('<img src=x'), (eh.match(/<h3[^>]*>[^<]*</) || ['?'])[0]);
check('display 转义为 &lt;', eh.includes('&lt;img src=x'), '');
check('模型 id 里的 < 被转义', !/<img/.test(eh) && eh.includes('a&lt;b'), (eh.match(/a&lt;b|a<b/) || ['?'])[0]);

console.log('\\n[13] providerModels join 不变量(裸名 ↔ 裸名)');
const jm = providerModels(SAMPLE.cline);
const jmById = id => jm.find(m => m.id === id);
check('catalogModels 与 join 结果一一对应',
  jm.length === SAMPLE.cline.catalogModels.length && SAMPLE.cline.catalogModels.every(c => jmById(c.id)),
  JSON.stringify(jm.map(m => m.id)));
check('下线模型 on=false 正确传递', jmById('old-removed-model').on === false, String(jmById('old-removed-model').on));
check('附加字段(cost) 未被 join 丢掉', jmById('deepseek/deepseek-v4-flash').cost === 'free',
  String(jmById('deepseek/deepseek-v4-flash').cost));
check('附加字段(status) 未被 join 丢掉', jmById('deepseek/deepseek-v4-flash').status === 'active',
  String(jmById('deepseek/deepseek-v4-flash').status));
check('models[].model 是裸名(与 catalogModels[].id 同一命名空间)',
  SAMPLE.cline.models.every(m => !m.id.includes(m.model + ':') && m.model.indexOf('cline:') !== 0),
  JSON.stringify(SAMPLE.cline.models.map(m => m.model)));
const gm = providerModels(SAMPLE.gemini);
check('单模型上游 join 无丢失', gm.length === 1 && gm[0].id === 'gemini-3.8-flash' && gm[0].on === true,
  JSON.stringify(gm));

console.log('\\n[14] 值断言: 渲染出的是真实文本, 不是空壳');
pvData = SAMPLE;
renderModelIndex();
// 表头也带 text-align:right, 所以取全部匹配再跳过表头(第 1 个)去看行值。
const syncVals = [...cardHtml('cline').matchAll(/text-align:right">([^<]*)[<]/g)].map(r => r[1]);
check('cline 同步时间列渲染出真实时间(非 "-")',
  syncVals.length >= 3 && syncVals.slice(1).every(v => /\\d{1,2}:\\d{2}:\\d{2}/.test(v)),
  JSON.stringify(syncVals));
check('cline 同步时间不是 Invalid Time', !/Invalid/.test(cardHtml('cline')));
check('cline 费用列显示「免费」', /model-tag free">免费/.test(cardHtml('cline')));
check('cline 状态列显示「可用」', /model-tag[^>]*">可用/.test(cardHtml('cline')));
check('cline 流式标记存在', /需要流式响应">流式/.test(cardHtml('cline')));
check('cline 已下架行渲染出「已下架」', /已下架[^<]*[<]/.test(cardHtml('cline')));
check('cline 说明含上次同步时间', /上次同步 2026\\/9\\/13/.test(cardHtml('cline')),
  (cardHtml('cline').match(/上次同步[^<]*/) || ['未找到'])[0]);
check('opencode 上下文列显示 1,000,000', cardHtml('opencode').includes('1,000,000'),
  (cardHtml('opencode').match(/<td style="font-size:var\\(--fs-sm\\)">[^<]*[<]/g) || []).join(' | '));
check('opencode 最大输出列显示 65,536', cardHtml('opencode').includes('65,536'));
check('opencode 每行都有两个数值列', (cardHtml('opencode').match(/font-size:var\\(--fs-sm\\)/g) || []).length === 4,
  (cardHtml('opencode').match(/font-size:var\\(--fs-sm\\)/g) || []).join(' | '));
check('每行 data-mrow 数量 = 模型数',
  (cardHtml('bai').match(/data-mrow/g) || []).length === 2 &&
  (cardHtml('cline').match(/data-mrow/g) || []).length === 2 &&
  (cardHtml('opencode').match(/data-mrow/g) || []).length === 2,
  ['bai', 'cline', 'opencode'].map(n => n + '=' + (cardHtml(n).match(/data-mrow/g) || []).length).join(' '));
check('data-row-search 与 data-model 都走 escAttr',
  /data-row-search="bai:glm-5\\.3-flash"/.test(cardHtml('bai')) && /data-model="glm-5\\.3-flash"/.test(cardHtml('bai')),
  (cardHtml('bai').match(/data-model="[^"]*"/) || ['?'])[0]);

console.log('\\n[15] title 属性上下文 XSS(堵 [12] 的盲区)');
check('esc 不转义引号(与生产一致, 否则下面的断言是假的)', esc('a"b') === 'a"b', JSON.stringify(esc('a"b')));
check('escAttr 会转义双引号与单引号', escAttr('a"b') === 'a&quot;b' && escAttr("a'b") === 'a&#39;b',
  JSON.stringify([escAttr('a"b'), escAttr("a'b")]));
// configured 必须为 true, 否则 providerStat 先返回「未配置 key」, 走不到 error 分支。
pvData = { q: { builtin: false, display: 'A', apiType: 'openai', enabled: true, keys: 1, baseUrl: 'u', catalog: false,
  runtime: { configured: true, error: '上游返回 " + document.cookie + "' },
  catalogModels: [], models: [], google: false } };
renderModelIndex();
const qc = cardHtml('q');
const qt = (qc.match(/<span class="pbadge pb-off" title="([^>]*)">/) || [])[1];
check('title 属性值里没有裸双引号', qt !== undefined && qt.indexOf('"') < 0, JSON.stringify(qt));
check('title 属性值里的引号被转成 &quot;', qt !== undefined && qt.includes('&quot;'), JSON.stringify(qt));
check('正文里的引号按文本显示(不转义是正常的)', qc.includes('" + document.cookie'), '');
check('h3 的 title 同样走 escAttr', /<h3 title="A">/.test(qc));

console.log('\\n[16] filterCardModels 显隐逻辑(直接单测, 不经 DOM 遍历)');
const rowOf = s => ({ style: {}, getAttribute: k => (k === 'data-row-search' ? s : null) });
const cardWith = rows => ({ querySelectorAll: () => rows });
const inputWith = (v, card) => ({ value: v, closest: () => card });
let r1 = rowOf('bai:glm-5.3-flash'), r2 = rowOf('bai:glm-4-air');
filterCardModels('bai', inputWith('', cardWith([r1, r2])));
check('空查询: 两行都显示', r1.style.display === '' && r2.style.display === '', JSON.stringify([r1.style.display, r2.style.display]));
filterCardModels('bai', inputWith('glm-5', cardWith([r1, r2])));
check('命中 glm-5: 只留命中行', r1.style.display === '' && r2.style.display === 'none',
  JSON.stringify([r1.style.display, r2.style.display]));
let r3 = rowOf('glm-4-air'), r4 = rowOf('deepseek-v4-flash');
filterCardModels('bai', inputWith('不存在', cardWith([r3, r4])));
check('无命中: 全部隐藏(勾选态不丢, 因为不重绘)', r3.style.display === 'none' && r4.style.display === 'none',
  JSON.stringify([r3.style.display, r4.style.display]));
check('card 为 null 时安全返回', (function () { try { filterCardModels('bai', inputWith('x', null)); return true; } catch (e) { return false; } })());

console.log('\\n[17] 显式盲区: 以下交互函数本脚本未执行');
const UNCOVERED = ['filterModelIndex', 'filterCardModels', 'toggleProviderModel', 'toggleModel',
  'saveProvider', 'editProvider', 'delProvider', 'testProviderByName', 'refreshOneCatalog',
  'refreshModels', 'refreshOcModels', 'saveOcConfig', 'loadOcRegions', 'loadOcStats', 'loadAccounts',
  'saveHeaders', 'saveRouter', 'renderRouter', 'switchTab', 'copyText', 'api', 'toast'];
const missing = UNCOVERED.filter(fn => !new RegExp('\\\\bfunction\\\\s+' + fn + '\\\\b|\\\\bconst\\\\s+' + fn + '\\\\s*=').test(script));
check('盲区清单里的函数都仍存在于源码(别让人以为已删掉)', missing.length === 0, '缺失: ' + JSON.stringify(missing));
console.log('  未覆盖(需真实浏览器/jsdom): ' + UNCOVERED.join(', '));
const _child = new El('c'), _par = new El('p');
_par.className = 'pi'; _child.parentElement = _par;
check('El.closest 能沿父链找到卡片(保证 [16] 不是跑在空替身上)', _child.closest('.pi') === _par);
check('El.closest 找不到时返回 null', _child.closest('.nope') === null);

console.log('\\n[18] providerModels 三来源归一化 + 手工模型不丢(堵 §6 盲区)');
// 1) catalogModels 非空 -> 用它, on 由 disabled===false 推出
const pm1 = providerModels({ builtin:false, catalogModels:[{id:'a',disabled:false},{id:'b',disabled:true}], models:[] });
check('catalog 非空: 用 catalog, on=!disabled', pm1.length===2 && pm1[0].on===true && pm1[1].on===false, JSON.stringify(pm1));
// 2) catalog 空 + modelEntries 非空 -> 用 modelEntries, on 由 enabled 推出
const pm2 = providerModels({ builtin:false, catalogModels:[], modelEntries:[{id:'a',enabled:true},{id:'b',enabled:false}], models:[] });
check('modelEntries: on=enabled', pm2.length===2 && pm2[0].on===true && pm2[1].on===false, JSON.stringify(pm2));
// 3) 都空 -> 回退 Object.keys(meta), 全部 on:true
const pm3 = providerModels({ builtin:false, catalogModels:[], modelEntries:[], models:[{model:'x'},{model:'y'}] });
check('双空回退 meta 全 on', pm3.length===2 && pm3.every(m => m.on===true), JSON.stringify(pm3));
// 4) 最有价值: 用户手工填的私有模型(只在 models 里, 不在上游目录) 不从卡片消失。
//    —— 即便走目录(catalog 非空), 目录里没有的私有模型也应被补回来(§6 的诚实盲区)。
const hand = { builtin:false, display:'H', catalogModels:[{id:'up-a',disabled:false}], modelEntries:[], models:[{model:'up-a'},{model:'private-1'}] };
const pm4 = providerModels(hand);
check('私有手工模型出现在卡片(目录优先但保留手工)', pm4.some(m => m.id==='private-1') && pm4.find(m => m.id==='private-1').on===true, JSON.stringify(pm4));
// 5) §6 原文场景: catalog 为空时, 手工模型同样不丢
const hand2 = { builtin:false, display:'H2', catalogModels:[], modelEntries:[], models:[{model:'private-2'}] };
const pm5 = providerModels(hand2);
check('catalog 空: 手工模型仍不丢', pm5.some(m => m.id==='private-2') && pm5.find(m => m.id==='private-2').on===true, JSON.stringify(pm5));
// 6) 端到端: 真正渲染一张含手工模型的卡片, 确认它出现在 DOM 里
pvData = { hand: hand };
renderModelIndex();
check('卡片渲染含手工私有模型行', els.modelIndex._html.indexOf('private-1') >= 0 && els.modelIndex._html.indexOf('data-row-search="hand:private-1"') >= 0, '');

console.log('\\n[19] 出口地区勾选面板 + 订阅折叠(替换节点列表)');
// 1) 地区面板: 7 个固定地区, 未勾选=不限制
ocRegionStats = [
  { id: 'us', label: '美国', total: 120, ok: 30 },
  { id: 'jp', label: '日本', total: 80, ok: 12 },
  { id: 'tw', label: '台湾', total: 40, ok: 8 },
  { id: 'hk', label: '香港', total: 60, ok: 20 },
  { id: 'sg', label: '新加坡', total: 50, ok: 15 },
  { id: 'eu', label: '欧洲', total: 200, ok: 40 },
  { id: 'other', label: '其他地区', total: 3000, ok: 5 }
];
ocRegions = [];
renderExitRegions();
const rbHtml = els.ocRegionBox._html;
check('地区面板渲染 7 个复选框', rbHtml.split('type="checkbox"').length - 1 === 7,
  '实际 ' + (rbHtml.split('type="checkbox"').length - 1));
check('每个地区显示 可用/共', rbHtml.indexOf('可用 30 / 共 120') >= 0);
check('未勾选时提示不限制', rbHtml.indexOf('未勾选 = 使用全部地区出口') >= 0);
check('面板不再罗列单个节点(无 exitIp 字段)', rbHtml.indexOf('exitIp') < 0 && rbHtml.indexOf('latencyMs') < 0);

// 2) 勾选: 单选 / 多选 / 清除
toggleExitRegion('us', true);
check('勾选美国 -> ocRegions=[us]', ocRegions.length === 1 && ocRegions[0] === 'us', JSON.stringify(ocRegions));
check('勾选后提示仅使用所选地区', els.ocRegionBox._html.indexOf('仅使用: 美国') >= 0);
toggleExitRegion('tw', true);
check('多选: 美国+台湾(保持界面顺序)', ocRegions.join(',') === 'us,tw', JSON.stringify(ocRegions));
check('多选提示含两个地区', els.ocRegionBox._html.indexOf('仅使用: 美国、台湾') >= 0);
toggleExitRegion('us', false);
check('取消美国后只剩台湾', ocRegions.join(',') === 'tw', JSON.stringify(ocRegions));
toggleExitRegion('', false);
check('清除限制后为空(不限制)', ocRegions.length === 0);
check('清除后回到不限制文案', els.ocRegionBox._html.indexOf('未勾选 = 使用全部地区出口') >= 0);

// 2b) 勾了地区但该地区出口数为 0 时要显式告警(后端此时会临时回退全部出口)
ocRegionStats = ocRegionStats.map(r => r.id === 'us' ? { id: 'us', label: '美国', total: 0, ok: 0 } : r);
ocRegions = [];
toggleExitRegion('us', true);
const warnHtml = els.ocRegionBox._html;
check('所选地区出口数为 0 时给红色告警', warnHtml.indexOf('所选地区当前没有出口') >= 0, warnHtml.slice(0, 200));
ocRegions = [];
renderExitRegions();
check('取消勾选后告警消失', els.ocRegionBox._html.indexOf('所选地区当前没有出口') < 0);

// 3) 订阅区整体折叠: 默认收起(toggle 按钮 + 条数摘要), 展开后单条链接平铺显示
//    (完整地址 + 抓取状态 + 删除按钮) —— 不做逐条折叠。
const SUB_URL = 'https://misub.bursaonline.eu/920731/rq?clash';
ocSubsArr = [SUB_URL];
ocSubsStatus = {};
ocSubsStatus[SUB_URL] = '🟢 09-14 19:23 · 98 节点';
ocSubsAreaOpen = false;
renderOcSubs();
check('默认收起: 列表不渲染(避免长平铺)', els.ocSubsList._html === '', els.ocSubsList._html.slice(0, 120));
check('默认收起: 区域容器隐藏', els.ocSubsArea.style.display === 'none', String(els.ocSubsArea.style.display));
check('默认收起: 按钮显示 展开', els.ocSubsToggle._text === '展开', els.ocSubsToggle._text);
check('摘要显示订阅条数', els.ocSubsSummary._text === '1 条订阅', els.ocSubsSummary._text);
toggleOcSubsArea();
const subOpen = els.ocSubsList._html;
check('展开后显示完整地址', subOpen.indexOf(SUB_URL) >= 0, subOpen.slice(0, 160));
check('展开后带抓取状态与节点数', subOpen.indexOf('98 节点') >= 0);
check('展开后出现删除按钮', subOpen.indexOf('delOcSub(0)') >= 0);
check('展开后是无点击折叠的平铺行(无 toggleOcSub)', subOpen.indexOf('toggleOcSub') < 0);
check('展开后区域容器显示', els.ocSubsArea.style.display === '', String(els.ocSubsArea.style.display));
check('展开后按钮显示 收起', els.ocSubsToggle._text === '收起', els.ocSubsToggle._text);
toggleOcSubsArea();
check('再点一次整区收回(完整地址消失)', els.ocSubsList._html.indexOf(SUB_URL) < 0);
check('收回后按钮回到 展开', els.ocSubsToggle._text === '展开', els.ocSubsToggle._text);
// 无订阅时摘要给出明确文案
ocSubsArr = [];
renderOcSubs();
check('无订阅时摘要为 暂无订阅', els.ocSubsSummary._text === '暂无订阅', els.ocSubsSummary._text);
// 有订阅但尚未抓取: 摘要要带出来(收起时不至于看不到)
ocSubsArr = [SUB_URL];
ocSubsStatus = {};
renderOcSubs();
check('未抓取时摘要提示 · 未抓取', els.ocSubsSummary._text === '1 条订阅 · 未抓取', els.ocSubsSummary._text);
ocSubsAreaOpen = false;   // 还原为默认收起, 不影响后续用例
renderOcSubs();
ocSubsArr = [];

console.log('\\n' + (FAILS === 0 ? '=== ALL PASS ===' : '=== ' + FAILS + ' FAILURES ==='));
process.exit(FAILS === 0 ? 0 : 1);
`;

// ---- 载入被测 JS, 替换 esc 实现 ----
// ★ 与生产 admin_html.go 里的 esc 完全同义: 只转 & < >, **不转引号**。
// 之前的 stub 多转了 " —— 那会让 title 属性的双引号闭合注入被测试掩盖掉。
let js = script.replace(
  /const esc = s => \{ const d=document\.createElement\('div'\); d\.textContent=s\|\|''; return d\.innerHTML; \};/,
  "const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');"
);
if (js === script) {
  console.error('FATAL: esc 实现未替换成功, DOM stub 无法运行');
  process.exit(2);
}
js += '\n/* ===== TEST PROBE ===== */\n' + PROBE;

eval(js);

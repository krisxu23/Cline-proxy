package app

// adminHTML 分段 2/4: 自动路由页(含路由预演) + 设置页。
// 由 admin_html.go 拆分而来(P2-21): 单文件近 2900 行的原始字符串难以评审,
// 按面板边界切成多段常量, 拼接结果与拆分前逐字节一致; 原始字符串内
// 仍然禁止出现反引号(会终止字符串)。
const adminHTMLPart2 = `<div id="tab-router" class="tab-panel" style="display:none">
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
        <div style="display:flex;gap:var(--sp-2)">
          <input id="arAlias" placeholder="auto-router" oninput="renderRouterExample()">
          <button type="button" class="btn btn-primary" style="width:auto;white-space:nowrap" onclick="saveRouter()">💾 保存</button>
        </div>
      </div>
      <div class="field" style="flex:1.4;min-width:300px"><label>客户端调用示例</label>
        <input id="arExample" readonly onclick="this.select()" style="font-family:var(--font-mono)">
      </div>
    </div>
    <p class="hint" style="margin:0">改名后点「保存」即可：旧名字下的勾选会自动迁移到新名字，已发布给客户端的旧模型名仍然可用（兼容保留）。</p>
  </div>
</div>

<div class="section">
  <div class="section-title">🏭 网关已加入的供应商
    <span style="margin-left:auto;display:flex;gap:var(--sp-2)">
      <button type="button" class="btn btn-sm" onclick="routerSelectAll(true)">全选</button>
      <button type="button" class="btn btn-sm" onclick="routerSelectAll(false)">全不选</button>
      <button type="button" class="btn btn-sm" onclick="refreshRouterCatalogs()">🔄 刷新全部目录</button>
    </span>
  </div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 var(--sp-3)">
      下面是网关探测到的全部供应商。勾选要参与自动路由的供应商，其模型会出现在下一节供逐项勾选。
    </p>
    <div id="arProviderList" style="display:flex;flex-direction:column;gap:var(--sp-2)">加载中...</div>
  </div>
</div>

<div class="section">
  <div class="section-title">🧩 参与自动路由的模型</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 var(--sp-3)">
      只有勾选的模型会参与。若一个都不勾，自动路由会回落到「全部供应商的全部免费模型」。
    </p>
    <div id="arModelList">加载中...</div>
    <div class="hint" id="arSelectionWarn" style="margin-top:var(--sp-2);font-size:var(--fs-xs);color:var(--text3)"></div>
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
    <div class="hint" style="margin-top:6px;font-size:var(--fs-xs);color:var(--text3)">
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
    <div class="hint" id="arUsageInfo" style="margin-top:6px;font-size:var(--fs-xs);color:var(--text3)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🛠 候选维护
    <span style="margin-left:auto;display:flex;gap:var(--sp-2)">
      <button type="button" class="btn btn-sm" onclick="routerMaintenance('cooling')">解除全部冷却</button>
      <button type="button" class="btn btn-sm" onclick="routerMaintenance('permanent')">清空永久剔除</button>
    </span>
  </div>
  <div class="section-body">
    <div class="form-row" style="margin-top:14px">
      <div class="field"><label>冷却中的候选</label>
        <div id="arCoolingBox" style="border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--inset);min-height:42px"></div>
      </div>
      <div class="field"><label>永久剔除的候选</label>
        <div id="arPermBox" style="border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--inset);min-height:42px;max-height:220px;overflow-y:auto"></div>
      </div>
    </div>
    <div style="margin-yle="margin-top:12px;border-top:1px solid var(--border);padding-top:10px">
      <div class="hint" style="margin-bottom:4px">🔎 路由预演（不发请求，试算这个名字会走哪些站、哪些被跳过）</div>
      <div style="display:flex;gap:var(--sp-2)">
        <input id="previewModel" placeholder="组合名 / 别名 / zen:xxx 等任意模型名" style="flex:1" onkeydown="if(event.key==='Enter'){previewRoute();}" />
        <button class="btn" onclick="previewRoute()">预演</button>
      </div>
      <div id="previewResult" style="margin-top:8px;font-size:var(--fs-xs)"></div>
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
    <div id="keyGenResult" style="margin-top:var(--sp-2)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">📨 请求头配置（模拟 Cline CLI 发出）</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>版本对齐方式</label>
        <div style="display:flex;gap:var(--sp-2);align-items:center">
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
    <div id="headerSaveResult" style="margin-top:var(--sp-2)"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🌐 出口代理与节点</div>
  <div class="section-body">
    <p class="hint" style="margin:0 0 14px">出口模式作用于整个网关：<strong style="color:var(--text)">所有上游、所有模型</strong>（Cline 账号池 / opencode / 通用 Provider / 订阅抓取）共用同一套出口决策。</p>
    <div class="form-row">
      <div class="field"><label>代理策略</label>
        <select id="ocStrategy"><option value="round_robin">轮询 round_robin</option><option value="random">随机 random</option><option value="fill">固定 fill</option><option value="latency">延迟优先 latency</option></select>
      </div>
      <div class="field"><label>出口模式</label>
        <select id="ocExitMode">
          <option value="proxy">节点出口（全部走下面节点列表）</option>
          <option value="direct">直连（不走任何节点）</option>
        </select>
      </div>
      <div class="field"><label>粘性会话</label>
        <select id="ocSticky">
          <option value="false">关闭（每次请求按策略选出口）</option>
          <option value="true">开启（同一客户端 30 分钟内固定同一出口）</option>
        </select>
      </div>
      <div class="field"><label>节点全挂时</label>
        <select id="ocRescue">
          <option value="true">允许直连兜底（推荐，经 sing-box 的 direct 出站）</option>
          <option value="false">严格走节点（一个可用节点都没有就直接失败）</option>
        </select>
      </div>
    </div>
    <p class="hint" style="margin:0 0 14px;font-size:var(--fs-xs);color:var(--text3)">
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
    <p class="hint" style="margin:0 0 14px;font-size:var(--fs-xs);color:var(--text3)">
      用于解析 <strong style="color:var(--text)">节点服务器域名</strong> 与直连目标域名；代理请求的目标域名仍由节点侧解析（本地不解析，无污染）。
      默认不用明文 8.8.8.8 —— 它在国内常被污染，表现为"节点时通时不通"。
    </p>
    <div class="form-row">
      <div class="field"><label>代理列表</label>
        <textarea id="ocProxies" rows="4" placeholder="每行一个: http://user:pass@host:port / socks5://host:port&#10;或节点链接: vmess:// vless:// trojan:// ss:// hy2:// tuic:// hysteria:// anytls:// ssh:// shadowtls:// snell://"></textarea>
      </div>
    </div>
    <div class="form-row">
      <div class="field">
        <label style="display:flex;align-items:center;gap:var(--sp-2)">
          <span>订阅链接</span>
          <button type="button" class="btn" id="ocSubsToggle" style="flex:none;padding:2px 8px;font-size:var(--fs-xs)" onclick="toggleOcSubsArea()">展开</button>
          <span id="ocSubsSummary" style="font-weight:400;font-size:var(--fs-xs);color:var(--text3)"></span>
        </label>
        <div id="ocSubsArea" style="display:none">
          <div id="ocSubsList" style="display:flex;flex-direction:column;gap:6px;margin-bottom:var(--sp-2)"></div>
          <div style="display:flex;gap:var(--sp-2)">
            <input id="ocSubNew" placeholder="https://订阅地址" style="flex:1" />
            <input id="ocSubRefresh" type="number" min="1" max="43200" title="自动刷新间隔（分钟）" placeholder="30" style="width:96px;flex:none" />
            <span style="align-self:center;font-size:var(--fs-xs);color:var(--text3);flex:none">分钟刷新</span>
            <button type="button" onclick="addOcSub()" style="flex:none;padding:9px 14px">添加</button>
          </div>
          <div class="hint" id="ocSubsInfo" style="margin-top:6px;font-size:var(--fs-xs);color:var(--text3);white-space:pre-wrap"></div>
        </div>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label style="display:flex;align-items:center;justify-content:space-between">出口地区（勾选后全网关只走所选地区的出口）
        <button type="button" id="ocCheckBtn" class="btn" style="padding:3px 10px;font-size:var(--fs-xs)" onclick="refreshOcNodes()">连通检测</button></label>
        <div id="ocRegionBox" style="border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--inset)"></div>
      </div>
    </div>
    <div class="form-row">
      <div class="field"><label>代理冷却</label>
        <div id="ocCooldownBox" style="max-height:190px;overflow-y:auto;border:1px solid var(--border);border-radius:var(--radius-sm);background:var(--inset);min-height:42px"></div>
      </div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">💾 保存出口配置</button></div>
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
`

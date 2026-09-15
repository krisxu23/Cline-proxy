package app

// adminHTML 分段 3/4: 请求日志页(筛选/分页/详情抽屉)。
// 由 admin_html.go 拆分而来(P2-21): 单文件近 2900 行的原始字符串难以评审,
// 按面板边界切成多段常量, 拼接结果与拆分前逐字节一致; 原始字符串内
// 仍然禁止出现反引号(会终止字符串)。
const adminHTMLPart3 = `<div id="tab-logs" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:var(--sp-4)">
  <h2>📜 请求日志 <span class="probe-pill" style="font-weight:normal">最近 500 条，落盘 data/requests.jsonl；点任意行看详情与路由决策</span></h2>
  <div style="display:flex;gap:var(--sp-2)">
    <button class="btn btn-sm" onclick="loadLogs()">🔄 刷新</button>
  </div>
</div>
<div class="section">
  <div class="section-body" style="padding:6px">
    <div style="display:flex;gap:var(--sp-2);flex-wrap:wrap;align-items:center;margin:6px 0 10px">
      <input id="logQ" placeholder="关键词: id/模型/路径/错误…" style="flex:1;min-width:180px" onkeydown="if(event.key==='Enter'){logPage=1;loadLogs();}" />
      <input id="logModel" placeholder="模型过滤" style="width:150px" onkeydown="if(event.key==='Enter'){logPage=1;loadLogs();}" />
      <select id="logUpstream" onchange="logPage=1;loadLogs();" style="width:150px">
        <option value="">全部上游</option>
        <option value="zen">zen</option>
        <option value="cline">cline 池</option>
        <option value="clinepass">clinepass</option>
      </select>
      <select id="logStatus" onchange="logPage=1;loadLogs();" style="width:110px">
        <option value="">全部状态</option>
        <option value="ok">仅成功</option>
        <option value="err">仅失败</option>
      </select>
      <button class="btn btn-sm" onclick="logPage=1;loadLogs()">查询</button>
    </div>
    <div class="table-wrap">
    <table>
      <thead>
        <tr><th>时间</th><th>来源</th><th>方法</th><th>路径</th><th>模型 → 实际</th><th>上游</th><th>状态</th><th>耗时/TTFT</th><th>tokens</th><th>摘要</th></tr>
      </thead>
      <tbody id="logsTableBody">
        <tr><td colspan="10" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    </div>
    <div style="display:flex;justify-content:space-between;align-items:center;margin-top:8px;font-size:var(--fs-xs);color:var(--text3)">
      <span id="logsPaging"></span>
      <div style="display:flex;gap:var(--sp-2)">
        <button class="btn btn-sm" onclick="logPagePrev()">‹ 上一页</button>
        <button class="btn btn-sm" onclick="logPageNext()">下一页 ›</button>
      </div>
    </div>
  </div>
</div>
<div id="logDetailPanel" style="display:none;position:fixed;top:0;right:0;bottom:0;width:min(560px,92vw);background:var(--bg2,#111827);border-left:1px solid var(--border);box-shadow:-8px 0 24px rgba(0,0,0,.35);z-index:60;overflow-y:auto;padding:var(--sp-4)"></div>
  </div>
</div>
</div>

<div id="toast" class="toast"></div>
`

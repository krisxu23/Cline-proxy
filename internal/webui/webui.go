// Package webui 管理后台整页资产(HTML + CSS + JS)。
//
// 从 internal/app 抽出(R2 审计后整理): 这 2800 余行是**纯字符串资产**, 不含
// 任何业务逻辑, 与 app 包的其它代码耦合仅为一个导出常量 HTML。单独成包后
// app 包的导航噪音显著下降, 且前端资产可被渲染冒烟测试
// (scripts/admin-render-test.js)按目录直接扫描。
//
// 分段(拼接结果与拆分前逐字节一致):
//
//	page_shell.go         框架/样式/导航 + 仪表盘 + 供应商管理页
//	page_router.go        自动路由页(含路由预演)
//	page_settings_logs.go 设置页 + 请求日志页
//	page_script.go        全部脚本(JS)
//
// 注意: 各分段是 Go **原始字符串**, 其内容与注释里都不能出现反引号,
// 否则会提前终止字符串导致编译失败(历史踩坑)。
package webui

// HTML 管理后台整页。分段常量的拼接顺序即最终页面结构, 调整顺序前先确认
// 各段首尾标签的衔接(page_shell 开 <html>, script_router 收 </body></html>)。
//
// 脚本段(script_*)由 page_script.go 按分节横幅切开, 必须整体按序拼接:
// 各段单独看都不是完整 JS, 跨段引用是常态。
const HTML = htmlShell +
	htmlRouter +
	htmlSettingsLogs +
	scriptCore +
	scriptAccounts +
	scriptLogs +
	scriptConfig +
	scriptProviders +
	scriptRouter

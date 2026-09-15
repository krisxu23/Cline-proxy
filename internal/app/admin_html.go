package app

// adminHTML 管理后台整页(HTML+CSS+JS)。
// P2-21 拆分: 原先是单个近 2900 行文件里的巨型原始字符串, 按"面板边界"
// 切成四个分段常量分别维护, 拼接结果与拆分前逐字节一致:
//
//	part1 框架/样式/导航 + 仪表盘 + 供应商管理页
//	part2 自动路由页(含组合模型与路由预演)
//	part3 设置页 + 请求日志页
//	part4 全部脚本(JS)
//
// 注意: 各分段仍是 Go 原始字符串, 注释与内容里不能出现反引号。
const adminHTML = adminHTMLPart1 +
	adminHTMLPart2 +
	adminHTMLPart3 +
	adminHTMLPart4

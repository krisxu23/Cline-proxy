package app

// 内嵌 IANA 时区库。
//
// 本程序是单 exe 分发: 候选层冷却要按 Google 免费额度的重置点(太平洋时间)计算,
// 配额账本要按本地时区(默认 Asia/Shanghai)划分日界。二者都不能依赖
// 运行机器上是否装有 zoneinfo —— Windows 上没有, 发行版之间路径也不一致。
// 内嵌后 time.LoadLocation 在任何机器上都返回真实时区(含夏令时规则),
// 代价是二进制增大约 450KB。
import _ "time/tzdata"

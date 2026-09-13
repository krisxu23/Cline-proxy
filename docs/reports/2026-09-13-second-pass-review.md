# Cline-proxy 二轮审查（P0-3 报告修复之后）

- 审查对象：`krisxu23/Cline-proxy`，`2026-09-13-iterative-review.md` 之后
- 说明：本报告**只列已修复报告之外**的问题。已修完的 P0/P1/P2 不重复。

## 结论摘要

| 级别 | 数量 | 说明 |
|---|---|---|
| P0（可崩进程 / 可丢数据） | 3 | stats 两处、账号 refresh 判定 |
| P1（真 bug，影响可用性 / 性能 / 安全） | 7 | 令牌、pool 解析、CORS、CORS 泄露点、日志、I/O、限流识别 |
| P2（代码质量 / 语义） | 3 | 关键词死代码、middleware 内存、handler 手写 CORS |

其中最严重的是 **stats 相关的数据竞争**：`zenStatsSnapshot` 返回裸指针后 admin 侧 JSON marshal，与 `recordZenStats` 并发写同一个 `map`，会触发 Go 运行时的 `fatal error: concurrent map read and map write`，`recover` 无效，进程直接退出。这与已修完的 P0-3 是**同一类问题**，只是漏在了 stats 侧。

---

## 一、P0

### P0-1 `loadStatsFromFile` 无锁写 `statsToday`（并发 map 写）

`internal/app/stats.go:169-188`：

```go
func loadStatsFromFile(path string) *zenStatsAgg {
    agg := newZenStatsAgg()
    ...
    for _, line := range lines {
        ...
        aggregateRecord(agg, &rec)
        if time.UnixMilli(rec.TS).Format("2006-01-02") == today {
            aggregateRecord(statsToday, &rec)   // ← 无 statsAggMu 保护
        }
    }
    return agg
}
```

- `statsToday` 是由 `statsAggMu` 保护的全局变量；`recordZenStats` 在 `statsAggMu` 下 `aggregateRecord(statsToday, ...)`。
- `aggregateRecord` 会向 `statsToday.ByModel` / `statsToday.ByUpstream`（`map[string]*zenStatsModel`）**新增键并写入**。
- `initStats()` 被 `statsInitMu` 保护，`loadStatsFromFile` 就在 `initStats` 内被调用；但 `statsInitMu` 与 `statsAggMu` 是**两把独立的锁**，锁不通用。
- 触发路径：启动时并发请求进来，某个 handler 走 `initStats` 尚未完成时，另一个 handler 已经通过 `recordZenStats` 拿到 `statsAggMu` 并开始写 `statsToday.ByModel`。两边并发写同一个 map → Go 抛 `fatal error`，进程退出。

**修复方向**：`loadStatsFromFile` 里对 `statsToday` 的写入要么整体挪到 `statsAggMu` 内，要么先构造好一份 "今日聚合"，再在 `statsAggMu` 内一次性 swap 进 `statsToday`。

### P0-2 `zenStatsSnapshot` 返回裸指针 → admin 侧 marshal 撞 map 写

`internal/app/stats.go:268-276`：

```go
func zenStatsSnapshot() map[string]any {
    initStats()
    statsAggMu.Lock()
    defer statsAggMu.Unlock()
    return map[string]any{
        "today": statsToday,   // ← 裸指针
        "total": statsTotal,   // ← 裸指针
    }
}
```

- `statsToday` / `statsTotal` 的 `ByModel` / `ByUpstream` 是 `map`。
- `handleZenStats` / `handleAdminStats`（admin.go）拿到这个 map 之后就在锁外做 `json.Marshal`。
- 只要面板打开时恰好有请求在记账，`recordZenStats` 会持 `statsAggMu` 写 `statsToday.ByModel`；同时 admin 无锁遍历这个 map → **concurrent map read and map write → fatal error**。
- 触发门槛极低：面板开着 + 任何一次上游请求成功记账。这是**日常运行就能踩到**的场景。

**修复方向**：`zenStatsSnapshot` 返回深拷贝（`ByModel` / `ByUpstream` 逐层 clone），或者 admin 侧改成走一个只读快照接口。参照 `config_clone.go` 里 `zenConfigData.clone()` 的写法。

### P0-3 `refreshAccountToken` 把所有错误都当成 "expired"

`internal/app/pool.go:154-174`：

```go
func refreshAccountToken(acc *Account) error {
    accessToken, refreshTokenOut, expiresAt, err := refreshAccountTokenFn(acc.RefreshToken)
    if err != nil {
        poolMu.Lock()
        acc.Status = "expired"       // ← 一律 expired
        savePoolLocked()
        poolMu.Unlock()
        return fmt.Errorf("token refresh failed: %w", err)
    }
    ...
}
```

- `refreshAccountTokenFn` 的错误可能是网络错误、上游 5xx、上游 4xx、上游限流 —— 全被这一处判成 expired。
- 后果：一次节点抖动 → 账号永久 expired，用户必须去 UI 手动"重置"才能恢复。多账号池场景下，几次网络抖动把池子全清空。
- `callClineAPI` 的 401 分支（proxy.go:800-826）也在这个基础上再次把 acc 标 expired，但语义一样过度。

**修复方向**：区分三类错误：
- 上游返回 401/403 明确说 refreshToken 无效 → `Status = "expired"`；
- 网络错误 / 5xx → `markAccountCooldown(acc, "refresh failed: "+err.Error(), 5*time.Minute)`；
- 其他 → 保守起见短冷却 + 记日志。

具体做法是让 `refreshAccountTokenFn` 返回一个 typed error（例如 `*upstreamError{Status, Body}`），或者返回 `(result, retryableErr)`，pool 侧按类型决定 expired 还是 cooldown。

---

## 二、P1

| # | 问题 | 位置 | 后果 |
|---|---|---|---|
| 1 | `adminTokenCache` 全局字符串无锁读写 | `admin_auth.go:47-73` | 首次并发调用 `loadOrCreateAdminToken` 时，两个 goroutine 都读到 `""`、各自 `rand.Read` 生成 token、各自返回，但只有一个写入 cache。已返回给启动横幅/托盘的 token 可能与 cache 里保存的不一致 → 用横幅里那个 token 请求管理接口会被 401 拒绝 |
| 2 | `loadPool` 解析失败静默清空账号池，不留证 | `pool.go:42-46` | `.cline-accounts.json` 损坏 → 账号全部丢失。与 `loadZenConfig` 的 `quarantineBadConfig(path)` 行为不一致，与审查报告 P1 #16 声明的修复不符 |
| 3 | `LoadRequestLogs` 返回裸 slice | `logs.go:175-179` | admin 侧无锁遍历可能与 `AppendReqLog` 的 append 并发；append 未扩容时写入同一底层 array → data race（TSAN 会报，但不会 fatal） |
| 4 | `LoadRequestLogsFromFile` 启动时 `os.ReadFile` 整个文件 | `logs.go:183-206` | 10MB 上限意味着启动时读入 10MB 内存并逐行解析。启动慢、峰值内存翻倍。可用尾读（读最后 N 行）替代 |
| 5 | `corsHandler` 无条件 `Access-Control-Allow-Origin: *`，`/v1/*` 允许无 API Key | `proxy.go:137-172` + `corsHandler` | 与 P0-2 修的 admin 攻击链同构：`/v1/*` 未配置任何 key 时，任意网页可跨站调用网关、消耗账号额度。README 有警告但代码层没有二次防线 |
| 6 | 三个 handler 手写 `Access-Control-Allow-Origin: *`，绕过 `corsHandler` | `proxy.go:916`、`handleAnthropicStreamWithUsage:1924`、`routing_dispatch.go:140` | 未来统一收紧 CORS 策略时这三处会漏改，形成策略不一致。至少应统一走一个常量/包装函数 |
| 7 | `pickAccount` / `bumpUsage` / `recordAccountTokens` / `markAccountCooldown` 每次调用都落盘 | `pool.go:176-358` | 每成功一次上游请求 → 序列化整个 pool → `WriteFileAtomicDefault` 落盘。多账号池 + 高并发下 `poolSaveMu` 会成为写入热点，磁盘 I/O 成为主瓶颈。改为定期 flush（例如 30s）+ 关键事件立即 flush 即可 |

### 关于 #1 `adminTokenCache` 的展开

```go
var adminTokenCache string

func loadOrCreateAdminToken() string {
    if adminTokenCache != "" {           // ← 无锁读
        return adminTokenCache
    }
    ...
    adminTokenCache = token              // ← 无锁写
    return token
}
```

- 首次启动时启动横幅（`proxy.go:432`）、托盘、`adminStaticHandler`、`adminAuth` 都可能在同一 tick 内调用它。
- 两个 goroutine 都读 `""`、都 `rand.Read` 生成 48 字节的 hex、各自返回。
- **只有最后一个写入 cache 的会被后续校验接受**。横幅里打印的那个 token，如果它没被写入 cache，用户按提示打开 `?token=<那个>` 会得到 401。
- 影响面：只在启动瞬间的并发窗口里。但**只要踩过一次**，用户就要看日志才知道 token 是什么。
- 修复方向：加一把锁 + 生成失败兜底；或者用 `sync.Once`；或者干脆改成 `loadOrCreateAdminToken()` 内部走 `atomic.Value` 缓存。

---

## 三、P2

| 问题 | 位置 | 说明 |
|---|---|---|
| `isRateLimited` 关键词 `resourceexhausted` 死代码 | `zen.go:421-434` | Google API 返回 `RESOURCE_EXHAUSTED`，转小写后是 `resource_exhausted`（带下划线），关键词写成 `resourceexhausted` 永不匹配。当前不影响功能，因为 429 已被 `status == http.StatusTooManyRequests` 命中；但 502/403 分支的语义上就是"用 body 兜底识别限流"，这个关键词应该是"能匹配到"的。 |
| `requestLogMiddleware` 每个请求读整个 body | `logs.go:281-290` | `io.ReadAll(r.Body)` 提取 model 字段后放回。多模态请求（图片 base64 塞进 body）会被完整读入内存两次。可改成只读前 N KB 或流式提取 `model` 字段 |
| `pool.go:85 savePool()` 持 `poolMu` 后再 `poolSaveMu.Lock()` | `pool.go:85-89` | 与 `pickAccount` / `bumpUsage` 等持 `poolMu` 调 `savePoolLocked`（内部 `poolSaveMu.Lock()`）锁顺序一致，不会死锁；但两层嵌套的持锁时长偏长，加剧 P1 #7 的写盘竞争 |

---

## 四、几个"看起来像问题其实不是"的点（避免误改）

- **`corsHandler` 对 `/v1/*` 的 `*`**：LLM 网关的设计就需要第三方客户端跨站调用，加 CORS 是合理的。真正的风险在 P1 #5（未配置 key 时的敞口），不是 CORS 本身。
- **`loadPool` 返回裸指针**：`loadPool()` 返回的 `pool` 是全局单例，其字段由 `poolMu` 保护。调用方如 `for _, a := range p.Accounts` 遍历 slice 时，slice header 引用稳定（append 不会替换原 slice，会扩容新建），只有 `p.Accounts = ...` 整体重新赋值才会破坏。当前代码 `addAccount` / `removeAccount` 都是 `p.Accounts = append(...)` / `p.Accounts = append(p.Accounts[:i], ...)`，**不会**替换底层数组（除非触发扩容）。因此**不 fatal**，但仍然是 data race（TSAN 会报）。建议 `loadPool` 也返回克隆体或提供 `listAccountsSnapshot()`。
- **`pickAccount` 内 `savePoolLocked`**：即使 `fill` / `random` 策略下索引不变，也会落盘。这不是正确性问题，只是 P1 #7 的性能问题。
- **`pickZenProxyWhere` 全池冷却返回 `("", -1)`**：审查报告 P2 里已经修完，`found` 记录逻辑正确。

---

## 五、修复建议（按性价比排序）

1. **P0-1 + P0-2**：把 stats 也走"锁内克隆、锁外读克隆"的规矩。参照 `config_clone.go` 的写法给 `zenStatsAgg` 加 `clone()`；`zenStatsSnapshot` 返回克隆体；`loadStatsFromFile` 里对 `statsToday` 的写入包在 `statsAggMu` 里。**这一项能直接消除两类可崩进程的场景**，优先级最高。
2. **P0-3**：让 `refreshAccountToken` 返回带类型的错误，pool 侧区分 expired 与 cooldown。这是**账号池可用性的分水岭**。
3. **P1 #1**：`adminTokenCache` 加锁或换 `sync.Once`。修复难度低，收益高（消除一类"启动后 admin 401"的诡异问题）。
4. **P1 #2**：把 `quarantineBadConfig` 复用到 `loadPool`。5 行代码的改动。
5. **P1 #7**：pool 写入改批量。这项改动稍大（涉及写盘时机），但对高并发下的稳定性是刚需。
6. 其他 P1 / P2 视优先级排入后续迭代。

---

## 六、验证方式（针对本次新发现）

`-race` 是本机的硬伤（无 gcc），所有 race 结论只能人工走查，无法本地编译验证。CI 里已有的 `-race` job 已经会抓 P0-1 / P0-2 / P1 #1 / P1 #3，只要跑一遍就能锁住。

新增回归测试建议：

| 测试 | 锁住的行为 |
|---|---|
| `TestZenStatsSnapshotReturnsDetachedCopy` | snapshot 里的 `ByModel` / `ByUpstream` 与全局脱钩 |
| `TestConcurrentStatsWriteAndMarshal` | 并发 `recordZenStats` + `zenStatsSnapshot`+`json.Marshal` 不产生 race |
| `TestRefreshAccountTokenNetworkErrorDoesNotExpire` | 上游网络错误时账号状态保持 active，进入 cooldown 而不是 expired |
| `TestLoadPoolCorruptFileQuarantinesAndPreservesEmpty` | 账号池文件损坏时改名留证，池为空而不是丢数据 |
| `TestLoadOrCreateAdminTokenConcurrentFirstCall` | 并发首次调用的返回值全部等于最终 cache 值 |

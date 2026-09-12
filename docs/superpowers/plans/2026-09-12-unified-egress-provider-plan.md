# Unified Egress + Unified Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Proxy必走代理、直连必走直连；供应商统一成一套，内置opencode+cline置顶，对外一个地址+一个key。

**Architecture:** 保留进程内sing-box与唯一拨号口zenDialContext；删除model_region全套改为provider可选开关；provider收编为统一结构并保留旧ID别名；key健康抄ai-gateway。

**Tech Stack:** Go 1.24, sing-box(box.New+Start), net/http+h2+uTLS, existing internal/app + internal/kit

## Global Constraints

- 构建标签硬要求：`go build/test -tags "with_quic,with_grpc,with_utls"`，缺标签reality/QUIC节点被剔除。
- Go不在PATH：`export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go"`。
- GitHub一律SSH 443：`git push origin HEAD`即可，失败重试不改代理。
- commit只描述最终状态，不引入新第三方依赖，保持单exe。
- 现有模型ID别名一个不能变：`zen/xxx`、`provider:model`、`cline-pass/xxx`、auto-router/free-best。
- rescueDirect语义保持默认true；strict模式设false即fail-closed。
- 新增日志必须用`reqExitKey(ctx)`回读真实出口，禁全局轮询位置冒充。
- 指定出口的新路径各自新建transport，不走共享zenHTTPClient h2池。
- 不提交二进制与缓存到git。

---

### Task 1: 统一Provider结构 + 别名兼容

**Files:**
- Modify: `internal/app/providers_config.go:25-38`
- Modify: `internal/app/routing_chain.go:24-30`
- Test: `internal/app/unified_provider_test.go`

**Interfaces:**
- Consumes: existing `providerConfig`, `routeCandidate{Upstream,Model}`
- Produces: `func normalizeModelID(id string) (provider, model string)`; `func isBuiltinProvider(id string) bool` (opencode/cline置顶)

- [ ] **Step 1: Write the failing test**

```go
func TestNormalizeModelID(t *testing.T) {
    p, m := normalizeModelID("zen/mimo-v2.5-free")
    if p != "opencode" || m != "mimo-v2.5-free" { t.Fatalf("got %s/%s", p, m) }
    p, m = normalizeModelID("openrouter:z-ai/glm-5.3:free")
    if p != "openrouter" || m != "z-ai/glm-5.3:free" { t.Fatalf("got %s/%s", p, m) }
    p, m = normalizeModelID("cline-pass/deepseek-v4-flash")
    if p != "clinepass" || m != "deepseek-v4-flash" { t.Fatalf("got %s/%s", p, m) }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestNormalizeModelID -v`
Expected: FAIL with "undefined: normalizeModelID"

- [ ] **Step 3: Write minimal implementation**

```go
func normalizeModelID(id string) (string, string) {
    id = strings.TrimSpace(id)
    if strings.HasPrefix(id, "zen/") { return "opencode", strings.TrimPrefix(id, "zen/") }
    if strings.HasPrefix(id, "cline-pass/") { return "clinepass", strings.TrimPrefix(id, "cline-pass/") }
    if i := strings.Index(id, ":"); i > 0 && !strings.Contains(id[:i], "/") { return id[:i], id[i+1:] }
    if i := strings.Index(id, "/"); i > 0 { return id[:i], id[i+1:] }
    return "", id
}
func isBuiltinProvider(id string) bool { return id == "opencode" || id == "cline" || id == "clinepass" }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestNormalizeModelID -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/unified_provider_test.go internal/app/providers_config.go internal/app/routing_chain.go
git commit -m "feat: unify provider model id aliases"
```

### Task 2: Key健康抄ai-gateway（5次冷却5分钟）

**Files:**
- Modify: `internal/app/cooldown.go`
- Test: `internal/app/key_health_test.go`

**Interfaces:**
- Consumes: `normalizeModelID` from Task 1
- Produces: `func recordKeyResult(provider, key string, status int, netErr bool)`; `func pickHealthyKeys(provider string) []string`

- [ ] **Step 1: Write the failing test**

```go
func TestKeyHealthDemoteRecover(t *testing.T) {
    p := "demo"; k := "sk-1"
    for i := 0; i < 5; i++ { recordKeyResult(p, k, 500, false) }
    if len(pickHealthyKeys(p)) != 0 { t.Fatalf("should demote") }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestKeyHealthDemoteRecover -v`
Expected: FAIL with "undefined: recordKeyResult"

- [ ] **Step 3: Write minimal implementation**

```go
var keyHealthMu sync.Mutex
var keyHealth = map[string]map[string]*keyHealthState{}
type keyHealthState struct { failures int; demotedAt int64; lastFailed bool }
func recordKeyResult(provider, key string, status int, netErr bool) {
    if status == 429 { return }
    fail := netErr || status == 401 || status == 403 || status >= 500
    keyHealthMu.Lock(); defer keyHealthMu.Unlock()
    m := keyHealth[provider]; if m == nil { m = map[string]*keyHealthState{}; keyHealth[provider] = m }
    st := m[key]; if st == nil { st = &keyHealthState{}; m[key] = st }
    if fail { st.failures++; st.lastFailed = true; if st.failures >= 5 { st.demotedAt = time.Now().UnixMilli() } } else { delete(m, key) }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestKeyHealth -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/key_health_test.go internal/app/cooldown.go
git commit -m "feat: add per-key health with cooldown recovery"
```

### Task 3: 统一出口（删region特例+fail-closed开关）

**Files:**
- Modify: `internal/app/proxy_pool.go:200-280`
- Modify: `internal/app/model_region.go`
- Test: `internal/app/exit_unified_test.go`

**Interfaces:**
- Consumes: `pickZenProxyWhere`, `rescueDirectEnabled`
- Produces: `func pickUnifiedExit(modelID string) (string, int)` (替代pickZenProxyForModel调用点)

- [ ] **Step 1: Write the failing test**

```go
func TestProxyModePoolEmptyFailsClosed(t *testing.T) {
    no := false
    withTestConfig(t, &zenConfigData{ExitMode: "proxy", RescueDirect: &no})
    if p, _ := pickUnifiedExit("opencode/mimo-v2.5-free"); p != "" { t.Fatalf("must not direct, got %q", p) }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestProxyModePoolEmptyFailsClosed -v`
Expected: FAIL with "undefined: pickUnifiedExit"

- [ ] **Step 3: Write minimal implementation**

```go
func pickUnifiedExit(modelID string) (string, int) {
    if exitModeDirectNow() { return "", -1 }
    p, idx := pickZenProxyWhere(func(q string) bool { return true })
    if p != "" { return p, idx }
    return "", -1
}
```

并将`zenDialContext`中`pickZenProxyForModel`调用改为`pickUnifiedExit`，`p==""`且`!rescueDirectEnabled()`时返回`fmt.Errorf("没有可用节点")`；`model_region.go`改为空壳保留文件避免引用断裂（probe函数直接return）。

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestProxyMode -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/exit_unified_test.go internal/app/proxy_pool.go internal/app/model_region.go
git commit -m "feat: unify egress selection with fail-closed option"
```

### Task 4: 全量回归+冒烟

**Files:**
- Test: existing `./internal/...`

- [ ] **Step 1: Run full tests**

Run: `export PATH="/c/Go/bin:$PATH" GOROOT="C:\\Go" && go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...`
Expected: PASS

- [ ] **Step 2: Commit if green (docs touch)**

```bash
git commit --allow-empty -m "test: unified egress and provider green"
git push origin HEAD
```

注：autogate三批（体卫生/吸收竞速/信誉熔断）按既有`docs/superpowers/plans/2026-09-12-autogate-merge-spec.md`另起后续计划执行，本计划先合入地基。

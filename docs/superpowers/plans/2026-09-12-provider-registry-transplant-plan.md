# Unified Provider Registry (ai-gateway transplant) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provider management becomes one ai-gateway-shaped registry (`{name, baseUrl, apiType, enabled, apiKeys[{key,enabled}], models[{id,enabled}]}`); opencode+cline are pinned builtin rows; all consumers read from it.

**Architecture:** New explicit-models fields added beside legacy free-determination with dual-read precedence (explicit wins); backfill migration on catalog refresh; consumers rewired one by one; legacy branches deleted last. Google endpoint/signature correctness kept; pricing/whitelist/discovery deleted.

**Tech Stack:** Go 1.24, existing internal/app + internal/kit, no new dependencies.

## Global Constraints

- Build/test tags mandatory: `go build/test -tags "with_quic,with_grpc,with_utls"`.
- Go env PowerShell: `$env:PATH = "C:\Go\bin;" + $env:PATH; $env:GOROOT = "C:\Go"`.
- GitHub via SSH 443: `git push origin HEAD`, retry on transient failure, never touch proxy vars/remotes.
- Commits describe final state only (`feat:`/`fix:`), no new third-party deps, single-exe distribution.
- Model ID formats unchanged: `zen/xxx`, `provider:model`, `cline-pass/xxx`, auto-router/free-best aliases.
- rescueDirect default true preserved; per-request real exit via `reqExitKey(ctx)` in any new logging.
- New exit-specific transports only (never shared zenHTTPClient h2 pool) for pinned-exit paths.
- TDD: failing test first, RED verified, then minimal implementation.

---

### Task 1: Explicit data model + dual-read + migration backfill

**Files:**
- Modify: `internal/app/providers_config.go:25-38` (providerConfig struct + freeSet/disabledSet area)
- Modify: `internal/app/providers_catalog.go:595-614` (freeModelIDs), `:89-119` (evalProviderFree)
- Test: `internal/app/providers_test.go`

**Interfaces:**
- Consumes: existing `providerConfig`, `catalogModel`, `freeSet()`
- Produces: `type providerAPIKey struct { Key string \`json:"key"\`; Enabled bool \`json:"enabled"\` }`; `type providerModelEntry struct { ID string \`json:"id"\`; Enabled bool \`json:"enabled"\` }`; `func (c providerConfig) apiKeys() []providerAPIKey` (migrates legacy single APIKey to `[{key,true}]` when list empty); `func (c providerConfig) explicitModels() (map[string]bool, bool)` (nil,false when Models empty = not yet migrated)

- [ ] **Step 1: Write the failing test**

```go
func TestExplicitModelsPrecedence(t *testing.T) {
	setTestProvider(t, "ex", providerConfig{BaseURL: "https://x", APIKey: "k",
		FreeModels: []string{"a", "b"},
		Models:     []providerModelEntry{{ID: "a", Enabled: true}, {ID: "b", Enabled: false}}})
	p := providerByName("ex")
	if !p.isFree("a") {
		t.Fatal("explicit enabled must be free")
	}
	if p.isFree("b") {
		t.Fatal("explicit disabled must not be free")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `$env:PATH = "C:\Go\bin;" + $env:PATH; $env:GOROOT = "C:\Go"; go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestExplicitModelsPrecedence -v`
Expected: FAIL with "unknown field Models" (build failed = missing feature, correct RED)

- [ ] **Step 3: Write minimal implementation**

```go
type providerAPIKey struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}
type providerModelEntry struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}
```

Add to providerConfig: `DisplayName string \`json:"name,omitempty"\``, `APIType string \`json:"apiType,omitempty"\`` ("openai" default, "anthropic" supported), `Enabled *bool \`json:"enabled,omitempty"\`` (nil = true), `APIKeys []providerAPIKey \`json:"apiKeys,omitempty"\``, `Models []providerModelEntry \`json:"models,omitempty"\``, `Migrated bool \`json:"migrated,omitempty"\``. Add `func (c providerConfig) isEnabled() bool { return c.Enabled == nil || *c.Enabled }` and `func (c providerConfig) apiKeys() []providerAPIKey` (returns APIKeys if non-empty else `[{APIKey,true}]` when APIKey != "").

In `freeModelIDs()`: after loading cfg, if `len(cfg.Models) > 0` → candidates = enabled entries only (as catalogModel{ID}), then existing eval filter (keeps rejected/disabledModels honoring during transition). In `evalProviderFree()`: after rejected/disabled checks, if `len(cfg.Models) > 0` → return explicit enabled state for modelID (no catalog lookup).

- [ ] **Step 4: Run test to verify it passes**

Run: same as Step 2.
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/app/providers_config.go internal/app/providers_catalog.go internal/app/providers_test.go
git commit -m "feat: explicit per-model enablement with legacy fallback"
```

### Task 2: Migration backfill on catalog refresh

**Files:**
- Modify: `internal/app/providers_catalog.go` (refreshCatalog success path)
- Test: `internal/app/providers_test.go`

**Interfaces:**
- Consumes: Task 1 explicit fields, existing `refreshCatalog`, `freeModelIDs`
- Produces: `func (p *modelProvider) maybeBackfillExplicitModels()` — after successful refresh, if `!cfg.Migrated` and catalog non-empty: snapshot current `freeModelIDs()` (legacy logic) into `cfg.Models` (all Enabled=true), set Migrated=true, persist via `mutateProvidersConfig`

- [ ] **Step 1: Write the failing test**

```go
func TestMigrationBackfillOnRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "m1"}, {"id": "m2"},
		}})
	}))
	defer srv.Close()
	setTestProvider(t, "mig", providerConfig{BaseURL: srv.URL, APIKey: "k", Catalog: true, FreeModels: []string{"m1"}})
	p := providerByName("mig")
	if err := p.refreshCatalog(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := providerConfigFor("mig")
	if !cfg.Migrated || len(cfg.Models) != 1 || cfg.Models[0].ID != "m1" {
		t.Fatalf("must backfill whitelist into explicit models: %+v", cfg.Models)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestMigrationBackfillOnRefresh -v` (same env prefix as Task 1)
Expected: FAIL (no Migrated field / no backfill)

- [ ] **Step 3: Write minimal implementation**

In `refreshCatalog` success path (after catalog stored, before return nil): call `p.maybeBackfillExplicitModels()`. Implementation reads cfg; if `cfg.Migrated` or catalog empty → return; else compute `p.freeModelIDs()` (legacy branches still intact), map to `[]providerModelEntry{{ID, true}}`, persist with Migrated=true via mutateProvidersConfig. Guard: if computed list empty → do NOT migrate (avoid wiping to empty on transient catalog).

- [ ] **Step 4: Run test to verify it passes**

Run: same as Step 2. Expected: PASS. Also re-run `TestRefreshCatalogPricingMode`, `TestRefreshCatalogPricingFallsBackToWhitelist`, `TestDisabledModelsExclude` — must stay green (legacy intact).

- [ ] **Step 5: Commit**

```bash
git add internal/app/providers_catalog.go internal/app/providers_test.go
git commit -m "feat: backfill explicit model lists on catalog refresh"
```

### Task 3: Multi-key rotation + apiType in Chat + test endpoint

**Files:**
- Modify: `internal/app/providers_chat.go:131-135` (key guard), `:176` (applyAuth call site)
- Modify: `internal/app/providers_config.go` (applyAuth: anthropic `x-api-key` + version header when APIType=="anthropic")
- Modify: `internal/app/admin_providers.go:186-190` (test endpoint key guard)
- Test: `internal/app/providers_chat_test.go`

**Interfaces:**
- Consumes: Task 1 `apiKeys()`, existing `recordKeyResult`/`pickHealthyKeys` (cooldown.go), `keyHealth` states
- Produces: key rotation order = healthy enabled keys shuffled + unhealthy tail (mirror ai-gateway proxy.ts); 429 skips key without penalty; 401/403/5xx + network mark via recordKeyResult

- [ ] **Step 1: Write the failing test**

```go
func TestChatRotatesKeysOnFailure(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	setTestProvider(t, "rk", providerConfig{BaseURL: srv.URL,
		APIKeys: []providerAPIKey{{Key: "bad", Enabled: true}, {Key: "good", Enabled: true}}})
	p := providerByName("rk")
	params := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 1}
	_ = params
	_ = p
	_ = seen
}
```

NOTE to implementer: the sketch above only scaffolds; before GREEN you must complete it into a real assertion (call `p.Chat` in non-stream mode and assert second key eventually succeeds OR assert key-order helper output). A test that asserts nothing is a plan-mandated defect — finish the assertion, do not commit the sketch as-is.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestChatRotatesKeysOnFailure -v`
Expected: FAIL (single-key Chat never tries second key)

- [ ] **Step 3: Write minimal implementation**

In `Chat`: replace single-key guard with `keys := enabledAPIKeys(cfg)` (new helper in providers_config.go: apiKeys() filtered Enabled, healthy-first via pickHealthyKeys ordering, unhealthy tail). Loop keys outer (key attempt) × existing exit-retry inner; on 429 continue to next key without recordKeyResult; on 401/403/5xx + network call recordKeyResult(name, key, status, netErr) then next key; first 200 returns. `applyAuth`: if `cfg.APIType == "anthropic"` set `x-api-key` + `anthropic-version: 2023-06-01`, else `Authorization: Bearer`. Test endpoint: same key loop with 60s ctx (first success returns).

- [ ] **Step 4: Run test to verify it passes**

Run: same + full `./internal/...`. Expected: PASS, no regressions.

- [ ] **Step 5: Commit**

```bash
git add internal/app/providers_chat.go internal/app/providers_config.go internal/app/admin_providers.go internal/app/providers_chat_test.go
git commit -m "feat: multi-key rotation with health and anthropic auth"
```

### Task 4: Delete legacy free-determination (pricing/whitelist/discovery/probe)

**Files:**
- Modify: `internal/app/providers_catalog.go` (delete pricing/allModels branches, evalProviderFree→explicit-only, delete isZeroCost/catalogHasPrices/parsePrice if unused elsewhere)
- Delete: `internal/app/discovery.go` (+ `discovery_test.go` only if fully orphaned — check `freeModelsFor`/discovery callers first: admin_router.go:347 area, providers_chat.go:281 comment)
- Modify: `internal/app/providers_config.go` (delete Catalog/Pricing/AllModels/FreeModels/DisabledModels/ProbeFreeTier/ModelsURL/ModelsKeyHeader/ChatPath/ModelsPath/Headers fields still referenced? KEEP BaseURL/APIKey(single, for migration display)/Catalog(fetch toggle)/Google derivation/implicit catalog fetch; delete rest field by field, compiler-guided)
- Modify: `internal/app/admin_providers.go`, `internal/app/admin_router.go` (freeModelsFor→explicit), `internal/app/routing_chain.go` (candidateSkip reason), `internal/app/admin_html.go` (remove mode dropdown/pricing UI)
- Test: update `providers_test.go`, `providers_google_test.go`, `providers_chat_test.go`, `discovery_test.go` (delete), `admin_router_test.go`, `routing_chain_test.go`, `cooldown_test.go` entries that pin legacy behavior

**Interfaces:**
- Consumes: Tasks 1–3 (explicit lists migrated)
- Produces: `freeModelIDs()` = explicit enabled only; `isFree()` = explicit only; no `evalProviderFree`

- [ ] **Step 1: Write the failing test** (none new — deletion task; gate = existing suite updated)

Run baseline: `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...` — record green set.

- [ ] **Step 2: Delete legacy branches one file at a time, compiler-guided, keeping suite green after each file**

Order: discovery.go + callers → catalog.go branches → config fields → admin/router/chain UI strings. After each file: run full suite; fix callers before proceeding.

- [ ] **Step 3: Run full suite**

Run: `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/...`
Expected: PASS with zero references to Pricing/AllModels/FreeModels/evalProviderFree/discovery (verify via `grep -rn "evalProviderFree\|\.Pricing\|AllModels\|discovery\." internal/app/*.go` returning only test-history comments or nothing)

- [ ] **Step 4: Commit (split if large: backend cutover, then UI removal)**

```bash
git add internal/app/
git commit -m "feat: single explicit-model registry for providers"
```

### Task 5: 模型列表 page becomes the single manager + builtins pinned

**Files:**
- Modify: `internal/app/admin_html.go` (move provider add-form + expandable rows with search + key count/type badges + enabled toggles into 模型列表 page; delete provider section from settings page; provider add = name/baseUrl/key only)
- Modify: `internal/app/admin_providers.go` (GET shape: id/name/apiType/enabled/keys count/models[{id,enabled}]; reject delete of `opencode`/`cline`; seed builtin rows)
- Modify: `internal/app/admin_router.go:89-113` (opencode/cline rows read from same registry: opencode models = zenFreeCatalog incl. health gate; cline = single "*" row gated by clinePoolReady)
- Test: `internal/app/admin_providers_test.go` (builtin delete rejected; GET shape has keys/models/enabled), `internal/app/admin_html_test.go` (div balance, existing)

**Interfaces:**
- Consumes: Tasks 1–4
- Produces: single manager page; settings page has no provider section

- [ ] **Step 1: Write the failing test**

```go
func TestBuiltinProvidersPinned(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(`{"name":"cline","remove":true}`))
	w := httptest.NewRecorder()
	handleProvidersUpdate(w, req)
	if w.Code == 200 {
		t.Fatal("builtin cline must not be deletable")
	}
	req2 := httptest.NewRequest("POST", "/admin/api/providers/update", strings.NewReader(`{"name":"opencode","remove":true}`))
	w2 := httptest.NewRecorder()
	handleProvidersUpdate(w2, req2)
	if w2.Code == 200 {
		t.Fatal("builtin opencode must not be deletable")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -count=1 -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run TestBuiltinProvidersPinned -v`
Expected: FAIL (removal succeeds today)

- [ ] **Step 3: Write minimal implementation**

In `handleProvidersUpdate` remove branch: if `req.Name == "opencode" || req.Name == "cline"` → 400. Seed builtins at startup (init or first providerNames() call): opencode (Catalog fetch on, key from zen config), cline (no key needed, models = ["*"]). Admin GET includes both with `builtin:true`. Panel: provider section moved verbatim (add-form + rows + renderPvModelBlock + search + toggles) into 模型列表 page container; settings section deleted; keep element IDs identical so existing JS works unchanged.

- [ ] **Step 4: Run test + full suite**

Run: focused then full `./internal/...`. Expected: PASS.

- [ ] **Step 5: Commit + build delivery binary**

```bash
git add internal/app/
git commit -m "feat: provider manager lives on model list page with pinned builtins"
CGO_ENABLED=0 go build -tags with_quic,with_grpc,with_utls -ldflags "-s -w -H=windowsgui" -o "D:/cline-proxy-windows-amd64/cline-proxy-new4.exe" .
```

## Self-Review

**Spec coverage:** ai-gateway parity (types/apiKeys/models/enabled/test/two-level enable) → Tasks 1,3,5. Single source of truth → Tasks 2,4 (migration + deletion). Builtins pinned → Task 5. Google correctness retained (endpoint derivation + thought signatures untouched). Cline pool untouched behind builtin row. Zen health gate (tonight's 21c4bdb) untouched — opencode row reads zenFreeCatalog which already excludes down models.
**Placeholder scan:** Task 3 Step 1 explicitly forbids committing the scaffolded assertion — implementer must complete it. All other steps carry verbatim commands and code. No TBD/TODO.
**Type consistency:** providerAPIKey/providerModelEntry defined once in Task 1; Tasks 2–5 reference identical names/shapes. `freeModelIDs() []catalogModel` signature unchanged throughout (entries built from explicit IDs). `isFree(modelID string) bool` unchanged.

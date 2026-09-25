# Gateway + Node Probe Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix gateway chat-handling bugs and simplify sing-box node probe/selection to reachable + opencode.ai + country only.

**Architecture:** Keep gateway request path (proxy.go/logs.go/stream/pool) behavior-compatible while delaying 200 header, tightening SSRF/exit accounting; replace 4-stage node probe with 2-gate lean probe plus new-node country fast lane; exclude unknown-country from region-restricted models.

**Tech Stack:** Go 1.26/1.27, sing-box box.New validation, SOCKS5 mixed inbounds, existing nodeHealth/exitRegion/upstream matrix stores.

**Spec:** This plan + conversation decisions 2026-09-24: probe keeps only reachable + can reach opencode.ai + country; drop credit/health scores, MITM, speed, WARP, classification; no real-dialogue hi probe; new-node country fast lane via sub diff + host:port reuse + single IP source first.

## Global Constraints

- GitHub access via SSH over 443 only: `ssh://git@ssh.github.com:443/<owner>/<repo>.git` for fetch and push; never `https://github.com/` direct.
- No new third-party dependencies; keep single-exe distribution.
- Build/test with tags: `go build -tags "with_quic,with_grpc,with_utls" ./...` and `go test -tags "with_quic,with_grpc,with_utls" ./...`.
- Check `docs/opencode-zen-facts.md`, `docs/CLIENT-COMPAT.md`, `docs/omniroute-mapping.md` before changing zen/client/mapped behavior; update fact table in same commit if covered behavior changes; never code from `[未知]` entries.
- TDD: failing test first, minimal fix, rerun; frequent commits; do not fix unrelated bugs.
- Do not commit unless explicitly requested.

---

## Task 1 — Gateway entry hardening (proxy.go StartProxy + chat dispatch)

**Files:** `internal/app/proxy.go:66` (StartProxy), `internal/app/routing_dispatch.go:45,50` (handleChainedChat), `internal/app/routing_chain.go:132` (resolveRouteChain), `internal/app/zen_call.go`, `internal/app/proxy_pool.go:646` (pickZenProxyForModel)

- [ ] 1.1 Add failing test: cold start with empty node pool must not cache direct dial for zen/region-restricted path; first zen request triggers syncNodeBox/loadSubCache path or returns retryable 503, never silent direct fallback that poisons h2 pool.
- [ ] 1.2 Fix: gate region-restricted models when pool empty (return 503 + retryable), keep unrestricted models on direct with explicit log; document in code comment.
- [ ] 1.3 Add failing test: chain dispatch failure path always populates reqTrace tried/skipped + error class via applyCandidateFailure/logChainResult; no silent drop.
- [ ] 1.4 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestChain|TestProxy|TestStart' -count=1`

## Task 2 — Request log + client IP + noise filter (logs.go)

**Files:** `internal/app/logs.go:372` (requestLogMiddleware), ClientIP block at 419-426, route detect 436-461, isRequestNoise path

- [ ] 2.1 Add failing test: X-Forwarded-For / X-Real-IP trusted handling — direct RemoteAddr stays as-is; with trusted proxy header, first public IP wins; spoofed private IP never overrides.
- [ ] 2.2 Fix ClientIP extraction accordingly; keep X-Request-Id echo behavior unchanged.
- [ ] 2.3 Add failing test: chunked / over-probe-limit chat paths (path suffix /chat/completions, /messages, /v1/messages, /responses) always route=cline even when model probe empty; meta/admin/other classification unchanged.
- [ ] 2.4 Add failing test: successful chat (2xx with usage or valuable chunk) never classified as noise; only true meta/health polling is noise.
- [ ] 2.5 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestRequestLog|TestNoise|TestClientIP|TestRoute' -count=1`

## Task 3 — Stream 200 delay + empty-stream verdict (proxy_stream.go)

**Files:** `internal/app/proxy_stream.go:57` (handleStreamResponseWithToolNameMap), :512 legitEmptyTerminalReason, :662 writeStreamEmptyContentError, :687 handleNonStream, :790 collectStreamResponse

- [ ] 3.1 Add failing test: no bytes (no header, no SSE frame) may be written to `w` until upstream headers received and first validated chunk parsed; non-Flusher writer still returns 500 without prior 200.
- [ ] 3.2 Fix: move `WriteHeader(200)` + Content-Type/CORS setup to after upstream status check + first-chunk validation; keep heartbeat (newSSEHeartbeat) output-side only.
- [ ] 3.3 Add failing test: upstream clean-empty stream (no content/tool_calls/finish, no valid usage) surfaces as visible error via writeStreamEmptyContentError path, not silent 200+[DONE].
- [ ] 3.4 Add failing test: usage-only stream (hasValidUsage=true) stays success; tool-name-map nil path byte-identical to old behavior.
- [ ] 3.5 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestStream|TestEmpty|TestSSE|TestNonStream' -count=1`

## Task 4 — Pool selection + exit accounting (proxy_pool.go)

**Files:** `internal/app/proxy_pool.go:502` (effectiveProxyList), :524 nodeDialable, :658 nodeUsable, :676 nodeSlow, :693 lastZenExitKey/:706 lastZenProxyIdx, plus pickZenProxy/pickZenProxyForModel/cooldown/sticky sections

- [ ] 4.1 Add failing test: effectiveProxyList applies NodeExcludeKeywords once (no per-item deep-copy) + filterByExitRegion; empty-region-filter warns once and keeps list non-empty (no total blackout).
- [ ] 4.2 Add failing test: nodeUsable=false iff healthOf==fail or folded-duplicate; unknown/unprobed stays usable; ordinary proxies always usable.
- [ ] 4.3 Add failing test: lastZenProxyIdx resolves from dial-layer setLastZenExit (atomic), not round-robin arithmetic; direct-fallback/removed-key returns -1.
- [ ] 4.4 Add failing test: cooldown only marks the real exit key actually attempted; healthy untouched exits never inherit cooldown.
- [ ] 4.5 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestPool|TestPick|TestCooldown|TestSticky|TestExit' -count=1`

## Task 5 — SSRF tightening (outbound_url.go + dial guard)

**Files:** `internal/app/outbound_url.go:46` (validateOutboundURL), :84 resolvedLinkLocalReason, :178 dialWithSSRFGuard, :38 blockedOutboundHosts

- [ ] 5.1 Add failing test: validateOutboundURL rejects file/gopher/ftp schemes, empty host, metadata hostnames, link-local literals, and DNS→link-local; private/loopback gated solely by FREE_ROUTER_ALLOW_PRIVATE_UPSTREAM.
- [ ] 5.2 Add failing test: dialWithSSRFGuard pre-check blocks metadata hostname + link-local literal without dialing; post-check closes conn and errors on DNS-rebinding to link-local; strips IPv6 zone before parse.
- [ ] 5.3 Fix gaps only; keep LAN/Ollama allowance semantics via env flag.
- [ ] 5.4 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestOutbound|TestSSRF|TestDialGuard' -count=1`

## Task 6 — Lean probe: reachable + opencode.ai + country (node_probe.go)

**Files:** `internal/app/node_probe.go:193` (testNodeComprehensive), :246 probeNodeLiveness, :311 probeNodeExitInfo, :501 probeNodeSpeed, :564 probeMITM, :594 probeWarp, :190 classifyNodeNetwork in node_classify.go; consts :37-52 timeouts/workers, :84 liveness URLs, :100 IP echo URLs

- [ ] 6.1 Add failing test: lean probe result = {Alive, LatencyMs, ExitIP, ExitCountry} only for health verdict; SpeedBPS/IsStalled/MITMRisk/IsWarp/NetworkType no longer influence Ok.
- [ ] 6.2 Fix testNodeComprehensive → two gates: (a) liveness via nodeLivenessURLs (keep 12s timeout, concurrent-first-win, all-fail receipts), (b) opencode.ai reachability + country via single fastest IP-echo source (existing nodeIPEchoURLs, first success wins; no 3-source vote in fast path).
- [ ] 6.3 Delete-or-gate speed/MITM/WARP/classify calls from hot path (remove funcs or keep only behind explicit debug env, default off); drop nodeSpeedSlowBPS/nodeSlow demotion from verdict (keep nodeSlow helper only if selection still uses it, else remove).
- [ ] 6.4 Keep P0 ctx-cancel-after-read rule + per-source timeouts; update nodeTestResult JSON shape with omitempty so old node-health.json still loads.
- [ ] 6.5 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestProbe|TestLiveness|TestExitInfo|TestComprehensive' -count=1`

## Task 7 — Health loop simplification (node_health.go)

**Files:** `internal/app/node_health.go:135` healthCheckTargets, :150 pruneStaleNodeHealth, :171 checkAllNodeHealth (record at :260, Ok at :261), :397 startNodeHealthLoop; stores nodeHealth/nodeHealthState:35, testNodeComprehensiveFn hook

- [ ] 7.1 Add failing test: record Ok = Alive && ExitCountry known-or-unknown (never MITM/speed/WARP); stage counters collapse to dead/alive (+country-unknown count).
- [ ] 7.2 Simplify checkAllNodeHealth: keep grouping by nodeRemoteEndpointOf + rep-first + pruneStaleNodeHealth/pruneStaleExitKeys/pruneNodeTraffic + workers 32→128 scaling; remove MITM/stalled/speed branches from summary log.
- [ ] 7.3 Keep persistence (node-health.json + TTL) and loop cadence unchanged; ensure unknown-country nodes persist and reload.
- [ ] 7.4 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestNodeHealth|TestCheckAll|TestPrune|TestHealthPersist' -count=1`

## Task 8 — New-node country fast lane (sub diff + host:port reuse + single source)

**Files:** `internal/app/sub.go:56` (subNodeKeysSnapshot), `internal/app/nodes.go:177` (syncNodeBox), :422 nodeRemoteEndpoints/:429 nodeRemoteEndpointOf, `internal/app/exit_region.go:179` rememberNodeCountry/:195 rememberedNodeCountry, health loop entry

- [ ] 8.1 Add failing test: new keys (sub diff not in nodeHealth) get country fast-lane: single IP-echo source, concurrency ≤8, completes ≪ full-cycle time; host:port reuse — same host:port as known key inherits country immediately without network call.
- [ ] 8.2 Implement fast-lane in syncNodeBox/health entry: diff active keys vs nodeHealth; reuse rememberedNodeCountry by outboundHostPort; else single-source probe then rememberNodeCountry.
- [ ] 8.3 Add failing test: fast-lane failure leaves country empty (unknown) but node still probe-queued; never marks fail solely for missing country.
- [ ] 8.4 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestFastLane|TestSubDiff|TestCountryReuse|TestSyncNode' -count=1`

## Task 9 — Region gating: unknown excluded from restricted models (exit_region.go + pool)

**Files:** `internal/app/exit_region.go:204` nodeExitRegion, :229 exitRegionFilterActive, :235 exitRegionAllowed, :276 filterByExitRegion, :179 rememberNodeCountry; `internal/app/proxy_pool.go:646` model-aware pick; `internal/app/node_upstream.go` matrix + `internal/app/model_region.go:161` probeModelAllNodes

- [ ] 9.1 Add failing test: region-restricted model candidate set excludes unknown-country nodes; unrestricted models keep them; global exit-filter UI (filterByExitRegion) unchanged.
- [ ] 9.2 Fix exitRegionAllowed/pickZenProxyForModel: unknown → false iff model is region-restricted (regionOfCountry/validExitRegions path); ensure warnRegionFilterEmpty fires when restriction empties pool and falls back to polling rather than direct.
- [ ] 9.3 Keep upstream-matrix/probeModelAllNodes semantics; unknown exclusion applies at selection, not at matrix storage.
- [ ] 9.4 Verify: `go test -tags "with_quic,with_grpc,with_utls" ./internal/app/ -run 'TestExitRegion|TestRegion|TestModelRoute|TestUpstreamMatrix' -count=1`

## Task 10 — Full verification + docs

- [ ] 10.1 Run: `go build -tags "with_quic,with_grpc,with_utls" ./...` then `go test -tags "with_quic,with_grpc,with_utls" ./...`; triage only failures touched by this plan, report unrelated ones without fixing.
- [ ] 10.2 Update `docs/opencode-zen-facts.md` fact table iff covered zen behavior changed; re-check CLIENT-COMPAT + omniroute-mapping for client/mapped behavior deltas.
- [ ] 10.3 Final sweep: no new deps, single-exe intact, no unrelated refactors; summarize per-task test→fix→rerun evidence.

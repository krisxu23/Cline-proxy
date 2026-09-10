// Package providers defines the upstream provider abstraction and
// implementations for the Cline reverse-proxy gateway.
//
// Three providers are supported:
//   - "cline": the original Cline account pool (internal/app/pool.go + cline auth)
//   - "zen": opencode zen free models (internal/app/zen.go)
//   - "clinepass": ClinePass subscription pool (borrowed idea from hayou2002/clinepass-proxy)
//
// The router (router.go) selects a provider by model name prefix:
//   - cline-pass/... -> clinepass
//   - *-free or opencode/... -> zen
//   - everything else -> cline
package providers
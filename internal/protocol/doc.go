// Package protocol provides protocol-level helpers used by the Cline
// reverse proxy gateway.
//
// The package is intentionally pure: no I/O, no globals, no state. Functions
// here convert request/response shapes and normalize SSE streams so the
// HTTP layer (internal/app) can stay focused on routing and account
// rotation.
//
// Borrowed ideas (with attribution preserved in source comments):
//   - defyma/cline-proxy (Node): empty-choices fallback and Anthropic SSE
//     open/close block accounting.
//   - hayou2002/clinepass-proxy (Python): reasoning field pass-through
//     in OpenAI responses.
//
// Source-of-truth modules:
//   - streaming.go: SSE normalization and choice-block guards.
//   - normalize.go: OpenAI chunk normalization (metadata stripping).
//
// (openai_anthropic.go / responses_chat.go 已删除: 生产零调用, 等价转换在
// internal/app 侧各自维护 —— 本次终审 P3 死代码清理。)
package protocol

package app

// 照抄 OmniRoute open-sse/utils/opencodeHeaders.ts（146 行）
// + executors/opencode.ts:736-789 的合成 CLI 身份逻辑。
//
// ── 为什么照抄（用户定位 + 参考实证）────────────────────────────
// opencode 免费 tier 的 "can only be used from within OpenCode" 拒绝字面来自
// Console 后端，但它前面的 Cloudflare 风控靠的是**请求的 opencode CLI 身份头**
// 判 "from OpenCode"：非 CLI 形态的 UA + 缺失 x-opencode-project / client 值错
// 会让数据中心出口被 FreeUsageLimitError 拒绝（参考 opencode.ts #5997 附注）。
// 我方旧实现(zen_call.go:154-160)用 FreshZenIdentity 随机合成
// `opencode/latest/x/cli` UA + `x-opencode-client: cli`，既没有 project、session
// 也不是确定性指纹 —— 与真实 opencode CLI 发的完全对不上，被识别为仿冒。
//
// 逐字照抄以下语义（opencodeHeaders.ts + opencode.ts:744-789）：
//   - UA 默认 "opencode"（配置可覆盖）；客户端 UA 若不是 opencode-cli/ 形态
//     则**替换**成 CLI UA（参考 #5997/#10229：数据中心 IP + 通用 UA 被拒）
//   - x-opencode-client 默认 "desktop"（真实桌面客户端，非 "cli"）
//   - x-opencode-project 默认 "global"
//   - x-opencode-session 用请求体(model/system/首条user/tools)的确定性哈希，
//     同会话请求命中同 session → 上游 prompt cache 命中（参考 generateSessionId）
//   - 转发客户端自己的 x-opencode-* / x-session-id / x-title（客户端值优先）
//   - Muse/Responses 端点要求 session 是 UUID 形态（scoped workaround, :779-789）

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// opencodeCliDefaults 对位 opencode.ts:744-757 的 cliDefaults。
type opencodeCliDefaults struct {
	userAgent string
	client    string
	project   string
}

// opencodeHeaderKeys 照抄 OPENCODE_HEADER_KEYS（opencodeHeaders.ts:9-14）。
var opencodeHeaderKeys = []string{
	"x-opencode-session",
	"x-opencode-request",
	"x-opencode-project",
	"x-opencode-client",
}

// opencodeAgentMetaKeys 照抄 AGENT_METADATA_HEADER_KEYS（:23）。
var opencodeAgentMetaKeys = []string{"x-session-id", "x-title"}

// applyOpencodeHeaders 对位 forwardOpencodeClientHeaders（opencodeHeaders.ts:60-114）
// + applyCliDefaults（:125-146）。就地填满 outbound headers 的 opencode 身份。
//
//	clientHeaders: 客户端的原始请求头(x-opencode-* / x-session-id / x-title 会被转发)
//	identity:      默认 CLI 身份(UA/client/project); session 用 bodyFingerprint 生成
//	bodyFingerprint: {model, system, firstUser, tools} 生成确定性 session, nil 时用随机 UUID
func applyOpencodeHeaders(outbound map[string]string, clientHeaders map[string]string, identity *opencodeCliDefaults, bodyFingerprint *opencodeBodyFingerprint) {
	if identity == nil {
		identity = &opencodeCliDefaults{userAgent: "opencode", client: "desktop", project: "global"}
	}
	// 1. User-Agent: 客户端有则先用; 非 CLI UA 会被替换(下面 applyCliDefaults 处理)
	for _, ua := range []string{headerMapValue(clientHeaders, "User-Agent"), headerMapValue(clientHeaders, "user-agent")} {
		if ua != "" {
			outbound["User-Agent"] = ua
			break
		}
	}
	// 2. 转发 x-opencode-* 客户端头
	for _, k := range opencodeHeaderKeys {
		if v := headerMapValue(clientHeaders, k); v != "" {
			outbound[k] = v
		}
	}
	// 2b. 转发 x-session-id / x-title 代理元数据头
	for _, k := range opencodeAgentMetaKeys {
		if v := headerMapValue(clientHeaders, k); v != "" {
			outbound[k] = v
		}
	}
	// 3. 补默认 CLI 身份(客户端没给的就填; UA 例外会替换)
	ua := outbound["User-Agent"]
	clientUaIsCliLike := strings.HasPrefix(strings.TrimSpace(ua), "opencode-cli/")
	if !clientUaIsCliLike {
		outbound["User-Agent"] = identity.userAgent
	}
	if outbound["x-opencode-client"] == "" {
		outbound["x-opencode-client"] = identity.client
	}
	if outbound["x-opencode-project"] == "" {
		outbound["x-opencode-project"] = identity.project
	}
	if outbound["x-opencode-request"] == "" {
		outbound["x-opencode-request"] = randomUUID()
	}
	if outbound["x-opencode-session"] == "" {
		outbound["x-opencode-session"] = genOpencodeSessionID(bodyFingerprint)
	}
}

// opencodeBodyFingerprint 供 session 指纹用的请求体字段(参考 sessionBody)。
type opencodeBodyFingerprint struct {
	model     string
	system    string
	firstUser string
	toolNames []string
	forceUUID bool // Muse/Responses 端点强制 UUID session(参考 :781-789)
}

// genOpencodeSessionID 照抄 generateSessionId(sessionManager.ts:103-145)。
// 顺序: model → system hash → 首条 user hash → tools(排序)hash, sha256[:16]。
// fp 为 nil / fingerprint 无分片时回退随机 UUID(参考 :144-145 的 || randomUUID())。
func genOpencodeSessionID(fp *opencodeBodyFingerprint) string {
	if fp != nil && fp.forceUUID {
		return randomUUID()
	}
	if fp == nil {
		return randomUUID()
	}
	var parts []string
	if fp.model != "" {
		parts = append(parts, "model:"+fp.model)
	}
	if s := hash16(fp.system); s != "" {
		parts = append(parts, "sys:"+s)
	}
	if u := hash16(fp.firstUser); u != "" {
		parts = append(parts, "user0:"+u)
	}
	if len(fp.toolNames) > 0 {
		t := append([]string(nil), fp.toolNames...)
		sort.Strings(t)
		if h := hash16(strings.Join(t, ",")); h != "" {
			parts = append(parts, "tools:"+h)
		}
	}
	if len(parts) == 0 {
		return randomUUID()
	}
	return hash16(strings.Join(parts, "|"))
}

// hash16 sha256 前 16 位 hex; 空串返回 ""。
func hash16(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// bodyFingerprint 从 zen 出站 body 提取 session 指纹所需的字段
// (对照 opencodeHeaders.ts:764-774 的 sessionBody)。
func bodyFingerprint(body map[string]any, forceUUID bool) *opencodeBodyFingerprint {
	if len(body) == 0 {
		return nil
	}
	fp := &opencodeBodyFingerprint{forceUUID: forceUUID}
	if m, ok := body["model"].(string); ok {
		fp.model = m
	}
	fp.system = firstSystemText(body["system"])
	fp.firstUser = firstUserText(body["messages"])
	fp.toolNames = toolNamesOf(body["tools"])
	return fp
}

// firstSystemText 从 system(字符串或数组) 取文本。
func firstSystemText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, p := range t {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// firstUserText 从 messages 取第一条 user 消息的文本。
func firstUserText(v any) string {
	msgs, ok := v.([]any)
	if !ok {
		return ""
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c
		case []any:
			var sb strings.Builder
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if s, ok := pm["text"].(string); ok {
						sb.WriteString(s)
					}
				}
			}
			return sb.String()
		}
	}
	return ""
}

// toolNamesOf 从 tools 提取名字列表(对照 sessionManager.ts:130-134)。
func toolNamesOf(v any) []string {
	tools, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := tm["name"].(string); ok && n != "" {
			out = append(out, n)
			continue
		}
		if fn, ok := tm["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// headerMapValue 大小写不敏感查 header record(参考 findHeader, opencodeHeaders.ts:28-30)。
func headerMapValue(h map[string]string, name string) string {
	if h == nil {
		return ""
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// randomUUID v4 UUID(参考 crypto.randomUUID)。
func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b)
}

func formatUUID(b []byte) string {
	const hexd = "0123456789abcdef"
	var out [36]byte
	idx := 0
	segs := []int{4, 2, 2, 2, 6}
	pos := 0
	for _, n := range segs {
		for i := 0; i < n; i++ {
			out[idx] = hexd[b[pos]>>4]
			out[idx+1] = hexd[b[pos]&0xf]
			idx += 2
			pos++
		}
		if idx < 36 {
			out[idx] = '-'
			idx++
		}
	}
	return string(out[:])
}

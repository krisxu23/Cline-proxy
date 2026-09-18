package app

// opencode CLI 身份头 —— 以官方源码为准(sst/opencode
// packages/opencode/src/session/llm/request.ts:prepare),
// providerID 以 "opencode" 开头时官方客户端必带:
//
//	"User-Agent":      "opencode/<InstallationVersion>"(如 opencode/1.18.31)
//	"x-opencode-client":  flags.client(OPENCODE_CLIENT, 默认 "cli")
//	"x-opencode-project": 真实项目 ID(prj_…)
//	"x-opencode-session": 真实会话 ID(ses_…)
//	"x-opencode-request": 真实用户 ID(usr_…)
//
// 实证(2026-09-17, 直连上游): 伪造但结构合法的 ses_/usr_/prj_ ID +
// UA opencode/1.18.31 + client cli → mimo-v2.5-free 200;
// 去掉 x-opencode-* 头 → 同一请求 403 FreeTierError。
// 旧实现(bare "opencode" UA / client desktop / project global / UUID session)
// 与官方形态完全对不上, 门禁收紧后一律被拒。
//
// ID 结构照抄官方 id.ts:create: prefix + "_" + 6 字节时间(hex) + 14 位 base62。
//
// ⚠️⚠️ 必须知道的反向结论（2026-09-18 A/B/C 三向实测）：
//
//	**这套身份头既不能造成、也不能修好 `403 FreeTierError`。**
//	同机直连、同一 body、同一 key, 只换身份头:
//	  A 本文件更早的形态(OmniRoute 式 opencode/desktop/global/16hex)  -> 403 FreeTierError
//	  B 本文件当前形态(官方式 opencode/<ver>/cli/prj_/ses_/usr_)        -> **同样 403**
//	  C 完全不带任何身份头                                              -> **同样 403**
//	三者逐字相同 —— 即上面第 13-17 行记的"伪造 ID 即 200、去头即 403"**无法复现**:
//	B 就是当时记的那套, 现在同样 403; 不带头也是 403(这条对上了)。
//
//	`FreeTierError` 只看**出口 IP**。最可能的解释是: 当时那次 200 的自变量是
//	**当次走的出口**, 不是请求头。所以:
//	  - 再遇到 FreeTierError, **先去查出口 IP, 不要来调这里的头**;
//	  - 本文件的存在理由只有"对齐官方客户端形态"(可能影响 Cloudflare 层限流),
//	    与 FreeTier 无关;
//	  - 两种形态在实测中不可区分, 因此"切换形态"不构成修复 —— 改之前先拿 A/B 证据。
//	详见 docs/opencode-zen-facts.md 第 3/4 节。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// opencodeClientVersion 官方客户端版本(UA 用)。上游可能校验版本新旧,
// opencode 发版后若又出现 FreeTierError, 先把这里跟到最新再查别的。
const opencodeClientVersion = "1.18.31"

// opencodeCliDefaults 默认 CLI 身份(UA/client/project)。
type opencodeCliDefaults struct {
	userAgent string
	client    string
	project   string
}

// defaultOpencodeIdentity 官方形态的默认身份: UA 带版本、client cli、
// project 为进程内稳定的伪造 prj_ ID(官方是真实项目 ID, 网关没有项目概念,
// 用稳定伪造值; 实测可通过门禁)。
func defaultOpencodeIdentity() *opencodeCliDefaults {
	return &opencodeCliDefaults{
		userAgent: "opencode/" + opencodeClientVersion,
		client:    "cli",
		project:   gatewayProjectID(),
	}
}

var (
	gatewayIDsOnce sync.Once
	gatewayIDs     struct{ project, user string }
)

// gatewayIDsInit 进程内稳定的伪造身份: project(prj_)与 user(usr_) 在官方
// 客户端里都是稳定值(项目/账号), 网关同样全进程复用同一对。
func gatewayIDsInit() {
	gatewayIDs.project = forgeOpencodeID("prj", nil)
	gatewayIDs.user = forgeOpencodeID("usr", nil)
}

func gatewayProjectID() string {
	gatewayIDsOnce.Do(gatewayIDsInit)
	return gatewayIDs.project
}

func gatewayUserID() string {
	gatewayIDsOnce.Do(gatewayIDsInit)
	return gatewayIDs.user
}

const opencodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// forgeOpencodeID 伪造官方结构 ID: prefix + "_" + 6 字节时间 hex + 14 位 base62。
// seed 为 nil 时后缀随机; 非 nil 时后缀由 seed 确定性派生(同输入同 ID)。
func forgeOpencodeID(prefix string, seed []byte) string {
	var timeBytes [6]byte
	now := uint64(time.Now().UnixMilli()) * 0x1000
	if seed != nil {
		// 确定性会话需要"时间戳看起来新鲜 + 后缀稳定": 时间取当日 UTC 零点,
		// 同一天同会话同 ID, 既过"新鲜"检查又命中上游 prompt cache。
		dayStart := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
		now = uint64(dayStart) * 0x1000
	}
	for i := 0; i < 6; i++ {
		timeBytes[i] = byte(now >> (40 - 8*i))
	}
	suffix := make([]byte, 14)
	if seed == nil {
		var rb [14]byte
		if _, err := rand.Read(rb[:]); err != nil {
			return ""
		}
		for i := range suffix {
			suffix[i] = opencodeBase62[rb[i]%62]
		}
	} else {
		sum := sha256.Sum256(seed)
		for i := range suffix {
			suffix[i] = opencodeBase62[sum[i]%62]
		}
	}
	return prefix + "_" + hex.EncodeToString(timeBytes[:]) + string(suffix)
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
		identity = defaultOpencodeIdentity()
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
		outbound["x-opencode-request"] = gatewayUserID()
	}
	if outbound["x-opencode-session"] == "" {
		outbound["x-opencode-session"] = genOpencodeSessionID(bodyFingerprint)
	}
}

// opencodeBodyFingerprint 供 session 指纹用的请求体字段。
type opencodeBodyFingerprint struct {
	model     string
	system    string
	firstUser string
	toolNames []string
}

// genOpencodeSessionID 生成官方 ses_ 形态 session。
// 有指纹时确定性派生(同会话同 ID, 命中上游 prompt cache), 无指纹时随机。
func genOpencodeSessionID(fp *opencodeBodyFingerprint) string {
	if fp == nil {
		return forgeOpencodeID("ses", nil)
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
		return forgeOpencodeID("ses", nil)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return forgeOpencodeID("ses", sum[:])
}

// hash16 sha256 前 16 位 hex; 空串返回 ""。
func hash16(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// bodyFingerprint 从 zen 出站 body 提取 session 指纹所需的字段。
func bodyFingerprint(body map[string]any) *opencodeBodyFingerprint {
	if len(body) == 0 {
		return nil
	}
	fp := &opencodeBodyFingerprint{}
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

package app

import (
	"testing"
)

// 照抄 OmniRoute open-sse/utils/opencodeHeaders.ts 的行为级测试。

func TestApplyOpencodeHeaders_默认CLI身份(t *testing.T) {
	out := map[string]string{}
	applyOpencodeHeaders(out, nil, &opencodeCliDefaults{userAgent: "opencode", client: "desktop", project: "global"}, nil)
	// UA 默认填 CLI UA
	if out["User-Agent"] != "opencode" {
		t.Fatalf("UA 应为 opencode, got %q", out["User-Agent"])
	}
	if out["x-opencode-client"] != "desktop" {
		t.Fatalf("client 应为 desktop, got %q", out["x-opencode-client"])
	}
	if out["x-opencode-project"] != "global" {
		t.Fatalf("project 应为 global, got %q", out["x-opencode-project"])
	}
	// session/request 缺省时生成
	if out["x-opencode-session"] == "" {
		t.Fatal("session 应生成")
	}
	if out["x-opencode-request"] == "" {
		t.Fatal("request 应生成")
	}
}

func TestApplyOpencodeHeaders_客户端头优先(t *testing.T) {
	client := map[string]string{
		"x-opencode-session": "client-session",
		"x-opencode-request": "client-req",
		"x-opencode-project": "my-project",
		"User-Agent":         "opencode-cli/1.2.3",
		"x-opencode-client":  "desktop",
		"x-session-id":       "sess_abc",
		"x-title":            "MyClient",
	}
	out := map[string]string{}
	applyOpencodeHeaders(out, client, &opencodeCliDefaults{userAgent: "opencode", client: "desktop", project: "global"}, nil)
	// 客户端 x-opencode-* 值优先
	if out["x-opencode-session"] != "client-session" {
		t.Fatalf("客户端 session 应优先, got %q", out["x-opencode-session"])
	}
	if out["x-opencode-request"] != "client-req" {
		t.Fatalf("客户端 request 应优先, got %q", out["x-opencode-request"])
	}
	if out["x-opencode-project"] != "my-project" {
		t.Fatalf("客户端 project 应优先, got %q", out["x-opencode-project"])
	}
	// opencode-cli/ 形态 UA 保留(参考 #5997)
	if out["User-Agent"] != "opencode-cli/1.2.3" {
		t.Fatalf("opencode-cli UA 应保留, got %q", out["User-Agent"])
	}
	// 代理元数据头转发
	if out["x-session-id"] != "sess_abc" {
		t.Fatalf("x-session-id 应转发, got %q", out["x-session-id"])
	}
	if out["x-title"] != "MyClient" {
		t.Fatalf("x-title 应转发, got %q", out["x-title"])
	}
}

// 非 opencode CLI UA 必须被替换成 CLI UA(参考 applyCliDefaults, :136-140)。
func TestApplyOpencodeHeaders_非CLI_UA被替换(t *testing.T) {
	client := map[string]string{"User-Agent": "curl/8.5.0"}
	out := map[string]string{}
	applyOpencodeHeaders(out, client, &opencodeCliDefaults{userAgent: "opencode", client: "desktop", project: "global"}, nil)
	if out["User-Agent"] != "opencode" {
		t.Fatalf("非 CLI UA(curl)应被替换成 opencode, got %q", out["User-Agent"])
	}
}

// 确定性 session: 相同 model/system/firstUser/tools → 相同 session;
// 不同首条消息 → 不同 session。
func TestGenOpencodeSessionID_确定性(t *testing.T) {
	fp1 := &opencodeBodyFingerprint{model: "mimo-v2.5-free", system: "you are claude", firstUser: "hi", toolNames: []string{"bash", "read"}}
	fp2 := &opencodeBodyFingerprint{model: "mimo-v2.5-free", system: "you are claude", firstUser: "hi", toolNames: []string{"read", "bash"}} // tools 顺序无关
	s1 := genOpencodeSessionID(fp1)
	s2 := genOpencodeSessionID(fp2)
	if s1 == "" || s2 == "" {
		t.Fatalf("session 不应为空: %q %q", s1, s2)
	}
	if s1 != s2 {
		t.Fatalf("tools 顺序无关但结果不同: %q vs %q", s1, s2)
	}
	// 不同内容 → 不同
	fp3 := &opencodeBodyFingerprint{model: "mimo-v2.5-free", system: "you are claude", firstUser: "different question", toolNames: []string{"bash"}}
	if s3 := genOpencodeSessionID(fp3); s3 == s1 {
		t.Fatal("不同首条消息应生成不同 session")
	}
}

// 空 fingerprint / nil → 回退随机 UUID(参考 :144-145)。
func TestGenOpencodeSessionID_回退UUID(t *testing.T) {
	if s := genOpencodeSessionID(nil); s == "" {
		t.Fatal("nil 应生成非空 session")
	}
	// forceUUID 强制 UUID 形态
	fp := &opencodeBodyFingerprint{model: "muse-spark-1.3-contributor-free", forceUUID: true}
	s := genOpencodeSessionID(fp)
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		t.Fatalf("forceUUID 应为 36 位 UUID, got %q(len=%d)", s, len(s))
	}
}

// bodyFingerprint 提取字段。
func TestBodyFingerprint_提取(t *testing.T) {
	body := map[string]any{
		"model":  "mimo-v2.5-free",
		"system": "you are a helpful assistant",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys-content"},
			map[string]any{"role": "user", "content": "first question"},
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "user", "content": "second"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
		},
	}
	fp := bodyFingerprint(body, false)
	if fp.model != "mimo-v2.5-free" {
		t.Fatalf("model 提取错误: %q", fp.model)
	}
	if fp.firstUser != "first question" {
		t.Fatalf("firstUser 应取首条 user 消息, got %q", fp.firstUser)
	}
	if len(fp.toolNames) != 2 || fp.toolNames[0] != "bash" || fp.toolNames[1] != "read" {
		t.Fatalf("tools 提取错误: %v", fp.toolNames)
	}
}

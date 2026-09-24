package app

import (
	"encoding/json"
	"net/http"
)

// CORS 策略常量: 未来收紧时改这一处就够。之前散落在 7 个 handler 里手写 "*"
// 是历史遗留 —— corsHandler 里是唯一的完整策略源, 但 handleStreamResponseWithUsage /
// handleAnthropicStreamWithUsage / handleResponses / handleClinePass /
// handleChainedChatAs(shapeResponses) 这几个流式/子协议 handler 内部又各自 Set
// 了一次 Origin, 收紧策略时这些点会漏改, 形成同一入口下策略不一致。抽成
// applyCORS / setCORSOrigin 之后, 改一个点就影响所有 handler。
// DECISION(2026-09-24, 用户确认): CORS 保持 `*` —— 部分用户面板/客户端跨域部署,
// 收紧会直接调不通。管理后台另有 ADMIN token 鉴权, 此处不收紧。勿改。
const (
	corsAllowOrigin  = "*"
	corsAllowMethods = "GET, POST, OPTIONS"
	corsAllowHeaders = "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta"
)

// applyCORS 在响应上写完整 CORS 头。用于 corsHandler 入口, 以及需要在子 handler
// 里补一遍的流式/子协议路径。
func applyCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
	w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
	w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
}

// setCORSOrigin 只补 Origin 头, 用于那些父级 handler 已设过 Methods/Headers、
// 但流式路径自己在 W.WriteHeader 前又刷一遍 Origin 的场景。
func setCORSOrigin(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", corsAllowOrigin)
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		applyCORS(w)

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}

// setRouteHeader 在响应头中标注本次请求的路由决策, 便于客户端侧排查
// (灵感来自 OmniRoute 的 X-OmniRoute-Decision)。
func setRouteHeader(w http.ResponseWriter, upstream, model, failover string) {
	v := "upstream=" + upstream + "; model=" + model
	if failover != "" {
		v += "; failover=" + failover
	}
	w.Header().Set("X-Proxy-Route", v)
}

// controlSanitizingReader / sanitizeJSONControlChars 说明(方案参照 OmniRoute
// 的 SSE 数据清洗思路, MIT; Go 侧实现):
//
// JSON 对裸控制字符的宽容度是**分位置**的 —— 字符串外部: \t \n \r 是合法
// 空白; 字符串内部: 所有 < 0x20 的字符(含 TAB)都非法。实测上游(C2PA 图片
// 元数据)会在字符串里塞裸控制字符, 既让解析失败, 又会被行切分当成换行把一条
// JSON 劈成多行, 后半段没有 "data:" 前缀遂走"原样透传"直达客户端, 客户端报
// "Bad control character in string literal"。因此在**行切分之前**按位置清洗。
// 必须跟踪字符串状态, 否则会把字符串内的 TAB 漏掉(实测回归)。

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// applyOverride 用 override.md 替换系统提示词(不存在则跳过)
func applyOverride(params map[string]any) {
	override := loadOverrideContent()
	if override == "" {
		return
	}
	if msgs, ok := params["messages"].([]any); ok {
		found := false
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if mm["role"] == "system" {
					mm["content"] = override
					found = true
					break
				}
			}
		}
		if !found {
			params["messages"] = append([]any{map[string]any{"role": "system", "content": override}}, msgs...)
		}
	}
}

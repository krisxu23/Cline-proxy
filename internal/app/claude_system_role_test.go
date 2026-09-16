package app

import (
	"encoding/json"
	"testing"
)

// 本文件锁定 claudeSystemRole.ts 的权威行为。
//
// ★ 期望值全部来自「用 Node 实跑参考实现」（探针 .negbak/probe_sysrole.mjs），
// 不是读代码推断出来的。每条 case 的编号与探针输出一一对应。

// jsonEqGo 把 Go 值序列化后与期望 JSON 文本比较（键序无关）。
func jsonEqGo(t *testing.T, name string, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("%s: 期望值不是合法 JSON: %v", name, err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("%s:\n  得   %s\n  期望 %s", name, gb, wb)
	}
}

// payloadOf 把载荷拆成 extractSystemRoleMessages 的三元入参。
func payloadOf(p map[string]any) ([]any, any, bool, any, bool) {
	msgs, _ := p["messages"].([]any)
	sys, hasSys := p["system"]
	oc, hasOC := p["output_config"]
	return msgs, sys, hasSys, oc, hasOC
}

// runExtract 复刻参考实现的「就位改写」：返回改写后的整套 payload。
func runExtract(p map[string]any) map[string]any {
	msgs, sys, hasSys, oc, hasOC := payloadOf(p)
	nm, ns, noc, changed := extractSystemRoleMessages(msgs, sys, hasSys, oc, hasOC)
	if !changed {
		return p
	}
	p["messages"] = nm
	if ns != nil {
		p["system"] = ns
	} else {
		delete(p, "system")
	}
	if noc != nil {
		p["output_config"] = noc
	} else if _, existed := p["output_config"]; !existed {
		// 参考实现对 `payload.output_config == null` 的赋值形态: 只在有值时设键
	}
	return p
}

// runRelocate 复刻参考实现的「就位改写」。
func runRelocate(p map[string]any) map[string]any {
	msgs, _ := p["messages"].([]any)
	oc, hasOC := p["output_config"]
	nm, noc, changed := relocateDirectiveOnlyMessages(msgs, oc, hasOC)
	if !changed {
		return p
	}
	p["messages"] = nm
	if noc != nil {
		p["output_config"] = noc
	}
	return p
}

// ---------------------------------------------------------------- extractSystemRoleMessages

// 探针 #1: 空 messages → 早退
func TestExtractSystemRole_空messages早退(t *testing.T) {
	p := map[string]any{"messages": []any{}, "system": "orig"}
	jsonEqGo(t, "空 messages", runExtract(p), `{"messages":[],"system":"orig"}`)
}

// 探针 #2: 无 system 角色 → 原样
func TestExtractSystemRole_无system角色原样(t *testing.T) {
	p := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	jsonEqGo(t, "无 system 角色", runExtract(p), `{"messages":[{"role":"user","content":"hi"}]}`)
}

// 探针 #3: 字符串 system 提升 + 顶层 string 转数组且**原值排在前**
func TestExtractSystemRole_字符串system提升(t *testing.T) {
	p := map[string]any{
		"system": "BASE",
		"messages": []any{
			map[string]any{"role": "system", "content": "S1"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	jsonEqGo(t, "字符串 system", runExtract(p),
		`{"system":[{"type":"text","text":"BASE"},{"type":"text","text":"S1"}],"messages":[{"role":"user","content":"hi"}]}`)
}

// 探针 #4: developer 角色等价 system（大小写不敏感）
func TestExtractSystemRole_developer等价system(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "DEVELOPER", "content": "D1"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "developer", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"D1"}]}`)
}

// 探针 #5: 顶层 system 为数组 → extraBlocks **追加到末尾**
func TestExtractSystemRole_顶层数组追加到末尾(t *testing.T) {
	p := map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "BASE"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "S1"},
		},
	}
	jsonEqGo(t, "顶层数组", runExtract(p),
		`{"system":[{"type":"text","text":"BASE"},{"type":"text","text":"S1"}],"messages":[{"role":"user","content":"hi"}]}`)
}

// 探针 #6: 数组 content 里只提升 text 块，非 text 块被丢，空文本块被丢
func TestExtractSystemRole_数组content只提升text块(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "A"},
			map[string]any{"type": "image", "source": map[string]any{"data": "x"}},
			map[string]any{"type": "text", "text": ""},
			map[string]any{"type": "text", "text": "B"},
		}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "数组 content", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}`)
}

// 探针 #7: 顶层 system 是空串 → 走 else 分支（不先进 array 分支）
func TestExtractSystemRole_顶层空串被覆盖(t *testing.T) {
	p := map[string]any{"system": "", "messages": []any{map[string]any{"role": "system", "content": "S1"}}}
	jsonEqGo(t, "空串 system", runExtract(p), `{"system":[{"type":"text","text":"S1"}],"messages":[]}`)
}

// 探针 #8: 顶层 system 是数字 → 走 else 分支
func TestExtractSystemRole_顶层非字符串非数组被替换(t *testing.T) {
	p := map[string]any{"system": 123, "messages": []any{map[string]any{"role": "system", "content": "S1"}}}
	jsonEqGo(t, "数字 system", runExtract(p), `{"system":[{"type":"text","text":"S1"}],"messages":[]}`)
}

// 探针 #9: 无可提升块（空文本被丢）→ messages 仍被过滤
// ★ 注意：空白串 "   " 长度 > 0，**会被提升**，故期望里有第二个块。
func TestExtractSystemRole_无可提升块仍过滤messages(t *testing.T) {
	p := map[string]any{
		"system":   "KEEP",
		"messages": []any{map[string]any{"role": "system", "content": "   "}, map[string]any{"role": "user", "content": "hi"}},
	}
	jsonEqGo(t, "空白串被提升", runExtract(p),
		`{"system":[{"type":"text","text":"KEEP"},{"type":"text","text":"   "}],"messages":[{"role":"user","content":"hi"}]}`)
}

// 探针 #10: content 为空数组 → 无块可提升，system 键不产生
func TestExtractSystemRole_空数组content不产生块(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "空数组 content", runExtract(p), `{"messages":[{"role":"user","content":"hi"}]}`)
}

// ---------------------------------------------------------------- output_config 折叠

// 探针 #11
func TestExtractSystemRole_directive折叠outputConfig(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "", "output_config": map[string]any{"format": map[string]any{"type": "json_schema"}}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "directive 折叠", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema"}}}`)
}

// 探针 #12: 顶层已有 output_config → 不覆盖
func TestExtractSystemRole_顶层outputConfig优先(t *testing.T) {
	p := map[string]any{
		"output_config": map[string]any{"keep": 1},
		"messages": []any{
			map[string]any{"role": "system", "content": "S", "output_config": map[string]any{"drop": 1}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	jsonEqGo(t, "顶层优先", runExtract(p),
		`{"output_config":{"keep":1},"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #13: output_config 是数组 → 不折叠
func TestExtractSystemRole_outputConfig是数组不折叠(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "S", "output_config": []any{1, 2}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "数组 output_config", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #14: 多条 directive → 第一个 wins
func TestExtractSystemRole_多条directive第一个生效(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"first": 1}},
		map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"second": 2}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "第一个 wins", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"output_config":{"first":1}}`)
}

// ---------------------------------------------------------------- cache_control 重锚

// 探针 #15: cache_control 移到最近可承载块
func TestExtractSystemRole_cacheControl移到最近可承载块(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "U1"}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "moved", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"text","text":"U1","cache_control":{"type":"ephemeral"}}]}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #16: 目标块已占且同为 5m → kept，标记留在 hoisted 上
func TestExtractSystemRole_同ttl冲突kept(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "U1", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}}},
	}}
	jsonEqGo(t, "kept", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"text","text":"U1","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`)
}

// 探针 #17: 目标块 1h + hoisted 5m → dropped（标记被删）
func TestExtractSystemRole_目标1h新5m被丢弃(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "U1", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "dropped", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"text","text":"U1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #18: 目标块 5m + hoisted 1h → kept
func TestExtractSystemRole_目标5m新1h保留(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "U1", "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}},
	}}
	jsonEqGo(t, "kept", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"text","text":"U1","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
}

// 探针 #19: 无可承载块 → kept
func TestExtractSystemRole_无可承载块kept(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "thinking", "thinking": "t"}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "thinking 不可作锚点", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"thinking","thinking":"t"}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]}`)
}

// 探针 #20: 空文本块不可作锚点 → 继续往前找到 U1
func TestExtractSystemRole_空文本块不可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "U1"}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ""}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "跳过空文本块", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"text","text":"U1","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":""}]}],"system":[{"type":"text","text":"S"}]}`)
}

// ---------------------------------------------------------------- tool_result 作锚点

// 探针 #21: tool_result 的 content 是非空字符串 → 可作锚点（marker 挂到 tool_result 块自身）
func TestExtractSystemRole_toolResult非空字符串可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "R"}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "tool_result 锚点", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"R","cache_control":{"type":"ephemeral"}}]}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #22: tool_result content 是空字符串 → 不可作锚点
func TestExtractSystemRole_toolResult空字符串不可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": ""}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "空串 tool_result", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":""}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]}`)
}

// 探针 #23: tool_result content 数组里有非空 text → 可作锚点
func TestExtractSystemRole_toolResult数组含非空text可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": []any{map[string]any{"type": "image"}, map[string]any{"type": "text", "text": "x"}}}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "数组含 text", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image"},{"type":"text","text":"x"}],"cache_control":{"type":"ephemeral"}}]}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 #24: tool_result content 数组只有 image → 不可作锚点
func TestExtractSystemRole_toolResult数组只有image不可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a", "content": []any{map[string]any{"type": "image"}}}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "只有 image", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image"}]}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]}`)
}

// 探针 #25: tool_result 完全没有 content/text/output → payload 为 null → 不可作锚点
func TestExtractSystemRole_toolResult无载荷不可作锚点(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "a"}}},
		map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "S", "cache_control": map[string]any{"type": "ephemeral"}}}},
	}}
	jsonEqGo(t, "无载荷", runExtract(p),
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]}],"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]}`)
}

// ---------------------------------------------------------------- relocateHoistedCacheBoundary

// 探针 R1a-R1f
func TestRelocateHoistedCacheBoundary(t *testing.T) {
	ephemeral := func() map[string]any { return map[string]any{"type": "ephemeral"} }

	t.Run("R1a 空preceding得kept", func(t *testing.T) {
		if got := relocateHoistedCacheBoundary(ephemeral(), nil); got != hoistedCacheBoundaryKept {
			t.Errorf("得 %q, 期望 kept", got)
		}
	})

	t.Run("R1b 正常移动且marker挂到目标块", func(t *testing.T) {
		marker := ephemeral()
		pre := []any{map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}}}
		if got := relocateHoistedCacheBoundary(marker, pre); got != hoistedCacheBoundaryMoved {
			t.Errorf("得 %q, 期望 moved", got)
		}
		bm := pre[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if bm["cache_control"] == nil {
			t.Fatal("marker 未挂到目标块")
		}
		// ★ 必须是**同一个对象引用**: 参考实现搬的是引用 (caller 随后会 delete 掉
		// hoisted 上的同名键, 若这里是深拷贝, 那个 delete 就删不掉真正的 marker)。
		// 验证方式: 通过 marker 变量改字段, 目标块上的值必须同步变化。
		marker["probe"] = "touched"
		if got := bm["cache_control"].(map[string]any)["probe"]; got != "touched" {
			t.Errorf("marker 不是引用搬运 (得 %v), 期望 touched", got)
		}
	})

	t.Run("R1c 目标1h新5m得dropped", func(t *testing.T) {
		pre := []any{map[string]any{"content": []any{map[string]any{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}}}
		if got := relocateHoistedCacheBoundary(ephemeral(), pre); got != hoistedCacheBoundaryDropped {
			t.Errorf("得 %q, 期望 dropped", got)
		}
	})

	t.Run("R1d 目标1h新1h得kept", func(t *testing.T) {
		pre := []any{map[string]any{"content": []any{map[string]any{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}}}
		if got := relocateHoistedCacheBoundary(map[string]any{"type": "ephemeral", "ttl": "1h"}, pre); got != hoistedCacheBoundaryKept {
			t.Errorf("得 %q, 期望 kept", got)
		}
	})

	t.Run("R1e 跳过非数组content", func(t *testing.T) {
		pre := []any{
			map[string]any{"content": "not-an-array"},
			map[string]any{"content": []any{map[string]any{"type": "text", "text": "y"}}},
		}
		if got := relocateHoistedCacheBoundary(ephemeral(), pre); got != hoistedCacheBoundaryMoved {
			t.Errorf("得 %q, 期望 moved", got)
		}
		bm := pre[1].(map[string]any)["content"].([]any)[0].(map[string]any)
		if bm["cache_control"] == nil {
			t.Error("marker 未挂到第二个元素的目标块")
		}
	})

	t.Run("R1f marker为null仍moved", func(t *testing.T) {
		pre := []any{map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}}}
		if got := relocateHoistedCacheBoundary(nil, pre); got != hoistedCacheBoundaryMoved {
			t.Errorf("得 %q, 期望 moved", got)
		}
	})

	// ★ 探针没覆盖但必须锁死的分支: nil 元素 / 非 map 元素不得 panic
	t.Run("R1g 非map元素安全跳过", func(t *testing.T) {
		pre := []any{nil, "str", 42}
		if got := relocateHoistedCacheBoundary(ephemeral(), pre); got != hoistedCacheBoundaryKept {
			t.Errorf("得 %q, 期望 kept", got)
		}
	})
}

// ---------------------------------------------------------------- relocateDirectiveOnlyMessages

// 探针 D1-D11
func TestRelocateDirectiveOnlyMessages(t *testing.T) {
	// D1
	t.Run("D1 空messages早退", func(t *testing.T) {
		jsonEqGo(t, "D1", runRelocate(map[string]any{"messages": []any{}}), `{"messages":[]}`)
	})
	// D2
	t.Run("D2 首条非空system早退", func(t *testing.T) {
		jsonEqGo(t, "D2", runRelocate(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}),
			`{"messages":[{"role":"user","content":"hi"}]}`)
	})
	// D3
	t.Run("D3 空system无directive被丢弃", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "yo"},
		}}
		jsonEqGo(t, "D3", runRelocate(p),
			`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`)
	})
	// D4
	t.Run("D4 directive移到首个真实回合之后", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "yo"},
		}}
		jsonEqGo(t, "D4", runRelocate(p),
			`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}},{"role":"assistant","content":"yo"}]}`)
	})
	// D5
	t.Run("D5 前导run多个emptySystem全部迁移", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			map[string]any{"role": "system", "content": []any{}},
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"b": 2}},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "yo"},
		}}
		jsonEqGo(t, "D5", runRelocate(p),
			`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}},{"role":"system","content":[],"output_config":{"b":2}},{"role":"assistant","content":"yo"}]}`)
	})
	// D6
	t.Run("D6 无真实回合折叠outputConfig并丢弃", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			map[string]any{"role": "system", "content": []any{}},
		}}
		jsonEqGo(t, "D6", runRelocate(p), `{"messages":[],"output_config":{"a":1}}`)
	})
	// D7
	t.Run("D7 无真实回合且顶层已有outputConfig则顶层优先", func(t *testing.T) {
		p := map[string]any{
			"output_config": map[string]any{"keep": 1},
			"messages": []any{
				map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			},
		}
		jsonEqGo(t, "D7", runRelocate(p), `{"output_config":{"keep":1},"messages":[]}`)
	})
	// D8
	t.Run("D8 带文本的system不作插入锚点", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "mid"}}},
			map[string]any{"role": "assistant", "content": "yo"},
		}}
		jsonEqGo(t, "D8", runRelocate(p),
			`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}},{"role":"system","content":[{"type":"text","text":"mid"}]},{"role":"assistant","content":"yo"}]}`)
	})
	// D9
	t.Run("D9 畸形non-object元素安全跳过", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
			nil,
			map[string]any{"role": "user", "content": "hi"},
		}}
		jsonEqGo(t, "D9", runRelocate(p),
			`{"messages":[null,{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}}]}`)
	})
	// D10
	t.Run("D10 只有emptySystem无directive则run被丢弃", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{}},
			map[string]any{"role": "user", "content": "hi"},
		}}
		jsonEqGo(t, "D10", runRelocate(p), `{"messages":[{"role":"user","content":"hi"}]}`)
	})
	// D11
	t.Run("D11 developer空role也参与", func(t *testing.T) {
		p := map[string]any{"messages": []any{
			map[string]any{"role": "developer", "content": []any{}, "output_config": map[string]any{"a": 1}},
			map[string]any{"role": "user", "content": "hi"},
		}}
		jsonEqGo(t, "D11", runRelocate(p),
			`{"messages":[{"role":"user","content":"hi"},{"role":"developer","content":[],"output_config":{"a":1}}]}`)
	})
}

// ---------------------------------------------------------------- 辅助函数

func TestEffectiveTtl(t *testing.T) {
	cases := []struct {
		name   string
		marker any
		want   string
	}{
		{"缺 ttl 默认 5m", map[string]any{"type": "ephemeral"}, "5m"},
		{"ttl 为 1h", map[string]any{"type": "ephemeral", "ttl": "1h"}, "1h"},
		{"ttl 非字符串走默认", map[string]any{"ttl": 60}, "5m"},
		{"nil 走默认", nil, "5m"},
		{"非对象走默认", "x", "5m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveTtl(c.marker); got != c.want {
				t.Errorf("得 %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestIsSystemRoleJS(t *testing.T) {
	cases := []struct {
		role any
		want bool
	}{
		{"system", true},
		{"SYSTEM", true},
		{"System", true},
		{"developer", true},
		{"DEVELOPER", true},
		{"user", false},
		{"assistant", false},
		{"tool", false},
		{"", false},
		{nil, false},
		{42, false},
	}
	for _, c := range cases {
		if got := isSystemRoleJS(c.role); got != c.want {
			t.Errorf("isSystemRoleJS(%#v) = %v, 期望 %v", c.role, got, c.want)
		}
	}
}

// providerSupportsMidConversationSystem 照抄 claudeIdentity.ts:326-341
func TestProviderSupportsMidConversationSystem(t *testing.T) {
	cases := []struct {
		name                string
		hasSystem, hasTools bool
		model               string
		want                bool
	}{
		{"opus + system + tools", true, true, "claude-opus-4-20250514", true},
		{"OPUS 大小写不敏感", true, true, "CLAUDE-OPUS-4", true},
		{"sonnet 不在清单", true, true, "claude-sonnet-4", false},
		{"无 tools", true, false, "claude-opus-4", false},
		{"无 system", false, true, "claude-opus-4", false},
		{"空模型", true, true, "", false},
		{"子串匹配: my-claude-opus-x", true, true, "my-claude-opus-x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := providerSupportsMidConversationSystem(c.hasSystem, c.hasTools, c.model); got != c.want {
				t.Errorf("得 %v, 期望 %v", got, c.want)
			}
		})
	}
}

// ★ 幂等性: 参考实现对已合规的载荷不得反复改写
func TestExtractSystemRole_幂等(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "S1"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	first := runExtract(p)
	b, _ := json.Marshal(first)
	second := runExtract(first)
	a2, _ := json.Marshal(second)
	if string(b) != string(a2) {
		t.Errorf("非幂等:\n  第一次 %s\n  第二次 %s", b, a2)
	}
}

// ---------------------------------------------------------------- 补充场景（探针 probe_sysrole2/3）

// 探针 E3: 顶层 system 是对象（既非 string 也非数组）→ 走 else 分支被替换
func TestExtractSystemRole_顶层system为对象被替换(t *testing.T) {
	p := map[string]any{
		"system": map[string]any{"weird": true},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "system", "content": "S"},
		},
	}
	jsonEqGo(t, "对象 system", runExtract(p),
		`{"messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 E4: 只有 developer 消息且无 system 键 → system 键被新建
func TestExtractSystemRole_无system键时新建(t *testing.T) {
	p := map[string]any{"messages": []any{map[string]any{"role": "developer", "content": "D"}}}
	jsonEqGo(t, "新建 system 键", runExtract(p),
		`{"messages":[],"system":[{"type":"text","text":"D"}]}`)
}

// 探针 F1: 数字元素原样保留（`.role` 为 undefined → 非 system 角色 → 保留）
func TestExtractSystemRole_数字元素保留(t *testing.T) {
	p := map[string]any{"messages": []any{42, map[string]any{"role": "user", "content": "hi"}}}
	jsonEqGo(t, "数字元素", runExtract(p), `{"messages":[42,{"role":"user","content":"hi"}]}`)
}

// 探针 F2: 数字元素在有 system 时也保留
func TestExtractSystemRole_数字元素与system共存(t *testing.T) {
	p := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "S"}, 42}}
	jsonEqGo(t, "数字元素共存", runExtract(p),
		`{"messages":[42],"system":[{"type":"text","text":"S"}]}`)
}

// 探针 E1: 前导 run 里混着"无 directive 的空 system" → 它被丢弃，
// 只有带 output_config 的那条被迁移。
func TestRelocateDirective_无directive空system被丢弃(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{}},
		map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
		map[string]any{"role": "user", "content": "hi"},
	}}
	jsonEqGo(t, "E1", runRelocate(p),
		`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}}]}`)
}

// 探针 E2: 迁移后的 directive 保持"空数组 content + output_config"形态不变形
func TestRelocateDirective_迁移后形态不变(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"a": 1}},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "yo"},
	}}
	jsonEqGo(t, "E2", runRelocate(p),
		`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"a":1}},{"role":"assistant","content":"yo"}]}`)
}

// ★★ 与参考实现的**已知差异**（有意为之，必须显式锁定）：
//
// 参考实现 `extractSystemRoleMessages` 在 messages[] 里遇到 `null` 元素时会
// **抛 TypeError**（探针 E5 实测：`Cannot read properties of null (reading 'role')`
// at claudeSystemRole.ts:110）。这是参考实现的缺陷 —— 一个畸形请求体会让整个
// 网关 handler 500。
//
// Go 侧**不照抄这个崩溃**：`null` 元素既不是 system 角色也不是可解析对象，
// 按"保留、不参与提升"处理（Go 的 nil 断言失败即天然走这条），请求继续正常处理。
// 差异只体现在"畸形输入不再 500"，对任何合法请求体行为完全一致。
func TestExtractSystemRole_null元素不崩溃(t *testing.T) {
	p := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "S"},
		nil,
		map[string]any{"role": "user", "content": "hi"},
	}}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("参考实现会抛 TypeError, 但我方必须不崩溃, 却 panic 了: %v", r)
		}
	}()
	jsonEqGo(t, "null 元素", runExtract(p),
		`{"messages":[null,{"role":"user","content":"hi"}],"system":[{"type":"text","text":"S"}]}`)
}

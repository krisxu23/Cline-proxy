package app

import (
	"os"
	"strings"
	"testing"
)

// 本文件是 toolCallShim 的**源码层接线锁**。
//
// ★ 为什么需要它: 复刻测试只能证明"清洗函数本身对不对", 证明不了"它在真实
//   流式路径上被调用了、且只在正确的位置被调用一次"。历史教训(NEG-CACHE-A):
//   只断言"调用第一次出现的位置落在某区间"会漏掉"调用被同时塞进两个分支"
//   这类作用域错误 —— 必须断言**次数 == 1**。

// TestToolCallShim接线_源码层锁死 锁定 emitToolBlock 内的调用清单、次数与顺序。
func TestToolCallShim接线_源码层锁死(t *testing.T) {
	b, err := os.ReadFile("anthropic.go")
	if err != nil {
		t.Fatalf("读取 anthropic.go 失败: %v", err)
	}
	s := string(b)

	// --- 1) emitToolBlock 函数体的边界 ---
	fnIdx := strings.Index(s, "emitToolBlock := func(acc *toolAccumulator) {")
	if fnIdx < 0 {
		t.Fatalf("未找到 emitToolBlock 定义 —— 接线点被整体移走了")
	}
	// 函数体结束: 下一个顶层 `\n\t}` 之后再接 `\n\n\t` 起新语句。
	// 更稳的定位: emitToolBlock 之后第一个出现的 `processSSELine := func`。
	endIdx := strings.Index(s[fnIdx:], "processSSELine := func(line string) {")
	if endIdx < 0 {
		t.Fatalf("未找到 processSSELine —— 无法界定 emitToolBlock 的结束边界")
	}
	body := s[fnIdx : fnIdx+endIdx]

	// --- 2) 调用次数必须 == 1（防"两处都放"） ---
	for _, marker := range []string{
		"hasToolCallShim(",
		"applyToolCallShimToBuffer(",
	} {
		if n := strings.Count(body, marker); n != 1 {
			t.Fatalf("emitToolBlock 内 %s 应恰好出现 1 次, 实际 %d 次", marker, n)
		}
	}

	// --- 3) 顺序: hasToolCallShim -> applyToolCallShimToBuffer ---
	//     （守卫在前，避免对无 shim 的工具做无谓的字符串往返）
	guardIdx := strings.Index(body, "hasToolCallShim(")
	applyIdx := strings.Index(body, "applyToolCallShimToBuffer(")
	if guardIdx > applyIdx {
		t.Fatalf("hasToolCallShim 必须早于 applyToolCallShimToBuffer")
	}

	// --- 4) 顺序: 必须在 filterToolInput 之后（先裁字段，再清洗结构） ---
	filterIdx := strings.Index(body, "filterToolInput(")
	if filterIdx < 0 {
		t.Fatalf("emitToolBlock 内未找到 filterToolInput —— 前置步骤被移走")
	}
	if guardIdx < filterIdx {
		t.Fatalf("shim 必须晚于 filterToolInput（acc.args -> argsObj -> filter -> shim）")
	}

	// --- 5) 顺序: 必须在 input_json_delta 的 emit 之前 ---
	//     这是参考实现明确规定的消费点: "just before they are re-emitted as a
	//     single Claude input_json_delta"。
	deltaIdx := strings.Index(body, `"input_json_delta"`)
	if deltaIdx < 0 {
		t.Fatalf("emitToolBlock 内未找到 input_json_delta —— 发出点被移走")
	}
	if applyIdx > deltaIdx {
		t.Fatalf("shim 必须早于 input_json_delta 的 emit（否则客户端先看到脏参数）")
	}

	// --- 6) 反向确认: shim 不得出现在流式**分片累加**路径上 ---
	//     参考实现要求"在拼装完成之后"清洗。若 shim 出现在 processSSELine 的
	//     分片累加里，会把还没拼完的半截 JSON 当完整 JSON 解析 -> 全部退化成 {}。
	//     ★ 用文本切片而不是整文件 Count，避免把 emitToolBlock 内的那次算进来。
	rest := s[fnIdx+endIdx:]
	procEnd := strings.Index(rest, "\tfor scanner.Scan()")
	if procEnd < 0 {
		// 退一步: 找 flush 收尾段。
		procEnd = len(rest)
	}
	procBody := rest[:procEnd]
	if strings.Contains(procBody, "applyToolCallShimToBuffer(") {
		t.Fatalf("applyToolCallShimToBuffer 不得出现在 processSSELine 分片路径中")
	}
}

// TestToolCallShim_调用点在emitToolBlock内 补一条更宽松但更直观的断言:
// 全文件里 applyToolCallShimToBuffer 的**唯一**调用点必须在 emitToolBlock 区间内。
func TestToolCallShim_全文件唯一调用点(t *testing.T) {
	b, err := os.ReadFile("anthropic.go")
	if err != nil {
		t.Fatalf("读取 anthropic.go 失败: %v", err)
	}
	s := string(b)

	callMarker := "applyToolCallShimToBuffer("
	// 定义在 tool_call_shim.go, 故 anthropic.go 里出现的**每一次**都是调用。
	if n := strings.Count(s, callMarker); n != 1 {
		t.Fatalf("anthropic.go 内 %s 应恰好出现 1 次, 实际 %d 次", callMarker, n)
	}
	callIdx := strings.Index(s, callMarker)

	fnIdx := strings.Index(s, "emitToolBlock := func(acc *toolAccumulator) {")
	endIdx := strings.Index(s[fnIdx:], "processSSELine := func(line string) {")
	if fnIdx < 0 || endIdx < 0 {
		t.Fatalf("无法界定 emitToolBlock 区间")
	}
	if callIdx < fnIdx || callIdx > fnIdx+endIdx {
		t.Fatalf("调用点不在 emitToolBlock 开区间内 (call=%d, fn=%d, end=%d)",
			callIdx, fnIdx, fnIdx+endIdx)
	}
}

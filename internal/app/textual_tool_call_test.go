package app

import (
	"encoding/json"
	"testing"
)

// 用例逐条对应 OmniRoute:
//   - open-sse/utils/textualToolCall.ts 的 4 个导出
//   - tests/unit/stream-utils.test.ts:330 / :554 / :595 / :2421 / :2461
//
// 所有字符串里的零宽字符 (U+200D) 均按参考用例原样保留, 不写转义, 以免
// "清洗"被测对象本身而让用例失去意义。

// ─────────────────────── stripZeroWidth ───────────────────────

func TestStripZeroWidth_字符串剔除零宽字符(t *testing.T) {
	// 参考: stream-utils.test.ts:556 —— 上游把 U+200D 混进路径里
	in := "sqlite3 /opt/O\u200dmniRoute/data/o\u200dmniroute.db"
	got := stripZeroWidth(in).(string)
	want := "sqlite3 /opt/OmniRoute/data/omniroute.db"
	if got != want {
		t.Fatalf("stripZeroWidth 结果错误\n got: %q\nwant: %q", got, want)
	}
}

func TestStripZeroWidth_四种零宽字符全覆盖(t *testing.T) {
	// U+200B 零宽空格 / U+200C 零宽非连接符 / U+200D 零宽连接符 / U+FEFF BOM
	in := "a\u200Bb\u200Cc\u200Dd\uFEFFe"
	if got := stripZeroWidth(in).(string); got != "abcde" {
		t.Fatalf("四种零宽字符都应被剔除, got %q", got)
	}
}

func TestStripZeroWidth_递归数组与对象(t *testing.T) {
	in := map[string]any{
		"a": "\u200Bx",
		"b": []any{"\u200Dy", 1, true, nil},
		"c": map[string]any{"d": "\uFEFFz"},
	}
	got := stripZeroWidth(in).(map[string]any)
	if got["a"] != "x" {
		t.Fatalf("对象值未清洗: %#v", got["a"])
	}
	arr := got["b"].([]any)
	if arr[0] != "y" {
		t.Fatalf("数组元素未清洗: %#v", arr[0])
	}
	// 非字符串原样保留 (对应 JS 的 return value 兜底分支)
	if arr[1] != 1 || arr[2] != true || arr[3] != nil {
		t.Fatalf("非字符串元素不应被改动: %#v", arr)
	}
	if got["c"].(map[string]any)["d"] != "z" {
		t.Fatalf("嵌套对象未递归清洗")
	}
}

func TestStripZeroWidth_非字符串标量原样返回(t *testing.T) {
	for _, in := range []any{42, 3.14, true, false, nil} {
		if got := stripZeroWidth(in); got != in {
			t.Fatalf("标量 %#v 应原样返回, got %#v", in, got)
		}
	}
}

// ─────────────────── isValidToolCallHeaderPrefix ───────────────────

func TestIsValidHeader_完整合法头部(t *testing.T) {
	cases := []string{
		"[Tool call: terminal]\nArguments: {\"command\":\"ls\"}",
		"[Tool call: terminal]\nArguments:",
		"[Tool call: terminal]\nArg", // "Arg" 是 "Arguments:" 的前缀
		"[Tool call: terminal]\n",
		"[Tool call: terminal]",
		"[Tool call: terminal]   \n  ",
	}
	for _, c := range cases {
		if !isValidToolCallHeaderPrefix(c) {
			t.Fatalf("应判为合法头部: %q", c)
		}
	}
}

func TestIsValidHeader_半截名字仍合法(t *testing.T) {
	// 对应 :23-27 —— `]` 还没到, 只要名字段不含换行/`[` 就算合法前缀
	for _, c := range []string{
		"[Tool call: ter",
		"[Tool call: ",
		"[Tool call:",
	} {
		if !isValidToolCallHeaderPrefix(c) {
			t.Fatalf("流式半截头部应判为合法: %q", c)
		}
	}
}

func TestIsValidHeader_非法情形(t *testing.T) {
	cases := []struct{ in, why string }{
		{"[Tool call: ter\nminal]", "名字段含换行 (:25/:30)"},
		{"[Tool call: ]\nArguments:", "名字 trim 后为空 (:30)"},
		{"[Tool call: terminal] garbage", "']' 后有非空白且无换行 (:41-43)"},
		{"[Tool call: terminal] X\nArguments:", "']' 后首段不是 Arguments (:54)"},
		{"not a tool call", "不以 '[Tool call:' 开头 (:20)"},
		{"", "空串"},
		// 无 ']' 情形才检查名字段里的 '[' (:25)。有 ']' 时该检查不执行,
		// 所以 "[Tool call: ter[foo]" 反而合法 —— 见下面的专项用例。
		{"[Tool call: te[r", "无 ']' 且名字段含 '[' (:25)"},
		{"[Tool call: te\nr", "无 ']' 且名字段含换行 (:25)"},
	}
	for _, c := range cases {
		if isValidToolCallHeaderPrefix(c.in) {
			t.Fatalf("应判为非法 (%s): %q", c.why, c.in)
		}
	}
}

// 名字段里的 '[' 只在"无 ']'"分支被检查 (:24-27)。
// 一旦串里出现 ']', 走的是 :29-30 分支, 那里只查换行与 trim 空。
// 因此 "[Tool call: ter[foo]" 是**合法**的 —— Node 实测为 true。
// 这条用例锁住这个反直觉行为, 防止后人"顺手补一个 '[' 检查"而偏离原实现。
func TestIsValidHeader_有右括号时名字可含左括号(t *testing.T) {
	in := "[Tool call: ter[foo]"
	if !isValidToolCallHeaderPrefix(in) {
		t.Fatal("含 ']' 时不应再检查名字段的 '[', 应判合法 (照抄 :29-30)")
	}
}

func TestIsValidHeader_零宽字符破坏前缀匹配(t *testing.T) {
	// 原实现只做 replace(strip), 不预先归一化 —— 零宽字符若插在
	// "[Tool call:" 之后, `startsWith` 仍成立 (Node 实测 true);
	// 但若插在 "Arguments:" 与名字之间, 后续分支行为随之改变。
	//
	// 关键: 该函数**不做**零宽清洗, 清洗发生在 parseTextualToolCallCandidate
	// 的入口 (:61)。这里锁住"本函数不负责清洗"这一职责边界。
	in := "[Tool call:\u200Bterminal]\nArguments: {}"
	// 前缀 "[Tool call:" 完好, 所以 startsWith 成立
	if !isValidToolCallHeaderPrefix(in) {
		t.Fatal("前缀完好时该函数应判合法 (它不做零宽清洗)")
	}
	// 若零宽字符插在前缀内部, startsWith 失败 → 判非法
	if isValidToolCallHeaderPrefix("[Tool call\u200B: terminal]\nArguments: {}") {
		t.Fatal("前缀被零宽字符切断时应判非法")
	}
}

// ─────────────────── parseTextualToolCallCandidate ───────────────────

func mustComplete(t *testing.T, text string) *textualToolCallCandidate {
	t.Helper()
	c := parseTextualToolCallCandidate(text)
	if c == nil || c.kind != "complete" {
		t.Fatalf("应解析为 complete, got %#v", c)
	}
	return c
}

func TestParse_标准形态解析成功(t *testing.T) {
	c := mustComplete(t, "[Tool call: terminal]\nArguments: {\"command\":\"ls -la\"}")
	if c.name != "terminal" {
		t.Fatalf("工具名错误: %q", c.name)
	}
	m := c.args.(map[string]any)
	if m["command"] != "ls -la" {
		t.Fatalf("参数错误: %#v", m)
	}
}

func TestParse_正文前缀不影响解析(t *testing.T) {
	// 对应 stream-utils.test.ts:461 —— 模型先说话再发起调用
	text := "Вот оно! Статические файлы Next.js отдают 404.\n\n" +
		"[Tool call: terminal]\nArguments: {\"command\":\"ls\"}"
	c := mustComplete(t, text)
	if c.name != "terminal" {
		t.Fatalf("工具名错误: %q", c.name)
	}
}

func TestParse_取最后一个标记(t *testing.T) {
	// :62 lastIndexOf —— 正文里先提到一次标记, 真正的调用在后
	text := "I will use [Tool call: x] format.\n" +
		"[Tool call: terminal]\nArguments: {\"a\":1}"
	c := mustComplete(t, text)
	if c.name != "terminal" {
		t.Fatalf("应取最后一个 '[Tool call:' 之后的调用, got %q", c.name)
	}
}

func TestParse_参数整体被引号包裹时走decoder1(t *testing.T) {
	// 对应 :83-99 的 decoders 循环。
	//
	// ⚠ 这条用例锁住一个反直觉、但经 Node 实测确认的参考行为:
	// 输入 `"{\"command\":\"ls\"}"` 时, **decoder 1 (恒等) 就已经成功** ——
	// `JSON.parse` 返回的是**字符串** `{"command":"ls"}`, 函数立刻返回,
	// 因此 args 类型是 string, 不是 object。decoder 2 永远执行不到。
	//
	// 我第一版曾据此"修 bug"把 args 改成对象, 那属于擅自改动参考实现,
	// 已回退。此用例的作用就是防止后人再次踩同一个坑。
	c := mustComplete(t, `[Tool call: terminal]`+"\n"+`Arguments: "{\"command\":\"ls\"}"`)
	s, ok := c.args.(string)
	if !ok {
		t.Fatalf("照抄参考实现: args 应为 string (decoder 1 先成功), 实际是 %T: %#v", c.args, c.args)
	}
	if s != `{"command":"ls"}` {
		t.Fatalf("args 内容错误: %q", s)
	}
}

func TestParse_参数为普通对象时不得被当成字符串(t *testing.T) {
	// 反向对照: 常规形态必须走 decoder 1, 且 args 是对象
	c := mustComplete(t, "[Tool call: terminal]\nArguments: {\"a\":1}")
	if _, ok := c.args.(map[string]any); !ok {
		t.Fatalf("args 必须是对象, 实际是 %T", c.args)
	}
}

func TestParse_参数为JSON数组也接受(t *testing.T) {
	// 原实现不限定 args 是对象, 数组同样 JSON.parse 成功即 complete
	c := mustComplete(t, "[Tool call: terminal]\nArguments: [1,2,3]")
	arr, ok := c.args.([]any)
	if !ok {
		t.Fatalf("args 应为数组, 实际 %T", c.args)
	}
	if len(arr) != 3 {
		t.Fatalf("数组长度错误: %#v", arr)
	}
}

func TestParse_参数内零宽字符被清洗(t *testing.T) {
	c := mustComplete(t, "[Tool call: terminal]\nArguments: {\"command\":\"sqlite3 /opt/O\u200dmniRoute\"}")
	m := c.args.(map[string]any)
	if m["command"] != "sqlite3 /opt/OmniRoute" {
		t.Fatalf("args 未做零宽清洗: %#v", m["command"])
	}
}

func TestParse_头部零宽字符先清洗再匹配(t *testing.T) {
	// :61 先 replace 再 lastIndexOf
	c := mustComplete(t, "[Tool call:\u200Bterminal]\nArguments: {\"a\":1}")
	if c.name != "terminal" {
		t.Fatalf("零宽清洗后应解析出 terminal, got %q", c.name)
	}
}

func TestParse_partial_参数未到齐(t *testing.T) {
	// 对应 stream-utils.test.ts:279 / :330 / :405 —— 头部与参数跨 chunk
	cases := []string{
		"[Tool call: terminal]\n",             // Arguments 还没来 (:79 headerMatch 失配)
		"[Tool call: terminal]\nArguments:",   // 参数体还没来 (:82 rawArgs 为空)
		"[Tool call: ter",                     // 连名字都没齐 (:69-71)
		"(empty)[Tool call:",                  // 只到了 "(empty)[" (:65-67)
		"[Tool call: terminal]\nArguments: {", // 参数是半截 JSON (:100 两个 decoder 都失败)
	}
	for _, c := range cases {
		got := parseTextualToolCallCandidate(c)
		if got == nil {
			t.Fatalf("应判为 partial (非 nil): %q", c)
		}
		if got.kind != "partial" {
			t.Fatalf("应判为 partial, got %q for %q", got.kind, c)
		}
	}
}

func TestParse_null_非候选(t *testing.T) {
	// 对应 stream-utils.test.ts:2423 / :2463 —— 误报场景必须返回 nil
	cases := []string{
		"Checking: [Tool call: terminal] was executed successfully.", // 标记后不是合法头部
		"[Tool call: terminal] was skipped.",
		"just normal text",
		"",
	}
	for _, c := range cases {
		if got := parseTextualToolCallCandidate(c); got != nil {
			t.Fatalf("应返回 nil (非候选), got kind=%q for %q", got.kind, c)
		}
	}
}

// 紧凑畸形形态 "[Tool call: name{k:v}]"。
//
// Node 实测: `]` 紧跟在名字后, 于是 rest 为空 → isValidToolCallHeaderPrefix
// 返回 **true**; 但头部正则要求 `]\nArguments:`, 匹配失败 → **partial**。
//
// 也就是说: 这种畸形串不是被解析器"判定为非候选"而拦下的, 而是被判为
// partial。真正让它不出现在下游的是调用方 —— 参考实现 stream.ts:2527 的
// `containsMalformedTextualToolCall` 分支会把 content 清空 (:2528)。
// 这个职责划分必须锁住, 否则后人可能误把拦截逻辑塞进解析器。
func TestParse_紧凑畸形形态为partial由调用方拦截(t *testing.T) {
	in := "[Tool call: search_files_ide{file_glob:*combos*.ts,path:/opt/OmniRoute,target:files}]"
	got := parseTextualToolCallCandidate(in)
	if got == nil {
		t.Fatal("照抄参考实现: 该形态应返回 partial, 而非 nil")
	}
	if got.kind != "partial" {
		t.Fatalf("应为 partial, got %q", got.kind)
	}
	// 但 isValidToolCallHeaderPrefix 认为它是合法头部 —— 两者并不矛盾
	if !isValidToolCallHeaderPrefix(in) {
		t.Fatal("照抄参考实现: 该形态的头部前缀判定应为 true")
	}
}

func TestParse_非字符串返回nil(t *testing.T) {
	for _, in := range []any{nil, 42, true, []any{"x"}, map[string]any{"a": 1}} {
		if got := parseTextualToolCallCandidate(in); got != nil {
			t.Fatalf("非字符串应返回 nil, got %#v (in=%#v)", got, in)
		}
	}
}

// ─────────────────── containsTextualToolCallMarker ───────────────────

func TestContainsMarker_完整标记(t *testing.T) {
	if !containsTextualToolCallMarker("[Tool call: terminal]\nArguments: {}") {
		t.Fatal("含 Arguments: 应判真 (:108)")
	}
}

func TestContainsMarker_仅有起始标记(t *testing.T) {
	if !containsTextualToolCallMarker("[Tool call: terminal") {
		t.Fatal("以 '[Tool call:' 开头应判真 (:110-111)")
	}
	if !containsTextualToolCallMarker("(empty)[Tool call:") {
		t.Fatal("'(empty)[Tool call:' 前缀应判真 (:111)")
	}
}

func TestContainsMarker_正文中提到标记(t *testing.T) {
	// 含 "Arguments:" 就判真, 即使它只是被讨论的文本 —— 照抄原实现语义
	if !containsTextualToolCallMarker("Docs say use [Tool call: x] and Arguments: y") {
		t.Fatal("含 Arguments: 应判真")
	}
}

func TestContainsMarker_无标记(t *testing.T) {
	for _, c := range []string{"", "plain text", "an [ unrelated bracket"} {
		if containsTextualToolCallMarker(c) {
			t.Fatalf("应判假: %q", c)
		}
	}
}

func TestContainsMarker_零宽字符先清洗(t *testing.T) {
	if !containsTextualToolCallMarker("[Tool call:\u200Bterminal]\nArguments: {}") {
		t.Fatal("零宽字符应先被清洗后仍能识别 (:105)")
	}
}

func TestContainsMarker_非字符串判假(t *testing.T) {
	for _, in := range []any{nil, 42, []any{"[Tool call: x]"}} {
		if containsTextualToolCallMarker(in) {
			t.Fatalf("非字符串应判假: %#v", in)
		}
	}
}

// ─────────────────── 与 JSON 序列化的协作 ───────────────────

func TestParse_复杂嵌套参数(t *testing.T) {
	raw := "[Tool call: search_files_ide]\nArguments: " +
		`{"file_glob":"*combos*.ts","path":"/opt/OmniRoute","target":"files","opts":{"deep":true,"max":10}}`
	c := mustComplete(t, raw)
	if c.name != "search_files_ide" {
		t.Fatalf("工具名错误: %q", c.name)
	}
	// args 必须能再次序列化回 JSON (供 collectPassthroughTextualToolCall 用)
	b, err := json.Marshal(c.args)
	if err != nil {
		t.Fatalf("args 无法回写 JSON: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("args 回写后无法再解析: %v", err)
	}
	if back["target"] != "files" {
		t.Fatalf("嵌套字段丢失: %#v", back)
	}
}

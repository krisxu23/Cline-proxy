package app

// 逐字照抄 OmniRoute open-sse/services/responsesItemId.ts (7 行)。
//
// 原注释 :1-4:
//
//	Shared by reasoningInputPolicy.ts and responsesInputSanitizer.ts: both strip a
//	Responses-API `input[]` item's `id` field when it isn't a valid string before
//	replay, so a malformed value (e.g. `null`, observed on opencode/zen) never
//	survives to trip a strict upstream with "Expected 'id' to be a string." (#11108).
//
// 这条与用户的场景**直接相关**: 报错来源就是 opencode/zen 路径 —— 回放会话历史时
// `input[]` 里的 `id` 字段若为 `null` 等非字符串值, 严格上游会以
// "Expected 'id' to be a string." 拒绝整个请求。修复方式是在回放前把非法 id 剥掉。
//
// 参考实现的类型守卫是 `id is string`, 即**仅**接受 string 类型 ——
// 数字 / null / 对象全部判非法 (不是"能转成字符串就算合法")。
func isValidResponsesItemId(id any) bool {
	// :5-7 `export function isValidResponsesItemId(id: unknown): id is string {
	//         return typeof id === "string";
	//       }`
	_, ok := id.(string)
	return ok
}

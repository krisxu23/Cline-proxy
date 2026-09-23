package app

import "testing"

// 判定源是生产在用的 isKeyDemoted(曾是 pickHealthyKeys, 已随死代码删除);
// 两者本就是同一谓词 —— pickHealthyKeys 只是把未降级的 key 收集成列表。
func TestKeyHealthDemoteRecover(t *testing.T) {
	resetKeyHealthState()
	p := "demo"
	k := "sk-1"
	for range 5 {
		recordKeyResult(p, k, 500, false)
	}
	if !isKeyDemoted(p, k) {
		t.Fatalf("should demote")
	}
	// Cooldown expired: backdate demotedAt past 5min, key is healthy again.
	keyHealthMu.Lock()
	keyHealth[p][k].demotedAt -= keyCooldownMs + 1
	keyHealthMu.Unlock()
	if isKeyDemoted(p, k) {
		t.Fatalf("should recover after cooldown")
	}
	// 429 never records: fresh key stays undemoted after any number of 429s.
	resetKeyHealthState()
	for range 10 {
		recordKeyResult(p, "sk-429", 429, false)
	}
	if isKeyDemoted(p, "sk-429") {
		t.Fatalf("429-only key must leave no record")
	}
}

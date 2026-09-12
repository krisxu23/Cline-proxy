package app

import "testing"

func TestKeyHealthDemoteRecover(t *testing.T) {
	resetKeyHealthState()
	p := "demo"
	k := "sk-1"
	for i := 0; i < 5; i++ {
		recordKeyResult(p, k, 500, false)
	}
	if len(pickHealthyKeys(p)) != 0 {
		t.Fatalf("should demote")
	}
	// Cooldown expired: backdate demotedAt past 5min, key is healthy again.
	keyHealthMu.Lock()
	keyHealth[p][k].demotedAt -= keyCooldownMs + 1
	keyHealthMu.Unlock()
	if got := pickHealthyKeys(p); len(got) != 1 || got[0] != k {
		t.Fatalf("should recover after cooldown, got %v", got)
	}
	// 429 never records: fresh key stays healthy after any number of 429s.
	resetKeyHealthState()
	for i := 0; i < 10; i++ {
		recordKeyResult(p, "sk-429", 429, false)
	}
	if got := pickHealthyKeys(p); len(got) != 0 {
		t.Fatalf("429-only key must leave no record, got %v", got)
	}
}

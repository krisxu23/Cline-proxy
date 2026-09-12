package app

import "testing"

func TestKeyHealthDemoteRecover(t *testing.T) {
	p := "demo"
	k := "sk-1"
	for i := 0; i < 5; i++ {
		recordKeyResult(p, k, 500, false)
	}
	if len(pickHealthyKeys(p)) != 0 {
		t.Fatalf("should demote")
	}
}

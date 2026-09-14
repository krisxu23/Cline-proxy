package app

import (
	"sync"
	"testing"
	"time"
)

// TestPoolSnapshotNoDataRace 证明: 只读路径(poolSnapshot)与写路径
// (pickAccount / markAccountCooldown, 均持 poolMu 修改账号字段)并发时不再构成
// 数据竞争。
//
// 旧实现里这些只读调用点直接 loadPool() 拿共享 *AccountPool, 在锁外读
// p.Accounts[i].Status 等字段, 与持 poolMu 的写方并发 → 真竞态。
// 修复后 poolSnapshot 在持 poolMu 期间把字段克隆进快照, 锁外读快照, 不再竞争。
//
// 负向验证(临时把 poolSnapshot 改成锁外裸读后再跑本测试)会在 Account.Status
// 处报 WARNING: DATA RACE, 修复后干净通过。
func TestPoolSnapshotNoDataRace(t *testing.T) {
	// 准备一个含两个账号的池子。
	poolMu.Lock()
	pool = &AccountPool{
		Accounts: []*Account{
			{AccountID: "race-a1", Email: "a1@e.z", Status: "active", RefreshToken: "rt1"},
			{AccountID: "race-a2", Email: "a2@e.z", Status: "active", RefreshToken: "rt2"},
		},
		CurrentIdx: 0,
		Keys:       []string{"k1", "k2"},
	}
	poolMu.Unlock()

	// rounds 取 400 而不是上万: 竞态检测靠的是两个 goroutine 长期交错, 单轮成本
	// 并不低(markAccountCooldown 会 markPoolDirty 触发落盘), 5000 轮实测要 36 秒,
	// 带 -race 会膨胀到分钟级, 挂进 CI 得不偿失。400 轮已经足够让 -race 稳定命中
	// (负向验证时依然必报 DATA RACE)。
	const rounds = 400
	var wg sync.WaitGroup

	// G1: 只读路径, 反复取快照并读取其中每个字段。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			s := poolSnapshot()
			_ = s.CurrentIdx
			_ = len(s.Accounts)
			_ = len(s.Keys)
			for _, a := range s.Accounts {
				_ = a.AccountID
				_ = a.Email
				_ = a.RefreshToken
				_ = a.Status
				_ = a.CooldownUntil
			}
		}
	}()

	// G2: 写路径, 反复选号 + 把账号切到瞬时冷却(下个 pickAccount 自动解除),
	// 持续在 poolMu 下改写 a.Status / a.CooldownUntil。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if acc := pickAccount(); acc != nil {
				// 用极小 duration 让冷却下一轮即过期, 保证写路径持续改写账号字段。
				markAccountCooldown(acc, "race-test", time.Nanosecond)
			}
		}
	}()

	wg.Wait()
}

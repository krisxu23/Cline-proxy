package app

import (
	"cline-go-proxy/internal/cline"
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	pool       *AccountPool
	poolMu     sync.Mutex
	poolSaveMu sync.Mutex
	poolPath   string

	// 写盘批量化: 之前每次 pickAccount / bumpUsage / recordAccountTokens 都直接
	// 落盘, 多账号池 + 高并发下 poolSaveMu 成为写入热点, 磁盘 I/O 是主瓶颈。
	// 现在的策略:
	//   - 关键状态变更(账号增删、token 刷新、cooldown 切换)→ 立即 flush;
	//   - 计数器类变更(usage / tokens / CurrentIdx)→ 只标记 dirty, 后台每 30s
	//     或下一次关键事件时 flush。
	// 进程崩溃时最坏丢 30s 内的计数器增量, 不丢账号/token/status 这类关键状态。
	poolDirty       atomic.Bool
	poolFlusherOnce sync.Once
	poolFlushCh     chan struct{}
	// poolFlushInterval 后台 flush 周期。30s 是"用户能感知到数据更新"与"写盘频率"
	// 的折中: 面板轮询通常 10-30s 一次, 更短没有意义; 更长则崩溃时丢的计数器更多。
	poolFlushInterval = 30 * time.Second
)

func init() {
	poolPath = kit.ResolveDataPath(".cline-accounts.json")
}

// startPoolFlusher 惰性启动写盘协程(全局一次)。
func startPoolFlusher() {
	poolFlusherOnce.Do(func() {
		poolFlushCh = make(chan struct{}, 1)
		go poolFlusherLoop()
	})
}

// poolFlusherLoop 常驻协程: 定期或收到 kick 时把 dirty 的 pool 落到磁盘。
// 注意这里不持 poolMu, 由 flushPoolLocked 自己拿锁, 避免死锁。
func poolFlusherLoop() {
	ticker := time.NewTicker(poolFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-poolFlushCh:
			if poolDirty.Swap(false) {
				flushPoolLocked()
			}
		case <-ticker.C:
			if poolDirty.Swap(false) {
				flushPoolLocked()
			}
		case <-appRootCtx.Done():
			// 收到退出信号: 停止后台落盘协程, 让进程能够真正停下。
			return
		}
	}
}

// markPoolDirty 只标记脏, 不立即写盘。用于计数器类更新(bumpUsage / recordAccountTokens
// / pickAccount 的 CurrentIdx 推进 / ListAccounts 释放冷却)。
func markPoolDirty() {
	poolDirty.Store(true)
	startPoolFlusher()
	select {
	case poolFlushCh <- struct{}{}:
	default:
	}
}

// flushPoolLocked 立即把 pool 落盘, 调用方不持 poolMu 也可以安全调用。
// 供关键状态变更(账号增删、token 刷新、cooldown 切换)使用。
func flushPoolLocked() {
	poolMu.Lock()
	// savePoolLocked 内部会释放 poolMu 并完成写盘。
	savePoolLocked()
}

// kit.ResolveDataPath 数据文件路径解析：优先可执行文件目录，其次当前工作目录。
// go run 运行时编译产物在临时目录，此时应回退到工作目录（项目根）查找数据文件。

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()
	loadPoolLocked()
	return pool
}

// loadPoolLocked 假定调用方已持有 poolMu, 负责在 pool 尚未初始化时从磁盘载入。
// 与 loadPool 拆开是为了让 poolSnapshot 等在持锁状态下复用同一套载入逻辑,
// 而无需二次加锁(否则会死锁)。
func loadPoolLocked() {
	if pool != nil {
		return
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		// 与 loadZenConfig 保持同一行为: 解析失败不能静默清空 —— .cline-accounts.json
		// 一旦坏了(半截 JSON / 编码错), 里面是所有账号的 refreshToken, 无声丢光的
		// 后果比"暂时用不上"严重得多。改个名留证, 让用户能从副本里人工恢复。
		log.Printf("pool parse failed (%s): %v; quarantining and starting with empty pool", poolPath, err)
		quarantineBadConfig(poolPath)
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
		return
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	pool = &p
	if p.DefaultModel != "" {
		// defaultModel 由 modelsMu 统一保护(与 setDefaultModel / getDefaultModel 同锁)。
		// 此处持 poolMu, 临时获取 modelsMu; 全代码库不存在"持 modelsMu 再取 poolMu"的
		// 同时持锁顺序, 因此不会与 setDefaultModel 的 modelsMu→poolMu 形成死锁。
		modelsMu.Lock()
		defaultModel = p.DefaultModel
		modelsMu.Unlock()
	}
}

// poolSnapshotAccount 是只读调用方需要的账号字段子集(深拷贝载体)。
// 只带调用方真正读取的字段, 避免把 AccessToken 等可写字段一并暴露成可共享对象。
type poolSnapshotAccount struct {
	AccountID     string
	Email         string
	RefreshToken  string
	Status        string
	CooldownUntil time.Time
}

// poolSnapshotData 账号池的只读深拷贝快照(见 poolSnapshot)。
type poolSnapshotData struct {
	CurrentIdx int
	Keys       []string
	Accounts   []poolSnapshotAccount
}

// poolSnapshot 返回账号池的只读深拷贝快照。
//
// 与 modelsCacheSnapshot / getZenConfig().clone() / statsAgg.clone() 同款设计:
// 在持 poolMu 期间把当前 pool 的状态克隆进一份与全局隔离的快照, 锁外返回快照,
// 调用方在锁外读取不会产生与写方(refreshAccountToken / pickAccount / addAccount
// 等持 poolMu 的写)的数据竞争。
//
// 此前调用方(admin 的 stats / export、zen 的 clinePoolReady 等)直接 loadPool()
// 拿到共享 *AccountPool 后在锁外读 p.Accounts[i].Status 等字段, 与持 poolMu 的写方
// 并发 —— pickAccount 是每请求都走的选号热路径, 因此这是真竞态, -race 能稳定复现。
// 改成走 poolSnapshot 后彻底消掉。
//
// 注意: 本函数内部会取 poolMu, 绝对不要在已经持有 poolMu 的代码块里调用它。
func poolSnapshot() poolSnapshotData {
	poolMu.Lock()
	// 复用在持锁前提下的载入逻辑, 不能调 loadPool()(它会再取一次 poolMu, 直接死锁)。
	loadPoolLocked()
	s := poolSnapshotData{
		CurrentIdx: pool.CurrentIdx,
		Keys:       append([]string(nil), pool.Keys...),
		Accounts:   make([]poolSnapshotAccount, 0, len(pool.Accounts)),
	}
	for _, a := range pool.Accounts {
		s.Accounts = append(s.Accounts, poolSnapshotAccount{
			AccountID:     a.AccountID,
			Email:         a.Email,
			RefreshToken:  a.RefreshToken,
			Status:        a.Status,
			CooldownUntil: a.CooldownUntil,
		})
	}
	poolMu.Unlock()
	return s
}

// setDefaultModel 持久化默认模型：更新内存全局(由 modelsMu 保护)并写入账号池文件
func setDefaultModel(modelID string) {
	initModelsCache()
	modelsMu.Lock()
	_, ok := modelsCache[modelID]
	if !ok {
		modelsMu.Unlock()
		return
	}
	// defaultModel 由 modelsMu 统一保护: 所有读写点都持 modelsMu, 避免数据竞争。
	defaultModel = modelID
	modelsMu.Unlock()
	p := loadPool()
	poolMu.Lock()
	p.DefaultModel = modelID
	poolMu.Unlock()
	savePool()
}

// savePool 把当前 pool 落盘。持池锁只做 json.Marshal(纯内存、很快), 随后释放
// 池锁, 真正耗时的磁盘 IO(temp+fsync+rename)交给 poolSaveMu, 不再阻塞热路径
// pickAccount 抢同一把池锁 → 消除 p99 延迟尖刺。落盘的是锁内那一刻的快照,
// "写的是最新状态"语义不退化: 即使写盘期间其它写方继续改 pool, 落盘内容仍然是
// 本次 marshal 时的一致视图。
func savePool() {
	poolMu.Lock()
	data, err := json.MarshalIndent(pool, "", "  ")
	poolMu.Unlock()
	if err != nil {
		// marshal 失败绝不能落盘: 写 nil 会清空账号池, 宁可保留旧文件。
		log.Printf("Failed to marshal accounts: %v", err)
		return
	}
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()
	if err := kit.WriteFileAtomicDefault(poolPath, data); err != nil {
		log.Printf("Failed to save accounts: %v", err)
	}
}

// savePoolLocked 调用方已持 poolMu 时使用的落盘入口: 在锁内完成 marshal 得到
// 字节, 释放池锁, 再由 poolSaveMu 承接磁盘 IO。注意: 调用后 poolMu 已被本函数
// 释放, 调用方不得再访问共享字段, 也不得再次 poolMu.Unlock()。
func savePoolLocked() {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		// marshal 失败绝不能落盘: 写 nil 会清空账号池, 宁可保留旧文件。
		log.Printf("Failed to marshal accounts: %v", err)
		poolMu.Unlock()
		return
	}
	poolMu.Unlock()
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()
	if err := kit.WriteFileAtomicDefault(poolPath, data); err != nil {
		log.Printf("Failed to save accounts: %v", err)
	}
}

func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	p.Accounts = append(p.Accounts, acc)
	poolMu.Unlock()
	savePool()
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			// savePoolLocked 内部会释放 poolMu, 之后不要再访问共享字段。
			savePoolLocked()
			return true
		}
	}
	poolMu.Unlock()
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

// refreshAccountTokenFn 是 cline.RefreshClineToken 的可替换包装(便于测试 stub 网络调用)。
// 返回解析后的 accessToken / refreshToken / 过期毫秒时间戳。生产默认直接转发到 cline 包。
var refreshAccountTokenFn = func(refreshToken string) (accessToken string, refreshTokenOut string, expiresAt int64, err error) {
	resp, e := cline.RefreshClineToken(refreshToken)
	if e != nil {
		return "", "", 0, e
	}
	return "workos:" + resp.Data.AccessToken, resp.Data.RefreshToken, cline.ParseExpiry(resp.Data.ExpiresAt) - 60000, nil
}

// classifyRefreshError 判定 refreshAccountTokenFn 返回的错误属于"refreshToken
// 真的无效"还是"上游/网络瞬态故障"。判定依据来自 cline.RefreshClineToken 的错误
// 字符串:
//   - "cline refresh failed: 401/403" → 上游明确拒绝, refreshToken 已失效;
//   - 其余("cline refresh: <neterr>" / "failed: 5xx" / decode 失败等) → 可能是
//     网络抖动或上游故障, 账号本身没坏。
//
// 之前的实现把两种都判成 expired: 一次节点抖动就让账号永久过期, 用户只能去 UI
// 手动"重置"。这里把瞬态故障走 cooldown(默认 5 分钟), 到期自动恢复, 避免池子
// 因为网络抖动整池清空。
func classifyRefreshError(err error) (isExpired bool, reason string) {
	if err == nil {
		return false, ""
	}
	s := err.Error()
	// cline.RefreshClineToken 在 HTTP 非 200 时返回 "cline refresh failed: <status>"
	for _, code := range []string{"failed: 401", "failed: 403"} {
		if strings.Contains(s, code) {
			return true, "refresh token rejected by upstream"
		}
	}
	// 上游限流/内部错误的 429/503 也算瞬态, 冷却后重试
	return false, "token refresh transient failure: " + s
}

func refreshAccountToken(acc *Account) error {
	if acc == nil {
		return fmt.Errorf("refreshAccountToken: nil account")
	}
	accessToken, refreshTokenOut, expiresAt, err := refreshAccountTokenFn(acc.RefreshToken)
	if err != nil {
		isExpired, reason := classifyRefreshError(err)
		poolMu.Lock()
		if isExpired {
			acc.Status = "expired"
			acc.LastReason = reason
		} else {
			// 短冷却 5 分钟, 到期后 pickAccount 会自动恢复为 active。
			acc.Status = "cooldown"
			acc.CooldownUntil = time.Now().Add(5 * time.Minute)
			acc.LastReason = reason
			log.Printf("  account %s refresh failed (%v), cooldown 5m", acc.AccountID, err)
		}
		// savePoolLocked 内部会释放 poolMu, 之后不要再访问共享字段。
		savePoolLocked()
		return fmt.Errorf("token refresh failed: %w", err)
	}

	poolMu.Lock()
	acc.AccessToken = accessToken
	if refreshTokenOut != "" {
		acc.RefreshToken = refreshTokenOut
	}
	acc.ExpiresAt = expiresAt
	acc.Status = "active"
	acc.CooldownUntil = time.Time{}
	acc.LastReason = ""
	// savePoolLocked 内部会释放 poolMu, 之后不要再访问共享字段。
	savePoolLocked()
	return nil
}

func pickAccount() *Account {
	p := loadPool()
	poolMu.Lock()

	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		// 自动解除已到期的冷却
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.Status == "active" {
			active = append(active, a)
		}
	}

	if len(active) == 0 {
		poolMu.Unlock()
		return nil
	}

	cfg := getProxyConfig()

	var acc *Account
	switch cfg.Strategy {
	case "fill":
		// Always pick the first available (fill)
		acc = active[0]
	case "random":
		// Random selection
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default: // round_robin
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}

	markPoolDirty()
	poolMu.Unlock()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	// 在 poolMu 下把字段读进局部变量, 避免与 refreshAccountToken 的写并发竞争。
	// 持锁只做快照, 不在锁内发网络请求(刷新由 refreshAccountToken 内部在无锁态完成)。
	poolMu.Lock()
	valid := acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt
	tok := acc.AccessToken
	poolMu.Unlock()
	if valid {
		return tok, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	// 刷新后再次在锁内读最新 token。
	poolMu.Lock()
	tok = acc.AccessToken
	poolMu.Unlock()
	return tok, nil
}

func ListAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()

	// 自动解除已到期的冷却，确保返回的列表是最新状态
	usageDate := time.Now().Format("2006-01-02")
	for _, a := range p.Accounts {
		if a.Status == "cooldown" && !a.CooldownUntil.IsZero() && time.Now().After(a.CooldownUntil) {
			a.Status = "active"
			a.CooldownUntil = time.Time{}
			a.LastReason = ""
		}
		if a.UsageDate != usageDate {
			a.UsageDate = usageDate
			a.UsageCountToday = 0
		}
		if a.TokensDate != usageDate {
			a.TokensDate = usageDate
			a.TokensToday = 0
		}
	}
	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		// Don't expose tokens
		result[i] = &Account{
			AccountID:       a.AccountID,
			Email:           a.Email,
			Status:          a.Status,
			LastUsed:        a.LastUsed,
			UsageCount:      a.UsageCount,
			UsageCountToday: a.UsageCountToday,
			UsageDate:       a.UsageDate,
			TokensTotal:     a.TokensTotal,
			TokensToday:     a.TokensToday,
			TokensDate:      a.TokensDate,
			CreatedAt:       a.CreatedAt,
			CooldownUntil:   a.CooldownUntil,
			LastReason:      a.LastReason,
		}
	}
	markPoolDirty()
	poolMu.Unlock()
	return result
}

// markAccountCooldown 将账号置为冷却状态，并记录预计恢复时间。
// duration 为冷却时长；duration<=0 时使用默认冷却。
func markAccountCooldown(acc *Account, reason string, duration time.Duration) {
	if acc == nil {
		return
	}
	if duration <= 0 {
		duration = 18 * time.Hour // 默认 18 小时（Cline 免费额度每日重置）
	}
	poolMu.Lock()
	acc.Status = "cooldown"
	acc.CooldownUntil = time.Now().Add(duration)
	acc.LastReason = reason
	// savePoolLocked 内部会释放 poolMu, 之后不要再访问共享字段。
	savePoolLocked()
}

// bumpUsage 递增本地成功调用计数（含今日计数），自动处理跨日重置。
// 计数器变更走 markPoolDirty: 进程崩溃最多丢 30s 的增量, 不丢账号/token/status。
func bumpUsage(acc *Account) {
	if acc == nil {
		return
	}

	poolMu.Lock()
	now := time.Now()
	today := now.Format("2006-01-02")
	if acc.UsageDate != today {
		acc.UsageDate = today
		acc.UsageCountToday = 0
	}
	acc.UsageCountToday++
	acc.UsageCount++
	acc.LastUsed = now
	markPoolDirty()
	poolMu.Unlock()
}

// resetTodayUsage 仅重置本地今日调用计数，不影响累计调用次数。
func resetTodayUsage(acc *Account) {
	if acc == nil {
		return
	}

	poolMu.Lock()
	acc.UsageDate = time.Now().Format("2006-01-02")
	acc.UsageCountToday = 0
	acc.TokensDate = time.Now().Format("2006-01-02")
	acc.TokensToday = 0
	// savePoolLocked 内部会释放 poolMu, 之后不要再访问共享字段。
	savePoolLocked()
}

// recordAccountTokens 记录账号本次请求消耗的 token（prompt+completion），
// 自动处理跨日重置，累计值不重置。tokens<=0 时忽略。
// 计数器变更走 markPoolDirty, 同上。
func recordAccountTokens(acc *Account, tokens int64) {
	if acc == nil || tokens <= 0 {
		return
	}

	poolMu.Lock()
	today := time.Now().Format("2006-01-02")
	if acc.TokensDate != today {
		acc.TokensDate = today
		acc.TokensToday = 0
	}
	acc.TokensToday += tokens
	acc.TokensTotal += tokens
	markPoolDirty()
	poolMu.Unlock()
}

// describePoolStatus 汇总当前账号池状态，用于错误诊断。
func describePoolStatus() string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	total := len(p.Accounts)
	if total == 0 {
		return "pool is empty, use --add-account or admin API to add accounts"
	}

	active, cooldown, expired := 0, 0, 0
	var nextRecover *time.Time
	for _, a := range p.Accounts {
		switch a.Status {
		case "active":
			active++
		case "cooldown":
			cooldown++
			if !a.CooldownUntil.IsZero() {
				if nextRecover == nil || a.CooldownUntil.Before(*nextRecover) {
					t := a.CooldownUntil
					nextRecover = &t
				}
			}
		case "expired":
			expired++
		}
	}

	s := fmt.Sprintf("total=%d active=%d cooldown=%d expired=%d", total, active, cooldown, expired)
	if cooldown > 0 && nextRecover != nil {
		s += fmt.Sprintf(", earliest recover at %s", nextRecover.Format("2006-01-02 15:04:05"))
	}
	return s
}

func AddAccountFromDeviceAuth() (*Account, error) {
	fmt.Println("\n=== Add New Cline Account (OAuth) ===")

	device, err := cline.WorkosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")

	_ = cline.OpenBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := cline.PollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	reg, err := cline.RegisterWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if reg.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if reg.Data.UserInfo != nil && reg.Data.UserInfo.Email != "" {
		email = reg.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: reg.Data.RefreshToken,
		AccessToken:  "workos:" + reg.Data.AccessToken,
		ExpiresAt:    cline.ParseExpiry(reg.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	addAccount(acc)
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}

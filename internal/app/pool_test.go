package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSavePoolAtomic 验证 savePool 走原子写(临时文件+rename), 落盘后是完整可解析的 JSON,
// 不会出现"写到一半被强杀留下半个 JSON"的半截文件。
func TestSavePoolAtomic(t *testing.T) {
	dir := t.TempDir()
	poolPath = filepath.Join(dir, ".cline-accounts.json")

	poolMu.Lock()
	pool = &AccountPool{
		Accounts:     []*Account{{AccountID: "a1", Email: "a@b.com", Status: "active"}},
		Keys:         []string{"a1"},
		DefaultModel: "deepseek/deepseek-v4-flash",
	}
	poolMu.Unlock()

	savePool()

	data, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("pool file not written: %v", err)
	}
	var got AccountPool
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("pool file not valid JSON (truncated/half-write?): %v\nraw=%s", err, data)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].AccountID != "a1" || got.DefaultModel != "deepseek/deepseek-v4-flash" {
		t.Fatalf("pool file content mismatch: %+v", got)
	}
}

// TestSetDefaultModelConcurrent 验证 defaultModel 在统一锁(modelsMu)保护下,
// 多 goroutine 并发 setDefaultModel + getDefaultModel 不 panic、不死锁, 且最终值一致。
//
// 注: 本机无 gcc, `go test -race` 会因 requires cgo 而无法运行; 此测试设计成不加 -race 也能通过,
// 加了 -race 才有意义(捕获 data race)。见报告"本机无法验证的部分"。
func TestSetDefaultModelConcurrent(t *testing.T) {
	dir := t.TempDir()
	poolPath = filepath.Join(dir, ".cline-accounts.json")

	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()

	modelsMu.Lock()
	modelsCache = map[string]*ModelInfo{
		"deepseek/deepseek-v4-flash": {ID: "deepseek/deepseek-v4-flash", Status: ModelActive},
		"poolside/laguna-s-2.1:free": {ID: "poolside/laguna-s-2.1:free", Status: ModelActive},
	}
	modelsMu.Unlock()

	origDM := defaultModel
	defer func() { defaultModel = origDM }()

	const modelID = "deepseek/deepseek-v4-flash"
	const goroutines = 32

	var wg sync.WaitGroup
	resCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			setDefaultModel(modelID)
			if got := getDefaultModel(); got != modelID {
				resCh <- fmt.Errorf("getDefaultModel=%q want %q", got, modelID)
				return
			}
			resCh <- nil
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("setDefaultModel/getDefaultModel deadlock or timeout")
	}
	for i := 0; i < goroutines; i++ {
		if err := <-resCh; err != nil {
			t.Error(err)
		}
	}
	if got := getDefaultModel(); got != modelID {
		t.Fatalf("final getDefaultModel=%q want %q", got, modelID)
	}
}

// TestEnsureAccountTokenConcurrentValid 验证 hotspot 路径(账号 token 有效, 不触发刷新)下,
// 多 goroutine 并发读 acc.AccessToken/ExpiresAt 在 poolMu 保护下无数据竞争。
func TestEnsureAccountTokenConcurrentValid(t *testing.T) {
	dir := t.TempDir()
	poolPath = filepath.Join(dir, ".cline-accounts.json")
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()

	acc := &Account{
		AccountID:   "a1",
		AccessToken: "workos:valid",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Status:      "active",
	}

	const goroutines = 32
	var wg sync.WaitGroup
	resCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := ensureAccountToken(acc)
			if err != nil || tok != "workos:valid" {
				resCh <- fmt.Errorf("tok=%q err=%v", tok, err)
				return
			}
			resCh <- nil
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ensureAccountToken(valid) deadlock or timeout")
	}
	for i := 0; i < goroutines; i++ {
		if err := <-resCh; err != nil {
			t.Error(err)
		}
	}
}

// TestEnsureAccountTokenConcurrentRefresh 验证刷新路径下, 多 goroutine 并发刷同一账号不会
// 死锁/panic。网络调用经 refreshAccountTokenFn stub 掉(见任务书允许"把网络调用 stub 掉")。
func TestEnsureAccountTokenConcurrentRefresh(t *testing.T) {
	dir := t.TempDir()
	poolPath = filepath.Join(dir, ".cline-accounts.json")
	poolMu.Lock()
	pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}}
	poolMu.Unlock()

	orig := refreshAccountTokenFn
	defer func() { refreshAccountTokenFn = orig }()
	refreshAccountTokenFn = func(refreshToken string) (string, string, int64, error) {
		return "workos:fresh", "", time.Now().Add(time.Hour).UnixMilli(), nil
	}

	// AccessToken 为空 => ensureAccountToken 必然走刷新分支
	acc := &Account{AccountID: "a2", RefreshToken: "rt", Status: "expired"}

	const goroutines = 32
	var wg sync.WaitGroup
	resCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := ensureAccountToken(acc)
			if err != nil || tok != "workos:fresh" {
				resCh <- fmt.Errorf("tok=%q err=%v", tok, err)
				return
			}
			resCh <- nil
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ensureAccountToken(refresh) deadlock or timeout")
	}
	for i := 0; i < goroutines; i++ {
		if err := <-resCh; err != nil {
			t.Error(err)
		}
	}
	// 最终应拿到刷新得到的 token
	if tok, _ := ensureAccountToken(acc); tok != "workos:fresh" {
		t.Fatalf("final token=%q want workos:fresh", tok)
	}
}

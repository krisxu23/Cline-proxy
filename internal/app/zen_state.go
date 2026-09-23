package app

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	zenConfig   = loadZenConfig()
	zenConfigMu sync.Mutex
)

func init() {
	// zenConfig 在包级变量初始化时已加载, 这里重建手动启用集合。
	// 之后每次 setZenConfig 都会再刷一次。
	refreshZenEnabledModels()
}

// ============ 限流防御状态机 ============

var (
	zenSem       chan struct{} // 并发信号量
	zenFailCount int           // 连续失败计数
	zenFailUntil time.Time     // 故障转移截止时间
	zenProbing   bool          // 熔断窗口过期后, 首个请求作为半开探测
	zenStateMu   sync.Mutex
)

func init() {
	rebuildZenSem()
}

func rebuildZenSem() {
	cfg := getZenConfig()
	n := cfg.MaxConcurrency
	if n <= 0 {
		n = 8
	}
	zenStateMu.Lock()
	zenSem = make(chan struct{}, n)
	zenStateMu.Unlock()
}

func markZenSuccess() {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenProbing = false
	zenStateMu.Unlock()
}

func markZenFail() {
	cfg := getZenConfig()
	thr := cfg.FailoverCount
	if thr <= 0 {
		thr = 3
	}
	window := cfg.FailoverMinutes
	if window <= 0 {
		window = 5
	}
	zenStateMu.Lock()
	if zenProbing {
		// 半开探测失败: 立即重新跳闸, 不再重新累计
		zenFailUntil = time.Now().Add(time.Duration(window) * time.Minute)
		zenProbing = false
		zenStateMu.Unlock()
		return
	}
	zenFailCount++
	if zenFailCount >= thr {
		zenFailUntil = time.Now().Add(time.Duration(window) * time.Minute)
	}
	zenStateMu.Unlock()
}

// zenFailedNow zen 是否处于故障转移状态。
// 窗口过期时放行一个半开探测请求: 探测成功则熔断清零(markZenSuccess),
// 探测失败则立即重新跳闸(markZenFail), 与标准熔断器 HALF-OPEN 语义一致。
// 放行是**一次性 CAS**: 只有第一个越过窗口的调用拿到 false, 探测完成前
// 其余调用按熔断处理(此前会清零 zenFailUntil 后集体放行, "放行一个探测"
// 形同虚设)。网络级全故障在 zen_call.go 重试耗尽时也调 markZenFail,
// 探测走网络错误路径同样能收尾。
func zenFailedNow() bool {
	zenStateMu.Lock()
	defer zenStateMu.Unlock()
	// 半开探测在途: 其余请求一律按熔断处理, 直到探测终局。
	if zenProbing {
		return true
	}
	if zenFailUntil.IsZero() {
		return false
	}
	if time.Now().After(zenFailUntil) {
		zenProbing = true
		zenFailCount = 0
		zenFailUntil = time.Time{}
		return false
	}
	return true
}

// clearZenProbing 结束半开探测但不改动熔断计数 —— 供"请求根本没发出去"
// (构造失败/客户端取消/排队中取消)这类**无判定**终局调用。上游可达与否
// 没有结论, 不该累计故障也不该重新跳闸; 但一次性 CAS 放行后 zenProbing
// 若无人清理会永久卡住, 后续请求全部按熔断处理。释放后若上游确实仍故障,
// 下一次真实请求的网络级失败会经 markZenFail 重新累计、重新跳闸。
func clearZenProbing() {
	zenStateMu.Lock()
	zenProbing = false
	zenStateMu.Unlock()
}

// zenCircuitStatus 供管理端展示: (熔断中, 探测在途)。
func zenCircuitStatus() (open bool, probing bool) {
	zenStateMu.Lock()
	defer zenStateMu.Unlock()
	if zenProbing {
		return false, true
	}
	if !zenFailUntil.IsZero() && time.Now().Before(zenFailUntil) {
		return true, false
	}
	return false, false
}

// isRateLimited 限流信号识别: 429/503 直接命中; 502/403 按错误体关键词
func isRateLimited(status int, body string) bool {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return true
	}
	if status == http.StatusBadGateway || status == http.StatusForbidden {
		low := strings.ToLower(body)
		// resource_exhausted 是 Google API 的错误码原样, 带下划线; 之前写成
		// "resourceexhausted" 是死代码 —— 429 已被上面的状态码命中, 但这个分支
		// 的语义本意就是"用 body 兜底识别限流", 关键词不匹配等于形同虚设。
		for _, kw := range []string{"resource_exhausted", "resourceexhausted", "limit reached", "rate limit", "too many", "overloaded", "busy"} {
			if strings.Contains(low, kw) {
				return true
			}
		}
	}
	return false
}

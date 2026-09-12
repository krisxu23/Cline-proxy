package app

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// 免费模型自动发现(spec §4.5)。
//
// 免费模型池一直在变: 上游悄悄上架/下架的免费模型, 靠人工维护必然滞后。
// 这里周期性拉一个"发现源"provider 的目录, 筛出零价且可聊天的模型,
// 与已收录集合求差集, 再对每个新模型**真实试跑一次** —— 只有拿到内容才算数。
//
// 两条边界:
//   - 手动配置永远优先: 候选链展开时手动条目排在最前, 发现的结果只追加尾部,
//     且发现过程不会删除或改写任何手动条目;
//   - 试跑失败的模型不会被无声丢弃: 永久性错误进 rejected 落盘,
//     可恢复错误留到下一轮重试。

const (
	defaultDiscoveryIntervalMs = int64(48 * time.Hour / time.Millisecond)
	defaultDiscoveryMaxPerRun  = 8
	defaultDiscoveryEvalTokens = 4000
	defaultDiscoveryUsageDays  = 12
)

// zenDiscoveryConfig 配置的 discovery 段。
type zenDiscoveryConfig struct {
	Enabled       bool                `json:"enabled"`
	Provider      string              `json:"provider"`
	IntervalMs    int64               `json:"intervalMs"`
	MaxPerRun     int                 `json:"maxPerRun"`
	EvalMaxTokens int                 `json:"evalMaxTokens"`
	UsageWeight   int                 `json:"usageWeight"`
	Exclude       zenDiscoveryExclude `json:"exclude"`
}

// zenDiscoveryExclude 排除规则: 命中即不收录。
// 模型名与描述文本分开配置, 因为两者噪声特征不同。
type zenDiscoveryExclude struct {
	ModelPatterns []string `json:"modelPatterns"`
	TextPatterns  []string `json:"textPatterns"`
}

// defaultModelExcludes 非聊天 / 专用域模型的默认排除规则。
var defaultModelExcludes = []string{
	`[-_]tts(?:$|[-_])`,
	`[-_]image(?:$|[-_])`,
	`(?:^|[:/])nano-banana`,
	`(?:^|[:/])lyria`,
	`[-_]transcribe(?:$|[-_])`,
	`robotics`,
	`computer-use`,
	`deep-research`,
	`(?:^|[:/])antigravity`,
	`[-_]latest$`,
}

// defaultTextExcludes 描述文本里的"垂直领域专用"措辞 —— 这类模型通常有额外用途约束。
var defaultTextExcludes = []string{
	`\b(finance|financial|investment|medicine|medical|healthcare|health|clinical|biomedical|pharmaceutical|legal|accounting|tax)[\s-]*(focused|specific|specialized|specialised|domain)\b`,
	`\bdomain-(specific|specialized|specialised)\b`,
}

// discoveredModel 已收录的发现模型。
type discoveredModel struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	AddedAt   string `json:"addedAt"`
	LastProbe string `json:"lastProbe,omitempty"`
	OK        bool   `json:"ok"`
}

// rejectedDiscovery 永久剔除的发现模型(与候选层永久剔除同存一份)。
type rejectedDiscovery struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type discoveredFile struct {
	Models   []discoveredModel   `json:"models"`
	Rejected []rejectedDiscovery `json:"rejected,omitempty"`
}

var (
	discoveredMu     sync.Mutex
	discoveredModels []discoveredModel
	discoveredLoaded bool

	discoveryExcludeMu   sync.Mutex
	discoveryExcludeKeys string
	discoveryModelRes    []*regexp.Regexp
	discoveryTextRes     []*regexp.Regexp
)

func discoveryConfig() zenDiscoveryConfig {
	var c zenDiscoveryConfig
	if cfg := getZenConfig(); cfg != nil {
		c = cfg.Discovery
	}
	if c.Provider == "" {
		c.Provider = "openrouter"
	}
	if c.IntervalMs <= 0 {
		c.IntervalMs = defaultDiscoveryIntervalMs
	}
	if c.MaxPerRun <= 0 {
		c.MaxPerRun = defaultDiscoveryMaxPerRun
	}
	if c.EvalMaxTokens <= 0 {
		c.EvalMaxTokens = defaultDiscoveryEvalTokens
	}
	if c.UsageWeight <= 0 {
		c.UsageWeight = defaultDiscoveryUsageDays
	}
	return c
}

func discoveredFilePath() string {
	return kit.ResolveDataPath("discovered-free-models.json")
}

// loadDiscovered 载入已收录模型与永久剔除, 并恢复候选层剔除集合。
func loadDiscovered() {
	raw, err := os.ReadFile(discoveredFilePath())
	discoveredMu.Lock()
	discoveredLoaded = true
	if err != nil {
		discoveredMu.Unlock()
		if !os.IsNotExist(err) {
			log.Printf("  discovery: 读取收录文件失败: %v", err)
		}
		return
	}
	var f discoveredFile
	if err := json.Unmarshal(raw, &f); err != nil {
		discoveredMu.Unlock()
		log.Printf("  discovery: 收录文件解析失败(将重建): %v", err)
		return
	}
	discoveredModels = f.Models
	discoveredMu.Unlock()

	perms := make(map[string]string, len(f.Rejected))
	for _, r := range f.Rejected {
		if r.Key != "" {
			perms[r.Key] = r.Reason
		}
	}
	candidatePermRestore(perms)
}

// saveDiscovered 落盘收录结果 + 候选层永久剔除。
func saveDiscovered() {
	discoveredMu.Lock()
	models := make([]discoveredModel, len(discoveredModels))
	copy(models, discoveredModels)
	discoveredLoadedCopy := discoveredLoaded
	discoveredMu.Unlock()
	if !discoveredLoadedCopy {
		return
	}
	perms := candidatePermSnapshot()
	rejected := make([]rejectedDiscovery, 0, len(perms))
	for k, v := range perms {
		rejected = append(rejected, rejectedDiscovery{Key: k, Reason: v})
	}
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Key < rejected[j].Key })

	raw, err := json.MarshalIndent(discoveredFile{Models: models, Rejected: rejected}, "", "  ")
	if err != nil {
		log.Printf("  discovery: 收录结果序列化失败: %v", err)
		return
	}
	path := discoveredFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("  discovery: 收录结果写入失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("  discovery: 收录结果落盘失败: %v", err)
	}
}

// discoveredCandidates 发现到的候选, 按近期用量倒序 —— 常用的排在前面,
// 让链尾也能先试跑得通、用户用得多的那些。
//
// usageWeight 用作排名窗口(天): 窗口内的请求数越多越靠前。
func discoveredCandidates() []routeCandidate {
	discoveredMu.Lock()
	models := make([]discoveredModel, len(discoveredModels))
	copy(models, discoveredModels)
	discoveredMu.Unlock()
	if len(models) == 0 {
		return nil
	}
	days := discoveryConfig().UsageWeight

	type scored struct {
		cand  routeCandidate
		score int
		added string
	}
	out := make([]scored, 0, len(models))
	for _, m := range models {
		if !m.OK {
			continue
		}
		if _, ok := providerConfigFor(m.Provider); !ok {
			continue
		}
		out = append(out, scored{
			cand:  routeCandidate{Upstream: m.Provider, Model: m.Model},
			score: usageRequestsOverDays(candidateKey(m.Provider, m.Model), days),
			added: m.AddedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].added < out[j].added
	})
	cands := make([]routeCandidate, 0, len(out))
	for _, s := range out {
		cands = append(cands, s.cand)
	}
	return cands
}

// usageRequestsOverDays 某候选最近 n 天的请求总数(账本聚合)。
func usageRequestsOverDays(key string, days int) int {
	if days <= 0 || key == "" {
		return 0
	}
	cutoff := time.Now().In(usageLocation()).AddDate(0, 0, -days+1).Format("2006-01-02")
	usageMu.Lock()
	defer usageMu.Unlock()
	total := 0
	for day, m := range usageDays {
		if day < cutoff {
			continue
		}
		if c := m[key]; c != nil {
			total += c.Req
		}
	}
	return total
}

// compileDiscoveryExcludes 把配置(或默认)规则编译成正则。
// 规则串变化时才重建, 避免每轮发现都编译一遍。
func compileDiscoveryExcludes() ([]*regexp.Regexp, []*regexp.Regexp) {
	cfg := discoveryConfig()
	modelPats := cfg.Exclude.ModelPatterns
	textPats := cfg.Exclude.TextPatterns
	if len(modelPats) == 0 {
		modelPats = defaultModelExcludes
	}
	if len(textPats) == 0 {
		textPats = defaultTextExcludes
	}
	key := strings.Join(modelPats, "\x00") + "\x01" + strings.Join(textPats, "\x00")

	discoveryExcludeMu.Lock()
	defer discoveryExcludeMu.Unlock()
	if discoveryExcludeKeys == key && discoveryModelRes != nil {
		return discoveryModelRes, discoveryTextRes
	}
	compile := func(pats []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(pats))
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				log.Printf("  discovery: 排除规则 %q 无效, 已跳过: %v", p, err)
				continue
			}
			out = append(out, re)
		}
		return out
	}
	discoveryModelRes = compile(modelPats)
	discoveryTextRes = compile(textPats)
	discoveryExcludeKeys = key
	return discoveryModelRes, discoveryTextRes
}

// discoveryShouldExclude 该模型是否应排除在收录之外。
func discoveryShouldExclude(modelID, description string) bool {
	mres, tres := compileDiscoveryExcludes()
	for _, re := range mres {
		if re.MatchString(modelID) {
			return true
		}
	}
	if description == "" {
		return false
	}
	for _, re := range tres {
		if re.MatchString(description) {
			return true
		}
	}
	return false
}

// discoveryAlreadyKnown 是否已收录或已永久剔除。
func discoveryAlreadyKnown(provider, model string) bool {
	discoveredMu.Lock()
	for _, m := range discoveredModels {
		if m.Provider == provider && m.Model == model {
			discoveredMu.Unlock()
			return true
		}
	}
	discoveredMu.Unlock()
	return candidateSkipReason(provider, model) != ""
}

// runDiscoveryOnce 执行一轮发现: 拉目录 -> 筛选 -> 差集 -> 试跑 -> 收录。
func runDiscoveryOnce(ctx context.Context) (int, error) {
	cfg := discoveryConfig()
	p := providerByName(cfg.Provider)
	if p == nil {
		return 0, nil
	}
	pc, ok := providerConfigFor(cfg.Provider)
	if !ok || pc.APIKey == "" {
		return 0, nil
	}
	if err := p.refreshCatalog(ctx, true); err != nil {
		log.Printf("  discovery: 拉取 %s 目录失败: %v", cfg.Provider, err)
		return 0, err
	}

	// 候选池 = 目录里零价且可聊天的模型
	candidates := p.freeModelIDs()
	added := 0
	probed := 0
	for _, m := range candidates {
		if probed >= cfg.MaxPerRun {
			break
		}
		if m.ID == "" || discoveryAlreadyKnown(cfg.Provider, m.ID) {
			continue
		}
		if discoveryShouldExclude(m.ID, m.Name) {
			continue
		}
		probed++
		ok, permanent, reason := probeDiscoveredModel(ctx, cfg.Provider, m.ID, cfg.EvalMaxTokens)
		if permanent {
			markCandidatePermanent(cfg.Provider, m.ID, reason)
			log.Printf("  discovery: %s:%s 永久剔除(%s)", cfg.Provider, m.ID, reason)
			continue
		}
		if !ok {
			continue
		}
		discoveredMu.Lock()
		discoveredModels = append(discoveredModels, discoveredModel{
			Provider:  cfg.Provider,
			Model:     m.ID,
			AddedAt:   time.Now().In(usageLocation()).Format(time.RFC3339),
			LastProbe: time.Now().In(usageLocation()).Format(time.RFC3339),
			OK:        true,
		})
		discoveredMu.Unlock()
		added++
		log.Printf("  discovery: 收录 %s:%s", cfg.Provider, m.ID)
	}
	if probed > 0 || added > 0 {
		saveDiscovered()
	}
	return added, nil
}

// probeDiscoveredModel 对单个模型试跑一次最小请求。
//
// 只有"HTTP 200 且确实拿到内容"才算通过 —— 免费模型列表里挂着的失效条目
// 相当多, 不试跑就会把死条目塞进候选链。
func probeDiscoveredModel(ctx context.Context, provider, model string, maxTokens int) (ok bool, permanent bool, reason string) {
	p := providerByName(provider)
	if p == nil {
		return false, false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	params := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": maxTokens,
		"stream":     false,
	}
	resp, err := p.Chat(ctx, params, false)
	if err != nil {
		status, body := chainErrorStatusBody(err)
		class, why := classifyCandidateFailure(status, body)
		if class == classPermanent {
			return false, true, why
		}
		// 可恢复错误: 本轮跳过, 下一轮再试
		return false, false, why
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, false, ""
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return chatBodyHasContent(buf), false, ""
}

// startDiscovery 载入收录文件, 并按配置周期跑发现。
func startDiscovery() {
	loadDiscovered()
	cfg := discoveryConfig()
	if !cfg.Enabled {
		return
	}
	go func() {
		// 启动后先等一会儿: 让目录刷新与节点探测先跑完, 别抢上游配额
		time.Sleep(2 * time.Minute)
		for {
			if _, err := runDiscoveryOnce(context.Background()); err != nil {
				log.Printf("  discovery: 本轮失败: %v", err)
			}
			time.Sleep(time.Duration(discoveryConfig().IntervalMs) * time.Millisecond)
		}
	}()
}

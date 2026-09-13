package app

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 请求头自动对齐官方 Cline CLI
//
// 面板上的请求头是「模拟 Cline CLI 发出」, 其中真正会过期的是版本号:
// 客户端升级后 User-Agent / X-CLIENT-VERSION 等还停在旧版本, 上游按版本做
// 灰度或风控时就会表现异常。这里从官方发行渠道读出当前版本并把版本类请求头
// 回写, 名称与固定值保持官方形状, 用户自己追加的头不会被抹掉。
// ============================================================================

const (
	// headersAutoSyncInterval 自动对齐的周期。版本发布不频繁, 12 小时足够。
	headersAutoSyncInterval = 12 * time.Hour
	// headersSyncTimeout 单次版本查询超时。
	headersSyncTimeout = 15 * time.Second

	headerUserAgent     = "User-Agent"
	headerClientVersion = "X-CLIENT-VERSION"
	headerPlatformVer   = "X-PLATFORM-VERSION"
	headerCoreVersion   = "X-CORE-VERSION"
)

// clineRegistrySources 官方版本来源, 按顺序尝试。
// npmmirror 镜像对大陆网络友好且与官方同步, 放在最前。
var clineRegistrySources = []string{
	"https://registry.npmmirror.com/cline/latest",
	"https://registry.npmjs.org/cline/latest",
	"https://registry.npmmirror.com/@cline%2fcore/latest",
	"https://registry.npmjs.org/@cline%2fcore/latest",
}

// clineFixedHeaders 官方形状里不随版本变化的字段。
var clineFixedHeaders = map[string]string{
	"HTTP-Referer":   "https://cline.bot",
	"X-Title":        "Cline",
	"X-IS-MULTIROOT": "false",
	"X-CLIENT-TYPE":  "cline-cli",
	"X-PLATFORM":     "terminal",
}

// clineVersionInfo 从官方渠道解析出的版本。
type clineVersionInfo struct {
	CLI    string
	Core   string
	Source string
}

// registryVersionPayload 只取需要的两个字段: 顶层 version 与依赖里的 @cline/core。
type registryVersionPayload struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Deps    map[string]string `json:"dependencies"`
}

// fetchClineVersions 依次尝试各官方来源, 返回解析到的版本。
// 请求走网关出口客户端, 因此出口模式为节点时同样经节点出去。
func fetchClineVersions(ctx context.Context) (clineVersionInfo, error) {
	client := getZenHTTPClient()
	var lastErr error
	for _, src := range clineRegistrySources {
		info, err := fetchVersionFrom(ctx, client, src)
		if err != nil {
			lastErr = err
			continue
		}
		if info.CLI == "" && info.Core == "" {
			lastErr = errNoVersionsInPayload
			continue
		}
		return info, nil
	}
	if lastErr == nil {
		lastErr = errNoVersionsInPayload
	}
	return clineVersionInfo{}, lastErr
}

var errNoVersionsInPayload = errString("no version fields in the registry response")

type errString string

func (e errString) Error() string { return string(e) }

// fetchVersionFrom 从单个 registry 地址解析版本。
// cline 包的顶层 version 是 CLI 版本, dependencies 里的 @cline/core 是核心版本;
// @cline/core 包自身的 version 就是核心版本。
func fetchVersionFrom(ctx context.Context, client *http.Client, src string) (clineVersionInfo, error) {
	reqCtx, cancel := context.WithTimeout(ctx, headersSyncTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, src, nil)
	if err != nil {
		return clineVersionInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return clineVersionInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return clineVersionInfo{}, errString("HTTP " + resp.Status + " from " + src)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return clineVersionInfo{}, err
	}

	info := clineVersionInfo{Source: src}
	var payloadFull registryVersionPayload
	if json.Unmarshal(payload, &payloadFull) != nil {
		// 非预期形状: 不让它变成"版本为空"的沉默失败
		return info, errString("unreadable registry payload from " + src)
	}
	v := cleanVersion(payloadFull.Version)
	core := cleanVersion(payloadFull.Deps["@cline/core"])
	if isCorePackageSource(src) {
		info.Core = core
		if info.Core == "" {
			info.Core = v
		}
		return info, nil
	}
	info.CLI = v
	info.Core = core
	return info, nil
}

// isCorePackageSource 该来源是否指向 @cline/core 包本身。
func isCorePackageSource(src string) bool {
	l := strings.ToLower(src)
	return strings.Contains(l, "%2fcore") || strings.Contains(l, "@cline/core")
}

// cleanVersion 去掉可能存在的 v 前缀与空白。
func cleanVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// headersFromVersions 由版本生成官方形状的请求头。
func headersFromVersions(info clineVersionInfo) map[string]string {
	out := map[string]string{}
	for k, v := range clineFixedHeaders {
		out[k] = v
	}
	if info.CLI != "" {
		out[headerUserAgent] = "Cline/" + info.CLI
		out[headerClientVersion] = info.CLI
		out[headerPlatformVer] = info.CLI
	}
	if info.Core != "" {
		out[headerCoreVersion] = info.Core
	}
	return out
}

// mergeOfficialHeaders 把官方形状合并进现有请求头:
// 官方字段以官方值为准, 用户自己加的额外头原样保留。
func mergeOfficialHeaders(cur map[string]string, official map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cur {
		out[k] = v
	}
	for k, v := range official {
		out[k] = v
	}
	return out
}

// headersSyncMu 串行化手动与自动同步, 避免两次同步交错写同一份配置。
var headersSyncMu sync.Mutex

// syncOfficialHeaders 查询官方版本并回写请求头, 返回新的请求头集合。
func syncOfficialHeaders(ctx context.Context) (map[string]string, clineVersionInfo, error) {
	headersSyncMu.Lock()
	defer headersSyncMu.Unlock()

	info, err := fetchClineVersions(ctx)
	if err != nil {
		return nil, clineVersionInfo{}, err
	}
	if info.CLI == "" && info.Core == "" {
		return nil, info, errNoVersionsInPayload
	}
	// 整体替换而不是就地改字段: getProxyConfig() 现在返回克隆体, 就地改克隆
	// 不会影响全局, 落盘会把改动丢掉。必须走 mutateProxyConfig 让替换与落盘
	// 在同一个写入口内完成。
	merged := mergeOfficialHeaders(getProxyConfig().Headers, headersFromVersions(info))
	label := versionLabel(info)
	mutateProxyConfig(func(cfg *proxyConfigData) {
		cfg.Headers = merged
		cfg.HeadersSyncedAt = time.Now().UnixMilli()
		cfg.HeadersAutoVersion = label
	})
	return merged, info, nil
}

// versionLabel 面板上展示的版本说明。
func versionLabel(info clineVersionInfo) string {
	parts := make([]string, 0, 2)
	if info.CLI != "" {
		parts = append(parts, "CLI "+info.CLI)
	}
	if info.Core != "" {
		parts = append(parts, "core "+info.Core)
	}
	return strings.Join(parts, " · ")
}

// headerNames 现有请求头的键(排序, 供测试与展示)。
func headerNames(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// startHeadersAutoSync 启动时对齐一次(仅当开关打开), 之后定期复检。
func startHeadersAutoSync() {
	go func() {
		run := func() {
			if !getProxyConfig().HeadersAuto {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), headersSyncTimeout)
			defer cancel()
			if _, info, err := syncOfficialHeaders(ctx); err != nil {
				log.Printf("  请求头自动对齐失败: %v", err)
			} else {
				log.Printf("  请求头已对齐官方版本: %s", versionLabel(info))
			}
		}
		run()
		t := time.NewTicker(headersAutoSyncInterval)
		defer t.Stop()
		for range t.C {
			run()
		}
	}()
}

package app

import (
	"cline-go-proxy/internal/kit"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Global proxy config (mutable via API)
var (
	proxyConfig   = loadProxyConfig()
	proxyConfigMu sync.Mutex
)

type proxyConfigData struct {
	Strategy string            `json:"strategy"`
	Headers  map[string]string `json:"headers"`
	// HeadersAuto 打开后由后台定期对齐官方 Cline CLI 的版本类请求头。
	HeadersAuto bool `json:"headersAuto"`
	// HeadersSyncedAt / HeadersAutoVersion 最近一次自动对齐的结果(展示用)。
	HeadersSyncedAt    int64  `json:"headersSyncedAt,omitempty"`
	HeadersAutoVersion string `json:"headersAutoVersion,omitempty"`
}

func defaultProxyConfig() *proxyConfigData {
	return &proxyConfigData{
		Strategy: "round_robin",
		Headers: map[string]string{
			"User-Agent":         "Cline/3.0.50",
			"HTTP-Referer":       "https://cline.bot",
			"X-Title":            "Cline",
			"X-IS-MULTIROOT":     "false",
			"X-CLIENT-TYPE":      "cline-cli",
			"X-CLIENT-VERSION":   "3.0.50",
			"X-PLATFORM":         "terminal",
			"X-PLATFORM-VERSION": "3.0.50",
			"X-CORE-VERSION":     "0.0.70",
		},
	}
}

func proxyConfigFile() string { return kit.ResolveDataPath(".proxy-config.json") }

// loadProxyConfig 读取持久化的客户端配置, 缺失或损坏时退回默认值。
// 面板上改过的请求头必须跨重启保留, 否则每次重启都会悄悄回到内置默认值。
//
// 解析失败不能"部分采用": json.Unmarshal 不是事务性的, 半截 JSON 会留下已经
// 解出来的字段、丢掉其余部分, 得到的是一份"看着正常但少了东西"的配置, 而且
// 面板上看不出任何异常。这里改成解析失败就整体退回默认值, 并把坏文件改名留证。
func loadProxyConfig() *proxyConfigData {
	path := proxyConfigFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultProxyConfig() // 首次运行没有文件是正常情况
	}
	next := defaultProxyConfig()
	if err := json.Unmarshal(data, next); err != nil {
		log.Printf("proxy config parse failed (%s): %v; 退回默认配置", path, err)
		quarantineBadConfig(path)
		return defaultProxyConfig()
	}
	if next.Headers == nil {
		next.Headers = map[string]string{}
	}
	if next.Strategy == "" {
		next.Strategy = "round_robin"
	}
	return next
}

// quarantineBadConfig 把解析不了的配置文件改名留证, 而不是删除 ——
// 用户往往需要从里面手工捞回请求头之类的内容。
func quarantineBadConfig(path string) {
	bak := path + ".bad-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, bak); err != nil {
		log.Printf("  保留坏配置失败(%v), 原文件仍在 %s", err, path)
		return
	}
	log.Printf("  坏配置已保留为 %s", bak)
}

func saveProxyConfig() {
	proxyConfigMu.Lock()
	data, err := json.MarshalIndent(proxyConfig, "", "  ")
	proxyConfigMu.Unlock()
	if err != nil {
		return
	}
	// 全仓此前唯一的"先截断再写"非原子写: 写到一半被强杀会留下半个 JSON, 下次
	// 启动解析失败 -> 配置静默丢失。改用原子写(临时文件 + fsync + rename)。
	if err := kit.WriteFileAtomicDefault(proxyConfigFile(), data); err != nil {
		log.Printf("proxy config save failed: %v", err)
	}
}

// getProxyConfig 返回当前客户端配置的深拷贝。
//
// 与 getZenConfig 同理: 调用方包含每个 cline 上游请求(clineHeaders 会遍历
// Headers)与选账号热路径, 它们在锁外持有引用; 返回裸指针时后台的请求头
// 自动同步一写就与这些遍历并发访问同一张 map。
func getProxyConfig() *proxyConfigData {
	proxyConfigMu.Lock()
	defer proxyConfigMu.Unlock()
	return proxyConfig.clone()
}

func setProxyConfig(c *proxyConfigData) {
	proxyConfigMu.Lock()
	proxyConfig = c.clone()
	proxyConfigMu.Unlock()
	saveProxyConfig()
}

// mutateProxyConfig 是 proxyConfig 的唯一写入口, 语义同 mutateProvidersConfig:
// 锁内克隆 → 回调改克隆 → 整体替换 → 落盘。回调内不得再读配置(会自锁)。
func mutateProxyConfig(fn func(cfg *proxyConfigData)) {
	proxyConfigMu.Lock()
	next := proxyConfig.clone()
	if next == nil {
		next = defaultProxyConfig()
	}
	fn(next)
	proxyConfig = next
	proxyConfigMu.Unlock()
	saveProxyConfig()
}

// GET /admin/api/keys
func handleAdminGetKeys(w http.ResponseWriter, r *http.Request) {
	snap := poolSnapshot()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"keys": snap.Keys}})
}

// POST /admin/api/keys/generate
func handleAdminGenerateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	// 此前用 UnixMilli/UnixNano 拼令牌, 时间可预测、相邻请求极易被猜出。
	// 改为 32 字节密码学随机 + hex, 不可预测。
	kb := make([]byte, 32)
	if _, err := rand.Read(kb); err != nil {
		writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "生成密钥失败: " + err.Error()})
		return
	}
	key := "cline_" + hex.EncodeToString(kb)
	p := loadPool()
	poolMu.Lock()
	p.Keys = append(p.Keys, key)
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{"key": key}})
}

// POST /admin/api/keys/delete  body: { key }
func handleAdminDeleteKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()
	var req struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}
	p := loadPool()
	poolMu.Lock()
	for i, k := range p.Keys {
		if k == req.Key {
			p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
			break
		}
	}
	poolMu.Unlock()
	savePool()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "Key deleted"})
}

// GET /admin/api/config
func handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg := getProxyConfig()
	address := r.Host
	if address == "" {
		address = proxyListenAddress
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"address": address,
		// listenAddr 是真实绑定地址(可能是 0.0.0.0), apiBase 是客户端应使用的本机入口。
		"listenAddr":   proxyListenAddress,
		"apiBase":      localOrigin(),
		"strategy":     cfg.Strategy,
		"version":      buildVersion,
		"poolPath":     poolPathValue(),
		"defaultModel": getDefaultModel(),
		"headers":      cfg.Headers,
		"headersAuto":  cfg.HeadersAuto,
		"headersSync": map[string]any{
			"auto":      cfg.HeadersAuto,
			"syncedAt":  cfg.HeadersSyncedAt,
			"version":   cfg.HeadersAutoVersion,
			"source":    clineRegistrySources[0],
			"intervalM": int(headersAutoSyncInterval / time.Minute),
		},
	}})
}

// POST /admin/api/config  body: { strategy?, headers?, defaultModel? }
func handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Strategy     string            `json:"strategy"`
		Headers      map[string]string `json:"headers"`
		DefaultModel string            `json:"defaultModel"`
		HeadersAuto  *bool             `json:"headersAuto"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
		return
	}

	cfg := getProxyConfig()
	changed := false

	if req.Strategy != "" {
		switch req.Strategy {
		case "round_robin", "fill", "random":
			cfg.Strategy = req.Strategy
			changed = true
		default:
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid strategy, must be: round_robin, fill, random"})
			return
		}
	}

	if req.Headers != nil {
		// 面板以「整表替换」提交: 先清掉已删除的键, 否则被删掉的头会一直残留。
		next := make(map[string]string, len(req.Headers))
		for k, v := range req.Headers {
			if strings.TrimSpace(k) == "" {
				continue
			}
			next[strings.TrimSpace(k)] = v
		}
		cfg.Headers = next
		changed = true
	}

	if req.HeadersAuto != nil {
		cfg.HeadersAuto = *req.HeadersAuto
		changed = true
	}

	if req.DefaultModel != "" {
		initModelsCache()
		modelsMu.Lock()
		_, ok := modelsCache[req.DefaultModel]
		modelsMu.Unlock()
		if !ok {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "unknown model: " + req.DefaultModel})
			return
		}
		setDefaultModel(req.DefaultModel)
		changed = true
	}

	if changed {
		setProxyConfig(cfg)
	}

	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"strategy":    cfg.Strategy,
		"headers":     cfg.Headers,
		"headersAuto": cfg.HeadersAuto,
		// 走 getDefaultModel() 而不是直接读包级变量: 后者由 modelsMu 保护,
		// 直接读会与并发的 setDefaultModel 构成数据竞争。
		"defaultModel": getDefaultModel(),
	}})
}

// POST /admin/api/config/headers/sync — 对齐官方 Cline CLI 的请求头版本
func handleAdminHeadersSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), headersSyncTimeout*2)
	defer cancel()
	headers, info, err := syncOfficialHeaders(ctx)
	if err != nil {
		writeAPI(w, http.StatusBadGateway, apiResponse{Error: "无法获取官方版本: " + err.Error()})
		return
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"headers": headers,
		"cli":     info.CLI,
		"core":    info.Core,
		"source":  info.Source,
		"version": versionLabel(info),
	}})
}

// GET /admin/api/models
func handleAdminModels(w http.ResponseWriter, r *http.Request) {
	ensureModelsFresh()
	// modelsSyncStamp 带锁读 lastSync: 同步协程在持锁状态下写它,
	// 直接裸读会与 /models/refresh 并发竞争。
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"models":   getFreeModels(),
		"lastSync": modelsSyncStamp(),
	}})
}

// POST /admin/api/models/refresh
func handleAdminModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	initModelsCache()
	modelsMu.Lock()
	syncing := modelsSyncing
	modelsMu.Unlock()
	if syncing {
		writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "sync already running"})
		return
	}
	go syncModelsOnce()
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "model sync started"})
}

// configBackupFiles 参与整体备份的配置文件(白名单: 不含日志/统计/缓存/令牌)。
// 注意其中两个含敏感凭据(refreshToken / API key), 导出文件务必妥善保管。
var configBackupFiles = []string{
	".zen-config.json",     // zen/上游/路由/压缩等主配置
	".proxy-config.json",   // 代理监听与网关 key
	".cline-accounts.json", // cline 账号池(含 refreshToken, 敏感)
	".clinepass-keys.json", // clinepass key 池(敏感)
	"node-regions.json",    // 出口地区探测缓存
}

// configBackupAllowed 文件名是否在备份白名单内(导入侧防路径穿越)。
func configBackupAllowed(name string) bool {
	for _, f := range configBackupFiles {
		if name == f {
			return true
		}
	}
	return false
}

// handleConfigExport GET /admin/api/config/export
// 把全部配置文件打包成一个 JSON(信封带 kind/version/exportedAt), 供换机/备份。
func handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	files := map[string]string{}
	for _, name := range configBackupFiles {
		if b, err := os.ReadFile(kit.ResolveDataPath(name)); err == nil && len(b) > 0 {
			files[name] = string(b)
		}
	}
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"kind":       "cline-proxy-config-backup",
		"version":    1,
		"exportedAt": time.Now().Format(time.RFC3339),
		"files":      files,
	}})
}

// handleConfigImport POST /admin/api/config/import
// body: {"files": {".zen-config.json": "<内容>", ...}}
//
// 安全与可靠性: 文件名必须命中白名单(防路径穿越); 内容必须是合法 JSON;
// 覆盖前把现有文件备份为 <name>.bak-import-<时间戳>(保留最近若干份, 可人工回滚)。
// 写入后提示重启 —— 各配置在启动时加载, 热加载不在本功能范围内。
func handleConfigImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	var body struct {
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "body 必须是 {\"files\": {\"<文件名>\": \"<内容>\"}}"})
		return
	}
	if len(body.Files) == 0 {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: "files 为空"})
		return
	}
	imported := make([]string, 0, len(body.Files))
	for name, content := range body.Files {
		if !configBackupAllowed(name) {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "文件名不在备份白名单内: " + name})
			return
		}
		if strings.TrimSpace(content) == "" {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: name + " 内容为空"})
			return
		}
		var check any
		if err := json.Unmarshal([]byte(content), &check); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: name + " 不是合法 JSON: " + err.Error()})
			return
		}
		// provider 名必须与正常写入路径(providers_config.go 的 providerIDRe)同源校验:
		// 导入此前只做 JSON/结构检查, 恶意 provider 名会绕过校验直接落盘, 再经面板
		// 渲染(script_providers 拼进 innerHTML)形成存储型 XSS(P2-12)。
		// 通用上游只存在 .zen-config.json 的 providers 字段里。
		if name == ".zen-config.json" {
			var z struct {
				Providers map[string]json.RawMessage `json:"providers"`
			}
			if err := json.Unmarshal([]byte(content), &z); err == nil {
				for pname := range z.Providers {
					if !providerIDRe.MatchString(pname) {
						writeAPI(w, http.StatusBadRequest, apiResponse{
							Error: fmt.Sprintf("provider 名不合法(文件 %s): %q, 需匹配 ^[a-z][a-z0-9_-]*$", name, pname),
						})
						return
					}
				}
			}
		}
	}
	// 全部校验通过才落盘(避免半套配置); 覆盖前把现有文件另存为
	// <name>.bak-import-<时间戳> 并只保留最近若干份 —— 单份 .bak-import 会被
	// 下一次导入覆盖, 多次导入就再也回不到更早的版本(审计 P3-13)。
	for name, content := range body.Files {
		path := kit.ResolveDataPath(name)
		if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
			backup := fmt.Sprintf("%s.bak-import-%s", path, time.Now().Format("20060102-150405"))
			_ = os.WriteFile(backup, old, 0600)
			pruneImportBackups(path, importBackupKeep)
		}
		if err := kit.WriteFileAtomicDefault(path, []byte(content)); err != nil {
			writeAPI(w, http.StatusInternalServerError, apiResponse{Error: "写入 " + name + " 失败: " + err.Error()})
			return
		}
		imported = append(imported, name)
	}
	log.Printf("  admin: 配置导入完成 (%d 个文件), 重启后生效", len(imported))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Data: map[string]any{
		"imported": imported,
		"note":     fmt.Sprintf("导入完成, 重启网关后生效; 原文件已备份为 <文件名>.bak-import-<时间戳>(最多保留 %d 份)", importBackupKeep),
	}})
}

// importBackupKeep 每个配置文件保留的导入备份份数。
const importBackupKeep = 5

// pruneImportBackups 删除最旧的导入备份, 只留最近 keep 份。
// 文件名里的时间戳格式可按字典序排序, 因此直接排序即可。
func pruneImportBackups(path string, keep int) {
	matches, err := filepath.Glob(path + ".bak-import-*")
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, old := range matches[:len(matches)-keep] {
		if err := os.Remove(old); err != nil {
			log.Printf("  admin: 清理旧备份失败 %s: %v", old, err)
		}
	}
}

package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 审计 P3-13: 导入备份从"单份覆盖"改为带时间戳轮转, 这里固化"只留最近 N 份"。
func TestPruneImportBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".zen-config.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 造 7 份备份(时间戳可字典序排序), 保留 3 份
	stamps := []string{
		"20260101-010101", "20260102-010101", "20260103-010101", "20260104-010101",
		"20260105-010101", "20260106-010101", "20260107-010101",
	}
	for _, s := range stamps {
		if err := os.WriteFile(target+".bak-import-"+s, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneImportBackups(target, 3)

	left, err := filepath.Glob(target + ".bak-import-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 {
		t.Fatalf("应只保留 3 份, 实际 %d: %v", len(left), left)
	}
	for _, s := range stamps[:4] {
		if _, err := os.Stat(target + ".bak-import-" + s); err == nil {
			t.Fatalf("最旧的备份 %s 应被清理", s)
		}
	}
	for _, s := range stamps[4:] {
		if _, err := os.Stat(target + ".bak-import-" + s); err != nil {
			t.Fatalf("最近的备份 %s 必须保留: %v", s, err)
		}
	}
	// 原文件不受影响
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("配置文件本体不能被误删: %v", err)
	}
}

func TestPruneImportBackupsNoopWhenFew(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".proxy-config.json")
	if err := os.WriteFile(target+".bak-import-20260101-000000", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneImportBackups(target, 5) // 少于保留数: 什么都不该删
	left, _ := filepath.Glob(target + ".bak-import-*")
	if len(left) != 1 {
		t.Fatalf("不足保留数时不应删除, 实际 %d", len(left))
	}
}

// 审计 P2: 导入 .zen-config.json 必须与面板直改同一口径过出站地址校验 ——
// 否则云元数据地址经导入落盘, 重启后 POST model=evil:any 即可让网关去取
// 元数据并把响应回传(SSRF + 回读)。169.254.169.254 是链路本地字面量,
// 判定不依赖 DNS, 结果确定; 校验环在任何写盘之前即拒绝, 零副作用。
func TestConfigImportRejectsLinkLocalOutboundURLs(t *testing.T) {
	cases := map[string]string{
		"providers.baseUrl": `{"providers":{"evil":{"baseUrl":"http://169.254.169.254/latest/meta-data"}}}`,
		"baseURLs":          `{"baseURLs":["http://169.254.169.254/v1"]}`,
		"subs":              `{"subs":["http://169.254.169.254/sub"]}`,
	}
	for what, zen := range cases {
		body, err := json.Marshal(map[string]any{"files": map[string]string{".zen-config.json": zen}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/admin/api/config/import", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		handleConfigImport(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 链路本地地址应被拒绝, got %d, body=%s", what, rec.Code, rec.Body.String())
		}
	}
}

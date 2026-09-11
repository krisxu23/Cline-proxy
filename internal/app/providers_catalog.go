package app

import (
	"regexp"
	"strings"
)

// 注: catalogModel 结构体已由 Task 1 在 providers_config.go 中声明, 此处不要重复声明。

// isZeroCost 明确标价 0 的模型。
func isZeroCost(m *catalogModel) bool {
	return m != nil && m.PricesKnown && m.PromptPrice == 0 && m.CompletionPrice == 0
}

var moderationRe = regexp.MustCompile(`(?i)content[-_ ]?safety|moderation|guard(?:[:/_-]|$)`)

// isChatModel 目录条目是否可作为聊天模型路由。
func isChatModel(m *catalogModel) bool {
	if m == nil {
		return false
	}
	if m.ChatCapable != nil {
		return *m.ChatCapable
	}
	outputs := m.OutputModalities
	if len(outputs) == 0 {
		outputs = []string{"text"}
	}
	for _, o := range outputs {
		if o != "text" {
			return false
		}
	}
	if m.Tokenizer == "Router" {
		return false
	}
	return !moderationRe.MatchString(m.ID)
}

// normalizeModelSlug 跨 provider 归一化模型名: 小写、剥 :free 后缀、取最后一段。
func normalizeModelSlug(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	s = strings.TrimSuffix(s, ":free")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// catalogLookup 精确命中优先, 否则按 slug 回退。
func catalogLookup(cat, slugs map[string]*catalogModel, modelID string) *catalogModel {
	if m, ok := cat[modelID]; ok {
		return m
	}
	slug := normalizeModelSlug(modelID)
	if slug == "" {
		return nil
	}
	return slugs[slug]
}

// evalProviderFree 免费判定三模式(纯函数):
//   - catalog+pricing: 目录按价格判定 isZeroCost && isChatModel
//   - 白名单: freeModels 命中; 目录已加载时还需目录中仍存在
//   - 永久拒绝缓存命中 → false
func evalProviderFree(cfg providerConfig, cat, slugs map[string]*catalogModel, rejected map[string]string, modelID string) bool {
	if cfg.APIKey == "" {
		return false
	}
	if _, bad := rejected[modelID]; bad {
		return false
	}
	if cfg.Catalog && cfg.Pricing {
		if len(cat) == 0 {
			return false
		}
		m := catalogLookup(cat, slugs, modelID)
		return m != nil && isZeroCost(m) && isChatModel(m)
	}
	if !cfg.freeSet()[modelID] {
		return false
	}
	if cfg.Catalog && len(cat) > 0 {
		return catalogLookup(cat, slugs, modelID) != nil
	}
	return true
}

// isFree 该模型在本 provider 上是否免费。
func (p *modelProvider) isFree(modelID string) bool {
	cfg, _ := providerConfigFor(p.name)
	p.mu.Lock()
	cat, slugs, rejected := p.catalog, p.slugs, p.rejected
	p.mu.Unlock()
	return evalProviderFree(cfg, cat, slugs, rejected, modelID)
}

// catalogEntry 精确或按 slug 查目录条目。
func (p *modelProvider) catalogEntry(modelID string) *catalogModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return catalogLookup(p.catalog, p.slugs, modelID)
}

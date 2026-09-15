package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"

	sjson "github.com/sagernet/sing/common/json"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
)

// sing-box 配置构建与端口分配(buildNodeParts/校验/稳定端口)。
// 由 nodes.go 拆分而来(P2 结构整理): nodes.go 只保留 Box 生命周期/订阅同步/
// TLS 清洗, 各职责独立成文件便于评审与测试。同包内无需导出调整。
func stringEntries(entries []any) []any {
	var out []any
	for _, e := range entries {
		if _, ok := e.(string); ok {
			out = append(out, e)
		}
	}
	return out
}

// buildNodeParts 为每个节点条目分配本地端口并生成 sing-box 配置部件
func buildNodeParts(entries []any) (ports map[string]int, inbounds, outbounds, rules []map[string]any, hasMap bool) {
	ports = map[string]int{}
	for i, e := range entries {
		var ob map[string]any
		var key string
		var port int
		var err error
		switch v := e.(type) {
		case string:
			if nodeExcludedByFilter(nodeDisplayName(v)) {
				log.Printf("  node %d(%s): 命中排除关键词已跳过", i+1, nodeDisplayName(v))
				continue
			}
			ob, err = nodeOutbound(v, fmt.Sprintf("out-%d", i))
			if err != nil {
				log.Printf("  node %d: 解析失败已跳过: %v", i+1, err)
				continue
			}
			// 链接解析出的出站必须同样过 sing-box 校验。订阅聚合源里常有 sing-box
			// 不认的写法(实测 ss 的 chacha20-poly1305), 只校验 map 分支会让这类坏
			// 节点一路进到 box.New, 把**整个实例**打死 → nodePorts 归零 → 健康检测
			// 直接跳过 → 面板上全部节点永久停在"未检测"。校验只做 box.New 不建连,
			// 成本极低, 逐节点剔除即可。
			if verr := validateOutboundEntry(ob); verr != nil {
				log.Printf("  node %d(%s): 出站无效已剔除: %v", i+1, nodeDisplayName(subEntryKey(v)), verr)
				continue
			}
			key = nodeLocalKey(v)
			// 稳定端口(P2): 优先复用历史分配, 订阅更新不再漂移端口
			port, _, err = assignStablePort(key)
			if err != nil {
				log.Printf("  node %d: 分配本地端口失败: %v", i+1, err)
				continue
			}
		case map[string]any:
			if nodeExcludedByFilter(subNodeDisplayName(subEntryKey(v))) {
				log.Printf("  node %d(%s): 命中排除关键词已跳过", i+1, subNodeDisplayName(subEntryKey(v)))
				continue
			}
			hasMap = true
			if verr := validateOutboundEntry(v); verr != nil {
				log.Printf("  node %d(%s): 出站无效已剔除: %v", i+1, nodeDisplayName(subEntryKey(v)), verr)
				continue
			}
			cp := map[string]any{"tag": fmt.Sprintf("out-%d", i)}
			for k, val := range v {
				if k != "tag" {
					cp[k] = val
				}
			}
			ob = cp
			key = subEntryKey(v)
			// 稳定端口(P2): 订阅出站同样复用历史端口, 跨更新不漂移
			port, _, err = assignStablePort(key)
			if err != nil {
				log.Printf("  node %d: 分配本地端口失败: %v", i+1, err)
				continue
			}
		default:
			continue
		}
		tag := ob["tag"].(string)
		sanitizeOutboundTLS(ob)
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": fmt.Sprintf("in-%d", i),
			"listen": "127.0.0.1", "listen_port": port,
		})
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{
			"action": "route", "inbound": []string{fmt.Sprintf("in-%d", i)}, "outbound": tag,
		})
		ports[key] = port
	}
	return ports, inbounds, outbounds, rules, hasMap
}

// startNodeInstance 组装并创建 sing-box 实例(不 Start)
func startNodeInstance(ctx context.Context, inbounds, outbounds, rules []map[string]any) (nodeBoxInstance, error) {
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	boxCfg := map[string]any{
		"log":       map[string]any{"disabled": true},
		"dns":       dnsCfg,
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]any{
			"rules":                   rules,
			"final":                   "direct",
			"default_domain_resolver": map[string]any{"server": resolverTag},
		},
	}
	data, err := json.Marshal(boxCfg)
	if err != nil {
		return nil, err
	}
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		return nil, err
	}
	return box.New(box.Options{Context: ctx, Options: opts})
}

// validateOutboundEntry 单独构建校验一个订阅出站, 单个坏节点不影响其他节点
func validateOutboundEntry(ob map[string]any) error {
	entry := map[string]any{"tag": "check"}
	for k, v := range ob {
		if k != "tag" {
			entry[k] = v
		}
	}
	// 按实际运行形态校验: buildNodeParts 落盘前必经 sanitize, 这里先对副本做同样的事,
	// 否则"校验时剔除、运行时能跑"(或反过来), 两边结论打架。
	sanitizeOutboundTLS(entry)
	dnsCfg, resolverTag := buildNodeDNS(getZenConfig())
	boxCfg := map[string]any{
		"log":       map[string]any{"disabled": true},
		"dns":       dnsCfg,
		"outbounds": []any{entry, map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"final":                   "direct",
			"default_domain_resolver": map[string]any{"server": resolverTag},
		},
	}
	data, err := json.Marshal(boxCfg)
	if err != nil {
		return err
	}
	ctx := include.Context(context.Background())
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		return err
	}
	instance, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		return err
	}
	instance.Close()
	return nil
}

// freeLocalPort 预分配一个空闲端口(存在极小竞态窗口, 冲突由 box 启动报错兜底)
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

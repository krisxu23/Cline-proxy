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
// nodeBuildItem 构建中间产物: 校验/解析完成的出站, 端口在最后统一分配。
type nodeBuildItem struct {
	key  string
	name string
	ob   map[string]any
	port int // 0 = 待分配
}

// buildNodeParts 两阶段构建(P2 修复):
//
//	阶段 A(慢, 分钟级): 解析 + 过滤 + 逐节点 sing-box 校验 —— 不碰端口;
//	阶段 B(快, 秒级): 端口分配 + inbound/outbound/rule 组装, 紧贴 Start。
//
// 旧实现边校验边分配端口, "分配→bind"窗口长达数分钟 —— 这期间其它进程
// (乃至本进程出站连接)可能占用端口, sing-box bind 必败。实测事故链的
// 收口修复。
//
// 端口分配两轮: 先复用稳定记录(订阅更新端口不漂移), 再顺序扫描补齐。
// 顺序分配 + 构建内 used 集合保证同批端口绝对唯一 —— 旧随机分配在
// 4269 次选择里按生日悖论必然自撞(实测每次重建都死在随机的某个 inbound)。
func buildNodeParts(entries []any) (ports map[string]int, inbounds, outbounds, rules []map[string]any, hasMap bool) {
	ports = map[string]int{}
	// 阶段 A: 按 key 去重 + 解析 + 校验(同一节点可能在多个订阅源重复出现,
	// 稳定端口下重复条目 = 同端口两个 inbound, 必须收敛)。
	items := make([]nodeBuildItem, 0, len(entries))
	seenBuild := map[string]bool{}
	for i, e := range entries {
		var key, name string
		var ob map[string]any
		var err error
		switch v := e.(type) {
		case string:
			key = nodeLocalKey(v)
			name = nodeDisplayName(v)
			if seenBuild[key] {
				continue
			}
			seenBuild[key] = true
			if nodeExcludedByFilter(name) {
				log.Printf("  node %d(%s): 命中排除关键词已跳过", i+1, name)
				continue
			}
			ob, err = nodeOutbound(v, fmt.Sprintf("out-%d", i))
			if err != nil {
				log.Printf("  node %d: 解析失败已跳过: %v", i+1, err)
				continue
			}
			// ★ 净化必须先于校验。validateOutboundEntry 走 sing-box 的 box.New,
			// 对任何它不认的取值一律判无效; 而订阅源普遍用 v2ray/Xray 的写法
			// (tcp/raw 表示裸 TCP、fp=unsafe 表示不校验指纹、flow=none 表示不设 flow),
			// 这些**语义都能表达**, 净化为等价的 sing-box 写法后就是可用节点。
			// 旧顺序(先校验后净化)会把这批节点白白丢掉 —— 2026-09-18 实测 72 条。
			sanitizeOutboundTLS(ob)
			sanitizeOutboundShape(ob)
			// 链接解析出的出站必须同样过 sing-box 校验。订阅聚合源里常有 sing-box
			// 不认的写法(实测 ss 的 chacha20-poly1305), 只校验 map 分支会让这类坏
			// 节点一路进到 box.New, 把**整个实例**打死 → nodePorts 归零 → 健康检测
			// 直接跳过 → 面板上全部节点永久停在"未检测"。校验只做 box.New 不建连,
			// 成本极低, 逐节点剔除即可。
			if verr := validateOutboundEntry(ob); verr != nil {
				log.Printf("  node %d(%s): 出站无效已剔除: %v", i+1, name, verr)
				continue
			}
		case map[string]any:
			key = subEntryKey(v)
			name = subNodeDisplayName(key)
			if seenBuild[key] {
				continue
			}
			seenBuild[key] = true
			if nodeExcludedByFilter(name) {
				log.Printf("  node %d(%s): 命中排除关键词已跳过", i+1, name)
				continue
			}
			hasMap = true
			cp := map[string]any{"tag": fmt.Sprintf("out-%d", i)}
			for k, val := range v {
				if k != "tag" {
					cp[k] = val
				}
			}
			// 同 string 分支: 净化先于校验(订阅下发的 sing-box JSON 是这类方言问题
			// 最集中的来源 —— transport:{"type":"tcp"} 正是从这里进来的)。
			sanitizeOutboundTLS(cp)
			sanitizeOutboundShape(cp)
			if verr := validateOutboundEntry(cp); verr != nil {
				log.Printf("  node %d(%s): 出站无效已剔除: %v", i+1, name, verr)
				continue
			}
			ob = cp
		default:
			continue
		}
		items = append(items, nodeBuildItem{key: key, name: name, ob: ob})
	}

	// 阶段 B: 端口分配(此刻离 Start 只差毫秒级) + 部件组装。
	used := map[int]bool{}
	// 第一轮: 稳定记录复用(订阅更新端口不漂移)。
	for idx := range items {
		if p := stablePortOf(items[idx].key, used); p != 0 {
			items[idx].port = p
			used[p] = true
		}
	}
	// 第二轮: 顺序扫描补齐(同批绝对唯一)。
	for idx := range items {
		if items[idx].port != 0 {
			continue
		}
		p, err := allocateNodePortSequential(used)
		if err != nil {
			log.Printf("  node(%s): 分配本地端口失败已跳过: %v", items[idx].name, err)
			continue
		}
		items[idx].port = p
	}
	recordStablePorts(portsFromItems(items))
	persistNodeStablePorts()

	n := 0
	for _, it := range items {
		if it.port == 0 {
			continue
		}
		tag := it.ob["tag"].(string)
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": fmt.Sprintf("in-%d", n),
			"listen": "127.0.0.1", "listen_port": it.port,
		})
		outbounds = append(outbounds, it.ob)
		rules = append(rules, map[string]any{
			"action": "route", "inbound": []string{fmt.Sprintf("in-%d", n)}, "outbound": tag,
		})
		ports[it.key] = it.port
		n++
	}
	return ports, inbounds, outbounds, rules, hasMap
}

// portsFromItems 收集已分配端口(key -> port), 供稳定表批量落盘。
func portsFromItems(items []nodeBuildItem) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		if it.port != 0 {
			out[it.key] = it.port
		}
	}
	return out
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

// validateOutboundEntry 单独构建校验一个订阅出站, 单个坏节点不影响其他节点。
//
// ⚠️ 覆盖范围有限(2026-09-18 实测确认): 它只抓 sing-box 在 box.New 阶段就会
// 报的错 —— 未知 outbound type、未知 transport type(如 xhttp)。**它不校验**
// uTLS 指纹、vless flow、以及 transport=tcp: 这几类在 box.New 时静默通过,
// 到握手/初始化 TLS 才失败。所以:
//   - 别把"过了这个校验"当成"节点能用";
//   - 这些取值必须靠 sanitizeOutboundShape 在**送进真实实例之前**处理掉,
//     不能指望校验兜底。
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
	sanitizeOutboundShape(entry)
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

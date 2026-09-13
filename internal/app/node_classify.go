package app

import (
	"net"
	"strings"
)

// ============================================================================
// 出口网络分类 — 移植自 freesub classify_network_type (main_v2.py:1538)
//
// 六信号优先级判定:
//   1. CDN / Anycast 网段 → cdn (置信度 95-100)
//   2. ip-api.com hosting/mobile/proxy 字段 → datacenter/mobile (置信度 85-90)
//   3. ASN 白名单(数据中心) / 黑名单(家宽) → datacenter/residential (置信度 80-82)
//   4. ISP/ASN 名称关键词 → datacenter/residential (置信度 70)
//   5. 默认 → unknown (置信度 30)
//
// 网关运行时不做 rDNS 反查(freesub 的第 5 信号) — 反向 DNS 在网关侧引入
// 额外网络往返和 DNS 污染风险, 且 30 分钟复检周期下增益有限。rDNS 信号在
// freesub 的 CI 管道里贡献约 3% 的判定准确率, 运行时牺牲这 3% 换取零额外
// 网络开销。
// ============================================================================

// --- CDN / Anycast 网段(命中即标 CDN) ---

// cloudflareIPNetworks Cloudflare 全量 IPv4 段
var cloudflareIPNetworks = []*net.IPNet{}

func init() {
	// Cloudflare
	for _, s := range []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
		"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
		"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
		"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			cloudflareIPNetworks = append(cloudflareIPNetworks, n)
		}
	}

	// Google / Fastly / Akamai / CDN77 等
	for _, s := range []string{
		// Google
		"8.8.4.0/24", "8.8.8.0/24", "8.34.208.0/20", "8.35.192.0/20",
		"34.64.0.0/10", "35.184.0.0/13", "35.192.0.0/14", "35.196.0.0/15",
		"35.200.0.0/13", "35.216.0.0/15", "35.220.0.0/14",
		"64.15.112.0/20", "64.233.160.0/19", "66.102.0.0/20", "66.249.64.0/19",
		"72.14.192.0/18", "74.125.0.0/16", "108.177.0.0/17", "142.250.0.0/15",
		"172.217.0.0/16", "173.194.0.0/16", "209.85.128.0/17",
		"216.58.192.0/19", "216.239.32.0/19",
		// Fastly
		"23.235.32.0/20", "43.249.72.0/22", "103.244.50.0/24", "103.245.222.0/23",
		"104.156.80.0/20", "140.248.64.0/18", "146.75.0.0/16", "151.101.0.0/16",
		"157.52.64.0/18", "167.82.0.0/17", "199.232.0.0/16", "204.129.196.0/22",
		// Akamai (核心段)
		"23.32.0.0/13", "23.64.0.0/14", "23.192.0.0/11", "23.197.0.0/16",
		"95.100.0.0/15", "104.64.0.0/10", "184.24.0.0/13", "184.84.0.0/14",
		// CDN77 / Cloudflare Spectrum
		"104.16.0.0/12",
	} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			cdnIPNetworksExtra = append(cdnIPNetworksExtra, n)
		}
	}
}

// cdnIPNetworksExtra 非 Cloudflare 的 CDN/云入口段
var cdnIPNetworksExtra = []*net.IPNet{}

// --- ASN 白/黑名单 ---

// datacenterASNS 数据中心 ASN(命中即标 datacenter)
var datacenterASNS = map[int64]bool{
	13335: true,              // Cloudflare
	16509: true, 14618: true, // AWS
	15169: true, 396982: true, // Google
	8075: true, 8068: true, // Microsoft
	24940: true,              // Hetzner
	16276: true,              // OVH
	14061: true,              // DigitalOcean
	31898: true, 63949: true, // Oracle
	45102:  true,               // Alibaba
	132203: true,               // Tencent
	20473:  true,               // Choopa/Vultr
	60068:  true,               // CDN77
	55081:  true, 197540: true, // Hostinger
	51167: true,              // Contabo
	8560:  true, 42708: true, // IONOS
	201814: true, 49981: true, 141995: true, 200019: true,
	136907: true, 39351: true, 9009: true, // 小型 IDC
	174: true, 3356: true, 1299: true, 2914: true, 6939: true, // 骨干
	199524: true, 206096: true, 49505: true, // Selectel/WorldStream
	62240: true, 49304: true, 34665: true, 209242: true, 219337: true,
	44477: true, 200651: true, 202685: true, 210644: true, 205628: true,
	51852: true, 204544: true, 397373: true, 140224: true, 54866: true,
	45899: true,              // 边缘云/数据中心
	62610: true, 60205: true, // Zenlayer(收购家宽段伪装)
	8342: true, // Deltacomputers
}

// residentialASNS 家宽 ASN(命中即标 residential)
var residentialASNS = map[int64]bool{
	// 台湾
	3462: true, 9924: true, 17709: true, 4780: true, 18049: true,
	9269: true, 3491: true,
	// 香港
	4760: true, 476: true, 4515: true, 9229: true, 9266: true, 10103: true,
	9059: true, 38861: true,
	// 日本
	4713: true, 2516: true, 17676: true, 4721: true, 2497: true, 9605: true,
	17511: true, 9318: true, 2518: true, 20193: true,
	4766: true, 3786: true, 17816: true, 9357: true,
	// 韩国(与日本部分重叠)
	// 美国
	701: true, 7018: true, 7922: true, 20115: true, 22773: true, 10796: true,
	20057: true, 11427: true, 10507: true, 6128: true, 33363: true,
	21928: true, 10777: true, 33660: true, 33661: true, 33662: true,
	36466: true, 53417: true, 55136: true, 19024: true, 12271: true,
	11404: true, 6983: true, 33554: true, 7155: true, 30162: true,
	10790: true, 702: true, 703: true, 704: true, 705: true, 706: true,
	709: true, 710: true, 711: true, 712: true, 713: true, 714: true,
	715: true, 2828: true, 20001: true, 3549: true, 6167: true, 6162: true,
	5056: true, 11351: true,
	// 英国
	2856: true, 5607: true, 20650: true, 13285: true, 12576: true,
	12725: true, 19541: true, 33950: true, 5413: true,
	// 德国
	3320: true, 3209: true, 6805: true, 8888: true, 9145: true, 13237: true,
	15366: true, 20879: true, 16097: true, 15594: true,
	// 法国
	3215: true, 12322: true, 15557: true, 5410: true, 21590: true,
	22869: true, 8228: true, 8220: true, 12670: true,
	// 荷兰/比利时
	33915: true, 20857: true, 5418: true, 6777: true, 15535: true,
	6830: true, 8683: true,
	// 加拿大
	577: true, 6539: true, 812: true, 7992: true, 22995: true, 23498: true,
	30645: true, 11260: true, 5645: true, 13331: true,
}

// --- ISP 名称关键词 ---

// idcNamePatterns 数据中心/云厂商名称关键词
var idcNamePatterns = []string{
	"hosting", "hoster", "datacenter", "data center", "cloud", "server",
	"vps", "dedicated", "colo", "colocation", "compute", "storage",
	"amazon", "aws", "google cloud", "microsoft", "azure", "oracle",
	"digitalocean", "linode", "vultr", "choopa", "hetzner", "ovh",
	"contabo", "m247", "leaseweb", "online s.a.s", "scaleway",
	"alibaba", "tencent", "huawei cloud", "ucloud", "jdcloud", "ksyun",
	"fastly", "cloudflare", "akamai", "cdn", "anycast", "edge network",
	"hostkey", "selectel", "aeza", "justhost", "idnica", "hostinger",
	"ionos", "1&1", "godaddy", "namecheap", "sucuri", "ispxk",
	"zenlayer", "zencom", "g-core", "gcore", "netcup",
}

// residentialNamePatterns 家宽/民用 ISP 名称关键词
var residentialNamePatterns = []string{
	"broadband", "pppoe", "pppoa", "dsl", "cable", "fiber", "ftth",
	"fibre", "dynamic", "dial", "dialup", "residential", "home",
	"consumer", "cust", "customer", "subscriber", "pool", "dynamic-ip",
	// 台湾
	"chunghwa", "hinet", "taiwanmobile", "aptg", "kbro", "tfn", "sparq",
	"seednet",
	// 香港
	"hkbn", "hong kong broadband", "pccw", "hkt", "hgc", "smartone",
	"netvigator", "citic telecom", "i-cable", "hk cable",
	// 日本
	"softbank", "ocn", "plala", "so-net", "kddi", "jcom", "biglobe",
	// 韩国
	"korea telecom", "kt corp", "sk broadband", "lgu+", "lg uplus",
	// 美国
	"comcast", "charter communications", "spectrum", "cox communications",
	"at&t", "bellsouth", "qwest", "centurylink", "verizon fios",
	"verizon online", "frontier communications", "windstream", "altice",
	"optimum online", "rcn", "wave broadband", "hughes", "viasat",
	"starlink",
	// 欧洲
	"deutsche telekom", "telekom deutschland", "vodafone", "kabel deutschland",
	"british telecom", "bt broadband", "virgin media", "sky uk", "talktalk",
	"orange sa", "sfr", "bouygues", "numericable", "kpn", "ziggo",
	"telenet", "proximus",
}

// --- 分类主函数 ---

// classifyNodeNetwork 对节点出口 IP 做六信号分类, 返回 (类型, 置信度)。
// 类型 ∈ {datacenter, residential, mobile, cdn, unknown}。
func classifyNodeNetwork(r *nodeTestResult) (string, int) {
	if r.ExitIP == "" {
		return "unknown", 0
	}
	ip := net.ParseIP(r.ExitIP)
	if ip == nil {
		return "unknown", 0
	}

	// 1) CDN / Anycast 网段(硬判据)
	for _, n := range cloudflareIPNetworks {
		if n.Contains(ip) {
			return "cdn", 100
		}
	}
	for _, n := range cdnIPNetworksExtra {
		if n.Contains(ip) {
			return "cdn", 95
		}
	}

	// 2) ip-api.com 在线字段(最高可信)
	//    proxy=true 硬否决: 收购家宽段的伪装云边网络(如 Zenlayer AS62610)
	if r.IPAPIHosting {
		return "datacenter", 90
	}
	if r.IPAPIProxy {
		return "datacenter", 88
	}
	if r.IPAPIMobile {
		return "mobile", 85
	}

	// 3) ASN 白/黑名单
	asn := r.ExitASN
	if asn != 0 {
		if datacenterASNS[asn] {
			return "datacenter", 80
		}
		if residentialASNS[asn] {
			return "residential", 82
		}
	}

	// 4) ISP/ASN 名称关键词
	orgLower := strings.ToLower(r.ExitISP + " " + r.ExitASNorg)
	if orgLower != "" {
		for _, kw := range idcNamePatterns {
			if strings.Contains(orgLower, kw) {
				return "datacenter", 70
			}
		}
		for _, kw := range residentialNamePatterns {
			if strings.Contains(orgLower, kw) {
				return "residential", 70
			}
		}
	}

	// 5) 默认: 无法判定
	return "unknown", 30
}

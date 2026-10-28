// Package config 定义 BoxHub 的持久化配置模型。
//
// 设计要点(改动前请先读):
//
//  1. 配置是一份「意图」而不是内核文件。面板保存配置后由 render 包编译成
//     sing-box JSON,用户永远看不到也不需要手写内核配置。
//  2. 链式代理(Chain)不是一种新协议,而是「出口 + 前置」的组合。前置必须是
//     节点组(NodeGroup)的 tag,出口是 socks5/http/https 一类上游。编译时出口
//     出站带 detour 指向前置组,于是流量先经前置再到出口。
//  3. 链式代理编译后的出站是一个普通节点,可以进静态节点组、可以被分流规则引用;
//     但它永远不能进动态组,前置组也不能包含链式节点本身——否则内核启动时会报
//     circular outbound dependency(实测见 internal/kernel 的测试)。
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CurrentSchema 是当前配置结构的版本号。结构不兼容变更时递增,并在 store 里
// 提供迁移。
const CurrentSchema = 1

// Config 是整体配置。保存时整体替换,便于导入导出与备份。
type Config struct {
	Schema int `json:"schema"`

	// Panel 面板设置(监听地址、语言、主题等)。
	Panel PanelSettings `json:"panel"`
	// DNS 域名解析设置。
	DNS DNSSettings `json:"dns"`
	// Inbounds 入站(本地代理端口、透明代理等)。
	Inbounds InboundSettings `json:"inbounds"`

	Subscriptions []Subscription `json:"subscriptions"`
	// Nodes 手工添加的节点。订阅节点单独存放,更新订阅不会覆盖手工节点。
	Nodes []Node `json:"nodes"`
	// Groups 节点组(自动择优 / 手动选择 / 故障转移 / 动态关键词组)。
	Groups []Group `json:"groups"`
	// Chains 链式代理。
	Chains []Chain `json:"chains"`
	// Filters 节点筛选(按名称自动归类,订阅更新后自动跟随)。
	Filters []Filter `json:"filters"`
	// Policies 目标分流规则,从上往下匹配,先命中的生效。
	Policies []Policy `json:"policies"`

	// UpdatedAt 最后保存时间,用于面板显示与并发检测。
	UpdatedAt time.Time `json:"updated_at"`
}

// NewDefault 返回一份可以直接启动的默认配置:本地混合端口 + 全部直连 + 国内直连。
func NewDefault() *Config {
	return &Config{
		Schema: CurrentSchema,
		Panel: PanelSettings{
			Language: "zh-CN",
			Theme:    "auto",
		},
		DNS: DefaultDNS(),
		Inbounds: InboundSettings{
			Mixed: true,
			MixedListen: "0.0.0.0",
			MixedPort:   7890,
			ClashAPI:    true,
			ClashAPIPort: 9090,
			AllowLAN:    true,
		},
		Policies: DefaultPolicies(),
		UpdatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// 面板
// ---------------------------------------------------------------------------

type PanelSettings struct {
	Language string `json:"language"`
	Theme    string `json:"theme"`
	// SubscriptionUserAgent 拉取订阅时使用的 UA。部分机场按 UA 返回不同格式。
	SubscriptionUserAgent string `json:"subscription_user_agent"`
	// SubscriptionInterval 订阅自动更新间隔,0 表示不自动更新。
	SubscriptionInterval time.Duration `json:"subscription_interval"`
}

// ---------------------------------------------------------------------------
// DNS
// ---------------------------------------------------------------------------

type DNSSettings struct {
	// Mode 指示路由规则里是否劫持 DNS。默认开启。
	Hijack bool `json:"hijack"`
	// DirectServers 直连域名(中国大陆)使用的上游。
	DirectServers []string `json:"direct_servers"`
	// ProxyServers 代理域名使用的上游(经代理查询,避免污染)。
	ProxyServers []string `json:"proxy_servers"`
	// FinalServer 未命中任何规则时使用的上游。
	FinalServer string `json:"final_server"`
	// Strategy prefer_ipv4 / ipv4_only / prefer_ipv6 / ipv6_only。
	Strategy string `json:"strategy"`
	// RedirectOnly 开启"直连不走内核"时,走代理的域名用 FakeIP 占位,直连域名
	// 走真实 DNS。这样入口只看 IP 就能分流,同时不影响直连站点的 CDN 就近。
	FakeIP bool `json:"fake_ip"`
	// Mode DNS 分流模式(对齐 SMBox 的「DNS 分流模式」):
	//   "proxy"(默认代理): 所有设备的域名解析走 sing-box(境外域名 FakeIP/代理DNS),
	//     ModeExceptions 里的设备例外,走直连 DNS 真实解析;
	//   "direct"(默认直连): 所有设备走直连 DNS 真实解析,
	//     ModeExceptions 里的设备例外,走 sing-box(FakeIP/代理DNS)。
	Mode string `json:"mode,omitempty"`
	// ModeExceptions 例外设备(来源 IP / CIDR 列表)。
	ModeExceptions []string `json:"mode_exceptions,omitempty"`
}

// DNSMode 取值。
const (
	DNSModeProxy  = "proxy"
	DNSModeDirect = "direct"
)

func DefaultDNS() DNSSettings {
	return DNSSettings{
		Hijack:        true,
		DirectServers: []string{"223.5.5.5", "119.29.29.29"},
		// 代理 DNS 默认用 TCP 明文 DNS(不走 DoH/DoT):
		// 经隧道型节点做 DNS-over-TLS 常被干扰(tls handshake failure),
		// 导致境外域名全部解析失败。
		// 更根本的是:连接域名时内核默认把域名直接交给节点解析,
		// 这些上游只在"确实需要本地解析"时才用到。
		ProxyServers:  []string{"tcp://8.8.8.8", "tcp://1.1.1.1"},
		FinalServer:   "proxy",
		Strategy:      "prefer_ipv4",
		FakeIP:        true,
		Mode:          DNSModeProxy,
	}
}

// ---------------------------------------------------------------------------
// 入站
// ---------------------------------------------------------------------------

type InboundSettings struct {
	// Mixed 同时监听 HTTP 与 SOCKS。在 systemd 下保留,方便局域网设备直接填端口。
	Mixed       bool   `json:"mixed"`
	MixedListen string `json:"mixed_listen"`
	MixedPort   int    `json:"mixed_port"`
	// TUN 透明代理。Linux 下需要 root 与 /dev/net/tun。
	TUN        bool   `json:"tun"`
	TUNAutoRoute bool `json:"tun_auto_route"`
	TUNStack   string `json:"tun_stack"`
	// TunInterfaceName 留空时使用内核默认。
	TunAddress []string `json:"tun_address"`
	// ClashAPI 提供 metacubexd 面板所需的 REST 接口。
	ClashAPI       bool   `json:"clash_api"`
	ClashAPIPort   int    `json:"clash_api_port"`
	ClashAPISecret string `json:"clash_api_secret"`
	// AllowLAN 允许局域网访问面板与代理端口。
	AllowLAN bool `json:"allow_lan"`
	// Sniff 入站协议嗅探(域名在 TUN 场景下必需)。
	Sniff bool `json:"sniff"`
	// BlockQUIC 拒绝走代理线路的 UDP 443,浏览器自动退回 TCP。
	BlockQUIC bool `json:"block_quic"`
}

// ---------------------------------------------------------------------------
// 订阅
// ---------------------------------------------------------------------------

type Subscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	UserAgent string    `json:"user_agent,omitempty"`
	// UpdateInterval 分钟,0 跟随面板设置。
	UpdateInterval int `json:"update_interval"`
	LastUpdate     time.Time `json:"last_update"`
	LastError      string    `json:"last_error,omitempty"`
	// Filter 只导入 name 含该关键词的节点,留空导入全部。
	Filter string `json:"filter,omitempty"`
	// Info 机场流量信息(订阅流量头)。
	Info SubscriptionInfo `json:"info"`
}

type SubscriptionInfo struct {
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Total    int64     `json:"total"`
	Expire   time.Time `json:"expire"`
}

// ---------------------------------------------------------------------------
// 节点
// ---------------------------------------------------------------------------

type Node struct {
	// ID 稳定标识。订阅节点为 "sub:<订阅ID>:<哈希>",手工节点为 "node:<随机>"。
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"` // sshadowsocks/vmess/vless/trojan/hysteria2/tuic/anytls/wireguard/socks/http
	Server string `json:"server"`
	Port   int    `json:"port"`
	// UUID / Password / Username 等认证字段,按协议使用。
	UUID     string `json:"uuid,omitempty"`
	Password string `json:"password,omitempty"`
	Username string `json:"username,omitempty"`

	// TLS 与传输层
	TLS        *TLSConfig        `json:"tls,omitempty"`
	Transport  *TransportConfig  `json:"transport,omitempty"`
	Multiplex  *MultiplexConfig  `json:"multiplex,omitempty"`
	Plugin     string            `json:"plugin,omitempty"`
	PluginOpts map[string]string `json:"plugin_opts,omitempty"`

	// 协议私有字段
	Method        string   `json:"method,omitempty"`
	Security      string   `json:"security,omitempty"`
	AlterID       int      `json:"alter_id,omitempty"`
	Flow          string   `json:"flow,omitempty"`
	Network       string   `json:"network,omitempty"`
	UpMbps        int      `json:"up_mbps,omitempty"`
	DownMbps      int      `json:"down_mbps,omitempty"`
	Obfs          string   `json:"obfs,omitempty"`
	ObfsPassword  string   `json:"obfs_password,omitempty"`
	CongestionControl string `json:"congestion_control,omitempty"`
	UDPOverTCP    bool     `json:"udp_over_tcp,omitempty"`
	HopPorts      string   `json:"hop_ports,omitempty"`
	HopInterval   int      `json:"hop_interval,omitempty"`

	// 来源:sub:<id> 或 manual
	Source string `json:"source"`
	// Disabled 禁用后不参与任何节点组。
	Disabled bool `json:"disabled,omitempty"`
	// Raw 原始的分享链接,便于解析失败时回显。
	Raw string `json:"raw,omitempty"`
}

type TLSConfig struct {
	Enabled    bool     `json:"enabled"`
	ServerName string   `json:"server_name,omitempty"`
	Insecure   bool     `json:"insecure,omitempty"`
	ALPN       []string `json:"alpn,omitempty"`
	// Reality
	RealityPublicKey string `json:"reality_public_key,omitempty"`
	RealityShortID   string `json:"reality_short_id,omitempty"`
	UTLSFingerprint  string `json:"utls_fingerprint,omitempty"`
	// ECH 由订阅提供时原样透传。
	ECH []string `json:"ech,omitempty"`
}

type TransportConfig struct {
	Type string `json:"type"` // ws/grpc/http/httpupgrade
	Path string `json:"path,omitempty"`
	Host string `json:"host,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	// Headers 仅 http 传输使用。
	Headers map[string]string `json:"headers,omitempty"`
}

type MultiplexConfig struct {
	Enabled    bool   `json:"enabled"`
	Protocol   string `json:"protocol,omitempty"`
	MaxStreams int    `json:"max_streams,omitempty"`
}

// ---------------------------------------------------------------------------
// 节点组
// ---------------------------------------------------------------------------

const (
	// GroupSelect 手动选择。
	GroupSelect = "select"
	// GroupURLTest 自动择优。
	GroupURLTest = "urltest"
	// GroupFallback 故障转移:按顺序主备,主用不可用才切备用。
	GroupFallback = "fallback"
	// GroupDynamic 动态关键词组:自动收编名称命中的节点。
	GroupDynamic = "dynamic"
)

type Group struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	// Members 成员节点 ID(select/urltest/fallback 使用)。
	Members []string `json:"members"`
	// Keywords 动态组关键词。
	Keywords []string `json:"keywords,omitempty"`
	// KeywordMode or / and。
	KeywordMode string `json:"keyword_mode,omitempty"`
	// FilterRegex 动态组附加的正则过滤(订阅给的名称很乱时用)。
	FilterRegex string `json:"filter_regex,omitempty"`
	// ExcludeChain 动态组是否排除链式代理。默认 true:链式代理是出口,
	// 混进动态组会导致"前置用自己"的循环依赖。
	IncludeChain bool `json:"include_chain,omitempty"`
	// URL / Interval / Tolerance 自动择优与故障转移的检测参数。
	URL      string `json:"url,omitempty"`
	Interval int    `json:"interval,omitempty"`
	Tolerance int   `json:"tolerance,omitempty"`
	// SuccessStatus 判定成功的 HTTP 状态码,0 表示仅看连通性。
	SuccessStatus int `json:"success_status,omitempty"`
	// Disabled 禁用后组不生成,引用它的地方自动跳过。
	Disabled bool `json:"disabled,omitempty"`
}

// Tag 是配置内部的引用标识(规则、链式代理前置用它指向组),稳定不随改名变化。
func (g Group) Tag() string { return "group:" + g.ID }

// OutboundTag 是节点组写到内核配置里的 tag,直接用名称,
// 以便在内核面板里与面板显示一致(与筛选组同一套做法)。
func (g Group) OutboundTag() string { return g.Name }

// ---------------------------------------------------------------------------
// 链式代理
// ---------------------------------------------------------------------------

const (
	ChainSocks5 = "socks5"
	ChainHTTP   = "http"
	ChainHTTPS  = "https"
)

// Chain 表示一条链式代理:**前置**一个节点组,**出口**一个 socks5/http/https 上游。
// 流量先经前置再到出口,出口看到的源 IP 是前置的地址(住宅 / 静态 IP 的典型用法)。
type Chain struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// FrontGroupID 前置出站引用:"group:<id>"(节点组)、"filter:<id>"(筛选组),
	// 或裸 ID(兼容旧数据,按节点组处理)。流量先走这个组当前选中的节点。
	// 必填:内核要求链式出站的 detour 指向一个具体出站,用独立的组
	// 才能保证不会引用到链式节点本身(否则循环依赖,内核起不来)。
	FrontGroupID string `json:"front_group_id"`
	// Type 出口类型:socks5 / http / https。
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// TLSServerName https 出口的 SNI,留空用 Host。
	TLSServerName string `json:"tls_server_name,omitempty"`
	Insecure      bool   `json:"insecure,omitempty"`
	// UDP 是否允许 UDP 走链(多数住宅代理不支持 UDP)。
	UDP bool `json:"udp,omitempty"`
	// Network tcp/udp,留空表示不限制。
	Network string `json:"network,omitempty"`
	Disabled bool  `json:"disabled,omitempty"`
	// Probe 最近一次探测结果,便于面板展示。
	Probe *ProbeResult `json:"probe,omitempty"`
	// Raw 用户粘贴的链接(socks5://user:pass@host:port)。
	Raw string `json:"raw,omitempty"`
}

// Tag 返回链式出站在内核配置里的名字。
func (c Chain) Tag() string { return "chain:" + c.ID }

// FrontRef 解析前置引用,返回 kind("group"/"filter") 与 ID。
// 裸 ID 按 group 处理(兼容旧配置)。
func (c Chain) FrontRef() (kind, id string) {
	switch {
	case strings.HasPrefix(c.FrontGroupID, "filter:"):
		return "filter", strings.TrimPrefix(c.FrontGroupID, "filter:")
	case strings.HasPrefix(c.FrontGroupID, "group:"):
		return "group", strings.TrimPrefix(c.FrontGroupID, "group:")
	default:
		return "group", c.FrontGroupID
	}
}

// FrontIsFilter 判断前置是否是筛选组。
// 筛选组成员只有节点,永远不会形成循环依赖,校验可以放宽。
func (c Chain) FrontIsFilter() bool {
	k, _ := c.FrontRef()
	return k == "filter"
}

// Validate 检查链式代理自身的字段完整性。前置组是否存在由 store 层校验,
// 因为需要访问其它集合。
func (c *Chain) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("链式代理名称不能为空")
	}
	switch c.Type {
	case ChainSocks5, ChainHTTP, ChainHTTPS:
	default:
		return fmt.Errorf("不支持的出口类型 %q,仅支持 socks5 / http / https", c.Type)
	}
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("出口地址不能为空")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("出口端口无效: %d", c.Port)
	}
	if strings.TrimSpace(c.FrontGroupID) == "" {
		return errors.New("必须指定前置节点组")
	}
	return nil
}

// ProbeResult 一次链路探测的结果。
type ProbeResult struct {
	At time.Time `json:"at"`
	// DelayMs 探测延时,-1 表示失败。
	DelayMs int `json:"delay_ms"`
	// ExitIP 出口公网 IP 与归属,失败时为空。
	ExitIP string `json:"exit_ip,omitempty"`
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	ISP     string `json:"isp,omitempty"`
	Error   string `json:"error,omitempty"`
	// ExitByProto 按协议区分的出口 IP。
	// 智能出口池会按目标协议分配不同出口(实测:HTTP 走国内池、HTTPS 走境外池),
	// 单看一个数字会误导用户,所以两个都记下来。
	ExitByProto map[string]string `json:"exit_by_proto,omitempty"`
	// PoolNote 出口池行为说明。
	PoolNote string `json:"pool_note,omitempty"`
}

// ---------------------------------------------------------------------------
// 目标分流
// ---------------------------------------------------------------------------

// Policy 一条分流规则。Match 里的条件之间的关系是 AND;多个 Policy 之间
// 从上往下匹配,先命中的算数。
type Policy struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Target 命中的连接走哪里:"direct" / "block" / 节点组 tag("group:<id>") /
	// 链式代理 tag("chain:<id>") / 节点 tag。面板上统一叫"出口"。
	Target string `json:"target"`
	// 匹配条件
	Match Match `json:"match"`
	// NoFakeIP 该规则命中的域名不返回 FakeIP,直接给真实地址。
	// 对应 SMBox 的 noFAKEIP 选项。
	NoFakeIP bool `json:"no_fake_ip,omitempty"`
	// PhoneOnly 仅手机端设备命中(来源 IP 在指定网段/设备列表)。
	// SMBox 用设备识别实现,BoxHub 以"来源 IP 列表"近似。
	PhoneOnly bool `json:"phone_only,omitempty"`
	// PhoneCIDRs PhoneOnly 的设备来源网段。
	PhoneCIDRs []string `json:"phone_cidrs,omitempty"`
	// Invert 反转匹配结果。
	Invert bool `json:"invert,omitempty"`
	// Groups 条件组(对齐 SMBox 的多条件组编辑器)。
	// 为空时渲染层退回扁平 Match(旧配置兼容)。
	Groups ConditionGroups `json:"groups,omitempty"`
	// RawJSON 「使用 Sing-box JSON 规则」逃生口:原样输出的规则数组 JSON 文本。
	// 条件组表达不了的冷门规则(如 protocol/logical 嵌套)用它兜底;
	// 非空时忽略 Groups/Match。内容由内核 check 把关。
	RawJSON string `json:"raw_json,omitempty"`
	// AsGroup 为 true 时,这条规则会在内核配置里生成一个**同名选择组**,
	// 组成员是 [总出口 Proxy, 所有筛选组..., Direct]。
	// 这是 SMBox 的做法:规则名既是分组名,也是出站名,
	// 于是每类站点(INS/Youtube/AI)都能在内核面板里单独选线路。
	AsGroup bool `json:"as_group,omitempty"`
}

type Match struct {
	// DomainSuffix / Domain 精确域名 / DomainKeyword / DomainRegex
	Domain       []string `json:"domain,omitempty"`
	DomainSuffix []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	DomainRegex  []string `json:"domain_regex,omitempty"`
	// IPCIDR 目标 IP 段。
	IPCIDR []string `json:"ip_cidr,omitempty"`
	// GeoIP 国家代码,如 cn。
	GeoIP []string `json:"geo_ip,omitempty"`
	// GeoSite 站点分类,如 cn / geolocation-!cn。
	GeoSite []string `json:"geo_site,omitempty"`
	// RuleSets 规则集链接(sing-box rule-set 格式)。
	RuleSets []string `json:"rule_sets,omitempty"`
	// Port / PortRange 目标端口。
	Port      []int  `json:"port,omitempty"`
	PortRange []string `json:"port_range,omitempty"`
	// ProcessName / PackageName 按进程分流(需内核支持)。
	ProcessName []string `json:"process_name,omitempty"`
	PackageName []string `json:"package_name,omitempty"`
	// ClientIP 按来源终端(局域网设备)分流。
	ClientIP []string `json:"client_ip,omitempty"`
	// MACAddress 按设备 MAC 分流(TUN 模式)。
	MACAddress []string `json:"mac_address,omitempty"`
	// Network tcp/udp。
	Network []string `json:"network,omitempty"`
	// Inbound 按入站。
	Inbound []string `json:"inbound,omitempty"`
	// ClashMode 按面板模式(rule/direct/global)。
	ClashMode []string `json:"clash_mode,omitempty"`
	// QueryType DNS 查询类型。
	QueryType []string `json:"query_type,omitempty"`
	// Protocol 嗅探出的协议(http/tls/quic/dns...)。
	Protocol []string `json:"protocol,omitempty"`
}

// Empty 判断匹配条件是否为空(空规则会命中所有流量,保存时要拒绝)。
func (m Match) Empty() bool {
	return len(m.Domain) == 0 && len(m.DomainSuffix) == 0 && len(m.DomainKeyword) == 0 &&
		len(m.DomainRegex) == 0 && len(m.IPCIDR) == 0 && len(m.GeoIP) == 0 &&
		len(m.GeoSite) == 0 && len(m.RuleSets) == 0 && len(m.Port) == 0 &&
		len(m.PortRange) == 0 && len(m.ProcessName) == 0 && len(m.PackageName) == 0 &&
		len(m.ClientIP) == 0 && len(m.MACAddress) == 0 && len(m.Network) == 0 &&
		len(m.Inbound) == 0 && len(m.ClashMode) == 0 && len(m.QueryType) == 0 &&
		len(m.Protocol) == 0
}

// DefaultPolicies 返回默认分流:本机/私网直连 → 中国大陆直连 → 兜底走代理。
// 目标 Target 里 "group:" 引用由 NewDefaultConfig 之后的 ensureDefaultGroups 补全,
// 这里先留空 fallback,由调用方注入默认组 ID。
func DefaultPolicies() []Policy {
	return []Policy{
		{
			ID: "policy-private", Name: "局域网与私网直连", Enabled: true, Target: "direct",
			Match: Match{IPCIDR: PrivateCIDRs, DomainSuffix: []string{"local", "lan"}},
		},
		{
			ID: "policy-cn", Name: "中国大陆直连", Enabled: true, Target: "direct",
			Match: Match{GeoSite: []string{"cn"}, GeoIP: []string{"cn"}},
		},
		{
			ID: "policy-quic", Name: "屏蔽走代理的 QUIC", Enabled: false, Target: "block",
			Match: Match{Network: []string{"udp"}, Port: []int{443}},
		},
	}
}

// PrivateCIDRs 私网与保留地址。
var PrivateCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::1/128",
	"fc00::/7", "fe80::/10",
}

// IDPattern 所有实体的 ID 只允许安全的字符,因为它们会出现在内核出站名与文件路径里。
var IDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// NewID 生成一个简短随机 ID(不带依赖,便于测试)。
func NewID(prefix string, rnd func(int) string) string {
	return prefix + "-" + rnd(8)
}

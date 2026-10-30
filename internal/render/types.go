package render

// 本文件是 sing-box 1.14 配置的 JSON 结构。字段名与内核严格一致:
// 写错一个字母内核就会 FATAL,所以这里的 json tag 必须对着官方文档或实测输出填。
// 未使用的字段一律 omitempty,避免内核因为不认识的零值字段报错。

type SBConfig struct {
	Log          SBLog           `json:"log"`
	DNS          *SBDNS          `json:"dns,omitempty"`
	Inbounds     []SBInbound     `json:"inbounds"`
	Outbounds    []SBOutbound    `json:"outbounds"`
	Route        *SBRoute        `json:"route,omitempty"`
	Experimental *SBExperimental `json:"experimental,omitempty"`
}

type SBLog struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// DNS
// ---------------------------------------------------------------------------

type SBDNS struct {
	Servers  []SBDNSServer `json:"servers"`
	Rules    []SBDNSRule   `json:"rules,omitempty"`
	Final    string        `json:"final,omitempty"`
	Strategy string        `json:"strategy,omitempty"`

	// ruleSetRefs 记录 DNS 规则引用到的规则集,编译结束后合并进 route.rule_set。
	// 内核里 dns.rules 只能引用 route.rule_set 中已声明的 tag。
	ruleSetRefs []string
}

type SBDNSServer struct {
	Tag        string `json:"tag"`
	Type       string `json:"type"`
	Server     string `json:"server,omitempty"`
	ServerPort int    `json:"server_port,omitempty"`
	Detour     string `json:"detour,omitempty"`
	// FakeIP 服务器专用(1.14 起 fakeip 是 servers 里的一种类型,
	// 旧的 dns.fakeip 顶层字段已被移除)
	Inet4Range string `json:"inet4_range,omitempty"`
	Inet6Range string `json:"inet6_range,omitempty"`
}

type SBDNSRule struct {
	QueryType    []string `json:"query_type,omitempty"`
	Invert       bool     `json:"invert,omitempty"`
	SourceIPCIDR []string `json:"source_ip_cidr,omitempty"`
	RewriteTTL   int      `json:"rewrite_ttl,omitempty"`
	Domain       []string `json:"domain,omitempty"`
	DomainSuffix []string `json:"domain_suffix,omitempty"`
	RuleSet      []string `json:"rule_set,omitempty"`
	ClashMode    string   `json:"clash_mode,omitempty"`
	Server       string   `json:"server"`
	DisableCache bool     `json:"disable_cache,omitempty"`
}

// ---------------------------------------------------------------------------
// 入站
// ---------------------------------------------------------------------------

type SBInbound struct {
	Type          string   `json:"type"`
	Tag           string   `json:"tag"`
	Listen        string   `json:"listen,omitempty"`
	ListenPort    int      `json:"listen_port,omitempty"`
	Address       []string `json:"address,omitempty"`
	AutoRoute     bool     `json:"auto_route,omitempty"`
	StrictRoute   bool     `json:"strict_route,omitempty"`
	Stack         string   `json:"stack,omitempty"`
	MTU           int      `json:"mtu,omitempty"`
	Sniff         bool     `json:"sniff,omitempty"`
	SniffOverride bool     `json:"sniff_override_destination,omitempty"`
}

// ---------------------------------------------------------------------------
// 出站
// ---------------------------------------------------------------------------

type SBOutbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server,omitempty"`
	ServerPort int    `json:"server_port,omitempty"`
	// detour 指向另一个出站:流量先经过它,再连到本出站的 server。
	// 链式代理就是靠这个字段实现的。
	Detour    string   `json:"detour,omitempty"`
	Outbounds []string `json:"outbounds,omitempty"` // selector / urltest
	Default   string   `json:"default,omitempty"`   // selector
	// urltest 参数
	URL           string           `json:"url,omitempty"`
	Interval      string           `json:"interval,omitempty"`
	Tolerance     int              `json:"tolerance,omitempty"`
	URLTestOption *SBURLTestOption `json:"url_test_options,omitempty"`

	// 认证
	UUID     string `json:"uuid,omitempty"`
	Password string `json:"password,omitempty"`
	Username string `json:"username,omitempty"`
	Version  string `json:"version,omitempty"`
	Method   string `json:"method,omitempty"`
	Security string `json:"security,omitempty"`
	AlterID  int    `json:"alter_id,omitempty"`
	Flow     string `json:"flow,omitempty"`

	// 传输
	TLS        *SBTLS            `json:"tls,omitempty"`
	Transport  *SBTransport      `json:"transport,omitempty"`
	Multiplex  *SBMultiplex      `json:"multiplex,omitempty"`
	Plugin     string            `json:"plugin,omitempty"`
	PluginOpts map[string]string `json:"plugin_opts,omitempty"`

	Network           string        `json:"network,omitempty"`
	UDPOverTCP        *SBUDPOverTCP `json:"udp_over_tcp,omitempty"`
	UpMbps            int           `json:"up_mbps,omitempty"`
	DownMbps          int           `json:"down_mbps,omitempty"`
	Obfs              *SBObfs       `json:"obfs,omitempty"`
	CongestionControl string        `json:"congestion_control,omitempty"`
	UDPRelayMode      string        `json:"udp_relay_mode,omitempty"`

	// wireguard
	LocalAddress []string `json:"address,omitempty"`
	PrivateKey   string   `json:"private_key,omitempty"`
	PublicKey    string   `json:"peer_public_key,omitempty"`
	Reserved     []int    `json:"reserved,omitempty"`

	// 通用
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`
	// DomainResolver 1.12+ 必需:指定解析 server 域名所用的 DNS 服务器 tag。
	// 不填会让内核在 1.14 直接 FATAL(除非设 ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER)。
	DomainResolver string `json:"domain_resolver,omitempty"`
}

type SBURLTestOption struct {
	SuccessHTTPResponse *SBSuccessStatus `json:"success_http_response,omitempty"`
}

type SBSuccessStatus struct {
	StatusCode int `json:"status_code,omitempty"`
}

type SBTLS struct {
	Enabled    bool       `json:"enabled"`
	ServerName string     `json:"server_name,omitempty"`
	Insecure   bool       `json:"insecure,omitempty"`
	ALPN       []string   `json:"alpn,omitempty"`
	UTLS       *SBUTLS    `json:"utls,omitempty"`
	Reality    *SBReality `json:"reality,omitempty"`
	ECH        *SBECH     `json:"ech,omitempty"`
}

type SBUTLS struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type SBReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id,omitempty"`
}

type SBECH struct {
	Enabled bool     `json:"enabled"`
	Config  []string `json:"config,omitempty"`
}

type SBTransport struct {
	Type        string            `json:"type"`
	Path        string            `json:"path,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Host        string            `json:"host,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
}

type SBMultiplex struct {
	Enabled    bool   `json:"enabled"`
	Protocol   string `json:"protocol,omitempty"`
	MaxStreams int    `json:"max_streams,omitempty"`
}

type SBUDPOverTCP struct {
	Enabled bool `json:"enabled"`
	Version int  `json:"version,omitempty"`
}

type SBObfs struct {
	Type     string `json:"type"`
	Password string `json:"password"`
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

type SBRoute struct {
	Rules                 []SBRule    `json:"rules,omitempty"`
	RuleSet               []SBRuleSet `json:"rule_set,omitempty"`
	Final                 string      `json:"final,omitempty"`
	AutoDetectInterface   bool        `json:"auto_detect_interface,omitempty"`
	DefaultDomainResolver string      `json:"default_domain_resolver,omitempty"`
}

type SBRule struct {
	// 匹配条件
	Inbound       []string `json:"inbound,omitempty"`
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	DomainRegex   []string `json:"domain_regex,omitempty"`
	Protocol      []string `json:"protocol,omitempty"`
	IPCIDR        []string `json:"ip_cidr,omitempty"`
	IPIsPrivate   bool     `json:"ip_is_private,omitempty"`
	Port          []int    `json:"port,omitempty"`
	PortRange     []string `json:"port_range,omitempty"`
	Network       []string `json:"network,omitempty"`
	ProcessName   []string `json:"process_name,omitempty"`
	PackageName   []string `json:"package_name,omitempty"`
	ClientIP      []string `json:"source_ip_cidr,omitempty"`
	ClashMode     string   `json:"clash_mode,omitempty"`
	RuleSet       []string `json:"rule_set,omitempty"`
	Invert        bool     `json:"invert,omitempty"`

	// 动作
	Action       string `json:"action,omitempty"`
	Outbound     string `json:"outbound,omitempty"`
	Server       string `json:"server,omitempty"` // action=resolve / hijack-dns
	NoDrop       bool   `json:"no_drop,omitempty"`
	SniffTimeout string `json:"sniff_timeout,omitempty"`

	// action=logical(组内 AND)
	Mode  string          `json:"mode,omitempty"`
	Rules []SBLogicalRule `json:"rules,omitempty"`
}

// SBLogicalRule logical 规则的子片段:纯匹配,不带动作。
type SBLogicalRule SBRule

type SBRuleSet struct {
	Tag    string `json:"tag"`
	Type   string `json:"type"`
	Format string `json:"format,omitempty"`
	URL    string `json:"url,omitempty"`
	// Path 本地规则集路径。有本地缓存时用 type:local + path,
	// 内核启动就不需要联网下载(下载失败会 FATAL,全屋断网)。
	Path           string `json:"path,omitempty"`
	UpdateInterval string `json:"update_interval,omitempty"`
	// DownloadDetour 规则集下载所经的出站;与 HTTPClient 互斥。
	DownloadDetour string        `json:"download_detour,omitempty"`
	HTTPClient     *SBHTTPClient `json:"http_client,omitempty"`
}

// SBHTTPClient 规则集下载用的自定义 HTTP 客户端(需要指定 detour 时使用)。
type SBHTTPClient struct {
	Detour string `json:"detour,omitempty"`
}

// ---------------------------------------------------------------------------
// 实验特性
// ---------------------------------------------------------------------------

type SBExperimental struct {
	CacheFile *SBCacheFile `json:"cache_file,omitempty"`
	ClashAPI  *SBClashAPI  `json:"clash_api,omitempty"`
}

type SBCacheFile struct {
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"`
	StoreRDRC bool   `json:"store_rdrc,omitempty"`
	// StoreFakeIP 持久化 FakeIP 映射。
	// 必须开启:boxd 改配置是"停内核 + 起内核"(sing-box 没有热重载),
	// 映射只存内存时,重启后客户端缓存里的假地址全部失效,
	// 内核日志刷 "missing fakeip record",这些目标再落进私网直连规则
	// 就是连不通的黑洞。Windows 的 DNS 缓存有 10 分钟,表现尤其明显。
	StoreFakeIP bool `json:"store_fakeip,omitempty"`
}

type SBClashAPI struct {
	ExternalController       string `json:"external_controller"`
	ExternalUI               string `json:"external_ui,omitempty"`
	ExternalUIDownloadURL    string `json:"external_ui_download_url,omitempty"`
	ExternalUIDownloadDetour string `json:"external_ui_download_detour,omitempty"`
	Secret                   string `json:"secret,omitempty"`
	DefaultMode              string `json:"default_mode,omitempty"`
}

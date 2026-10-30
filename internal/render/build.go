// Package render 把 panel 配置编译成 sing-box 内核配置。
//
// 所有内核版本细节都以实测为准(sing-box 1.14.4):
//
//   - clash_api 不是入站类型,而是 experimental.clash_api
//   - transient/legacy inbound 字段(sniff 等)已在 1.13 移除,改用 route 规则
//     的 action:"sniff" / "hijack-dns"
//   - remote rule-set 的 download_detour 在 1.14 弃用,改用 route.rule_set 的
//     detour 或默认路由
//   - 出站的 detour 指向节点组时,组里不能含该出站本身,否则
//     circular outbound dependency 直接 FATAL
package render

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"syscall"

	"boxhub/internal/config"
)

// FakeIPAnswerTTL 是 FakeIP 应答的 TTL(秒)。
//
// 为什么不用内核默认的 600 秒:
//
//	boxd 保存配置是"停内核 + 起内核"(sing-box 没有热重载),FakeIP 映射默认
//	只存内存,重启即清空。cache_file 已开 store_fakeip 让映射跨重启保留,
//	但只要映射还是丢了(cache.db 被删、内核被 OOM/强杀、换内核版本、改地址段),
//	客户端缓存里的假地址就会全部变成黑洞,内核日志刷 "missing fakeip record"。
//	TTL 600 秒意味着客户端要卡十分钟才肯重新解析——这正是"电脑断网十几分钟、
//	手机没事"的来源(Windows DNS 缓存严格按 TTL 存,手机缓存短)。
//
// 压到 60 秒后,即使映射全丢,最坏一分钟客户端就会重新解析拿到新映射,
// 把"十分钟级故障"降成"一分钟级抖动"。
const FakeIPAnswerTTL = 60

// Build 生成 sing-box 配置对象(未序列化,便于在测试里断言)。
func Build(cfg *config.Config, ctx Context) (*SBConfig, error) {
	r := config.NewResolver(cfg)
	// 一次性分配所有出站 tag(可读名称 + 重名区分),后续各阶段共用。
	tg := tagsOf(cfg)
	out := &SBConfig{
		Log: SBLog{Level: "info", Timestamp: true},
	}

	// ---------------- 入站 ----------------
	inbounds, err := buildInbounds(cfg, ctx)
	if err != nil {
		return nil, err
	}
	out.Inbounds = inbounds

	// ---------------- 出站 ----------------
	// 出站必须先于 DNS 构建:代理 DNS 的 detour 要指向一个**存在**的代理出站,
	// 否则内核会因为 detour 指向不存在/直连的出站而 FATAL。
	outbounds, err := buildOutbounds(cfg, r, tg)
	if err != nil {
		return nil, err
	}
	out.Outbounds = outbounds

	// ---------------- DNS ----------------
	dns, err := buildDNS(cfg, r, tg)
	if err != nil {
		return nil, err
	}
	out.DNS = dns

	// ---------------- 路由 ----------------
	route, err := buildRoute(cfg, r, ctx, tg)
	if err != nil {
		return nil, err
	}
	// DNS 规则引用到的规则集必须在 route.rule_set 里声明,否则内核启动失败
	for _, ref := range dns.ruleSetRefs {
		url, fmtType, ok := normalizeRuleSetURL(ref)
		if !ok {
			return nil, fmt.Errorf("DNS: 无法识别的规则集 %q", ref)
		}
		tag := ruleSetTag(ref)
		found := false
		for _, rs := range route.RuleSet {
			if rs.Tag == tag {
				found = true
				break
			}
		}
		if !found {
			// 与 addRuleSet 保持一致:有本地缓存就用本地文件,
			// 否则内核启动仍会为它联网下载(国内直连 github 不通 → FATAL)。
			if localPath, ok := ctx.RuleSetPaths[url]; ok && localPath != "" {
				route.RuleSet = append(route.RuleSet, SBRuleSet{
					Tag: tag, Type: "local", Format: fmtType, Path: localPath,
				})
				continue
			}
			route.RuleSet = append(route.RuleSet, SBRuleSet{
				Tag: tag, Type: "remote", Format: fmtType, URL: url,
				UpdateInterval: "24h", DownloadDetour: downloadDetour(cfg, r),
			})
		}
	}
	out.Route = route

	// ---------------- 实验特性 ----------------
	exp := &SBExperimental{}
	if cfg.Inbounds.ClashAPI {
		listen := "127.0.0.1"
		if cfg.Inbounds.AllowLAN {
			listen = "0.0.0.0"
		}
		exp.ClashAPI = &SBClashAPI{
			ExternalController: fmt.Sprintf("%s:%d", listen, portOrDefault(cfg.Inbounds.ClashAPIPort, 9090)),
			Secret:             cfg.Inbounds.ClashAPISecret,
			DefaultMode:        "rule",
		}
		if ctx.ClashUI != "" {
			exp.ClashAPI.ExternalUI = ctx.ClashUI
			exp.ClashAPI.ExternalUIDownloadURL = ""
		}
	}
	if ctx.CacheFile != "" {
		// StoreFakeIP:改配置会重启内核,FakeIP 映射必须跨重启保留,
		// 否则客户端缓存里的假地址全部变成黑洞(电脑端症状最重)。
		exp.CacheFile = &SBCacheFile{
			Enabled: true, Path: ctx.CacheFile,
			StoreRDRC: true, StoreFakeIP: true,
		}
	}
	if exp.ClashAPI != nil || exp.CacheFile != nil {
		out.Experimental = exp
	}
	return out, nil
}

// Context 编译期需要的外部信息(路径等),与配置分离以免污染导出文件。
type Context struct {
	// CacheFile 内核缓存路径,空表示不启用缓存。
	CacheFile string
	// ClashUI 内核面板(zashboard)静态目录,空表示不挂载。
	ClashUI string
	// RuleSetDir 规则集缓存目录。
	RuleSetDir string
	// RuleSetPaths 已落地的本地规则集:URL -> 本地文件路径。
	// 命中的规则集渲染成 type:local,内核启动不再联网下载。
	RuleSetPaths map[string]string
	// KernelRunning 表示渲染这一刻内核正在运行(即将被停掉换成新配置)。
	//
	// 它只影响 53 端口 DNS 入站的取舍:内核自己在跑的时候必然占着 53,
	// 这时候探测端口得到的 EADDRINUSE 是"自己占的",不能据此判定
	// "端口被别的程序占了"。详见 dnsInbound 的说明。
	KernelRunning bool
}

// ---------------------------------------------------------------------------
// 出站
// ---------------------------------------------------------------------------

func buildOutbounds(cfg *config.Config, r *config.Resolver, tg tags) ([]SBOutbound, error) {
	var (
		nodes  []SBOutbound // 普通节点,保持添加顺序
		chains []SBOutbound // 链式代理
		groups []SBOutbound // 节点组
		named  = map[string]bool{}
	)

	add := func(ob SBOutbound, kind string) error {
		if named[ob.Tag] {
			return fmt.Errorf("出站名重复: %s", ob.Tag)
		}
		named[ob.Tag] = true
		switch kind {
		case "node":
			nodes = append(nodes, ob)
		case "chain":
			chains = append(chains, ob)
		case "group":
			groups = append(groups, ob)
		}
		return nil
	}

	// 节点
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		ob, err := nodeOutbound(n, tg.Tag("node:"+n.ID))
		if err != nil {
			return nil, fmt.Errorf("节点「%s」: %w", n.Name, err)
		}
		if err := add(ob, "node"); err != nil {
			return nil, err
		}
	}

	// 链式代理:先算出每个动态组的成员,判断某个链式能不能进组
	dynMembers := dynamicMembers(cfg, r)

	// 筛选组先于链式代理生成:链式代理的前置可以指向筛选组,
	// detour 指向的出站必须已经确定存在(或即将存在)。
	filters, err := filterOutbounds(cfg, r, tg)
	if err != nil {
		return nil, err
	}
	for _, ob := range filters {
		if err := add(ob, "group"); err != nil {
			return nil, err
		}
	}

	// 链式代理出站
	for _, ch := range cfg.Chains {
		if ch.Disabled {
			continue
		}
		kind, fid := ch.FrontRef()
		// 兜底防线:前置组里若含这条链自己,detour 就是环,内核会 FATAL。
		// 正常路径由 ValidateAll 在保存时拦截;这里防的是手改/导入的配置
		// 绕过校验直接进编译的情况。筛选组的成员按构造只有节点,无需检查。
		if kind == "group" {
			if g := r.Group(fid); g != nil {
				for _, tid := range g.Members {
					if tid == ch.Tag() || tid == "chain:"+ch.ID {
						return nil, fmt.Errorf("链式代理「%s」: 前置组包含了它自身,内核会因循环依赖无法启动", ch.Name)
					}
				}
			}
		}
		var frontTag string
		if kind == "filter" {
			f := r.Filter(fid)
			if f == nil {
				return nil, fmt.Errorf("链式代理「%s」: 前置筛选组不存在", ch.Name)
			}
			if len(f.Matches(cfg.Nodes)) == 0 {
				return nil, fmt.Errorf("链式代理「%s」: 前置筛选「%s」还没有命中任何节点", ch.Name, f.Name)
			}
			frontTag = tg.Tag(f.Tag())
		} else {
			front := r.Group(fid)
			if front == nil {
				return nil, fmt.Errorf("链式代理「%s」: 前置组不存在", ch.Name)
			}
			if !hasMembers(*front, r) {
				return nil, fmt.Errorf("链式代理「%s」: 前置组「%s」还没有可用节点,请先往里加节点", ch.Name, front.Name)
			}
			frontTag = tg.Tag(front.Tag())
		}
		ob, err := chainOutbound(ch, frontTag, tg.Tag(ch.Tag()))
		if err != nil {
			return nil, fmt.Errorf("链式代理「%s」: %w", ch.Name, err)
		}
		if err := add(ob, "chain"); err != nil {
			return nil, err
		}
	}

	// 节点组:成员的解析顺序很关键 —— 组之间不互相引用(内核不支持嵌套组),
	// 所以组的成员只能是节点或链式代理。
	for _, g := range cfg.Groups {
		if g.Disabled {
			continue
		}
		ob, err := groupOutbound(g, r, dynMembers, tg)
		if err != nil {
			return nil, err
		}
		if ob == nil {
			continue // 空组不生成,避免内核因空出站报错
		}
		if err := add(*ob, "group"); err != nil {
			return nil, err
		}
	}

	// 所有出站的 server 字段都可能是域名(mesh 场景很常见)。1.12+ 要求
	// 每个出站显式声明 domain_resolver,否则内核在 1.14 直接 FATAL。
	directTag := firstTagAddr(cfg.DNS.DirectServers, "dns-direct")
	if directTag == "" {
		directTag = firstTagAddr(cfg.DNS.ProxyServers, "dns-proxy")
	}
	for i := range nodes {
		if nodes[i].DomainResolver == "" {
			nodes[i].DomainResolver = directTag
		}
	}
	for i := range chains {
		if chains[i].DomainResolver == "" {
			chains[i].DomainResolver = directTag
		}
	}

	out := make([]SBOutbound, 0, len(nodes)+len(chains)+len(groups)+len(cfg.Filters)+8)
	out = append(out, nodes...)
	out = append(out, chains...)
	out = append(out, groups...)
	out = append(out, SBOutbound{Type: "direct", Tag: DirectTag})

	// ---- Proxy 总出口组 + 规则组(对齐 SMBox 的结构)----
	//
	// SMBox 的做法:有一个叫 Proxy 的 selector,成员是"全部筛选组 + 全部节点组 + Direct"。
	// 每条"规则组"型规则(INS / Youtube / AI ...)又是一个 selector,
	// 第一个成员固定是 Proxy,后面是全部筛选组 + Direct。
	// 好处:用户既能整体切线路(选 Proxy),也能给单个类别单独指定线路。
	if extra, err := routingGroups(cfg, r, out, tg); err != nil {
		return nil, err
	} else {
		out = append(out, extra...)
	}

	// 再补一个标准 direct:内核与规则里到处引用它,保留最稳妥
	out = append(out, SBOutbound{Type: "direct", Tag: "direct"},
		SBOutbound{Type: "block", Tag: "block"})
	generatedOutbounds = make(map[string]bool, len(out))
	for _, ob := range out {
		generatedOutbounds[ob.Tag] = true
	}
	return out, nil
}

// ProxyTag 是总出口组的 tag。
const ProxyTag = "Proxy"

// proxyExists 判断是否会生成 Proxy 组(有至少一个可切换成员)。
func proxyExists(cfg *config.Config, r *config.Resolver) bool {
	n := 0
	for _, f := range cfg.Filters {
		if f.Enabled && !f.CountOnly && len(f.Matches(cfg.Nodes)) > 0 {
			n++
		}
	}
	for _, g := range cfg.Groups {
		if !g.Disabled && hasMembers(g, r) {
			n++
		}
	}
	return n > 0
}

// DirectTag 是 SMBox 用的直连出站名(带箭头,便于在内核面板里区分)。
const DirectTag = "➜ Direct"

// routingGroups 生成 Proxy 总出口组与各规则组。
func routingGroups(cfg *config.Config, r *config.Resolver, existing []SBOutbound, tg tags) ([]SBOutbound, error) {
	// 筛选组 tag 列表(只有启用的才进组)
	var filterTags []string
	for _, f := range cfg.Filters {
		if !f.Enabled || f.CountOnly {
			continue
		}
		if len(f.Matches(cfg.Nodes)) == 0 {
			continue
		}
		filterTags = append(filterTags, f.OutboundTag())
	}

	// 节点组 tag 列表
	var groupTags []string
	for _, g := range cfg.Groups {
		if g.Disabled {
			continue
		}
		if hasMembers(g, r) {
			groupTags = append(groupTags, tg.Tag(g.Tag()))
		}
	}

	// Proxy 的成员:节点组 + 筛选组 + 链式代理 + Direct(与 SMBox 一致,Direct 放最后)。
	// 链式代理必须进 Proxy:否则它只存在于内核自动的 GLOBAL 里,
	// 用户既没法在总出口选它,探测"临时切到它"也会因找不到可切换组而失败。
	var chainTags []string
	// 注意:不能用 outboundExists —— generatedOutbounds 要到本函数返回后
	// 才填充,这里读到的是上一次编译的残留。用传入的 existing 判断。
	inSet := map[string]bool{}
	for _, ob := range existing {
		inSet[ob.Tag] = true
	}
	for _, ch := range cfg.Chains {
		if !ch.Disabled && inSet[tg.Tag(ch.Tag())] {
			chainTags = append(chainTags, tg.Tag(ch.Tag()))
		}
	}
	proxyMembers := append(append(append(append([]string{}, groupTags...), filterTags...), chainTags...), DirectTag)
	// 没有可切换成员时,Proxy 里只放 Direct 会形成自引用,直接不生成
	var out []SBOutbound
	if len(proxyMembers) > 1 {
		out = append(out, SBOutbound{
			Type: "selector", Tag: ProxyTag, Outbounds: proxyMembers, Default: proxyMembers[0],
		})
	}

	// 规则组:每条 AsGroup 的规则生成一个同名 selector
	seen := map[string]bool{}
	for _, p := range cfg.Policies {
		if !p.Enabled || !p.AsGroup {
			continue
		}
		tag := strings.TrimSpace(p.Name)
		if tag == "" || seen[tag] || tag == ProxyTag {
			continue
		}
		seen[tag] = true
		members := append([]string{}, proxyMembers...)
		if len(proxyMembers) > 1 {
			// 第一个成员是 Proxy:默认跟随全局选择
			members = append([]string{ProxyTag}, proxyMembers...)
		}
		out = append(out, SBOutbound{
			Type: "selector", Tag: tag, Outbounds: members, Default: members[0],
		})
	}
	return out, nil
}

// dynamicMembers 计算动态组收编的节点 ID(不含链式代理,除非显式开启)。
func dynamicMembers(cfg *config.Config, r *config.Resolver) map[string][]string {
	res := map[string][]string{}
	for _, g := range cfg.Groups {
		if g.Type != config.GroupDynamic || g.Disabled {
			continue
		}
		var ids []string
		// 手工节点 + 订阅节点,按配置顺序;订阅节点的名称已带订阅前缀
		for _, n := range cfg.Nodes {
			if n.Disabled {
				continue
			}
			if keywordHit(n.Name, g) {
				ids = append(ids, "node:"+n.ID)
			}
		}
		if g.IncludeChain {
			for _, ch := range cfg.Chains {
				if ch.Disabled {
					continue
				}
				if keywordHit(ch.Name, g) {
					ids = append(ids, "chain:"+ch.ID)
				}
			}
		}
		res[g.ID] = ids
	}
	return res
}

func keywordHit(name string, g config.Group) bool {
	lower := strings.ToLower(name)
	if g.FilterRegex != "" {
		// 正则已在校验阶段编译验证过
		if !regexMatch(g.FilterRegex, name) {
			return false
		}
	}
	if len(g.Keywords) == 0 {
		return g.FilterRegex != ""
	}
	hits := 0
	for _, kw := range g.Keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) {
			hits++
		}
	}
	if g.KeywordMode == "and" {
		nonEmpty := 0
		for _, kw := range g.Keywords {
			if strings.TrimSpace(kw) != "" {
				nonEmpty++
			}
		}
		return nonEmpty > 0 && hits == nonEmpty
	}
	return hits > 0
}

func groupOutbound(g config.Group, r *config.Resolver, dyn map[string][]string, tg tags) (*SBOutbound, error) {
	var members []string
	if g.Type == config.GroupDynamic {
		for _, t := range dyn[g.ID] {
			members = append(members, tg.Tag(t))
		}
	} else {
		for _, m := range g.Members {
			if t := r.MemberTag(m); t != "" {
				// 内部引用(node:xx/chain:xx)转成分配好的出站名
				members = append(members, tg.Tag(t))
			}
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	ob := &SBOutbound{Tag: tg.Tag(g.Tag()), Outbounds: members}
	switch g.Type {
	case config.GroupSelect:
		ob.Type = "selector"
		ob.Default = members[0]
	case config.GroupURLTest:
		ob.Type = "urltest"
		ob.URL = urlOrDefault(g.URL, "https://www.gstatic.com/generate_204")
		ob.Interval = intervalOrDefault(g.Interval)
		if g.Tolerance > 0 {
			ob.Tolerance = g.Tolerance
		} else {
			ob.Tolerance = 50
		}
		if g.SuccessStatus > 0 {
			ob.URLTestOption = &SBURLTestOption{SuccessHTTPResponse: &SBSuccessStatus{StatusCode: g.SuccessStatus}}
		}
	case config.GroupDynamic:
		// 动态组的内核形态是自动择优:成员由关键词在编译期展开,新增订阅节点
		// 重启内核后自动进组。这里用 urltest 让它在成员间自动选最快的一条。
		ob.Type = "urltest"
		ob.URL = urlOrDefault(g.URL, "https://www.gstatic.com/generate_204")
		ob.Interval = intervalOrDefault(g.Interval)
		ob.Tolerance = 50
	case config.GroupFallback:
		ob.Type = "urltest"
		// 内核没有"主备"语义的组类型;备用顺序语义由面板侧选择实现
		// (选出第一个可用的),内核侧用 urltest 做兜底健康检查。
		ob.URL = urlOrDefault(g.URL, "https://www.gstatic.com/generate_204")
		ob.Interval = intervalOrDefault(g.Interval)
		ob.Tolerance = 1
	default:
		return nil, fmt.Errorf("节点组「%s」: 未知类型 %q", g.Name, g.Type)
	}
	return ob, nil
}

// chainOutbound 生成一条链式代理的内核出站。
//
// 关键点:出口出站(dialer)的 detour 指向前置组的 tag,内核在建立到出口的
// 连接之前会先经前置建立隧道,于是"先前置、再出口"。这条出站本身拥有独立的
// tag,可以像普通节点一样被节点组引用。
func chainOutbound(ch config.Chain, frontTag, selfTag string) (SBOutbound, error) {
	// 前置组里若含这条链自己,detour 就是环,内核会 FATAL。
	// 正常路径由 ValidateAll 在保存时拦截;这里防的是手改/导入的配置
	// 绕过校验直接进编译的情况。
	ob := SBOutbound{
		Tag:        selfTag,
		Server:     ch.Host,
		ServerPort: ch.Port,
		Detour:     frontTag,
	}
	switch ch.Type {
	case config.ChainSocks5:
		ob.Type = "socks"
		ob.Version = "5"
		ob.Username = ch.Username
		ob.Password = ch.Password
		ob.Network = chainNetwork(ch)
	case config.ChainHTTP, config.ChainHTTPS:
		ob.Type = "http"
		ob.Username = ch.Username
		ob.Password = ch.Password
		// HTTP 出站不支持 network 字段(实测 1.14.4 会 FATAL: unknown field "network")
		ob.Network = ""
		if ch.Type == config.ChainHTTPS {
			sni := ch.TLSServerName
			if sni == "" {
				sni = ch.Host
			}
			ob.TLS = &SBTLS{Enabled: true, ServerName: sni, Insecure: ch.Insecure}
		}
	default:
		return ob, fmt.Errorf("不支持的出口类型 %q", ch.Type)
	}
	return ob, nil
}

// chainNetwork 把"是否支持 UDP"翻译成内核的 network 字段。
// 只有 socks 出站支持该字段;HTTP 代理本身不支持 UDP,内核里也没有对应字段。
// 住宅代理大多不支持 UDP,关闭时显式限制为 tcp,避免 UDP 流量在链路上静默失败。
func chainNetwork(ch config.Chain) string {
	if ch.Network != "" {
		return ch.Network
	}
	if !ch.UDP {
		return "tcp"
	}
	return ""
}

// ---------------------------------------------------------------------------
// DNS
// ---------------------------------------------------------------------------

func buildDNS(cfg *config.Config, r *config.Resolver, tg tags) (*SBDNS, error) {
	d := cfg.DNS
	out := &SBDNS{Strategy: d.Strategy}

	// 直连 DNS(用于中国大陆域名与"直连不走内核"的真实解析)
	for i, addr := range d.DirectServers {
		s, err := dnsServer(fmt.Sprintf("dns-direct-%d", i+1), addr, "")
		if err != nil {
			return nil, err
		}
		out.Servers = append(out.Servers, s)
	}
	// 代理 DNS:经代理线路查询,避免污染。
	//
	// detour 的取值有讲究(实测):
	//   - 必须指向**真实存在的代理出站**,不能是 "direct"
	//     (内核会 FATAL: detour to an empty direct outbound makes no sense)
	//   - 指向空组同样会 FATAL(outbound detour not found)
	// 所以这里先取兜底出口,校验存在且不是 direct,否则退化为不指定 detour
	// (由内核按路由决定落地线路)。
	// 代理 DNS 的 detour 留空,由内核按路由决定落地线路。
	//
	// 为什么不写死走代理(实测结论):
	//   很多中转/WS 隧道型节点对 DNS 流量(UDP 53 / TCP 53 / DoH 443)支持很差,
	//   实测出现 "tls: handshake failure" 或 "unexpected EOF",
	//   结果是所有境外域名解析失败、站点全打不开。
	//   而这些节点访问普通 443 站点完全正常。
	//   留空后 DNS 查询按普通流量走路由,兼容性最好。
	//   (真正的污染问题由"域名直接交给节点解析"解决,见上面关于 resolve 的说明)
	proxyDetour := ""
	for i, addr := range d.ProxyServers {
		s, err := dnsServer(fmt.Sprintf("dns-proxy-%d", i+1), addr, proxyDetour)
		if err != nil {
			return nil, err
		}
		out.Servers = append(out.Servers, s)
	}

	if len(out.Servers) == 0 {
		return nil, fmt.Errorf("DNS: 至少需要配置一个上游服务器")
	}

	// 规则:中国大陆域名直连 DNS;man 模式(直连模式)全部直连 DNS
	if len(out.Servers) > 0 {
		out.Rules = append(out.Rules, SBDNSRule{ClashMode: "direct", Server: out.Servers[0].Tag})
		directTag := out.Servers[0].Tag
		for _, s := range out.Servers {
			if strings.HasPrefix(s.Tag, "dns-direct") {
				directTag = s.Tag
				break
			}
		}
		// 1.14 里 DNS 规则集用 rule_set 引用,不再有 geosite 内建字段
		out.Rules = append(out.Rules,
			SBDNSRule{RuleSet: []string{ruleSetTag("geosite:cn")}, Server: directTag},
		)
		// DNS 规则集需要单独声明,由调用方在 route 之外一并注册
		out.ruleSetRefs = append(out.ruleSetRefs, "geosite:cn")
	}

	// final:显式指定优先,否则第一个代理 DNS
	switch d.FinalServer {
	case "direct":
		out.Final = firstTag(out.Servers, "dns-direct")
	case "proxy", "":
		out.Final = firstTag(out.Servers, "dns-proxy")
	default:
		out.Final = firstTag(out.Servers, "dns-proxy")
	}
	if out.Final == "" {
		out.Final = out.Servers[0].Tag
	}

	// FakeIP:走代理的域名拿占位地址,直连域名走真实解析。
	// 1.14 的写法是"把 fakeip 当成一个 DNS 服务器",再由 query_type 规则把
	// A/AAAA 查询导向它。旧的 dns.fakeip 顶层字段已被移除。
	if d.FakeIP {
		out.Servers = append(out.Servers, SBDNSServer{
			Tag: "dns-fakeip", Type: "fakeip",
			Inet4Range: "198.18.0.0/15", Inet6Range: "fc00::/18",
		})
		// 规则顺序很重要:先让普通 DNS 查询走真实上游(否则连直连域名也会拿到假地址),
		// 只把发往代理出口的 A/AAAA 查询交给 fakeip。这里用最保守的组合:
		// 直连域名(中国大陆)始终真实解析,fakeip 只作为 A/AAAA 的兜底来源。
		rules := make([]SBDNSRule, 0, len(out.Rules)+2)
		for _, r := range out.Rules {
			rules = append(rules, r)
		}
		// 非 A/AAAA 查询(如 HTTPS/SVCB)一律走真实上游
		rules = append(rules, SBDNSRule{
			QueryType: []string{"A", "AAAA"}, Invert: true, Server: out.Final,
		})
		// A/AAAA:国内域名直连解析,其余交给 fakeip
		directTag := firstTag(out.Servers, "dns-direct")
		if directTag != "" {
			rules = append(rules, SBDNSRule{
				QueryType: []string{"A", "AAAA"},
				RuleSet:   []string{ruleSetTag("geosite:cn")},
				Server:    directTag,
			})
		}
		// 分流模式的例外设备:走与"默认"相反的解析路径。
		mods := dnsModeRules(d, directTag)
		rules = append(rules, mods...)
		if d.Mode == config.DNSModeDirect {
			// 默认直连:其余设备 A/AAAA 全部真实直连解析
			rules = append(rules, SBDNSRule{QueryType: []string{"A", "AAAA"}, Server: directTag})
		} else {
			rules = append(rules, SBDNSRule{
				QueryType: []string{"A", "AAAA"},
				Server:    "dns-fakeip",
				// 兜底 TTL:见 FakeIPAnswerTTL 的说明。
				RewriteTTL: FakeIPAnswerTTL,
			})
		}
		out.Rules = rules
	}
	return out, nil
}

// dnsModeRules 生成"DNS 分流模式"的来源 IP 规则。
//
// 语义(对齐 SMBox):
//   - 默认代理(proxy): 所有设备走 sing-box 解析(FakeIP);例外设备真实直连解析。
//   - 默认直连(direct): 所有设备真实直连解析;例外设备走 sing-box(FakeIP)。
//
// 注意这些规则只影响 A/AAAA;国内域名的直连解析规则在前面,天然优先于这里。
func dnsModeRules(d config.DNSSettings, directTag string) []SBDNSRule {
	if len(d.ModeExceptions) == 0 {
		return nil
	}
	var out []SBDNSRule
	base := SBDNSRule{QueryType: []string{"A", "AAAA"}, SourceIPCIDR: d.ModeExceptions}
	if d.Mode == config.DNSModeDirect {
		// 例外设备 → fakeip(其余由末尾的 direct 规则兜住)
		r := base
		r.Server = "dns-fakeip"
		r.RewriteTTL = 1
		out = append(out, r)
	} else {
		// 默认代理:例外设备 → 真实直连解析
		r := base
		r.Server = directTag
		out = append(out, r)
	}
	return out
}

// dnsServer 支持 udp / tcp / tls / https / h3 / quic 与纯 IP 写法。
func dnsServer(tag, addr, detour string) (SBDNSServer, error) {
	s := SBDNSServer{Tag: tag, Detour: detour}
	scheme := "udp"
	rest := addr
	if i := strings.Index(addr, "://"); i > 0 {
		scheme = strings.ToLower(addr[:i])
		rest = addr[i+3:]
	}
	// 去掉 path(https 的 /dns-query)
	if i := strings.Index(rest, "/"); i > 0 {
		rest = rest[:i]
	}
	host, port := splitHostPortDefault(rest, schemeDefaultPort(scheme))
	if host == "" {
		return s, fmt.Errorf("DNS 服务器地址无效: %q", addr)
	}
	switch scheme {
	case "udp":
		s.Type = "udp"
	case "tcp":
		s.Type = "tcp"
	case "tls":
		s.Type = "tls"
	case "https":
		s.Type = "https"
	case "h3", "quic":
		s.Type = "h3"
	default:
		return s, fmt.Errorf("不支持的 DNS 协议 %q", scheme)
	}
	s.Server = host
	if port != schemeDefaultPort(scheme) || scheme != "udp" && scheme != "tcp" {
		s.ServerPort = port
	}
	return s, nil
}

func schemeDefaultPort(scheme string) int {
	switch scheme {
	case "tls", "https", "h3", "quic":
		return 443
	case "tcp", "udp":
		return 53
	}
	return 53
}

// ---------------------------------------------------------------------------
// 入站
// ---------------------------------------------------------------------------

func buildInbounds(cfg *config.Config, ctx Context) ([]SBInbound, error) {
	var out []SBInbound
	if cfg.Inbounds.Mixed {
		listen := cfg.Inbounds.MixedListen
		if listen == "" {
			listen = "0.0.0.0"
		}
		out = append(out, SBInbound{
			Type: "mixed", Tag: "mixed-in", Listen: listen,
			ListenPort: portOrDefault(cfg.Inbounds.MixedPort, 7890),
		})
	}
	if cfg.Inbounds.TUN {
		stack := cfg.Inbounds.TUNStack
		if stack == "" {
			stack = "mixed"
		}
		addr := cfg.Inbounds.TunAddress
		if len(addr) == 0 {
			addr = defaultTunAddress
		} else if !hasAddrFamily(addr, ':') {
			// 用户自定义地址时补上 ULA v6:只有 IPv4 会让 strict_route
			// 注入 `ip -6 rule unreachable` 黑洞,v6 节点直接拨不出去
			addr = append(append([]string{}, addr...), defaultTunAddress[1])
		}
		out = append(out, SBInbound{
			Type: "tun", Tag: "tun-in", Address: addr,
			AutoRoute: cfg.Inbounds.TUNAutoRoute, Stack: stack,
			// strict_route 会拦截部分全局流量,只在 auto_route 时开启
			StrictRoute: cfg.Inbounds.TUNAutoRoute,
			MTU:         9000,
		})
	}
	if cfg.DNS.Hijack {
		if ib := dnsInbound(cfg.Inbounds.DNSListen, cfg.Inbounds.DNSPort, ctx.KernelRunning); ib != nil {
			out = append(out, *ib)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("入站: 至少需要开启一个(混合端口或 TUN)")
	}
	// 渲染完成的入站 tag 快照,供 inboundTags() 在 buildRoute 里引用,
	// 保证嗅探规则覆盖本次真正生成出来的全部入站。
	generatedInboundTags = generatedInboundTags[:0]
	for _, ib := range out {
		generatedInboundTags = append(generatedInboundTags, ib.Tag)
	}
	return out, nil
}

// defaultTunAddress 是 TUN 入站的默认地址段。
//
// 必须同时带 IPv6(ULA,fd00::/8 本地段,不会与公网冲突):
// strict_route 开启后,sing-box 发现 tun 没有 v6 地址会在
// `ip -6 rule` 里注入一条 `from all unreachable` 黑洞,
// 把包括**拨号 IPv6 节点本身**在内的所有 v6 出站全部丢掉,
// 表现为「IPv6 节点全部 network is unreachable」。
var defaultTunAddress = []string{"172.19.0.1/30", "fd12:3456:789a::1/64"}

// hasAddrFamily 判断地址列表里是否已含指定族(v4 用 '.',v6 用 ':')。
// IPv6 的 CIDR 前缀也写成 /64,所以用地址体里是否含冒号区分。
func hasAddrFamily(addr []string, sep byte) bool {
	for _, a := range addr {
		body := a
		if i := strings.IndexByte(a, '/'); i >= 0 {
			body = a[:i]
		}
		if sep == ':' && strings.ContainsRune(body, ':') {
			return true
		}
		if sep == '.' && strings.ContainsRune(body, '.') && !strings.ContainsRune(body, ':') {
			return true
		}
	}
	return false
}

// dnsInbound 构造 53 端口的 DNS 入站(direct inbound,查询交给 hijack-dns)。
// 网关模式下局域网设备把 DNS 指向本机,在这里进入内核按分流规则解析。
//
// 端口被占用(例如手工装过 dnsmasq)时返回 nil:让内核沿用现有入口,
// 而不是绑定失败导致整个内核起不来。权限不足等其它错误多半出现在
// 无 root 的测试环境,此时保留入站交给真实内核处理。
//
// kernelRunning 为真时跳过探测。boxd 改配置的顺序是"先渲染、再停旧内核、
// 再起新内核",渲染这一刻旧内核还活着并占着 53,探测必然 EADDRINUSE。
// 若据此删掉 dns-in,新内核会照常起来但没人监听 53,局域网 DNS 全部超时
// (电脑 ERR_NAME_NOT_RESOLVED,而手机不走网关 DNS 所以看着正常)。
// 旧内核在 Reload 里随后会被停掉、端口随之释放,所以这种情况必须生成入站。
func dnsInbound(enabled bool, port int, kernelRunning bool) *SBInbound {
	if !enabled {
		return nil
	}
	if port <= 0 {
		port = 53
	}
	if !kernelRunning {
		if pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port)); err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				return nil
			}
		} else {
			pc.Close()
		}
	}
	return &SBInbound{
		Type: "direct", Tag: "dns-in", Listen: "0.0.0.0", ListenPort: port,
	}
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

// outboundExists 检查 tag 是否在本次生成的出站集合里。
// 由 buildOutbounds 结束时写入的包级快照提供。
func outboundExists(tag string) bool {
	_, ok := generatedOutbounds[tag]
	return ok
}

var generatedOutbounds = map[string]bool{}

func buildRoute(cfg *config.Config, r *config.Resolver, ctx Context, tg tags) (*SBRoute, error) {
	// 直连 DNS 的 tag,规则集下载解析要用它
	directResTag := firstTagAddr(cfg.DNS.DirectServers, "dns-direct")
	if directResTag == "" {
		directResTag = firstTagAddr(cfg.DNS.ProxyServers, "dns-proxy")
	}
	directRes := firstTagAddr(cfg.DNS.DirectServers, "dns-direct")
	if directRes == "" {
		directRes = firstTagAddr(cfg.DNS.ProxyServers, "dns-proxy")
	}
	// 代理侧解析器:境外域名必须用它,避免污染
	proxyRes := firstTagAddr(cfg.DNS.ProxyServers, "dns-proxy")
	if proxyRes == "" {
		proxyRes = directRes
	}
	rt := &SBRoute{
		AutoDetectInterface: true,
		Final:               defaultProxyTarget(cfg, r),
		// 默认域名解析器用直连 DNS。
		//
		// 这里的取舍(踩过坑):
		//   - 规则集在启动时**直连下载**,必须能用直连 DNS 解析
		//     raw.githubusercontent.com;若这里指向代理 DNS,内核还没起来
		//     连不到代理,DNS-over-TLS 握手直接失败,内核起不来。
		//   - 境外域名的污染问题由 dns.final(指向代理 DNS)解决:
		//     查询走 dns.rules → final,不由这个字段决定。
		DefaultDomainResolver: directRes,
	}

	// 规则集收集
	rsByTag := map[string]SBRuleSet{}
	addRuleSet := func(raw string) string {
		tag := ruleSetTag(raw)
		if tag == "" {
			return ""
		}
		if _, ok := rsByTag[tag]; ok {
			return tag
		}
		url, fmtType, ok := normalizeRuleSetURL(raw)
		if !ok {
			return ""
		}
		// 本地已有缓存就用本地文件:内核启动不必联网,也就不会因为
		// 下载失败(国内直连 github 不通)FATAL 起不来。
		if localPath, ok := ctx.RuleSetPaths[url]; ok && localPath != "" {
			rsByTag[tag] = SBRuleSet{
				Tag: tag, Type: "local", Format: fmtType, Path: localPath,
			}
			return tag
		}
		rsByTag[tag] = SBRuleSet{
			Tag: tag, Type: "remote", Format: fmtType, URL: url,
			UpdateInterval: "24h",
			// 1.14 里该字段在 rule_set 上叫 download_detour(旧的 route 级写法已弃用);
			// 注意 HTTPClient 与 DownloadDetour 互斥,不能同时出现。
			DownloadDetour: downloadDetour(cfg, r),
		}
		return tag
	}

	// 内置规则集:中国大陆
	if cfg.DNS.Hijack {
		rt.Rules = append(rt.Rules, SBRule{Inbound: inboundTags(cfg), Action: "sniff"})
		rt.Rules = append(rt.Rules, SBRule{Protocol: []string{"dns"}, Action: "hijack-dns"})
	}

	// 伪 DNS/私网
	rt.Rules = append(rt.Rules, SBRule{
		IPIsPrivate: true, Action: "route", Outbound: "direct",
	})

	// 设备分流模式:按来源设备决定流量走不走代理(与 DNS 分流模式无关)。
	// 必须排在业务分流规则之前 —— 例外设备是硬覆盖,不该再被后面的
	// 域名/规则集规则改判,否则"设成直连的设备访问 youtube"还是会被拽进代理。
	// 返回的收尾规则要等业务规则全部追加完再放进去(回环的兜底必须最后)。
	deviceTrailing := applyDeviceRouting(rt, cfg, r)

	// 屏蔽 QUIC:走代理的 UDP 443 直接拒绝,浏览器自动退回 TCP
	if cfg.Inbounds.BlockQUIC {
		rt.Rules = append(rt.Rules, SBRule{
			Network: []string{"udp"}, Port: []int{443}, Action: "reject",
			// 直连站点的 QUIC 不受影响:先放行私网与直连站点
			RuleSet: []string{ruleSetTag("geosite:cn")},
			Invert:  true,
		})
	}

	for _, p := range cfg.Policies {
		if !p.Enabled {
			continue
		}
		// 「使用 Sing-box JSON 规则」逃生口:原样使用用户提供的规则 JSON。
		if strings.TrimSpace(p.RawJSON) != "" {
			var extra []SBRule
			if err := json.Unmarshal([]byte(p.RawJSON), &extra); err != nil {
				return nil, fmt.Errorf("分流规则「%s」的自定义 JSON 无效: %w", p.Name, err)
			}
			if len(extra) == 0 {
				return nil, fmt.Errorf("分流规则「%s」的自定义 JSON 不能是空数组", p.Name)
			}
			rt.Rules = append(rt.Rules, extra...)
			continue
		}
		// 条件组优先(SMBox 同款多组编辑器);旧配置退回扁平 Match。
		outbound, err := policyOutbound(p, r, tg)
		if err != nil {
			return nil, fmt.Errorf("分流规则「%s」: %w", p.Name, err)
		}
		var rules []SBRule
		if len(p.Groups) > 0 {
			rules, err = policyRulesFromGroups(p, p.Groups, outbound, addRuleSet)
			if err != nil {
				return nil, fmt.Errorf("分流规则「%s」: %w", p.Name, err)
			}
		} else {
			rule, err := policyRule(p, addRuleSet, r, tg)
			if err != nil {
				return nil, fmt.Errorf("分流规则「%s」: %w", p.Name, err)
			}
			if rule != nil {
				rules = []SBRule{*rule}
			}
		}
		// 仅手机:用来源 IP 限定设备(近似 SMBox 的设备识别)。
		if p.PhoneOnly && len(p.PhoneCIDRs) > 0 {
			for i := range rules {
				rules[i].ClientIP = append(rules[i].ClientIP, p.PhoneCIDRs...)
			}
		}
		rt.Rules = append(rt.Rules, rules...)
	}

	// 规则集落地
	tags := make([]string, 0, len(rsByTag))
	for t := range rsByTag {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		rt.RuleSet = append(rt.RuleSet, rsByTag[t])
	}

	// 这里**刻意不加 `action: resolve`**。
	//
	// 原因(实测踩坑,很关键):
	//   resolve 会在路由阶段强制真实解析域名。当代理 DNS 必须经代理查询时
	//   (境外域名必须如此,否则被污染),内核要先建代理连接才能查 DNS;
	//   而很多中转/WS 隧道节点对 DNS 端口(UDP 53 / TCP 53 / DoH 443)支持很差,
	//   实测会 TLS 握手失败或 EOF → 域名解析失败 → 该站点直接打不开。
	//   更糟的是内核自己对同域名测速(IP 直连)反而是通的,极难定位。
	//
	// 不加 resolve 时,域名保持为域名一路传到出站,由**代理节点侧解析**:
	//   - 不受本地 DNS 污染影响;
	//   - 不需要内核在代理可用前先查 DNS;
	//   - 直连流量(域名规则已拦截)也不受影响。
	// 代价:仅靠 IP 段/geoip 匹配的规则对"只有域名"的连接不生效——这是可接受的,
	// 因为默认配置里域名类规则已经用 domain_suffix / geosite 覆盖了主要场景。

	// 设备分流模式里"必须排在最后"的规则(目前只有回环兜底)。
	rt.Rules = append(rt.Rules, deviceTrailing...)

	// 兜底出口必须存在。这里再核一遍:final 指向的出站必须在 outbounds 里生成过,
	// 否则内核 run 阶段会 FATAL(default outbound not found)。
	if rt.Final == "" || !outboundExists(rt.Final) {
		rt.Final = "direct"
	}
	// 规则里引用的出站同样要核,引用不存在的一律退回 direct 并记录警告
	for i := range rt.Rules {
		r := &rt.Rules[i]
		if r.Outbound != "" && r.Action == "route" && !outboundExists(r.Outbound) {
			r.Outbound = "direct"
		}
	}
	// 规则集引用了不存在的 tag 会让 run 阶段报错
	for _, ru := range rt.Rules {
		for _, tag := range ru.RuleSet {
			found := false
			for _, rs := range rt.RuleSet {
				if rs.Tag == tag {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("分流规则引用了未声明的规则集 %q", tag)
			}
		}
	}
	return rt, nil
}

func policyRule(p config.Policy, addRuleSet func(string) string, r *config.Resolver, tg tags) (*SBRule, error) {
	m := p.Match
	rule := &SBRule{Invert: p.Invert}
	rule.Domain = m.Domain
	rule.DomainSuffix = m.DomainSuffix
	rule.DomainKeyword = m.DomainKeyword
	rule.DomainRegex = m.DomainRegex
	rule.IPCIDR = m.IPCIDR
	rule.Port = m.Port
	rule.PortRange = m.PortRange
	rule.Network = m.Network
	rule.ProcessName = m.ProcessName
	rule.PackageName = m.PackageName
	rule.ClientIP = m.ClientIP
	rule.Protocol = m.Protocol
	rule.Inbound = m.Inbound
	if len(m.ClashMode) > 0 {
		rule.ClashMode = m.ClashMode[0]
	}
	for _, rs := range m.RuleSets {
		if t := addRuleSet(rs); t != "" {
			rule.RuleSet = append(rule.RuleSet, t)
		}
	}
	// GeoIP / GeoSite 通过本地规则集实现(不依赖内核内置 geo 数据库下载)
	for _, g := range m.GeoIP {
		if t := addRuleSet("geoip:" + g); t != "" {
			rule.RuleSet = append(rule.RuleSet, t)
		}
	}
	for _, g := range m.GeoSite {
		if t := addRuleSet("geosite:" + g); t != "" {
			rule.RuleSet = append(rule.RuleSet, t)
		}
	}

	// 出口
	switch {
	case p.Target == config.SelfTarget:
		// 由下方 AsGroup 分支决定实际出站
		rule.Action = "route"
	case p.Target == "block":
		rule.Action = "reject"
	case p.Target == "direct":
		rule.Action = "route"
		rule.Outbound = "direct"
	case strings.HasPrefix(p.Target, "group:"):
		rule.Action = "route"
		gid := strings.TrimPrefix(p.Target, "group:")
		if g := r.Group(gid); g != nil {
			// 配置里用 group:<id> 引用,内核里用组名
			rule.Outbound = g.OutboundTag()
		} else {
			rule.Outbound = p.Target
		}
	case strings.HasPrefix(p.Target, "chain:"):
		rule.Action = "route"
		rule.Outbound = tg.Tag(p.Target)
	case strings.HasPrefix(p.Target, "filter:"):
		rule.Action = "route"
		fid := strings.TrimPrefix(p.Target, "filter:")
		if f := r.Filter(fid); f != nil {
			// 配置里用 filter:<id> 引用,内核里用筛选的名称作为出站 tag
			rule.Outbound = f.OutboundTag()
		} else {
			rule.Outbound = p.Target
		}
	case strings.HasPrefix(p.Target, "node:"):
		rule.Action = "route"
		rule.Outbound = tg.Tag(p.Target)
	default:
		return nil, fmt.Errorf("未知出口 %q", p.Target)
	}

	// 标记为"选线组"的规则:规则会路由到自己那个同名组,
	// 这样在内核面板里可以给这类站点单独选线路。
	// 注意 group tag 就是规则名,与 routingGroups 里生成的 tag 对应。
	// 出口为 self(或标记了选线组)时,规则路由到自己那个同名组。
	if p.AsGroup && strings.TrimSpace(p.Name) != "" {
		rule.Action = "route"
		rule.Outbound = strings.TrimSpace(p.Name)
	}
	return rule, nil
}

// DefaultProxyTarget 是 defaultProxyTarget 的导出版本,供面板判断
// "某条链式代理是否是当前兜底出口"。
func DefaultProxyTarget(cfg *config.Config, r *config.Resolver) string {
	return defaultProxyTarget(cfg, r)
}

// defaultProxyTarget 决定兜底走哪:
//  1. 名为"代理"的节点组
//  2. 第一个启用的链式代理
//  3. 第一个启用的非动态节点组
//  4. 第一个启用的节点
//  5. direct
//
// applyDeviceRouting 按「设备分流模式」改写最终出口与来源 IP 规则。
//
// 语义:
//   - 默认代理:所有设备走代理;ModeExceptions 里的设备走直连。
//   - 默认直连:所有设备走直连;ModeExceptions 里的设备走代理。
//
// 直连模式下"其余设备走直连"用 invert 表达:sing-box 没有"来源不在列表里"
// 的直接写法,但可以对整条规则取反,效果就是"除例外设备以外的所有来源"。
// 不这么做的话,后面按域名匹配的分流规则会把它们重新送进代理。
// 返回值是"要追加到所有业务规则之后"的收尾规则。
func applyDeviceRouting(rt *SBRoute, cfg *config.Config, r *config.Resolver) []SBRule {
	ex := normalizeSourceCIDRs(cfg.Routing.ModeExceptions)
	proxyTag := defaultProxyTarget(cfg, r)
	if cfg.Routing.Mode == config.RouteModeDirect {
		rt.Final = "direct"
		// 非例外设备 → 直连。回环要排除,否则内核自己的测速/诊断会被
		// 当成"非例外设备"掐成直连。
		guard := normalizeSourceCIDRs(append(append([]string{}, ex...), loopbackCIDRs...))
		rt.Rules = append(rt.Rules, SBRule{
			ClientIP: guard, Invert: true, Action: "route", Outbound: "direct",
		})
		// 例外设备 + 回环的兜底,必须排在所有业务规则**之后**。
		//
		// 早期版本把"例外设备 → 代理"放在最前面,导致例外设备的所有流量
		// 都被硬钉进代理组,INS/Youtube/Google 这些按域名匹配的规则全部失效
		// (实测:例外设备访问 gateway.instagram.com 命中的是
		//  source_ip_cidr=[...] => route(Proxy),而不是 INS)。
		//
		// 正确语义是"按规则分流,没命中时兜底走代理":
		//   国内 → 前面的 geoipcn/geositecn 直连
		//   境外 → 前面的 INS/Youtube/... 各自的分组
		//   其余 → 落到这里走 Proxy
		// 回环也放这条:直连模式 final=direct,不放的话内核测不到境外站点。
		fallback := normalizeSourceCIDRs(append(append([]string{}, ex...), loopbackCIDRs...))
		return []SBRule{{
			ClientIP: fallback, Action: "route", Outbound: proxyTag,
		}}
	}
	if len(ex) == 0 {
		return nil
	}
	// 默认代理:例外设备兜底走直连,同样放到业务规则之后,
	// 让国内/境外的正常分流逻辑优先。
	return []SBRule{{
		ClientIP: ex, Action: "route", Outbound: "direct",
	}}
}

// loopbackCIDRs 内核自身流量的来源地址(回环)。它们不是局域网设备,
// 不该被「设备分流模式」当成普通客户端处理。
var loopbackCIDRs = []string{"127.0.0.1/32", "::1/128"}

// normalizeSourceCIDRs 去空白、去重,并丢弃空行。
func normalizeSourceCIDRs(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func defaultProxyTarget(cfg *config.Config, r *config.Resolver) string {
	// 1. 有 Proxy 总出口组时兜底走它(与 SMBox 一致)。
	//    这是最符合直觉的:用户在"Proxy"里选什么,兜底就是什么。
	if proxyExists(cfg, r) {
		return ProxyTag
	}
	// 2. 已存在的节点组
	for _, g := range cfg.Groups {
		if !g.Disabled && g.Name == "代理" && hasMembers(g, r) {
			return g.Tag()
		}
	}
	for _, ch := range cfg.Chains {
		if !ch.Disabled {
			return ch.Tag()
		}
	}
	for _, g := range cfg.Groups {
		if g.Disabled || g.Type == config.GroupDynamic {
			continue
		}
		if hasMembers(g, r) {
			return g.Tag()
		}
	}
	for _, n := range cfg.Nodes {
		if !n.Disabled {
			return "node:" + n.ID
		}
	}
	if len(cfg.Chains) > 0 {
		return cfg.Chains[0].Tag()
	}
	if len(cfg.Groups) > 0 {
		return cfg.Groups[0].Tag()
	}
	return "direct"
}

// hasMembers 判断这个组在生成的内核配置里**是否真的会有出站**。
//
// 这一点很重要:空组不会生成出站,而 route.final 和规则里引用了不存在的出站
// 时,`sing-box check` 能通过、`sing-box run` 却会 FATAL
// (实测报错:default outbound not found: group:xxx)。
// 所以任何"是否可用"的判断都必须和真正生成的出站一一对应。
func hasMembers(g config.Group, r *config.Resolver) bool {
	if g.Disabled {
		return false
	}
	if g.Type == config.GroupDynamic {
		// 动态组的成员在编译期按关键词展开;只有关键词非空才可能收到节点
		return strings.TrimSpace(strings.Join(g.Keywords, "")) != "" || strings.TrimSpace(g.FilterRegex) != ""
	}
	return len(r.GroupMemberTargets(&g)) > 0
}

func inboundTags(cfg *config.Config) []string {
	// buildInbounds 已经渲染过:以实际生成结果为准(含 dns-in,
	// 不含端口被占用时跳过的入站)。
	if generatedInboundTags != nil {
		return generatedInboundTags
	}
	var out []string
	if cfg.Inbounds.Mixed {
		out = append(out, "mixed-in")
	}
	if cfg.Inbounds.TUN {
		out = append(out, "tun-in")
	}
	return out
}

// generatedInboundTags 最近一次 buildInbounds 渲染出的入站 tag。
// Build 串行执行(入站先于路由),包级快照与 generatedOutbounds 同一模式。
var generatedInboundTags []string

// downloadDetour 决定规则集下载走哪个出口。
//
// 原实现固定直连。但国内网络直连 github.com 基本不通(实测 8 秒超时),
// 只要有一个规则集没被 cache.db 缓存过,sing-box 启动时就会 FATAL
// (start router: initialize rule-set ... i/o timeout),内核直接起不来 ——
// 表现为整机断网,而且从面板上看只是"服务 active 但内核没跑"。
//
// 有代理出口时改走代理:出站与 DNS 都在规则集之前初始化,不会形成循环依赖。
// 没有可用代理(纯直连部署)时保持直连,行为与以前一致。
func downloadDetour(cfg *config.Config, r *config.Resolver) string {
	t := defaultProxyTarget(cfg, r)
	if t != "" && t != "direct" && outboundExists(t) {
		return t
	}
	return "direct"
}

// ruleSetTag 生成稳定、唯一、合法的规则集 tag。
//
// 唯一性是硬要求:tag 是规则引用规则集的唯一凭据。只截取 URL 前缀会撞车——
// MetaCubeX 的规则集全部共享
// https://github.com/MetaCubeX/meta-rules-dat/raw/refs/heads/sing/geo/ 前缀,
// 截断后 instagram.srs / youtube.srs / geolocation-!cn.srs 会得到同一个 tag,
// 于是所有规则都指向最先出现的那一个(实测指向 apple@cn.json),
// 表现为"INS 规则明明配了 instagram,流量却走了兜底 Proxy"。
//
// 所以:可读前缀(便于排障) + URL 的 SHA-256 前 4 字节,保证不同 URL 不撞车。
func ruleSetTag(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, c := range raw {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == '-' || c == '_' || c == '.':
			b.WriteRune('-')
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[:32]
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%s-%x", s, sum[:4])
}

// normalizeRuleSetURL 把三种写法统一成可下载地址:
//
//	"srs:https://..."      → sing-box 二进制规则集
//	"https://...srs"       → 二进制
//	"https://...json"      → 源格式
//	"geoip:cn"             → sing-geoip 官方仓库
//	"geosite:cn"           → sing-geosite 官方仓库
func normalizeRuleSetURL(raw string) (url, format string, ok bool) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return "", "", false
	case strings.HasPrefix(raw, "geoip:"):
		code := strings.TrimPrefix(raw, "geoip:")
		return "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-" + code + ".srs", "binary", true
	case strings.HasPrefix(raw, "geosite:"):
		code := strings.TrimPrefix(raw, "geosite:")
		return "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-" + code + ".srs", "binary", true
	case strings.HasPrefix(raw, "srs:"):
		return strings.TrimPrefix(raw, "srs:"), "binary", true
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		if strings.HasSuffix(strings.ToLower(raw), ".json") {
			return raw, "source", true
		}
		return raw, "binary", true
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func portOrDefault(p, def int) int {
	if p <= 0 || p > 65535 {
		return def
	}
	return p
}

func urlOrDefault(u, def string) string {
	if strings.TrimSpace(u) == "" {
		return def
	}
	return u
}

func intervalOrDefault(min int) string {
	if min <= 0 {
		min = 5
	}
	return fmt.Sprintf("%dm", min)
}

// firstTagAddr 返回按 prefix 命名的 DNS 服务器 tag(dnsServer 生成 tag 的规则
// 固定为 "<prefix>-<序号>",这里只取第一个)。
func firstTagAddr(addrs []string, prefix string) string {
	if len(addrs) == 0 {
		return ""
	}
	return prefix + "-1"
}

func firstTag(servers []SBDNSServer, prefix string) string {
	for _, s := range servers {
		if strings.HasPrefix(s.Tag, prefix) {
			return s.Tag
		}
	}
	return ""
}

func splitHostPortDefault(hostport string, def int) (string, int) {
	if i := strings.LastIndex(hostport, ":"); i > 0 {
		// IPv6 字面量 [::1]:53
		if strings.HasPrefix(hostport, "[") {
			if j := strings.Index(hostport, "]"); j > 0 {
				host := hostport[1:j]
				if k := strings.LastIndex(hostport, ":"); k > j {
					return host, atoiDefault(hostport[k+1:], def)
				}
				return host, def
			}
		}
		if !strings.Contains(hostport[i+1:], ":") {
			return hostport[:i], atoiDefault(hostport[i+1:], def)
		}
	}
	return hostport, def
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 || n > 65535 {
		return def
	}
	return n
}

// Marshal 序列化内核配置,缩进便于排查。
func (c *SBConfig) Marshal() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }

// filterOutbounds 把筛选编译成出站。
//
// 每个筛选生成一个 urltest 组:成员是该筛选命中的全部节点。
// 这样订阅更新后节点自动进出,用户不需要手工维护组。
// 没有命中任何节点的筛选不生成出站(空出站会让内核 FATAL)。
func filterOutbounds(cfg *config.Config, r *config.Resolver, tg tags) ([]SBOutbound, error) {
	var out []SBOutbound
	for _, f := range cfg.Filters {
		if !f.Enabled || f.CountOnly {
			continue
		}
		matched := f.Matches(cfg.Nodes)
		if len(matched) == 0 {
			continue
		}
		members := make([]string, 0, len(matched))
		for _, n := range matched {
			members = append(members, tg.Tag("node:"+n.ID))
		}
		ob := SBOutbound{Tag: tg.Tag(f.Tag()), Outbounds: members}
		if f.GroupType() == "urltest" {
			ob.Type = "urltest"
			ob.URL = "https://www.gstatic.com/generate_204"
			ob.Interval = "5m"
			ob.Tolerance = 50
		} else {
			ob.Type = "selector"
			// 默认选第一个命中节点,用户可在面板/内核面板里改
			ob.Default = members[0]
		}
		out = append(out, ob)
	}
	return out, nil
}

// policyOutbound 解析规则出口为内核出站名(引用格式转名称)。
func policyOutbound(p config.Policy, r *config.Resolver, tg tags) (string, error) {
	switch {
	case p.Target == "direct":
		return "direct", nil
	case p.Target == "block":
		return "block", nil
	case p.Target == config.SelfTarget:
		if p.AsGroup {
			return strings.TrimSpace(p.Name), nil
		}
		return "", fmt.Errorf("出口为 self 时必须开启选线组")
	case strings.HasPrefix(p.Target, "group:"):
		return tg.Tag(p.Target), nil
	case strings.HasPrefix(p.Target, "filter:"):
		return tg.Tag(p.Target), nil
	case strings.HasPrefix(p.Target, "chain:"), strings.HasPrefix(p.Target, "node:"):
		return tg.Tag(p.Target), nil
	}
	return "", fmt.Errorf("未知出口 %q", p.Target)
}

package api

// 链式代理探测。
//
// 探测一条链式代理时,必须让流量真的走「前置 → 出口」这条路径。做法:
//  1. 找到这条链式代理所在的、可在 Clash API 里切换的组;
//  2. 临时把该组切到这条链式代理,经**本地混合入口**发起请求(内核按当前选择转发);
//  3. 拿到延时与出口 IP(经 ip 回显接口),再切回原来的选择。
//
// 这样测出来的就是用户真实使用时的路径,而不是"出口本身通不通"。

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/render"
	"boxhub/internal/probe"
)

// probeChain 探测一条链式代理。
func (s *Server) probeChain(ctx context.Context, chainID string) config.ProbeResult {
	cfg := s.store.Current()
	var ch *config.Chain
	for i := range cfg.Chains {
		if cfg.Chains[i].ID == chainID {
			ch = &cfg.Chains[i]
			break
		}
	}
	if ch == nil {
		return config.ProbeResult{At: time.Now(), DelayMs: -1, Error: "链式代理不存在"}
	}
	opts := probe.DefaultOptions()
	opts.Timeout = 15 * time.Second
	if ch.Type == config.ChainHTTPS {
		// https 出口的证书问题不应该让探测显示"不通",交由用户决定是否忽略
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout+3*time.Second)
	defer cancel()

	cl := s.clash()
	// Clash API 只认内核里的出站名(即可读节点名),内部引用 chain:<id> 会 404
	tag := render.OutboundTagFor(cfg, ch.Tag())

	// 探测原则(两条,都是踩坑换来的):
	//
	// 1. **必须走完整链路**。住宅/静态 IP 代理常是动态出口池:
	//    经前置连入分配一个 IP,直连出口(不经前置)会拿到完全不同的 IP。
	//    实测同一代理:经前置 = 123.x(国内),直连出口 = 159.28.x(日本)。
	//    退化到"直接连出口"会给出与用户实际使用完全不符的结果。
	//
	// 2. **尽量不改动用户当前选择**。临时切组会让用户正在走的流量改道,
	//    而且如果链式代理同时在多个组里,可能切错组导致结果对不上。
	//    所以优先利用"它本来就是兜底出口"这一常见情况,直接经本地入口请求。
	res := probe.Result{}
	if s.isDefaultOutbound(ch) {
		// 兜底出口:一次请求同时拿到延时和出口 IP
		localAddr := s.localProxyAddr()
		if localAddr != "" {
			d := &probe.ChainDialer{ProxyAddr: localAddr}
			pr := probe.Run(ctx, d, opts)
			res = probe.Result{
				Delay: pr.Delay, ExitIP: pr.ExitIP, Country: pr.Country,
				Region: pr.Region, ISP: pr.ISP, Error: pr.Error,
				ExitByProto: pr.ExitByProto, PoolNote: pr.PoolNote,
			}
			if res.Delay <= 0 {
				res.Delay = -1
			}
			// 已经拿到完整结果,直接落库
			return s.saveProbe(ch, res)
		}
	}
	delayRes := cl.Delay(ctx, tag, opts.TestURL, opts.Timeout)
	res = probe.Result{Delay: delayRes.Delay}
	if delayRes.Error != "" || delayRes.Delay <= 0 {
		res.Delay = -1
		res.Error = delayRes.Error
		if res.Error == "" {
			res.Error = "测速失败"
		}
	} else {
		// 延时拿到了,再取出口 IP 归属。这一步必须经完整链路发起,
		// 所以继续用本地入口(此时不需要切换组:链式代理已经是兜底出口时可以直连;
		// 不是兜底出口时,临时切到它测完立刻切回,并加锁避免并发干扰)。
		res = s.probeExitIP(ctx, cl, ch, res, opts)
	}

	return s.saveProbe(ch, res)
}

// saveProbe 把探测结果写入内存缓存与配置(便于下次打开面板直接看到)。
func (s *Server) saveProbe(ch *config.Chain, res probe.Result) config.ProbeResult {
	out := config.ProbeResult{
		At: time.Now(), DelayMs: res.Delay, ExitIP: res.ExitIP,
		Country: res.Country, Region: res.Region, ISP: res.ISP, Error: res.Error,
		ExitByProto: res.ExitByProto, PoolNote: res.PoolNote,
	}
	if out.DelayMs == 0 {
		out.DelayMs = -1
	}
	s.mu.Lock()
	if s.probes == nil {
		s.probes = map[string]config.ProbeResult{}
	}
	s.probes[ch.ID] = out
	s.mu.Unlock()

	// 结果写回配置。探测结果属于"附加信息",写盘失败不该影响这次请求。
	_, _ = s.apply(func(c *config.Config) error {
		for i := range c.Chains {
			if c.Chains[i].ID == ch.ID {
				p := out
				c.Chains[i].Probe = &p
			}
		}
		return nil
	})
	return out
}

// findSwitchableGroup 找到包含该链式代理、且当前有选中项的选择组。
func (s *Server) findSwitchableGroup(ctx context.Context, cl *ClashClient, tag string) (group, prev string) {
	proxies, err := cl.Proxies(ctx)
	if err != nil {
		return "", ""
	}
	// map 遍历顺序随机,必须按类型定优先级,否则可能选中 GLOBAL
	// (内核自动组,对它发 PUT 会 Resource not found,探测就永远失败)。
	// 优先级:Proxy 总出口 > 普通 Selector > URLTest > Fallback;GLOBAL 排除。
	type cand struct {
		name string
		now  string
		rank int
	}
	var best *cand
	for name, info := range proxies {
		if name == "GLOBAL" {
			continue
		}
		rank := -1
		switch info.Type {
		case "Selector":
			rank = 1
		case "URLTest":
			rank = 2
		case "Fallback", "LoadBalance":
			rank = 3
		}
		if rank < 0 {
			continue
		}
		if name == "Proxy" {
			rank = 0 // 总出口最优先
		}
		found := false
		for _, m := range info.All {
			if m == tag {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if best == nil || rank < best.rank {
			best = &cand{name, info.Now, rank}
		}
	}
	if best == nil {
		return "", ""
	}
	return best.name, best.now
}

// localProxyAddr 返回本地混合入站地址(探测需要在本机发起流量)。
func (s *Server) localProxyAddr() string {
	cfg := s.store.Current()
	if cfg == nil || !cfg.Inbounds.Mixed {
		return ""
	}
	listen := cfg.Inbounds.MixedListen
	if listen == "" || listen == "0.0.0.0" || listen == "::" {
		listen = "127.0.0.1"
	}
	port := cfg.Inbounds.MixedPort
	if port <= 0 {
		port = 7890
	}
	return net.JoinHostPort(listen, strconv.Itoa(port))
}

// 避免未使用告警
var _ = fmt.Sprint
var _ = strings.TrimSpace

// probeExitIP 在已经拿到延时之后,补测出口 IP 与归属。
//
// 出口 IP 必须经"前置 → 出口"真实发起请求才能拿到。
//
// 关键(踩过的坑):住宅/静态 IP 代理常常是**动态出口池**——
// 从前置连入时分配一个 IP,直连出口(不经过前置)时会分配到完全不同的出口。
// 实测:同一住宅代理,经前置出口是 123.x(国内),直连出口是 159.28.x(日本)。
// 所以探测**绝不能退化到"直接连出口"**,否则测出来的 IP 和延时都不是用户实际使用的链路。
//
// 因此这里只有一个原则:必须让流量走完整的"前置 → 出口"。
// 由于链式代理本身就是一个出站,探测可以直接针对该出站发起
// (Clash API 的 delay 接口内部就会走完整链路),出口 IP 则经它转发拿到。
func (s *Server) probeExitIP(ctx context.Context, cl *ClashClient, ch *config.Chain, res probe.Result, opts probe.Options) probe.Result {
	localAddr := s.localProxyAddr()
	if localAddr == "" {
		res.Error = "未开启混合入站,无法获取出口 IP(延时测速结果仅供参考)"
		return res
	}
	// Clash API 与路由匹配都认"内核里的出站名",不是内部引用 chain:<id>
	tag := render.OutboundTagFor(s.store.Current(), ch.Tag())

	// 情况一:该链式代理已经是路由兜底出口 —— 本地入口进来的流量天然走它,
	// 直接请求即可,不碰任何组的选择。
	if s.isDefaultOutbound(ch) {
		d := &probe.ChainDialer{ProxyAddr: localAddr}
		ipRes := probe.Run(ctx, d, opts)
		if ipRes.ExitIP != "" {
			res.ExitIP, res.Country, res.Region, res.ISP = ipRes.ExitIP, ipRes.Country, ipRes.Region, ipRes.ISP
			res.ExitByProto, res.PoolNote = ipRes.ExitByProto, ipRes.PoolNote
		} else if res.Error == "" {
			res.Error = ipRes.Error
		}
		return res
	}

	// 情况二:它不是兜底出口。找一个包含它的可切换组临时切过去,
	// 测完立刻切回,并串行化避免并发探测互相干扰。
	group, prev := s.findSwitchableGroup(ctx, cl, tag)
	needSwitch := group != "" && prev != tag

	if needSwitch {
		s.probeMu.Lock()
		defer s.probeMu.Unlock()
		if err := cl.Select(ctx, group, tag); err != nil {
			res.Error = "无法临时切换到该链式代理,跳过出口 IP 查询: " + err.Error()
			return res
		}
		defer func() {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = cl.Select(bg, group, prev)
		}()
	}

	d := &probe.ChainDialer{ProxyAddr: localAddr}
	ipRes := probe.Run(ctx, d, opts)
	if ipRes.ExitIP != "" {
		res.ExitIP = ipRes.ExitIP
		res.Country = ipRes.Country
		res.Region = ipRes.Region
		res.ISP = ipRes.ISP
	} else if res.Error == "" && ipRes.Error != "" {
		res.Error = ipRes.Error
	}
	return res
}

// isDefaultOutbound 判断这条链式代理是否就是当前路由的兜底出口。
func (s *Server) isDefaultOutbound(ch *config.Chain) bool {
	cfg := s.store.Current()
	if cfg == nil {
		return false
	}
	r := config.NewResolver(cfg)
	// 兜底出口引用先转成内核出站名再比较
	return render.OutboundTagFor(cfg, render.DefaultProxyTarget(cfg, r)) == render.OutboundTagFor(cfg, ch.Tag())
}

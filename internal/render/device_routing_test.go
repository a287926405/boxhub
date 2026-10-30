package render_test

// 「设备分流模式」的回归测试。
//
// 这是流量层面的分流:按来源设备 IP 决定流量走不走代理。
// 与「DNS 分流模式」(只决定域名怎么解析)是两件完全不同的事,
// 之前只有后者,导致用户想要的"这台设备不走代理"根本没法表达。
//
// 语义:
//   默认代理:所有设备走代理,例外设备走直连;
//   默认直连:所有设备走直连,例外设备走代理。

import (
	"fmt"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func deviceRoute(t *testing.T, mode string, ex []string) *render.SBRoute {
	t.Helper()
	cfg := config.NewDefault()
	// 放一个节点,让"代理出口"是个真实 tag。
	// 空配置下 defaultProxyTarget 会兜底成 direct,那样就分不清
	// "本来就没代理"和"模式把出口改成了直连"。
	cfg.Nodes = []config.Node{{
		ID: "n1", Name: "测试节点", Type: "vless", Server: "1.2.3.4", Port: 443,
		UUID: "00000000-0000-0000-0000-000000000000",
	}}
	// 建一个含该节点的组:兜底出口只有指向"真实生成的出站"才不会被
	// 末尾的守卫退化成 direct,否则测不出模式本身的差别。
	seq := 0
	config.EnsureDefaultGroups(cfg, func() string { seq++; return fmt.Sprintf("g%d", seq) })
	cfg.Routing.Mode = mode
	cfg.Routing.ModeExceptions = ex
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	return sb.Route
}

// 找到第一条带来源 IP 条件的规则。
func firstSourceRule(rt *render.SBRoute) *render.SBRule {
	for i := range rt.Rules {
		if len(rt.Rules[i].ClientIP) > 0 {
			return &rt.Rules[i]
		}
	}
	return nil
}

// 默认代理 + 例外设备:例外设备被钉到直连,兜底仍然是代理。
func TestDeviceRoutingProxyModeExceptionGoesDirect(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeProxy, []string{"10.10.10.35", "10.10.10.42"})

	r := firstSourceRule(rt)
	if r == nil {
		t.Fatal("默认代理 + 例外设备:没有生成来源 IP 规则,例外设备不会走直连")
	}
	if r.Outbound != "direct" {
		t.Fatalf("默认代理模式下例外设备应走 direct,实际 %q", r.Outbound)
	}
	if r.Invert {
		t.Fatal("默认代理模式不需要 invert:例外设备本身就该被钉到直连")
	}
	if len(r.ClientIP) != 2 {
		t.Fatalf("例外设备数量不对: %v", r.ClientIP)
	}
	if rt.Final == "direct" {
		t.Fatal("默认代理模式的兜底出口不该是 direct")
	}
}

// 默认直连:兜底出口必须是 direct。
func TestDeviceRoutingDirectModeFinalIsDirect(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeDirect, nil)
	if rt.Final != "direct" {
		t.Fatalf("默认直连模式的兜底出口应为 direct,实际 %q", rt.Final)
	}
	// 没有配置例外设备时,来源规则只会用来放行回环(内核自身流量),
	// 不能出现任何局域网设备地址。
	for _, r := range rt.Rules {
		for _, c := range r.ClientIP {
			if c != "127.0.0.1/32" && c != "::1/128" {
				t.Fatalf("没配置例外设备却生成了设备来源规则: %v", r.ClientIP)
			}
		}
	}
}

// 默认直连 + 例外设备:例外走代理,其余走直连。
func TestDeviceRoutingDirectModeExceptionGoesProxy(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeDirect, []string{"10.10.10.35"})
	if rt.Final != "direct" {
		t.Fatalf("兜底出口应为 direct,实际 %q", rt.Final)
	}

	var toProxy, toDirectInverted *render.SBRule
	for i := range rt.Rules {
		r := &rt.Rules[i]
		if len(r.ClientIP) == 0 {
			continue
		}
		if r.Invert && r.Outbound == "direct" {
			toDirectInverted = r
		} else if !r.Invert && r.Outbound != "direct" {
			toProxy = r
		}
	}
	if toProxy == nil {
		t.Fatal("默认直连 + 例外设备:缺少「例外设备走代理」的规则")
	}
	if toDirectInverted == nil {
		// 没有这条,后面按域名匹配的分流规则会把非例外设备重新送进代理,
		// "默认直连"就形同虚设。
		t.Fatal("默认直连 + 例外设备:缺少「其余设备走直连」的反向规则")
	}

	// 例外规则必须排在反向规则前面,否则例外设备会被反向规则先吃掉。
	idxProxy, idxInvert := -1, -1
	for i := range rt.Rules {
		if &rt.Rules[i] == toProxy {
			idxProxy = i
		}
		if &rt.Rules[i] == toDirectInverted {
			idxInvert = i
		}
	}
	if idxProxy > idxInvert {
		t.Fatalf("「例外设备走代理」必须排在「其余设备走直连」之前(proxy=%d invert=%d)", idxProxy, idxInvert)
	}
}

// 设备分流规则必须排在业务分流规则之前,否则会被域名规则改判。
func TestDeviceRoutingPrecedesPolicies(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Routing.Mode = config.RouteModeDirect
	cfg.Routing.ModeExceptions = []string{"10.10.10.35"}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}

	firstSource := -1
	firstDomain := -1
	for i, r := range sb.Route.Rules {
		if len(r.ClientIP) > 0 && firstSource < 0 {
			firstSource = i
		}
		if len(r.RuleSet) > 0 && firstDomain < 0 {
			firstDomain = i
		}
	}
	if firstSource < 0 {
		t.Fatal("没有生成来源 IP 规则")
	}
	if firstDomain >= 0 && firstSource > firstDomain {
		t.Fatalf("设备分流规则(第 %d 条)必须排在域名规则(第 %d 条)之前", firstSource, firstDomain)
	}
}

// 空行/重复项要被清掉,避免生成 "": 这种非法 CIDR 让内核起不来。
func TestDeviceRoutingNormalizesExceptions(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeProxy, []string{" 10.10.10.35 ", "", "10.10.10.35"})
	r := firstSourceRule(rt)
	if r == nil {
		t.Fatal("没有生成来源规则")
	}
	if len(r.ClientIP) != 1 || r.ClientIP[0] != "10.10.10.35" {
		t.Fatalf("例外设备没有去空白/去重: %v", r.ClientIP)
	}
}

// 内核自身流量(来源 127.0.0.1)不能被当成"设备"。
//
// 这是真实故障:用户把设备分流模式设成"默认直连"后,面板首页的测速
// (内核从 127.0.0.1 经混合端口发起)被当成非例外设备走了直连,
// 境外站点全部超时,测速四项只剩百度有数。
func TestDeviceRoutingDirectModeExcludesLoopback(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeDirect, nil)

	var proxyRule, invertRule *render.SBRule
	for i := range rt.Rules {
		r := &rt.Rules[i]
		if len(r.ClientIP) == 0 {
			continue
		}
		if r.Invert {
			invertRule = r
		} else {
			proxyRule = r
		}
	}
	if invertRule == nil || proxyRule == nil {
		t.Fatal("默认直连模式应生成「例外→代理」和「其余→直连」两条规则")
	}
	hasLoopback := func(r *render.SBRule) bool {
		for _, c := range r.ClientIP {
			if c == "127.0.0.1/32" || c == "::1/128" {
				return true
			}
		}
		return false
	}
	if !hasLoopback(invertRule) {
		t.Fatal("「其余设备走直连」的反向规则必须排除回环,否则内核自己的测速/诊断会被掐断")
	}
	if !hasLoopback(proxyRule) {
		t.Fatal("回环要显式走代理,面板测速才能测到境外站点")
	}
}

// 默认代理模式下回环本来就按正常规则走代理,不该被塞进"例外设备→直连"里。
func TestDeviceRoutingProxyModeKeepsLoopbackOut(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeProxy, []string{"10.10.10.35"})
	r := firstSourceRule(rt)
	if r == nil {
		t.Fatal("没有生成来源规则")
	}
	for _, c := range r.ClientIP {
		if c == "127.0.0.1/32" || c == "::1/128" {
			t.Fatalf("默认代理模式的例外规则不该包含回环: %v", r.ClientIP)
		}
	}
}

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

// 默认直连 + 例外设备:其余设备被钉到直连,例外设备走正常分流、
// 兜底才走代理。
//
// 关键点:例外设备的兜底规则必须排在**所有业务规则之后**。
// 早期版本放在最前面,导致例外设备访问 instagram 命中的是
// source_ip_cidr => route(Proxy),INS 规则完全失效。
func TestDeviceRoutingDirectModeExceptionFallsThroughToProxy(t *testing.T) {
	rt := deviceRoute(t, config.RouteModeDirect, []string{"10.10.10.35"})
	if rt.Final != "direct" {
		t.Fatalf("兜底出口应为 direct,实际 %q", rt.Final)
	}

	var invertRule *render.SBRule
	for i := range rt.Rules {
		if rt.Rules[i].Invert && len(rt.Rules[i].ClientIP) > 0 {
			invertRule = &rt.Rules[i]
		}
	}
	if invertRule == nil {
		t.Fatal("缺少「其余设备走直连」的反向规则")
	}
	if invertRule.Outbound != "direct" {
		t.Fatalf("反向规则应走 direct,实际 %q", invertRule.Outbound)
	}

	// 例外设备的兜底必须是最后一条,且指向代理。
	last := rt.Rules[len(rt.Rules)-1]
	hasEx := false
	for _, c := range last.ClientIP {
		if c == "10.10.10.35" {
			hasEx = true
		}
	}
	if !hasEx {
		t.Fatalf("最后一条应是例外设备的兜底规则,实际: %+v", last)
	}
	if last.Outbound == "direct" {
		t.Fatal("例外设备的兜底应指向代理")
	}

	// 而且不能出现在业务规则之前 —— 否则会抢在 INS/Youtube 之前改判。
	for i, r := range rt.Rules {
		if i == len(rt.Rules)-1 {
			continue
		}
		for _, c := range r.ClientIP {
			if c == "10.10.10.35" && !r.Invert && r.Outbound != "direct" {
				t.Fatalf("例外设备的代理规则出现在第 %d 条(业务规则之前),会抢掉 INS 等分流", i)
			}
		}
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
	rt := deviceRoute(t, config.RouteModeDirect, []string{"10.10.10.35"})

	hasLoopback := func(r *render.SBRule) bool { return containsLoopback(r.ClientIP) }

	var invertRule *render.SBRule
	for i := range rt.Rules {
		if rt.Rules[i].Invert && len(rt.Rules[i].ClientIP) > 0 {
			invertRule = &rt.Rules[i]
		}
	}
	if invertRule == nil {
		t.Fatal("缺少「其余设备走直连」的反向规则")
	}
	if !hasLoopback(invertRule) {
		t.Fatal("反向规则必须排除回环,否则内核自己的测速/诊断会被掐成直连")
	}

	// 回环的兜底必须排在最后,且指向代理(直连模式 final=direct)。
	last := rt.Rules[len(rt.Rules)-1]
	if !hasLoopback(&last) {
		t.Fatalf("回环兜底应是最后一条(实际: %+v)", last)
	}
	if last.Outbound == "direct" {
		t.Fatal("回环兜底应指向代理,否则直连模式下内核测不到境外站点")
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

// containsLoopback 判断来源列表里是否含回环地址。
func containsLoopback(list []string) bool {
	for _, c := range list {
		if c == "127.0.0.1/32" || c == "::1/128" {
			return true
		}
	}
	return false
}

package render_test

// 这组测试锁死"路由与 DNS 的配合方式"。
//
// 背景(真实故障):某机场订阅的节点是 Cloudflare WS 隧道型,这类节点:
//   - 访问普通 443 站点完全正常
//   - 但对 DNS 流量(UDP 53 / TCP 53 / DoH 443)支持很差,
//     实测 "tls: handshake failure" / "unexpected EOF"
//
// 早期实现在 route.rules 里加了 `action: resolve`,强制在路由阶段真实解析域名。
// 域名是境外的 → 必须用代理侧 DNS → 而代理侧 DNS 又走代理 → 该节点不支持 →
// 解析失败 → 所有境外站点打不开。
// 而内核自己测速(IP 直连)却是通的,故障极难定位。
//
// 正确做法:
//   1. 路由里**不加** resolve,让域名保持原样交给节点侧解析;
//   2. 代理 DNS 不写死 detour,避免把 DNS 强绑到隧道节点上。

import (
	"testing"

	"boxhub/internal/render"
)

func TestNoResolveActionInRoute(t *testing.T) {
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sb.Route.Rules {
		if r.Action == "resolve" {
			t.Fatalf("路由里不应出现 action:resolve(会让域名被迫本地解析," +
				"隧道节点场景下会导致境外站点全部打不开)")
		}
	}
}

func TestSniffComesFirst(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DNS.Hijack = true
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Route.Rules) == 0 {
		t.Fatal("没有路由规则")
	}
	// sniff 必须是第一条(或至少在所有域名规则之前),
	// 否则拿不到域名,域名类规则全部失效。
	firstSniff := -1
	firstDomain := -1
	for i, r := range sb.Route.Rules {
		if r.Action == "sniff" && firstSniff < 0 {
			firstSniff = i
		}
		if (len(r.Domain) > 0 || len(r.DomainSuffix) > 0 || len(r.RuleSet) > 0) && firstDomain < 0 {
			firstDomain = i
		}
	}
	if firstSniff < 0 {
		t.Fatal("缺少 action:sniff 规则")
	}
	if firstDomain >= 0 && firstSniff > firstDomain {
		t.Fatalf("sniff(第 %d 条)必须在域名规则(第 %d 条)之前", firstSniff, firstDomain)
	}
}

func TestDNSProxyDetourNotHardcoded(t *testing.T) {
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.DNS == nil {
		t.Fatal("缺少 DNS 配置")
	}
	for _, s := range sb.DNS.Servers {
		if s.Type == "fakeip" {
			continue
		}
		// 写死 detour 会把 DNS 强绑到某个节点;隧道型节点通常不支持 DNS 流量
		if s.Detour == "direct" {
			t.Errorf("DNS 服务器 %s 的 detour 不该是 direct(内核会 FATAL)", s.Tag)
		}
	}
}

func TestFakeIPEnabledByDefault(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DNS.FakeIP = true
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sb.DNS.Servers {
		if s.Type == "fakeip" {
			found = true
			if s.Inet4Range == "" {
				t.Fatal("fakeip 缺少 inet4_range")
			}
		}
	}
	if !found {
		t.Fatal("开启 FakeIP 时应生成 fakeip DNS 服务器")
	}
}

// 确保直连 DNS 仍然存在——国内域名必须走它,否则解析也会被拖慢。
func TestDirectDNSStillPresent(t *testing.T) {
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	hasDirect := false
	for _, s := range sb.DNS.Servers {
		if len(s.Tag) >= 10 && s.Tag[:10] == "dns-direct" {
			hasDirect = true
		}
	}
	if !hasDirect {
		t.Fatal("缺少直连 DNS 服务器")
	}
}

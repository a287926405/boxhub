package render_test

// TUN 地址段的回归测试。
//
// 网关模式下 strict_route 开启时,如果 tun 只配了 IPv4 地址,
// sing-box 会在 `ip -6 rule` 注入 `from all unreachable` 黑洞,
// 把主机所有 IPv6 出站(包括拨号 IPv6 节点本身)全部丢弃,
// 表现为「v6 节点全部 network is unreachable」。
// 所以渲染出的 tun 入站必须始终带一个 IPv6 地址:
//   1. 默认地址要同时含 v4 与 v6(ULA);
//   2. 用户只配 v4 时自动补 ULA;
//   3. 用户已配 v6 时保持原样。

import (
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func tunAddresses(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	cfg.Inbounds.TUN = true
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ib := range sb.Inbounds {
		if ib.Tag == "tun-in" {
			return ib.Address
		}
	}
	t.Fatal("没有渲染出 tun-in 入站")
	return nil
}

func hasV6(addr []string) bool {
	for _, a := range addr {
		body := a
		if i := strings.IndexByte(a, '/'); i >= 0 {
			body = a[:i]
		}
		if strings.Contains(body, ":") {
			return true
		}
	}
	return false
}

func TestTUNAddressIncludesIPv6ByDefault(t *testing.T) {
	cfg := config.NewDefault()
	addr := tunAddresses(t, cfg)
	if len(addr) != 2 {
		t.Fatalf("默认应该有两个地址(v4+v6),得到 %v", addr)
	}
	if !hasV6(addr) {
		t.Fatalf("默认地址缺 IPv6,strict_route 会黑洞所有 v6 出站: %v", addr)
	}
}

func TestTUNAddressAppendsIPv6WhenCustomIsV4Only(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.TunAddress = []string{"10.99.0.1/24"}
	addr := tunAddresses(t, cfg)
	if addr[0] != "10.99.0.1/24" {
		t.Fatalf("自定义 v4 地址被改掉了: %v", addr)
	}
	if !hasV6(addr) {
		t.Fatalf("只配 v4 时应自动补 ULA v6 地址: %v", addr)
	}
}

func TestTUNAddressKeepsUserProvidedIPv6(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.TunAddress = []string{"10.99.0.1/24", "fd99::1/64"}
	addr := tunAddresses(t, cfg)
	if len(addr) != 2 || addr[1] != "fd99::1/64" {
		t.Fatalf("用户已配 v6 时不应再追加默认 ULA: %v", addr)
	}
}

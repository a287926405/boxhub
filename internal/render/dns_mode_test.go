package render_test

// DNS 分流模式(对齐 SMBox 的「DNS 分流模式」卡片):
// 按来源设备 IP 决定域名解析走不走 sing-box。
//   默认代理: 全部设备 FakeIP/代理DNS,例外设备真实直连解析
//   默认直连: 全部设备直连 DNS 真实解析,例外设备走 sing-box

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func dnsModeCfg(mode string, ex []string) *config.Config {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.ClashAPI = false
	cfg.DNS.FakeIP = true
	cfg.DNS.Mode = mode
	cfg.DNS.ModeExceptions = ex
	return cfg
}

func dnsRulesOf(t *testing.T, cfg *config.Config) []map[string]any {
	t.Helper()
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sb.DNS.Rules)
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestDNSModeDefaultProxy(t *testing.T) {
	rules := dnsRulesOf(t, dnsModeCfg("proxy", []string{"10.10.10.35"}))
	// 例外设备 → direct,且在兜底 fakeip 规则之前
	exIdx, fakeIdx := -1, -1
	for i, r := range rules {
		b, _ := json.Marshal(r)
		s := string(b)
		if strings.Contains(s, "source_ip_cidr") && strings.Contains(s, "dns-direct") {
			exIdx = i
		}
		if r["server"] == "dns-fakeip" {
			fakeIdx = i
		}
	}
	if exIdx < 0 || fakeIdx < 0 || exIdx > fakeIdx {
		t.Fatalf("例外设备规则应在 fakeip 兜底之前(例外=%d fakeip=%d): %v", exIdx, fakeIdx, rules)
	}
}

func TestDNSModeDefaultDirect(t *testing.T) {
	rules := dnsRulesOf(t, dnsModeCfg("direct", []string{"192.168.1.0/24"}))
	exIdx, defIdx := -1, -1
	for i, r := range rules {
		b, _ := json.Marshal(r)
		s := string(b)
		if strings.Contains(s, "source_ip_cidr") && strings.Contains(s, "dns-fakeip") {
			exIdx = i
		}
		if r["server"] == "dns-direct-1" && r["source_ip_cidr"] == nil {
			// 排除前面的 geosite:cn 规则(有 rule_set)
			if _, hasRS := r["rule_set"]; !hasRS {
				defIdx = i
			}
		}
	}
	if exIdx < 0 {
		t.Fatalf("direct 模式下例外设备应走 fakeip: %v", rules)
	}
	if defIdx < 0 || exIdx > defIdx {
		t.Fatalf("例外规则应在默认直连兜底之前(例外=%d 默认=%d)", exIdx, defIdx)
	}
}

func TestDNSModeInvalid(t *testing.T) {
	cfg := dnsModeCfg("weird", []string{"not-an-ip"})
	errs := config.ValidateAll(cfg)
	if len(errs) != 2 {
		t.Fatalf("模式与非法 IP 都应报错,实际: %v", errs)
	}
}

// 真实内核必须接受带 source_ip_cidr 的 DNS 规则(1.14 语法核对)
func TestDNSModeRunsOnKernel(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("未找到 sing-box")
	}
	cfg := dnsModeCfg("direct", []string{"10.10.10.35", "10.10.10.0/24"})
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.DNS.Hijack = false
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := sb.Marshal()
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	alive, out := runOnly(t, bin, path, dir, 3*time.Second)
	if !alive {
		t.Fatalf("内核拒绝 DNS 分流配置:\n%s", out)
	}
}

package render_test

// FakeIP 应答 TTL 的回归测试。
//
// 背景:用户在面板上改一次 DNS 分流模式,家里 Windows 电脑立刻全网上不去,
// 手机却正常,要等十几分钟才自己恢复。原因是:
//
//   - boxd 保存配置 = 停内核 + 起内核,FakeIP 映射随之清空;
//   - 客户端缓存里 TTL 600 秒的假地址(198.18.0.0/15)全部失效,
//     连接进内核后报 "missing fakeip record" 被直接丢弃;
//   - Windows 严格按 TTL 缓存,600 秒内不会重新解析,于是一挂就是十分钟。
//
// store_fakeip 让映射跨重启保留,是主要修复;这里的短 TTL 是第二道保险:
// 只要映射还是丢了(cache.db 被删/内核被强杀/换内核),客户端也能在一分钟内
// 自己重新解析,而不是干等十分钟。

import (
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

// 默认代理模式下,发给客户端的假地址必须带短 TTL。
func TestFakeIPAnswersCarryShortTTL(t *testing.T) {
	rules := dnsRulesOf(t, dnsModeCfg(config.DNSModeProxy, nil))

	var fallback map[string]any
	for _, r := range rules {
		if r["server"] == "dns-fakeip" {
			fallback = r
		}
	}
	if fallback == nil {
		t.Fatal("默认代理模式缺少 fakeip 兜底规则")
	}
	ttl, ok := fallback["rewrite_ttl"].(float64)
	if !ok {
		t.Fatalf("fakeip 规则没有 rewrite_ttl,客户端会按内核默认 600 秒缓存: %v", fallback)
	}
	if int(ttl) != render.FakeIPAnswerTTL {
		t.Fatalf("fakeip TTL 应为 %d,实际 %v", render.FakeIPAnswerTTL, ttl)
	}
	if ttl > 60 {
		t.Fatalf("fakeip TTL 过大(%v 秒),映射丢失后客户端要等太久才重新解析", ttl)
	}
}

// 真实解析(国内域名)不能被改 TTL,否则会拖慢正常访问、加重上游 DNS 压力。
func TestRealAnswersKeepUpstreamTTL(t *testing.T) {
	rules := dnsRulesOf(t, dnsModeCfg(config.DNSModeProxy, nil))

	var checked int
	for _, r := range rules {
		server, _ := r["server"].(string)
		if server == "dns-fakeip" || server == "" {
			continue
		}
		if _, ok := r["rewrite_ttl"]; ok {
			// 例外设备那条规则是刻意设成 1 秒的(切换模式要立刻生效),
			// 这里只检查"没有来源 IP 限制"的通用解析规则。
			if _, hasSrc := r["source_ip_cidr"]; hasSrc {
				continue
			}
			t.Fatalf("真实解析规则不该改 TTL: %v", r)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没有找到任何真实解析规则,测试无效")
	}
}

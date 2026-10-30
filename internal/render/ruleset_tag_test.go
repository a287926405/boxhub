package render_test

// 规则集 tag 唯一性的回归测试。
//
// 真实故障:INS 规则里配的是 instagram.srs,但流量实际走了兜底 Proxy。
// 原因是 ruleSetTag 只保留 URL 前 40 个安全字符,MetaCubeX 的规则集又全部
// 共享同一前缀,instagram / youtube / geolocation-!cn 全部塌缩成同一个 tag,
// 于是所有规则都指向最先出现的那一个规则集(实测是 apple@cn.json)。

import (
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

// 同一前缀、不同文件必须得到不同 tag。
func TestRuleSetTagDistinguishesSamePrefixURLs(t *testing.T) {
	cfg := config.NewDefault()
	base := "https://github.com/MetaCubeX/meta-rules-dat/raw/refs/heads/sing/geo/"
	cfg.Policies = []config.Policy{
		{ID: "p-ins", Name: "INS", Enabled: true, Target: "self", AsGroup: true,
			Match: config.Match{RuleSets: []string{
				base + "geosite/instagram.srs",
				base + "geoip/facebook.srs",
			}}},
		{ID: "p-yt", Name: "Youtube", Enabled: true, Target: "self", AsGroup: true,
			Match: config.Match{RuleSets: []string{base + "geosite/youtube.srs"}}},
		{ID: "p-cn", Name: "!CN", Enabled: true, Target: "self", AsGroup: true,
			Match: config.Match{RuleSets: []string{base + "geosite/geolocation-!cn.srs"}}},
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}

	// tag -> URL,同时校验 tag 唯一
	byTag := map[string]string{}
	for _, rs := range sb.Route.RuleSet {
		if prev, ok := byTag[rs.Tag]; ok && prev != rs.URL {
			t.Fatalf("不同规则集共用了同一个 tag %q: %s / %s", rs.Tag, prev, rs.URL)
		}
		byTag[rs.Tag] = rs.URL
	}
	if len(byTag) != len(sb.Route.RuleSet) {
		t.Fatalf("规则集 tag 有重复: %d 条规则集只有 %d 个唯一 tag", len(sb.Route.RuleSet), len(byTag))
	}

	// 每个期望的规则集都必须被某条规则真实引用到
	referenced := map[string]bool{}
	for _, r := range sb.Route.Rules {
		for _, tag := range r.RuleSet {
			if u, ok := byTag[tag]; ok {
				referenced[u] = true
			}
		}
	}
	for _, want := range []string{
		base + "geosite/instagram.srs",
		base + "geoip/facebook.srs",
		base + "geosite/youtube.srs",
		base + "geosite/geolocation-!cn.srs",
	} {
		if !referenced[want] {
			t.Fatalf("规则集 %s 没有被任何规则引用到(说明 tag 撞车被别的 URL 顶掉了)", want)
		}
	}
}

// 内置的 geoip:cn / geosite:cn 也要稳定且互不相同。
func TestRuleSetTagBuiltinStable(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Policies = nil
	sb1, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	sb2, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sb1.Route.RuleSet) != len(sb2.Route.RuleSet) {
		t.Fatal("同一配置两次渲染的规则集数量不一致")
	}
	for i := range sb1.Route.RuleSet {
		if sb1.Route.RuleSet[i].Tag != sb2.Route.RuleSet[i].Tag {
			t.Fatalf("规则集 tag 不稳定: %s vs %s",
				sb1.Route.RuleSet[i].Tag, sb2.Route.RuleSet[i].Tag)
		}
		if sb1.Route.RuleSet[i].URL != sb2.Route.RuleSet[i].URL {
			t.Fatal("同一配置两次渲染的规则集 URL 不一致")
		}
	}
	// 两个内置规则集不能共用一个 tag
	seen := map[string]string{}
	for _, rs := range sb1.Route.RuleSet {
		if prev, ok := seen[rs.Tag]; ok && prev != rs.URL {
			t.Fatalf("不同 URL 共用了同一个 tag %q: %s / %s", rs.Tag, prev, rs.URL)
		}
		seen[rs.Tag] = rs.URL
	}
}

package render_test

// GitHub 网页链接归一化的回归测试。
//
// 真实故障:面板里填的是 GitHub 文件页地址(/blob/…),下载回来是 HTML,
// 内核报 invalid sing-box rule-set file,重启失败、全屋断网。

import (
	"strings"
	"testing"

	"boxhub/internal/render"
)

func buildRuleSetURLs(t *testing.T, raw string) []string {
	t.Helper()
	cfg := ruleSetInboundConfig()
	cfg.Policies[0].Match.RuleSets = []string{raw}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, rs := range sb.Route.RuleSet {
		if rs.URL != "" {
			urls = append(urls, rs.URL)
		}
	}
	return urls
}

func TestGitHubBlobRuleSetURLNormalizedToRaw(t *testing.T) {
	const want = "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/cloudflare.srs"
	urls := buildRuleSetURLs(t, "https://github.com/MetaCubeX/meta-rules-dat/blob/sing/geo/geoip/cloudflare.srs")
	for _, u := range urls {
		if strings.Contains(u, "/blob/") {
			t.Fatalf("GitHub 网页链接没有归一化,内核会下载到 HTML: %s", u)
		}
	}
	found := false
	for _, u := range urls {
		if u == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有生成 raw 规则集地址 %s,实际: %v", want, urls)
	}
}

// 已经在用的 /raw/ 地址必须保持原样:它是本地缓存文件名的哈希输入,
// 改了会让已有缓存全部失效。
func TestGitHubRawRuleSetURLStaysUnchanged(t *testing.T) {
	const raw = "https://github.com/MetaCubeX/meta-rules-dat/raw/sing/geo/geoip/cloudflare.srs"
	urls := buildRuleSetURLs(t, raw)
	for _, u := range urls {
		if u == raw {
			return
		}
	}
	t.Fatalf("raw 地址被改动,会导致旧缓存失效: %v", urls)
}

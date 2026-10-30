package render_test

// 规则集本地预取的回归测试。
//
// 背景:sing-box 启动时先下载所有 remote 规则集,下载失败直接 FATAL,
// 内核起不来 —— 表现为"服务 active 但全屋断网"。国内直连 github 不通,
// 只能让下载走代理;但代理又依赖内核,节点全挂就永远起不来。
//
// 解法:规则集落地成本地文件,渲染时用 type:local + path,
// 内核启动零网络依赖。

import (
	"os"
	"path/filepath"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
	"boxhub/internal/ruleset"
)

func ruleSetInboundConfig() *config.Config {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{{
		ID: "n1", Name: "测试节点", Type: "vless", Server: "1.2.3.4", Port: 443,
		UUID: "00000000-0000-0000-0000-000000000000",
	}}
	seq := 0
	config.EnsureDefaultGroups(cfg, func() string { seq++; return "g" + string(rune('0'+seq)) })
	cfg.Policies = []config.Policy{{
		ID: "p-ins", Name: "INS", Enabled: true, Target: "self", AsGroup: true,
		Match: config.Match{RuleSets: []string{
			"https://github.com/MetaCubeX/meta-rules-dat/raw/refs/heads/sing/geo/geosite/instagram.srs",
		}},
	}}
	return cfg
}

// 有本地缓存时必须渲染成 local,且 URL 不再出现(否则内核仍会去下载)。
func TestRuleSetUsesLocalPathWhenCached(t *testing.T) {
	cfg := ruleSetInboundConfig()

	// 第一遍:拿到远程 URL
	base, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for _, rs := range base.Route.RuleSet {
		if rs.Type == "remote" && rs.URL != "" {
			target = rs.URL
			break
		}
	}
	if target == "" {
		t.Fatal("没有生成 remote 规则集")
	}

	// 第二遍:模拟"本地已有缓存"。
	// 必须给全部远程规则集都准备本地文件:内置的 geoip:cn/geosite:cn 也在
	// 列表里,只备一份的话其余仍是 remote,看起来就像"没转成本地"。
	dir := t.TempDir()
	paths := map[string]string{}
	for _, rs := range base.Route.RuleSet {
		if rs.Type != "remote" || rs.URL == "" {
			continue
		}
		// 每个 URL 一个独立文件,便于断言"目标规则集用的是它自己那份"
		p := filepath.Join(dir, ruleset.FileName(rs.URL, rs.Format))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[rs.URL] = p
	}
	local := paths[target]
	if local == "" {
		t.Fatal("目标规则集没有本地路径")
	}
	sb, err := render.Build(cfg, render.Context{RuleSetPaths: paths})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, rs := range sb.Route.RuleSet {
		if rs.URL != "" {
			t.Fatalf("有本地缓存却仍生成 remote 规则集 %s(内核仍会联网下载)", rs.URL)
		}
		if rs.Type != "local" || rs.Path == "" {
			t.Fatalf("规则集 %s 不是 local 或缺少 path", rs.Tag)
		}
		if rs.DownloadDetour != "" {
			t.Fatal("local 规则集不该带 download_detour")
		}
		if rs.Path == local {
			found = true
		}
	}
	if !found {
		t.Fatal("没有找到转成本地的目标规则集")
	}
}

// 没有缓存时必须保持 remote,不能把内核搞成起不来。
func TestRuleSetStaysRemoteWithoutCache(t *testing.T) {
	sb, err := render.Build(ruleSetInboundConfig(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rs := range sb.Route.RuleSet {
		if rs.Type == "remote" && rs.URL != "" {
			n++
			if rs.DownloadDetour == "" {
				t.Fatalf("remote 规则集缺少 download_detour: %s", rs.Tag)
			}
		}
	}
	if n == 0 {
		t.Fatal("没有生成 remote 规则集")
	}
}

// 规则里的 tag 引用在本地化之后必须还能对上。
func TestRuleSetTagStillMatchesAfterLocalize(t *testing.T) {
	cfg := ruleSetInboundConfig()
	base, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	local := filepath.Join(t.TempDir(), "c.srs")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rs := range base.Route.RuleSet {
		if rs.Type == "remote" && rs.URL != "" {
			paths[rs.URL] = local
		}
	}
	sb, err := render.Build(cfg, render.Context{RuleSetPaths: paths})
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, rs := range sb.Route.RuleSet {
		known[rs.Tag] = true
	}
	for _, r := range sb.Route.Rules {
		for _, tag := range r.RuleSet {
			if !known[tag] {
				t.Fatalf("规则引用了不存在的规则集 tag %q(本地化后 tag 变了)", tag)
			}
		}
	}
}

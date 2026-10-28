package render_test

// 对齐 SMBox 的出站结构。
//
// 真实结构(从 SMBox 生成的 sing-box 配置里读出来的):
//
//	outbounds:
//	  - {tag: "🇯🇵 日本", type: selector|urltest, outbounds: [节点...]}   ← 筛选组
//	  - {tag: Proxy, type: selector, outbounds: [筛选组..., "➜ Direct"]}  ← 总出口
//	  - {tag: "INS", type: selector,
//	     outbounds: [Proxy, 筛选组..., "➜ Direct"]}                      ← 规则组
//	route.final: Proxy
//
// 两个容易做错、且会直接影响用户的点:
//   1. 筛选组 tag 必须**是可读名称**而不是内部 ID —— 否则内核面板里全是
//      "filter:f3a2" 这种看不懂的组名,没法手动选线。
//   2. 规则组第一个成员必须是 Proxy —— 这样"给某类站点单独选线路"的默认状态
//      是跟随全局,符合直觉;否则用户以为切了 Proxy,实际不生效。

import (
	"os"
	"strings"
	"testing"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func smboxLikeConfig() *config.Config {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "日本 01", Type: "vless", Server: "1.1.1.1", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "n2", Name: "日本 02", Type: "vless", Server: "1.1.1.2", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "n3", Name: "美国 01", Type: "vless", Server: "1.1.1.3", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
	}
	cfg.Filters = []config.Filter{
		{ID: "f1", Name: "🇯🇵 日本", Enabled: true, Keywords: []string{"日本"}, Mode: "select"},
		{ID: "f2", Name: "🇺🇸 美国", Enabled: true, Keywords: []string{"美国"}, Mode: "urltest"},
	}
	cfg.Policies = append(cfg.Policies, config.Policy{
		ID: "p1", Name: "AI", Enabled: true, Target: "direct", AsGroup: true,
		Match: config.Match{DomainSuffix: []string{"openai.com"}},
	})
	return cfg
}

func findOb(sb *render.SBConfig, tag string) *render.SBOutbound {
	for i := range sb.Outbounds {
		if sb.Outbounds[i].Tag == tag {
			return &sb.Outbounds[i]
		}
	}
	return nil
}

// 筛选组 tag 必须是可读名称
func TestFilterGroupTagIsReadableName(t *testing.T) {
	sb, err := render.Build(smboxLikeConfig(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	jp := findOb(sb, "🇯🇵 日本")
	if jp == nil {
		t.Fatalf("找不到筛选组「🇯🇵 日本」;实际组: %v", allTags(sb))
	}
	if jp.Type != "selector" {
		t.Fatalf("select 模式的筛选组类型应为 selector,实际 %s", jp.Type)
	}
	if len(jp.Outbounds) != 2 {
		t.Fatalf("日本组应含 2 个节点,实际 %v", jp.Outbounds)
	}
	us := findOb(sb, "🇺🇸 美国")
	if us == nil || us.Type != "urltest" {
		t.Fatalf("urltest 模式的筛选组类型不对: %+v", us)
	}
	// 不该出现内部 ID 形式的 tag
	for _, o := range sb.Outbounds {
		if strings.HasPrefix(o.Tag, "filter:") {
			t.Errorf("筛选组的 tag 不该是内部 ID: %s", o.Tag)
		}
	}
}

// Proxy 组结构与兜底
func TestProxyGroup(t *testing.T) {
	sb, err := render.Build(smboxLikeConfig(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	p := findOb(sb, "Proxy")
	if p == nil {
		t.Fatalf("缺少 Proxy 组;实际: %v", allTags(sb))
	}
	if p.Type != "selector" {
		t.Fatalf("Proxy 应为 selector,实际 %s", p.Type)
	}
	hasJP, hasUS, hasDirect := false, false, false
	for _, m := range p.Outbounds {
		switch m {
		case "🇯🇵 日本":
			hasJP = true
		case "🇺🇸 美国":
			hasUS = true
		case "➜ Direct":
			hasDirect = true
		}
	}
	if !hasJP || !hasUS || !hasDirect {
		t.Fatalf("Proxy 成员应含全部筛选组与 ➜ Direct,实际 %v", p.Outbounds)
	}
	if sb.Route.Final != "Proxy" {
		t.Fatalf("兜底应为 Proxy,实际 %q", sb.Route.Final)
	}
}

// 规则组第一个成员必须是 Proxy
func TestRuleGroupStartsWithProxy(t *testing.T) {
	sb, err := render.Build(smboxLikeConfig(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	g := findOb(sb, "AI")
	if g == nil {
		t.Fatalf("缺少规则组 AI;实际: %v", allTags(sb))
	}
	if len(g.Outbounds) == 0 || g.Outbounds[0] != "Proxy" {
		t.Fatalf("规则组第一个成员必须是 Proxy,实际 %v", g.Outbounds)
	}
	if g.Default != "Proxy" {
		t.Fatalf("规则组默认应选 Proxy,实际 %q", g.Default)
	}
}

// 规则应指向规则组本身
func TestRuleTargetsItsGroup(t *testing.T) {
	cfg := smboxLikeConfig()
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range sb.Route.Rules {
		if r.Outbound == "AI" {
			found = true
		}
	}
	if !found {
		t.Fatal("规则应路由到 AI 组")
	}
}

// 规则引用筛选时,内核里的 outbound 必须是筛选的**名称**
func TestRuleTargetsFilterName(t *testing.T) {
	cfg := smboxLikeConfig()
	cfg.Policies = append(cfg.Policies, config.Policy{
		ID: "p2", Name: "日语站点", Enabled: true, Target: "filter:f1",
		Match: config.Match{DomainSuffix: []string{"example.jp"}},
	})
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range sb.Route.Rules {
		if r.Outbound == "🇯🇵 日本" {
			found = true
		}
		if strings.HasPrefix(r.Outbound, "filter:") {
			t.Errorf("规则里的 outbound 不该是内部 ID: %s", r.Outbound)
		}
	}
	if !found {
		t.Fatal("规则应指向筛选组的名称")
	}
}

// 没有任何可切换成员时不该生成 Proxy(否则是空组,内核会报错)
func TestNoProxyWhenNoMembers(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if findOb(sb, "Proxy") != nil {
		t.Fatal("没有节点时不该生成 Proxy 组")
	}
	if sb.Route.Final == "Proxy" {
		t.Fatal("没有节点时兜底不该是 Proxy")
	}
}

// 规则组重名必须被拒绝
func TestDuplicateRuleGroupNameRejected(t *testing.T) {
	cfg := smboxLikeConfig()
	cfg.Policies = append(cfg.Policies, config.Policy{
		ID: "p3", Name: "AI", Enabled: true, Target: "direct", AsGroup: true,
		Match: config.Match{DomainSuffix: []string{"another.com"}},
	})
	errs := config.ValidateAll(cfg)
	hit := false
	for _, e := range errs {
		if strings.Contains(e.Message, "重复") || strings.Contains(e.Message, "同名") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("重名规则组应报错,实际: %v", errs)
	}
}

// 保留名不能当组名
func TestReservedGroupNameRejected(t *testing.T) {
	cfg := smboxLikeConfig()
	cfg.Policies = append(cfg.Policies, config.Policy{
		ID: "p4", Name: "Proxy", Enabled: true, Target: "direct", AsGroup: true,
		Match: config.Match{DomainSuffix: []string{"x.com"}},
	})
	errs := config.ValidateAll(cfg)
	if len(errs) == 0 {
		t.Fatal("用保留名作组名应报错")
	}
}

// 整份配置必须能被真实内核接受
func TestSMBoxStructureRunsOnKernel(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("未找到 sing-box")
	}
	cfg := smboxLikeConfig()
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.Inbounds.ClashAPI = true
	cfg.Inbounds.ClashAPIPort = freePort(t) + 1
	cfg.Inbounds.AllowLAN = false
	cfg.DNS.FakeIP = false
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
		t.Fatalf("内核拒绝启动:\n%s", out)
	}
}

func allTags(sb *render.SBConfig) []string {
	var tags []string
	for _, o := range sb.Outbounds {
		if o.Type == "selector" || o.Type == "urltest" {
			tags = append(tags, o.Tag)
		}
	}
	return tags
}

// 编译层兜底:前置组含链式代理自身时,即使绕过保存校验也要报错。
func TestChainSelfReferenceCompileGuard(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "JP", Type: "vless", Server: "1.1.1.1", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
	}
	cfg.Groups = []config.Group{
		{ID: "gf", Name: "front", Type: config.GroupSelect, Members: []string{"node:n1", "chain:c1"}},
	}
	cfg.Chains = []config.Chain{
		{ID: "c1", Name: "home", Type: "socks5", Host: "1.1.1.2", Port: 1080, FrontGroupID: "gf"},
	}
	_, err := render.Build(cfg, render.Context{})
	if err == nil || !strings.Contains(err.Error(), "循环依赖") {
		t.Fatalf("前置组含链自身应编译报错,实际: %v", err)
	}
}

// 链式代理必须进 Proxy 总出口组。
// 否则它只存在于内核自动的 GLOBAL 里:用户没法在总出口选它,
// 探测"临时切到它"也会失败(GLOBAL 不接受 PUT)。
func TestChainsJoinProxyGroup(t *testing.T) {
	cfg := smboxLikeConfig()
	// 给链式代理配一个可用前置组
	cfg.Groups = []config.Group{
		{ID: "gf", Name: "front", Type: config.GroupSelect, Members: []string{"node:n1"}},
	}
	cfg.Chains = []config.Chain{
		{ID: "c1", Name: "住宅", Type: config.ChainSocks5, Host: "1.2.3.4", Port: 1080, FrontGroupID: "gf"},
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	p := findOb(sb, "Proxy")
	if p == nil {
		t.Fatal("缺少 Proxy 组")
	}
	found := false
	for _, m := range p.Outbounds {
		if m == "住宅" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Proxy 成员应含链式代理(按名称),实际 %v", p.Outbounds)
	}
}

// 链式代理的前置可以是筛选组:detour 指向筛选组的出站名。
func TestChainFrontByFilter(t *testing.T) {
	cfg := smboxLikeConfig()
	cfg.Chains = []config.Chain{
		{ID: "c1", Name: "住宅", Type: config.ChainSocks5, Host: "1.2.3.4", Port: 1080, FrontGroupID: "filter:f1"},
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("筛选组做前置应能编译: %v", err)
	}
	var chain *render.SBOutbound
	for i := range sb.Outbounds {
		if sb.Outbounds[i].Tag == "住宅" {
			chain = &sb.Outbounds[i]
		}
	}
	if chain == nil {
		t.Fatal("链式出站未生成")
	}
	if chain.Detour != "🇯🇵 日本" {
		t.Fatalf("detour 应指向筛选组出站名,实际 %q", chain.Detour)
	}
}

// 重名节点/组必须被分配唯一 tag:sing-box 里两个同名出站会直接 FATAL。
// 常见场景:两个订阅有同名节点、组名与筛选名相同。
func TestDuplicateNamesGetUniqueTags(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.DNS.FakeIP = false
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "日本 01", Type: "vless", Server: "1.1.1.1", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "n2", Name: "日本 01", Type: "vless", Server: "1.1.1.2", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "n3", Name: "日本 01", Type: "vless", Server: "1.1.1.3", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
	}
	cfg.Groups = []config.Group{
		{ID: "g1", Name: "日本 01", Type: config.GroupSelect, Members: []string{"node:n1", "node:n2"}},
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("重名配置应能编译(自动加后缀): %v", err)
	}
	seen := map[string]bool{}
	for _, o := range sb.Outbounds {
		if seen[o.Tag] {
			t.Fatalf("出站 tag 重复: %s", o.Tag)
		}
		seen[o.Tag] = true
	}
	// 首个使用者保留原名,后来者加 ID 尾缀
	names := []string{}
	for _, o := range sb.Outbounds {
		names = append(names, o.Tag)
	}
	t.Logf("分配结果: %v", names)
	if !seen["日本 01"] {
		t.Fatal("第一个使用者应保留原名")
	}
}

// 真实内核必须接受重名生成的配置。
func TestDuplicateNamesRunOnKernel(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("无 sing-box")
	}
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.Inbounds.ClashAPI = true
	cfg.Inbounds.ClashAPIPort = freePort(t) + 1
	cfg.Inbounds.AllowLAN = false
	cfg.DNS.FakeIP = false
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "同名节点", Type: "vless", Server: "1.1.1.1", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
		{ID: "n2", Name: "同名节点", Type: "vless", Server: "1.1.1.2", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
	}
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
		t.Fatalf("重名配置内核拒绝:\n%s", out)
	}
}

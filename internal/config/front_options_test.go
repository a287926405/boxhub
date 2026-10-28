package config_test

import (
	"testing"

	"boxhub/internal/config"
)

func frontCfg() *config.Config {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "JP 01", Type: "vless", Server: "1.1.1.1", Port: 443, UUID: "11111111-2222-3333-4444-555555555555"},
	}
	cfg.Groups = []config.Group{
		{ID: "g-main", Name: "main", Type: config.GroupSelect, Members: []string{"node:n1"}},
		{ID: "g-front", Name: "front", Type: config.GroupSelect, Members: []string{"node:n1"}},
		{ID: "g-chain", Name: "haschain", Type: config.GroupSelect, Members: []string{"node:n1", "chain:c1"}},
		{ID: "g-empty", Name: "empty", Type: config.GroupSelect, Members: nil},
		{ID: "g-dyn", Name: "dyn", Type: config.GroupDynamic, Keywords: []string{"JP"}},
	}
	cfg.Chains = []config.Chain{
		{ID: "c1", Name: "home", Type: "socks5", Host: "1.1.1.2", Port: 1080, FrontGroupID: "g-front"},
	}
	return cfg
}

func names(gs []*config.Group) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// 新建场景:空组/动态组/含链组都不该出现在候选里。
// 空组选了保存会报错(白填表单),含链组容易成环。
func TestFrontOptionsNew(t *testing.T) {
	cfg := frontCfg()
	r := config.NewResolver(cfg)
	got := names(r.FrontOptions(""))
	if has(got, "empty") {
		t.Errorf("空组不该作为前置候选(选了也保存不了): %v", got)
	}
	if has(got, "dyn") {
		t.Errorf("动态组不该作为前置候选: %v", got)
	}
	if has(got, "haschain") {
		t.Errorf("新建时含链式代理的组不该作为前置候选: %v", got)
	}
	if !has(got, "main") || !has(got, "front") {
		t.Errorf("普通可用组应在候选里: %v", got)
	}
}

// 编辑场景:链式代理自己的前置组必须始终在候选里,
// 否则打开编辑框时原选择"消失",保存后前置就丢了。
func TestFrontOptionsEditKeepsCurrent(t *testing.T) {
	cfg := frontCfg()
	// 让 c1 的前置是一个含别的链式代理的组(合法:只要不成环)
	cfg.Chains = append(cfg.Chains, config.Chain{
		ID: "c2", Name: "home2", Type: "socks5", Host: "1.1.1.3", Port: 1080, FrontGroupID: "g-front",
	})
	cfg.Groups[2].Members = []string{"node:n1", "chain:c2"} // haschain 含 c2
	cfg.Chains[0].FrontGroupID = "g-chain"                  // c1 前置 = haschain
	r := config.NewResolver(cfg)

	got := names(r.FrontOptions("c1"))
	if !has(got, "haschain") {
		t.Errorf("编辑 c1 时,它当前使用的前置组必须保留在候选里: %v", got)
	}
	// 但含 c1 自己的组必须被排除(会成环)
	cfg.Groups[2].Members = append(cfg.Groups[2].Members, "chain:c1")
	got2 := names(r.FrontOptions("c1"))
	if has(got2, "haschain") {
		t.Errorf("包含 c1 自身的组必须排除: %v", got2)
	}
}


// 筛选组可作为前置:FrontChoices 同时返回节点组与筛选组,
// 引用格式为 "filter:<id>" / "group:<id>"。
func TestFrontChoicesIncludeFilters(t *testing.T) {
	cfg := frontCfg()
	cfg.Filters = []config.Filter{
		{ID: "f1", Name: "JP组", Enabled: true, Keywords: []string{"JP"}},
		{ID: "f2", Name: "空组", Enabled: true, Keywords: []string{"不存在"}},
		{ID: "f3", Name: "仅统计", Enabled: true, CountOnly: true, Keywords: []string{"JP"}},
	}
	r := config.NewResolver(cfg)
	opts := r.FrontChoices("")
	var kinds = map[string]string{}
	for _, o := range opts {
		kinds[o.Name] = o.Kind
	}
	if kinds["JP组"] != "filter" {
		t.Errorf("命中节点的筛选组应出现在前置候选: %v", kinds)
	}
	if _, ok := kinds["空组"]; ok {
		t.Errorf("没命中任何节点的筛选组不该出现(选了保存不了): %v", kinds)
	}
	if _, ok := kinds["仅统计"]; ok {
		t.Errorf("仅统计的筛选组不该出现: %v", kinds)
	}
	if kinds["front"] != "group" {
		t.Errorf("节点组仍应在候选里: %v", kinds)
	}
}

// 筛选组做前置时,ValidateAll 通过;筛选被删后 CleanupDangling 正确禁用。
func TestFilterFrontValidation(t *testing.T) {
	cfg := frontCfg()
	cfg.Filters = []config.Filter{
		{ID: "f1", Name: "JP组", Enabled: true, Keywords: []string{"JP"}},
	}
	cfg.Chains[0].FrontGroupID = "filter:f1"
	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		t.Fatalf("筛选组做前置应合法: %v", errs)
	}
	// 删掉筛选组后清理:链应被禁用而不是崩溃
	cfg.Filters = nil
	config.CleanupDangling(cfg)
	if !cfg.Chains[0].Disabled || cfg.Chains[0].FrontGroupID != "" {
		t.Fatalf("前置筛选被删后,链应被禁用并清空前置: %+v", cfg.Chains[0])
	}
}

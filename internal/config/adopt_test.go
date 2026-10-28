package config_test

import (
	"testing"

	"boxhub/internal/config"
)

func node(id string) config.Node {
	return config.Node{ID: id, Name: id, Type: "shadowsocks", Server: "1.2.3.4", Port: 8388, Method: "aes-256-gcm", Source: "manual"}
}

// 空组应该自动吸收新节点——首装后导入订阅,用户不该看到"有节点但组是空的"。
func TestAdoptNewMembersIntoEmptyGroup(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{node("a"), node("b")}
	cfg.Groups = []config.Group{
		{ID: "g1", Name: "手动选择", Type: config.GroupSelect},
		{ID: "g2", Name: "前置线路", Type: config.GroupSelect},
	}
	notes := config.AdoptNewMembers(cfg)
	if len(notes) != 2 {
		t.Fatalf("应有两个组被补成员,实际 %v", notes)
	}
	for _, g := range cfg.Groups {
		if len(g.Members) != 2 {
			t.Fatalf("组「%s」应有 2 个成员,实际 %v", g.Name, g.Members)
		}
	}
	if cfg.Groups[0].Members[0] != "node:a" || cfg.Groups[0].Members[1] != "node:b" {
		t.Fatalf("成员顺序应与节点顺序一致: %v", cfg.Groups[0].Members)
	}
}

// 已有成员的组绝不能被自动改动——用户的手动选择必须保留。
func TestAdoptDoesNotTouchNonEmptyGroup(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{node("a"), node("b"), node("c")}
	cfg.Groups = []config.Group{
		{ID: "g1", Name: "我选好的", Type: config.GroupSelect, Members: []string{"node:c"}},
		{ID: "g2", Name: "空的", Type: config.GroupSelect},
	}
	config.AdoptNewMembers(cfg)
	if len(cfg.Groups[0].Members) != 1 || cfg.Groups[0].Members[0] != "node:c" {
		t.Fatalf("非空组被改动了: %v", cfg.Groups[0].Members)
	}
	if len(cfg.Groups[1].Members) != 3 {
		t.Fatalf("空组应被填充: %v", cfg.Groups[1].Members)
	}
}

// 动态组不参与自动补成员(它的成员靠关键词)。
func TestAdoptSkipsDynamicGroup(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{node("a")}
	cfg.Groups = []config.Group{
		{ID: "g1", Name: "动态", Type: config.GroupDynamic, Keywords: []string{"香港"}},
	}
	config.AdoptNewMembers(cfg)
	if len(cfg.Groups[0].Members) != 0 {
		t.Fatalf("动态组不该被填入成员: %v", cfg.Groups[0].Members)
	}
}

// 禁用节点不该被加入组。
func TestAdoptSkipsDisabledNodes(t *testing.T) {
	cfg := config.NewDefault()
	n1 := node("a")
	n2 := node("b")
	n2.Disabled = true
	cfg.Nodes = []config.Node{n1, n2}
	cfg.Groups = []config.Group{{ID: "g1", Name: "组", Type: config.GroupSelect}}
	config.AdoptNewMembers(cfg)
	if len(cfg.Groups[0].Members) != 1 || cfg.Groups[0].Members[0] != "node:a" {
		t.Fatalf("只应收录启用的节点: %v", cfg.Groups[0].Members)
	}
}

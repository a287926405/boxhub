package config_test

import (
	"testing"

	"boxhub/internal/config"
)

func mkNode(id, name string) config.Node {
	return config.Node{ID: id, Name: name, Type: "vless", Server: "1.2.3.4", Port: 443, Source: "manual"}
}

// 关键词 OR 逻辑:任一命中即入选
func TestFilterKeywordOR(t *testing.T) {
	f := config.Filter{Name: "日本", Keywords: []string{"日本", "JP", "东京"}}
	cases := map[string]bool{
		"日本 01":     true,
		"JP-Premium":  true,
		"东京 直连":     true,
		"美国 01":     false,
		"japan node":  false, // 只认配置的关键词,不做额外推断
	}
	for name, want := range cases {
		if got := f.Match(name); got != want {
			t.Errorf("Match(%q) = %v, 期望 %v", name, got, want)
		}
	}
}

// AND 关键词:必须全部包含
func TestFilterKeywordAND(t *testing.T) {
	f := config.Filter{Name: "日本专线", KeywordsAnd: []string{"日本", "专线"}}
	if !f.Match("日本 专线 01") {
		t.Error("应命中同时含两个词的节点")
	}
	if f.Match("日本 普通 01") {
		t.Error("缺少「专线」不该命中")
	}
	if f.Match("美国 专线 01") {
		t.Error("缺少「日本」不该命中")
	}
}

// 排除词优先于入选
func TestFilterExclude(t *testing.T) {
	f := config.Filter{Name: "日本", Keywords: []string{"日本"}, Exclude: []string{"备用", "过期"}}
	if !f.Match("日本 01") {
		t.Error("正常节点应命中")
	}
	if f.Match("日本 01 备用") {
		t.Error("含排除词不该命中")
	}
	if f.Match("日本 过期节点") {
		t.Error("含排除词不该命中")
	}
}

// 正则与关键词是 AND 关系
func TestFilterRegex(t *testing.T) {
	f := config.Filter{Name: "JP 编号", Keywords: []string{"JP"}, Regex: `^JP\s+\d+$`}
	if !f.Match("JP 12") {
		t.Error("应命中")
	}
	if f.Match("JP ABC") {
		t.Error("正则不匹配不该命中")
	}
	if f.Match("US 12") {
		t.Error("缺关键词不该命中")
	}
}

// 只有正则、没有关键词时也能工作
func TestFilterRegexOnly(t *testing.T) {
	f := config.Filter{Name: "纯正则", Regex: `^CF-`}
	if !f.Match("CF-01") {
		t.Error("应命中")
	}
	if f.Match("XX-01") {
		t.Error("不该命中")
	}
}

// 地区识别
func TestGuessRegion(t *testing.T) {
	cases := map[string]string{
		"🇯🇵 日本 01": "jp",
		"香港 HK-01":  "hk",
		"US West":    "us",
		"未知节点":       "other",
		"台湾 中华电信":    "tw",
	}
	for name, want := range cases {
		if got := config.GuessRegion(name); got != want {
			t.Errorf("GuessRegion(%q) = %q, 期望 %q", name, got, want)
		}
	}
}

// 被禁用节点不参与筛选
func TestFilterSkipsDisabled(t *testing.T) {
	f := config.Filter{Name: "日本", Keywords: []string{"日本"}}
	n1 := mkNode("a", "日本 01")
	n2 := mkNode("b", "日本 02")
	n2.Disabled = true
	got := f.Matches([]config.Node{n1, n2})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("只应收录启用节点,实际 %d 个", len(got))
	}
}

// MaxNodes 上限
func TestFilterMaxNodes(t *testing.T) {
	f := config.Filter{Name: "日本", Keywords: []string{"日本"}, MaxNodes: 2}
	nodes := []config.Node{
		mkNode("a", "日本 01"), mkNode("b", "日本 02"), mkNode("c", "日本 03"),
	}
	if got := f.Matches(nodes); len(got) != 2 {
		t.Fatalf("应限制为 2 个,实际 %d", len(got))
	}
}

// 自动生成地区筛选:不重复创建已有的
func TestAutoFilters(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{
		mkNode("a", "日本 01"), mkNode("b", "日本 02"),
		mkNode("c", "香港 01"), mkNode("d", "美国 01"),
		mkNode("e", "未知节点"),
	}
	id := 0
	newID := func() string { id++; return "f" + string(rune('0'+id)) }
	created := config.AutoFilters(cfg, newID)
	if len(created) != 3 {
		t.Fatalf("应生成 3 个地区筛选(日/港/美),实际 %d: %+v", len(created), created)
	}
	// 再跑一次不应重复创建
	again := config.AutoFilters(cfg, newID)
	if len(again) != 0 {
		t.Fatalf("不应重复创建,实际 %d", len(again))
	}
	// 验证命中数
	stats := config.ComputeFilterStats(cfg)
	total := 0
	for _, s := range stats {
		total += s.Count
	}
	if total != 4 {
		t.Fatalf("4 个可识别地区的节点应被覆盖,实际 %d", total)
	}
}

// 校验:空关键词要报错
func TestFilterValidate(t *testing.T) {
	bad := config.Filter{Name: "空"}
	if err := bad.Validate(); err == nil {
		t.Error("没有任何关键词应报错")
	}
	bad2 := config.Filter{Name: "坏正则", Regex: "("}
	if err := bad2.Validate(); err == nil {
		t.Error("无效正则应报错")
	}
	good := config.Filter{Name: "正常", Keywords: []string{"日本"}}
	if err := good.Validate(); err != nil {
		t.Errorf("合法配置不该报错: %v", err)
	}
}

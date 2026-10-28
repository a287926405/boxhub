package render_test

// 条件组编译:对齐 SMBox 的关键逻辑。
//   - 同族条件合并进一条规则(sing-box 同字段多值天然 OR)
//   - 不同族拆成多条规则(否则会被 sing-box 当 AND)
//   - 组内 AND 用 logical 规则(mode=and)包族规则,子片段不带 action/outbound

import (
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func condCfg() *config.Config {
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.ClashAPI = false
	cfg.DNS.FakeIP = false
	cfg.Policies = []config.Policy{
		{ID: "pp", Name: "私网", Enabled: true, Target: "direct",
			Match: config.Match{DomainSuffix: []string{"local"}}},
		{
			ID: "ins", Name: "INS", Enabled: true, Target: "self", AsGroup: true,
			Groups: config.ConditionGroups{
				{Mode: "or", Items: []config.RuleCondition{
					{Type: config.CondRuleSet, Values: []string{"https://example.com/instagram.srs", "https://example.com/facebook.srs"}},
					{Type: config.CondDomain, Values: []string{"fbsbx.com"}},
				}},
			},
		},
		{
			ID: "andrule", Name: "组合且", Enabled: true, Target: "direct",
			Groups: config.ConditionGroups{
				{Mode: "and", Items: []config.RuleCondition{
					{Type: config.CondDomainSuffix, Values: []string{"cdn.example.com"}},
					{Type: config.CondPort, Values: []string{"8080"}},
				}},
			},
		},
	}
	return cfg
}

func TestConditionGroupCompile(t *testing.T) {
	sb, err := render.Build(condCfg(), render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	// INS 应拆成两条:一条 rule_set(含两个规则集),一条 domain
	var ruleSetRule, domainRule, logical *sbRuleAlias
	for i := range sb.Route.Rules {
		r := &sb.Route.Rules[i]
		if strings.Contains(strings.Join(r.RuleSet, ","), "example-cominstagram") && len(r.Domain) == 0 {
			ruleSetRule = (*sbRuleAlias)(r)
		}
		if len(r.Domain) > 0 && r.Domain[0] == "fbsbx.com" {
			domainRule = (*sbRuleAlias)(r)
		}
		if r.Action == "logical" {
			logical = (*sbRuleAlias)(r)
		}
	}
	if ruleSetRule == nil || domainRule == nil {
		t.Fatalf("同族合并+异族拆分失败: rule_set=%v domain=%v", ruleSetRule, domainRule)
	}
	if logical == nil {
		t.Fatalf("AND 组应生成 logical 规则: %v", sb.Route.Rules)
	}
	if len(logical.Rules) != 2 || logical.Mode != "and" {
		t.Fatalf("logical 子片段不对: %+v", logical)
	}
	for _, sub := range logical.Rules {
		if sub.Action != "" || sub.Outbound != "" {
			t.Fatalf("logical 子片段不能带动作: %+v", sub)
		}
	}
}

type sbRuleAlias = render.SBRule

func TestConditionValidationRejectsBadValues(t *testing.T) {
	badCases := []config.RuleCondition{
		{Type: config.CondRuleSet, Values: []string{"ftp://bad"}},
		{Type: config.CondDestIP, Values: []string{"1.2.3.999"}},
		{Type: config.CondPort, Values: []string{"abc"}},
		{Type: config.CondPortRange, Values: []string{"9000-6000"}},
		{Type: config.CondNetwork, Values: []string{"icmp"}},
	}
	for _, c := range badCases {
		g := config.ConditionGroup{Items: []config.RuleCondition{c}}
		if err := g.Validate(); err == nil {
			t.Errorf("非法条件应被拒绝: %+v", c)
		}
	}
	ok := config.ConditionGroup{Items: []config.RuleCondition{
		{Type: config.CondDestIP, Values: []string{"geoip:cn", "10.0.0.0/8"}},
		{Type: config.CondPortRange, Values: []string{"6000-6500"}},
	}}
	if err := ok.Validate(); err != nil {
		t.Errorf("合法条件被误拒: %v", err)
	}
}

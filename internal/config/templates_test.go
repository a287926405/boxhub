package config_test

import (
	"strings"
	"testing"

	"boxhub/internal/config"
)

// 模板必须完整、可用:名称唯一、地址合法、有规则集。
func TestRuleTemplatesValid(t *testing.T) {
	tpls := config.RuleTemplates()
	if len(tpls) < 10 {
		t.Fatalf("模板数量过少: %d", len(tpls))
	}
	names := map[string]bool{}
	keys := map[string]bool{}
	for _, tp := range tpls {
		if tp.Name == "" || tp.Key == "" {
			t.Errorf("模板缺少名称或 key: %+v", tp)
			continue
		}
		if names[tp.Name] {
			t.Errorf("模板名称重复: %s", tp.Name)
		}
		names[tp.Name] = true
		if keys[tp.Key] {
			t.Errorf("模板 key 重复: %s", tp.Key)
		}
		keys[tp.Key] = true
		if len(tp.RuleSets) == 0 {
			t.Errorf("模板 %s 没有规则集", tp.Name)
		}
		for _, u := range tp.RuleSets {
			if !strings.HasPrefix(u, "https://") {
				t.Errorf("模板 %s 的规则集地址非法: %s", tp.Name, u)
			}
			if !strings.HasSuffix(u, ".srs") && !strings.HasSuffix(u, ".json") {
				t.Errorf("模板 %s 的规则集后缀不合法: %s", tp.Name, u)
			}
		}
		// 走代理的模板都应该生成选线组
		if tp.Target == "" && !tp.AsGroup {
			t.Errorf("走代理的模板 %s 应生成选线组", tp.Name)
		}
	}
}

// 与 SMBox 对齐的关键分类必须存在。
func TestRuleTemplatesMatchSMBox(t *testing.T) {
	want := []string{"CN", "CN-CDN", "Youtube", "Telegram", "Netflix", "AI", "INS", "Google", "Github"}
	got := map[string]bool{}
	for _, tp := range config.RuleTemplates() {
		got[tp.Name] = true
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("缺少与 SMBox 对齐的分类: %s", n)
		}
	}
	// 直连类必须是 direct
	for _, tp := range config.RuleTemplates() {
		if tp.Name == "CN" || tp.Name == "CN-CDN" {
			if tp.Target != "direct" {
				t.Errorf("%s 应该直连,实际 target=%q", tp.Name, tp.Target)
			}
			if tp.AsGroup {
				t.Errorf("%s 直连规则不该生成选线组", tp.Name)
			}
		}
	}
}

// 应用模板后应生成合法的 Policy,且能被校验通过。
func TestApplyTemplate(t *testing.T) {
	cfg := config.NewDefault()
	id := 0
	newID := func() string {
		id++
		return "p" + string(rune('0'+id))
	}
	for _, tp := range config.RuleTemplates() {
		if config.AlreadyHasTemplate(cfg, tp) {
			continue
		}
		cfg.Policies = append(cfg.Policies, config.ApplyTemplate(tp, newID))
	}
	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("校验失败: %v", e)
		}
	}
	// 幂等:再判断一次应该都认为已添加
	for _, tp := range config.RuleTemplates() {
		if tp.Key == "steam" || tp.Key == "notcn" {
			continue
		}
		if !config.AlreadyHasTemplate(cfg, tp) {
			t.Errorf("模板 %s 应用后应被识别为已添加", tp.Name)
		}
	}
}

// 规则名即出站组名:模板名必须是合法且不含保留字。
func TestTemplateNamesAreSafeTags(t *testing.T) {
	reserved := map[string]bool{"Proxy": true, "direct": true, "block": true}
	for _, tp := range config.RuleTemplates() {
		if reserved[tp.Name] {
			t.Errorf("模板名 %s 是保留字,不能作为出站 tag", tp.Name)
		}
	}
}

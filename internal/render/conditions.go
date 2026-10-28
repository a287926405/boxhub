package render

// 条件组 → sing-box 规则编译。
//
// sing-box 的匹配语义决定了拆分方式(这是 SMBox 生成配置里一个 INS 规则
// 出现三条路由规则的根因):
//   - 同一条规则内,不同字段(domain / domain_suffix / rule_set / port ...)是 **AND**;
//   - 同一字段的多个值是 **OR**。
//
// 所以:
//   - 组内 OR → 每个字段各出一条规则,同字段的多个条件合并取值;
//   - 组内 AND → 生成 action=logical、mode=and 的规则,把各字段包成
//     纯匹配子片段(子片段不能带 action/outbound);
//   - 组间 → 天然 OR,各自出一批规则,出口相同。
//
// 特殊展开:目标 IP 条件里的 "geoip:xx" 会转成规则集引用(与 ip_cidr 是不同
// 字段,OR 模式下拆成两条规则 —— 与 SMBox 完全一致)。

import (
	"fmt"
	"strconv"
	"strings"

	"boxhub/internal/config"
)

// fieldRule 是一个"字段桶":同名字段的取值集合。
type fieldRule struct {
	domain        []string
	domainSuffix  []string
	domainKeyword []string
	ruleSet       []string
	ipCIDR        []string
	sourceIP      []string
	port          []int
	portRange     []string
	network       []string
}

func (f *fieldRule) empty() bool {
	return len(f.domain)+len(f.domainSuffix)+len(f.domainKeyword)+len(f.ruleSet)+
		len(f.ipCIDR)+len(f.sourceIP)+len(f.port)+len(f.portRange)+len(f.network) == 0
}

// add 写入一条条件;返回错误用于非法值。
func (f *fieldRule) add(c config.RuleCondition, addRuleSet func(string) string) error {
	for _, v := range c.Values {
		switch c.Type {
		case config.CondDomain:
			f.domain = appendUnique(f.domain, v)
		case config.CondDomainSuffix:
			f.domainSuffix = appendUnique(f.domainSuffix, v)
		case config.CondDomainKeyword:
			f.domainKeyword = appendUnique(f.domainKeyword, v)
		case config.CondRuleSet:
			if t := addRuleSet(v); t != "" {
				f.ruleSet = appendUnique(f.ruleSet, t)
			}
		case config.CondDestIP:
			switch {
			case strings.HasPrefix(v, "geoip:"), strings.HasPrefix(v, "geosite:"):
				if t := addRuleSet(v); t != "" {
					f.ruleSet = appendUnique(f.ruleSet, t)
				}
			default:
				f.ipCIDR = appendUnique(f.ipCIDR, normalizeCIDR(v))
			}
		case config.CondSourceIP:
			f.sourceIP = appendUnique(f.sourceIP, normalizeCIDR(v))
		case config.CondPort:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("端口无效: %q", v)
			}
			f.port = appendUniqueInt(f.port, n)
		case config.CondPortRange:
			lo, hi, err := parsePortRange(v)
			if err != nil {
				return err
			}
			f.portRange = appendUnique(f.portRange, fmt.Sprintf("%d-%d", lo, hi))
		case config.CondNetwork:
			f.network = appendUnique(f.network, v)
		default:
			return fmt.Errorf("不支持的条件类型 %q", c.Type)
		}
	}
	return nil
}

func parsePortRange(v string) (int, int, error) {
	sep := "-"
	if strings.Contains(v, ":") {
		sep = ":"
	}
	parts := strings.SplitN(v, sep, 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("端口范围无效: %q", v)
	}
	lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("端口范围无效: %q", v)
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("端口范围起点不能大于终点: %q", v)
	}
	return lo, hi, nil
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func appendUniqueInt(xs []int, v int) []int {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// bucket 把组内条件按字段归桶。
func bucketGroup(g config.ConditionGroup, addRuleSet func(string) string) (*fieldRule, error) {
	f := &fieldRule{}
	for _, c := range g.Items {
		if err := f.add(c, addRuleSet); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// expand 把字段桶展开成"每字段一条纯匹配规则"(OR 语义)。
func (f *fieldRule) expand() []SBRule {
	var out []SBRule
	if len(f.domain) > 0 {
		out = append(out, SBRule{Domain: f.domain})
	}
	if len(f.domainSuffix) > 0 {
		out = append(out, SBRule{DomainSuffix: f.domainSuffix})
	}
	if len(f.domainKeyword) > 0 {
		out = append(out, SBRule{DomainKeyword: f.domainKeyword})
	}
	if len(f.ruleSet) > 0 {
		out = append(out, SBRule{RuleSet: f.ruleSet})
	}
	if len(f.ipCIDR) > 0 {
		out = append(out, SBRule{IPCIDR: f.ipCIDR})
	}
	if len(f.sourceIP) > 0 {
		out = append(out, SBRule{ClientIP: f.sourceIP})
	}
	if len(f.port) > 0 {
		out = append(out, SBRule{Port: f.port})
	}
	if len(f.portRange) > 0 {
		out = append(out, SBRule{PortRange: f.portRange})
	}
	if len(f.network) > 0 {
		out = append(out, SBRule{Network: f.network})
	}
	return out
}

// policyRulesFromGroups 编译条件组为路由规则组。
func policyRulesFromGroups(p config.Policy, groups config.ConditionGroups, outbound string, addRuleSet func(string) string) ([]SBRule, error) {
	var rules []SBRule
	for gi, g := range groups {
		f, err := bucketGroup(g, addRuleSet)
		if err != nil {
			return nil, fmt.Errorf("条件组 %d: %w", gi+1, err)
		}
		if f.empty() {
			continue
		}
		parts := f.expand()
		if g.Mode == "and" && len(parts) > 1 {
			// AND:各字段规则做子片段(不带 action/outbound),外层 logical 定出口
			subs := make([]SBLogicalRule, 0, len(parts))
			for _, r := range parts {
				subs = append(subs, SBLogicalRule(r))
			}
			rules = append(rules, SBRule{
				Action: "logical", Mode: "and", Rules: subs,
				Outbound: outbound, Invert: p.Invert,
			})
			continue
		}
		for _, r := range parts {
			r.Action = "route"
			r.Outbound = outbound
			r.Invert = p.Invert
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// normalizeCIDR 裸 IP 自动补全主机掩码(/32 或 /128)。
func normalizeCIDR(v string) string {
	if strings.Contains(v, "/") {
		return v
	}
	if strings.Contains(v, ":") {
		return v + "/128"
	}
	return v + "/32"
}

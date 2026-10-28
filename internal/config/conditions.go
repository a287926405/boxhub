// Package config(续):规则条件组模型。
//
// 对齐 SMBox 的「编辑规则」弹窗:一条规则由**多个条件组**组成,
// 组与组之间是 OR(任一命中即走这条规则的出口);组内可选 OR 或 AND。
// 每个条件是一个「类型 + 值列表」:
//
//	规则集(远程 srs/json) / 域名后缀 / 域名关键字 / 精确域名 /
//	源 IP(CIDR) / 目标 IP(CIDR 或 geoip 引用) / 目标端口 /
//	目标端口范围 / 网络类型(tcp/udp)
//
// 编译到 sing-box 时按"域名族 / IP 族"拆开:
//   - 域名族条件合成一条规则(同族多值是天然 OR,不同族不能混在一条规则里,
//     否则 sing-box 会把它们当 AND);
//   - IP 族、端口、网络各自成规则;
//   - 组内 AND 时用 logical 规则(mode=and)把各族包起来。
// 这正是 SMBox 生成配置里 {"rule_set":["INS-r0-0","INS-r0-2"]} 与
// {"rule_set":["INS-r0-1"]} 分成两条的原因。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// ConditionType 条件类型。与 SMBox 下拉一一对应。
type ConditionType string

const (
	CondRuleSet      ConditionType = "rule_set"       // 规则集 URL
	CondDomainSuffix ConditionType = "domain_suffix"  // 域名后缀
	CondDomainKeyword ConditionType = "domain_keyword" // 域名关键字
	CondDomain       ConditionType = "domain"         // 完整域名
	CondSourceIP     ConditionType = "source_ip_cidr" // 源 IP
	CondDestIP       ConditionType = "ip_cidr"        // 目标 IP
	CondPort         ConditionType = "port"           // 目标端口
	CondPortRange    ConditionType = "port_range"     // 目标端口范围
	CondNetwork      ConditionType = "network"        // tcp/udp
)

// ConditionTypeLabels 下拉显示顺序与文案(SMBox 同款)。
var ConditionTypeLabels = []struct {
	Type ConditionType
	Name string
}{
	{CondRuleSet, "规则集"},
	{CondDomainSuffix, "域名后缀"},
	{CondDomainKeyword, "域名关键字"},
	{CondDomain, "域名"},
	{CondSourceIP, "源IP"},
	{CondDestIP, "目标IP"},
	{CondPort, "目标端口"},
	{CondPortRange, "目标端口范围"},
	{CondNetwork, "网络类型"},
}

// RuleCondition 一条条件(类型 + 多个值)。
type RuleCondition struct {
	Type   ConditionType `json:"type"`
	Values []string      `json:"values"`
}

// ConditionGroup 一个条件组:组内多条条件按 Mode 组合。
type ConditionGroup struct {
	// Mode 组内组合方式:"or"(同族合并为一条 sing-box 规则)或 "and"(logical 规则)。
	Mode string `json:"mode,omitempty"`
	Items []RuleCondition `json:"items"`
}

// ConditionGroups 规则的多个条件组(组间 OR)。
// 空表示规则没有条件(旧数据迁移前的占位)。
type ConditionGroups []ConditionGroup

// Validate 检查条件组的类型与值格式。
func (g ConditionGroup) Validate() error {
	switch g.Mode {
	case "", "or", "and":
	default:
		return fmt.Errorf("组内匹配模式只能是 or 或 and,实际 %q", g.Mode)
	}
	if len(g.Items) == 0 {
		return fmt.Errorf("条件组不能为空")
	}
	for _, c := range g.Items {
		if len(c.Values) == 0 {
			return fmt.Errorf("条件「%s」没有值", c.Type)
		}
		for _, v := range c.Values {
			if err := validateConditionValue(c.Type, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateConditionValue(t ConditionType, v string) error {
	switch t {
	case CondRuleSet:
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return fmt.Errorf("规则集地址必须是 http(s):// 开头: %q", v)
		}
	case CondSourceIP, CondDestIP:
		if strings.HasPrefix(v, "geoip:") || strings.HasPrefix(v, "geosite:") {
			return nil // geo 引用交给渲染层展开
		}
		if err := validateCIDROrIP(v); err != nil {
			return fmt.Errorf("%s 无效(%s): %v", labelOfType(t), v, err)
		}
	case CondPort:
		if !regexp.MustCompile(`^\d{1,5}$`).MatchString(v) {
			return fmt.Errorf("端口无效: %q", v)
		}
	case CondPortRange:
		m := regexp.MustCompile(`^(\d{1,5})(:|-)(\d{1,5})$`).FindStringSubmatch(v)
		if m == nil {
			return fmt.Errorf("端口范围应为 起-止 或 起:止,实际 %q", v)
		}
		// 格式对还不够:起 > 止 内核会拒绝,保存时就要拦
		var lo, hi int
		fmt.Sscanf(m[1], "%d", &lo)
		fmt.Sscanf(m[3], "%d", &hi)
		if lo > hi {
			return fmt.Errorf("端口范围起点不能大于终点: %q", v)
		}
	case CondNetwork:
		if v != "tcp" && v != "udp" {
			return fmt.Errorf("网络类型只能是 tcp 或 udp,实际 %q", v)
		}
	}
	return nil
}

func labelOfType(t ConditionType) string {
	for _, l := range ConditionTypeLabels {
		if l.Type == t {
			return l.Name
		}
	}
	return string(t)
}

// TypeSummary 返回条件组涉及的类型中文名(去重,用于卡片标签)。
func (g ConditionGroup) TypeSummary() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range g.Items {
		n := labelOfType(c.Type)
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// CountConditions 全部条件组的条件总数(卡片上的"规则数")。
func (cg ConditionGroups) CountConditions() int {
	n := 0
	for _, g := range cg {
		n += len(g.Items)
	}
	return n
}

// FromMatch 把旧的扁平 Match 转成条件组(单一 OR 组)。
// 只用于读取旧配置时的自动迁移,保存后即为条件组格式。
func FromMatch(m Match) ConditionGroups {
	var items []RuleCondition
	add := func(t ConditionType, vals []string) {
		if len(vals) > 0 {
			items = append(items, RuleCondition{Type: t, Values: vals})
		}
	}
	add(CondDomain, m.Domain)
	add(CondDomainSuffix, m.DomainSuffix)
	add(CondDomainKeyword, m.DomainKeyword)
	for _, r := range m.DomainRegex {
		_ = r // 旧版正则条件并入原始 JSON 逃生口处理,条件组暂无对应类型
	}
	if ipVals := ipRefValues(m); len(ipVals) > 0 {
		items = append(items, RuleCondition{Type: CondDestIP, Values: ipVals})
	}
	add(CondSourceIP, m.ClientIP)
	add(CondPort, intListToStrs(m.Port))
	add(CondPortRange, m.PortRange)
	add(CondNetwork, m.Network)
	add(CondRuleSet, m.RuleSets)
	if len(items) == 0 {
		return nil
	}
	return ConditionGroups{{Mode: "or", Items: items}}
}

// ipRefValues 把 GeoIP/IPCIDR 合并成一个目标 IP 条件的值列表
// (geoip:cn 形式由渲染层展开成规则集引用)。
func ipRefValues(m Match) []string {
	var vals []string
	for _, g := range m.GeoIP {
		vals = append(vals, "geoip:"+g)
	}
	vals = append(vals, m.IPCIDR...)
	return vals
}

// validateCIDROrIP 接受单个 IP 或 CIDR。
func validateCIDROrIP(v string) error {
	if net.ParseIP(v) != nil {
		return nil
	}
	if _, _, err := net.ParseCIDR(v); err == nil {
		return nil
	}
	return fmt.Errorf("不是合法的 IP/CIDR")
}

func intListToStrs(xs []int) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, fmt.Sprint(x))
	}
	return out
}


// ValidatePolicy 校验一条规则的出口与条件:
// 条件组优先;没有条件组时扁平 Match 至少得有一项。
func ValidatePolicy(p Policy) error {
	if strings.TrimSpace(p.RawJSON) != "" {
		// 自定义 JSON:结构合法即可(是不是有效规则由内核 check 把关)
		var probe []map[string]any
		if err := json.Unmarshal([]byte(p.RawJSON), &probe); err != nil {
			return fmt.Errorf("自定义规则 JSON 无法解析: %w", err)
		}
		if len(probe) == 0 {
			return fmt.Errorf("自定义规则 JSON 至少包含一条规则")
		}
		return nil
	}
	if p.PhoneOnly {
		for _, c := range p.PhoneCIDRs {
			if net.ParseIP(c) == nil {
				if _, _, err := net.ParseCIDR(c); err != nil {
					return fmt.Errorf("手机设备网段无效: %q", c)
				}
			}
		}
	}
	if len(p.Groups) > 0 {
		for i, g := range p.Groups {
			if err := g.Validate(); err != nil {
				return fmt.Errorf("条件组 %d: %w", i+1, err)
			}
		}
		return nil
	}
	if p.Match.Empty() {
		return fmt.Errorf("没有填写任何匹配条件")
	}
	return nil
}

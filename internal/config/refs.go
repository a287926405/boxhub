package config

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// compileRegex 校验用户输入的正则。
func compileRegex(expr string) (*regexp.Regexp, error) { return regexp.Compile(expr) }

// 本文件解决链式代理最容易出错的地方:**引用关系**。
//
// 内核要求链式出站的 detour 指向一个已存在的出站;如果那个出站(直接或间接)又
// 指回链式出站,内核启动时直接 FATAL:
//
//	circular outbound dependency: select-main -> chain-hk -> select-main
//
// 这在真机上是最难排查的故障之一(面板看着正常,内核起不来)。所以我们在**保存时**
// 就把所有引用关系算清楚并拒绝非法的组合,而不是等内核报错。

// RefError 描述一处非法的引用。
type RefError struct {
	Where   string // 例如 "链式代理「香港住宅」"
	Message string
}

func (e RefError) Error() string { return e.Where + ": " + e.Message }

// Resolver 在给定配置上提供引用查询。所有方法都容忍 nil 与不存在的 ID,
// 便于在编辑过程中做渐进校验。
type Resolver struct {
	groups  map[string]*Group
	nodes   map[string]*Node
	chains  map[string]*Chain
	filters map[string]*Filter
}

// NewResolver 建立索引(不改动原配置)。
func NewResolver(cfg *Config) *Resolver {
	r := &Resolver{
		groups: make(map[string]*Group, len(cfg.Groups)),
		nodes:  make(map[string]*Node, len(cfg.Nodes)),
		chains: make(map[string]*Chain, len(cfg.Chains)),
	}
	for i := range cfg.Groups {
		r.groups[cfg.Groups[i].ID] = &cfg.Groups[i]
	}
	for i := range cfg.Nodes {
		r.nodes[cfg.Nodes[i].ID] = &cfg.Nodes[i]
	}
	for i := range cfg.Chains {
		r.chains[cfg.Chains[i].ID] = &cfg.Chains[i]
	}
	r.filters = make(map[string]*Filter, len(cfg.Filters))
	for i := range cfg.Filters {
		r.filters[cfg.Filters[i].ID] = &cfg.Filters[i]
	}
	return r
}

func (r *Resolver) Group(id string) *Group   { return r.groups[id] }
func (r *Resolver) Node(id string) *Node     { return r.nodes[id] }
func (r *Resolver) Chain(id string) *Chain     { return r.chains[id] }
func (r *Resolver) Filter(id string) *Filter   { return r.filters[id] }
func (r *Resolver) NodeCount() int           { return len(r.nodes) }
func (r *Resolver) ChainCount() int          { return len(r.chains) }

// GroupTagOptions 返回一个节点组引用出去的"成员出站名"。动态组返回 nil,
// 因为它的成员在编译期才展开,调用方需自行处理。
func (r *Resolver) GroupMemberTargets(g *Group) []string {
	if g == nil || g.Type == GroupDynamic {
		return nil
	}
	out := make([]string, 0, len(g.Members))
	seen := map[string]bool{}
	for _, m := range g.Members {
		t := r.MemberTag(m)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// MemberTag 把一个成员 ID 翻译成内核出站名:节点用 "node:<id>",链式代理用 "chain:<id>"。
// 两种成员存在同一张成员列表里,靠前缀区分。
func (r *Resolver) MemberTag(id string) string {
	if strings.HasPrefix(id, "chain:") {
		if r.chains[strings.TrimPrefix(id, "chain:")] != nil {
			return id
		}
		return ""
	}
	if n := r.nodes[strings.TrimPrefix(id, "node:")]; n != nil {
		return "node:" + n.ID
	}
	// 也接受裸 ID,兼容手工导入的旧配置
	if n := r.nodes[id]; n != nil {
		return "node:" + n.ID
	}
	return ""
}

// FrontOptions 返回某个链式代理**可以**选作前置的组。
//
// 规则(按优先级):
//  1. 必须有可用成员——空组在内核里不会生成出站,选了也保存不进去,
//     只会让用户白填一遍表单。这里直接不列出,前端也就不会默认选中它。
//  2. 不能是动态组——成员按关键词浮动,无法保证稳定。
//  3. 不能包含自己(编辑场景)——会导致 circular outbound dependency。
//  4. 新建场景(chainID 为空)额外排除"已含其它链式代理"的组:
//     这类组当前能用,但再多一条链式代理后极易绕成环,
//     而且用户很难理解为什么"刚才还能选"。
//
// 编辑现有链式代理时,它自己当前用的前置组**始终保留在列表里**,
// 即使因为上面第 4 条被排除也不要紧——编辑场景传的是它自己的 ID,不受影响。
func (r *Resolver) FrontOptions(chainID string) []*Group {
	var res []*Group
	for _, g := range r.groups {
		if g.Disabled || g.Type == GroupDynamic {
			continue
		}
		targets := r.GroupMemberTargets(g)
		// 必须有可用成员
		if len(targets) == 0 {
			continue
		}
		ok := true
		for _, tid := range targets {
			if !strings.HasPrefix(tid, "chain:") {
				continue
			}
			other := strings.TrimPrefix(tid, "chain:")
			if chainID == "" {
				// 新建:不允许选已包含链式代理的组
				ok = false
				break
			}
			if other == chainID {
				// 包含自己:会造成环
				ok = false
				break
			}
		}
		if ok {
			res = append(res, g)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res
}

// FrontOption 是链式代理前置下拉的一个候选:节点组或筛选组。
// 筛选组在 SMBox 式架构里同样是内核 selector/urltest,当前置完全合理,
// 而且它的成员永远只有节点(不含链式代理),天然不可能成环。
type FrontOption struct {
	Ref   string `json:"ref"` // "group:<id>" / "filter:<id>"
	Name  string `json:"name"`
	Kind  string `json:"kind"` // group / filter
	Count int    `json:"count"`
}

// FrontChoices 返回新建/编辑链式代理时可选的全部前置(节点组 + 筛选组)。
// chainID 为空表示新建。编辑时链当前使用的前置一定保留。
func (r *Resolver) FrontChoices(chainID string) []FrontOption {
	var out []FrontOption
	for _, g := range r.FrontOptions(chainID) {
		out = append(out, FrontOption{
			Ref: g.Tag(), Name: g.Name, Kind: "group",
			Count: len(r.GroupMemberTargets(g)),
		})
	}
	for _, f := range r.filters {
		if !f.Enabled || f.CountOnly {
			continue
		}
		n := len(f.Matches(r.nodeList()))
		if n == 0 {
			continue // 空筛选组不会生成出站,选了也保存不了
		}
		// 显示名带图标:内核出站 tag 也是这个,列表和实际出站保持一致
		out = append(out, FrontOption{
			Ref: f.Tag(), Name: f.Display(), Kind: "filter", Count: n,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeList 返回全部节点(用于筛选匹配)。
func (r *Resolver) nodeList() []Node {
	out := make([]Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, *n)
	}
	return out
}

// FrontRef 解析一个前置引用为内核出站 tag。找不到返回 ""。
func (r *Resolver) FrontRefTag(ref string) string {
	kind, id := splitRef(ref)
	switch kind {
	case "filter":
		if f := r.filters[id]; f != nil {
			return f.OutboundTag()
		}
	default:
		if g := r.groups[id]; g != nil {
			return g.OutboundTag()
		}
	}
	return ""
}

// splitRef 拆 "group:<id>" / "filter:<id>" / 裸ID(按 group)。
func splitRef(ref string) (kind, id string) {
	switch {
	case strings.HasPrefix(ref, "filter:"):
		return "filter", strings.TrimPrefix(ref, "filter:")
	case strings.HasPrefix(ref, "group:"):
		return "group", strings.TrimPrefix(ref, "group:")
	default:
		return "group", ref
	}
}

// ValidateAll 检查整份配置的一致性与引用完整性,返回人类可读的问题列表。
// 面板保存、导入、编译前都会调用。
func ValidateAll(cfg *Config) []RefError {
	var errs []RefError
	r := NewResolver(cfg)

	// ---- 节点 ----
	dup := map[string]string{}
	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if !IDPattern.MatchString(n.ID) {
			errs = append(errs, RefError{fmt.Sprintf("节点「%s」", n.Name), "ID 不合法"})
		}
		if _, ok := dup["node:"+n.ID]; ok {
			errs = append(errs, RefError{fmt.Sprintf("节点「%s」", n.Name), "ID 重复"})
		}
		dup["node:"+n.ID] = n.Name
		if strings.TrimSpace(n.Name) == "" {
			errs = append(errs, RefError{fmt.Sprintf("节点 %s", n.ID), "名称不能为空"})
		}
		if n.Server == "" || n.Port <= 0 || n.Port > 65535 {
			errs = append(errs, RefError{fmt.Sprintf("节点「%s」", n.Name), "服务器地址或端口无效"})
		}
	}

	// ---- 节点组 ----
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		where := fmt.Sprintf("节点组「%s」", g.Name)
		if _, ok := dup["group:"+g.ID]; ok {
			errs = append(errs, RefError{where, "ID 重复"})
		}
		dup["group:"+g.ID] = g.Name
		switch g.Type {
		case GroupSelect, GroupURLTest, GroupFallback, GroupDynamic:
		default:
			errs = append(errs, RefError{where, fmt.Sprintf("未知的组类型 %q", g.Type)})
		}
		if g.Type == GroupDynamic {
			if strings.TrimSpace(strings.Join(g.Keywords, "")) == "" && strings.TrimSpace(g.FilterRegex) == "" {
				errs = append(errs, RefError{where, "动态组至少要有一个关键词或正则"})
			}
			if g.FilterRegex != "" {
				if _, err := compileRegex(g.FilterRegex); err != nil {
					errs = append(errs, RefError{where, "正则表达式无效: " + err.Error()})
				}
			}
			continue
		}
		// 空组是合法的"还没配好"状态:内核侧不生成出站,面板上提示用户去加节点。
		// 首装时默认会建空组,若这里报错,整个首次启动都会失败。
		for _, m := range g.Members {
			if r.MemberTag(m) == "" {
				errs = append(errs, RefError{where, fmt.Sprintf("成员 %q 不存在(可能已被删除)", m)})
			}
		}
	}

	// ---- DNS 分流模式 ----
	switch cfg.DNS.Mode {
	case "", DNSModeProxy, DNSModeDirect:
	default:
		errs = append(errs, RefError{"DNS 分流模式", fmt.Sprintf("未知模式 %q(应为 proxy 或 direct)", cfg.DNS.Mode)})
	}
	for _, ip := range cfg.DNS.ModeExceptions {
		if net.ParseIP(ip) == nil {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				errs = append(errs, RefError{"DNS 分流模式", fmt.Sprintf("例外设备地址无效: %q(需 IP 或 CIDR)", ip)})
			}
		}
	}

	// ---- 设备分流模式(流量走向) ----
	switch cfg.Routing.Mode {
	case "", RouteModeProxy, RouteModeDirect:
	default:
		errs = append(errs, RefError{"设备分流模式", fmt.Sprintf("未知模式 %q(应为 proxy 或 direct)", cfg.Routing.Mode)})
	}
	for _, ip := range cfg.Routing.ModeExceptions {
		if net.ParseIP(ip) == nil {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				errs = append(errs, RefError{"设备分流模式", fmt.Sprintf("例外设备地址无效: %q(需 IP 或 CIDR)", ip)})
			}
		}
	}

	// ---- 筛选 ----
	for i := range cfg.Filters {
		f := &cfg.Filters[i]
		where := fmt.Sprintf("筛选「%s」", f.Name)
		if _, ok := dup["filter:"+f.ID]; ok {
			errs = append(errs, RefError{where, "ID 重复"})
		}
		dup["filter:"+f.ID] = f.Name
		if err := f.Validate(); err != nil {
			errs = append(errs, RefError{where, err.Error()})
		}
	}

	// ---- 链式代理 ----
	for i := range cfg.Chains {
		ch := &cfg.Chains[i]
		where := fmt.Sprintf("链式代理「%s」", ch.Name)
		if err := ch.Validate(); err != nil {
			errs = append(errs, RefError{where, err.Error()})
			continue
		}
		_, fid := ch.FrontRef()
		front := r.Group(fid)
		frontFilter := r.Filter(fid)
		if front == nil && frontFilter == nil {
			errs = append(errs, RefError{where, fmt.Sprintf("前置组 %q 不存在", ch.FrontGroupID)})
		} else if frontFilter != nil {
			// 筛选组做前置:成员只有节点,不可能成环,只校验可用性
			if !frontFilter.Enabled || frontFilter.CountOnly {
				errs = append(errs, RefError{where,
					fmt.Sprintf("前置筛选「%s」已停用或仅统计,不能作为前置", frontFilter.Name)})
			} else if len(frontFilter.Matches(cfg.Nodes)) == 0 {
				errs = append(errs, RefError{where,
					fmt.Sprintf("前置筛选「%s」还没有命中任何节点", frontFilter.Name)})
			}
		} else {
			if front.Disabled {
				errs = append(errs, RefError{where, fmt.Sprintf("前置节点组「%s」已被禁用", front.Name)})
			}
			if front.Type == GroupDynamic {
				errs = append(errs, RefError{where, "前置组不能是动态关键词组,请建一个手动选择组作为前置"})
			}
			targets := r.GroupMemberTargets(front)
			for _, tid := range targets {
				if tid == ch.Tag() {
					errs = append(errs, RefError{where,
						fmt.Sprintf("前置组「%s」里包含了这条链式代理自身,内核会因循环依赖无法启动", front.Name)})
				}
			}
			// 空组在内核配置里不会生成出站,链式出站的 detour 指向它会让内核报
			// dependency[...] not found。前置组必须有至少一个可用成员。
			if len(targets) == 0 {
				errs = append(errs, RefError{where,
					fmt.Sprintf("前置组「%s」还没有可用节点,请先往里加节点", front.Name)})
			}
		}
	}

	// ---- 环检测(链式 → 前置组 → 链式 → ...)----
	errs = append(errs, detectChainCycles(cfg, r)...)

	// ---- 出站名唯一性 ----
	// 节点组、筛选组、规则组都会以内核出站的形式出现,且 tag 直接用名称。
	// 任何重名都会让内核 FATAL,必须在保存时拦住。
	{
		owner := map[string]string{}
		claim := func(name, where string) {
			name = strings.TrimSpace(name)
			if name == "" {
				return
			}
			if prev, ok := owner[name]; ok {
				errs = append(errs, RefError{where,
					fmt.Sprintf("名称「%s」与%s重复,内核里会出现两个同名出站", name, prev)})
				return
			}
			owner[name] = where
		}
		for _, g := range cfg.Groups {
			if !g.Disabled {
				claim(g.Name, "节点组")
			}
		}
		for _, f := range cfg.Filters {
			if f.Enabled && !f.CountOnly {
				// 显示名(含图标)才是内核里的出站 tag,按它查重
				claim(f.Display(), "筛选")
			}
		}
	}

	// ---- 规则组名唯一性 ----
	// AsGroup 的规则会用规则名作为出站 tag。重名会让内核遇到两个同名出站,
	// 直接 FATAL,所以必须在保存时就拦住。
	groupNames := map[string]string{}
	for _, p := range cfg.Policies {
		if !p.Enabled || !p.AsGroup {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		if name == "Proxy" || name == "direct" || name == "block" {
			errs = append(errs, RefError{fmt.Sprintf("规则「%s」", p.Name),
				"该名称被系统保留(Proxy/direct/block),不能用作选线组名"})
			continue
		}
		if prev, ok := groupNames[name]; ok {
			errs = append(errs, RefError{fmt.Sprintf("规则「%s」", p.Name),
				fmt.Sprintf("选线组名与规则「%s」重复,请改一个名字", prev)})
			continue
		}
		groupNames[name] = p.Name
	}
	// 也要避免与筛选组同名
	for _, f := range cfg.Filters {
		if f.Enabled && !f.CountOnly {
			if _, ok := groupNames[strings.TrimSpace(f.Name)]; ok {
				errs = append(errs, RefError{fmt.Sprintf("筛选「%s」", f.Name),
					"与某个选线组同名,内核里会出现两个同名出站"})
			}
		}
	}

	// ---- 分流目标 ----
	for _, p := range cfg.Policies {
		where := fmt.Sprintf("分流规则「%s」", p.Name)
		switch {
		case p.Target == "direct" || p.Target == "block":
		case p.Target == SelfTarget:
			if !p.AsGroup {
				errs = append(errs, RefError{where, "出口为 self 时必须开启选线组"})
			}
		case strings.HasPrefix(p.Target, "group:"):
			gid := strings.TrimPrefix(p.Target, "group:")
			if r.Group(gid) == nil {
				errs = append(errs, RefError{where, fmt.Sprintf("出口节点组 %q 不存在", gid)})
			}
		case strings.HasPrefix(p.Target, "chain:"):
			cid := strings.TrimPrefix(p.Target, "chain:")
			if r.Chain(cid) == nil {
				errs = append(errs, RefError{where, fmt.Sprintf("出口链式代理 %q 不存在", cid)})
			}
		case strings.HasPrefix(p.Target, "filter:"):
			fid := strings.TrimPrefix(p.Target, "filter:")
			if r.Filter(fid) == nil {
				errs = append(errs, RefError{where, fmt.Sprintf("出口筛选 %q 不存在", fid)})
			}
		case strings.HasPrefix(p.Target, "node:"):
			nid := strings.TrimPrefix(p.Target, "node:")
			if r.Node(nid) == nil {
				errs = append(errs, RefError{where, fmt.Sprintf("出口节点 %q 不存在", nid)})
			}
		case p.Target == "":
			errs = append(errs, RefError{where, "未设置出口"})
		default:
			errs = append(errs, RefError{where, fmt.Sprintf("未知的出口 %q", p.Target)})
		}
		if err := ValidatePolicy(p); err != nil {
			errs = append(errs, RefError{where, err.Error()})
		}
	}

	return errs
}

// detectChainCycles 用 DFS 找出链式代理之间的环。依赖边:
//
//	链式 C --依赖--> 前置组 F 的所有成员里出现的链式代理
//
// 只要某条边最终走回 C 自己,内核的 outbound 依赖就是环。
func detectChainCycles(cfg *Config, r *Resolver) []RefError {
	// deps[C] = 依赖的其它链式代理 ID 列表
	deps := map[string][]string{}
	for _, ch := range cfg.Chains {
		g := r.Group(ch.FrontGroupID)
		if g == nil {
			continue
		}
		for _, tid := range r.GroupMemberTargets(g) {
			if strings.HasPrefix(tid, "chain:") {
				deps[ch.ID] = append(deps[ch.ID], strings.TrimPrefix(tid, "chain:"))
			}
		}
	}
	var out []RefError
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, next := range deps[id] {
			switch color[next] {
			case gray:
				// 找到环,回溯出路径
				start := 0
				for i, s := range stack {
					if s == next {
						start = i
						break
					}
				}
				path := append(append([]string{}, stack[start:]...), next)
				names := make([]string, 0, len(path))
				for _, p := range path {
					if c := r.Chain(p); c != nil {
						names = append(names, "「"+c.Name+"」")
					} else {
						names = append(names, p)
					}
				}
				out = append(out, RefError{
					"链式代理",
					"检测到循环依赖 " + strings.Join(names, " → ") + ",内核将无法启动",
				})
				return true
			case white:
				if visit(next) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return out
}

// ChainReferences 统计某条链式代理被哪些地方引用,删除前的确认提示要用。
func ChainReferences(cfg *Config, chainID string) []string {
	tag := "chain:" + chainID
	var refs []string
	for _, g := range cfg.Groups {
		if g.Disabled || g.Type == GroupDynamic {
			// 动态组默认不收链式代理,但 IncludeChain 时会收
			if g.Type == GroupDynamic && !g.IncludeChain {
				continue
			}
			if g.Type == GroupDynamic {
				refs = append(refs, fmt.Sprintf("动态组「%s」", g.Name))
				continue
			}
		}
		for _, m := range g.Members {
			if m == tag {
				refs = append(refs, fmt.Sprintf("节点组「%s」", g.Name))
				break
			}
		}
	}
	for _, p := range cfg.Policies {
		if p.Target == tag {
			refs = append(refs, fmt.Sprintf("分流规则「%s」", p.Name))
		}
	}
	return refs
}

// GroupReferences 统计节点组的引用情况:被哪些链式代理当作前置、被哪些分流规则使用。
func GroupReferences(cfg *Config, groupID string) []string {
	var refs []string
	for _, ch := range cfg.Chains {
		if k, id := splitRef(ch.FrontGroupID); k == "group" && id == groupID {
			refs = append(refs, fmt.Sprintf("链式代理「%s」(作为前置)", ch.Name))
		}
	}
	for _, p := range cfg.Policies {
		if p.Target == "group:"+groupID {
			refs = append(refs, fmt.Sprintf("分流规则「%s」", p.Name))
		}
	}
	return refs
}

// FilterReferences 统计筛选被哪些地方引用:分流规则、链式代理前置。
func FilterReferences(cfg *Config, filterID string) []string {
	var refs []string
	for _, p := range cfg.Policies {
		if p.Target == "filter:"+filterID {
			refs = append(refs, fmt.Sprintf("分流规则「%s」", p.Name))
		}
	}
	for _, ch := range cfg.Chains {
		if k, id := splitRef(ch.FrontGroupID); k == "filter" && id == filterID {
			refs = append(refs, fmt.Sprintf("链式代理「%s」(作为前置)", ch.Name))
		}
	}
	return refs
}

// NodeReferences 统计节点被哪些静态组引用。
func NodeReferences(cfg *Config, nodeID string) []string {
	var refs []string
	want := "node:" + nodeID
	for _, g := range cfg.Groups {
		if g.Type == GroupDynamic {
			continue
		}
		for _, m := range g.Members {
			if m == want || m == nodeID {
				refs = append(refs, fmt.Sprintf("节点组「%s」", g.Name))
				break
			}
		}
	}
	for _, p := range cfg.Policies {
		if p.Target == want {
			refs = append(refs, fmt.Sprintf("分流规则「%s」", p.Name))
		}
	}
	return refs
}

// CleanupDangling 在删除节点/组/链式代理之后清理悬空引用,返回清理说明。
// 这是"删除后配置仍然可用"的保证:内核不会因为一个悬空成员而整体起不来。
func CleanupDangling(cfg *Config) []string {
	r := NewResolver(cfg)
	var notes []string

	// 节点组:剔除不存在的成员
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		if g.Type == GroupDynamic {
			continue
		}
		kept := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if r.MemberTag(m) == "" {
				notes = append(notes, fmt.Sprintf("节点组「%s」移除了已删除的成员 %s", g.Name, m))
				continue
			}
			kept = append(kept, m)
		}
		g.Members = kept
	}

	// 链式代理:前置组被删则清空前置并禁用,不偷偷换成别的组
	for i := range cfg.Chains {
		ch := &cfg.Chains[i]
		if ch.FrontGroupID == "" {
			continue
		}
		kind, fid := splitRef(ch.FrontGroupID)
		missing := false
		if kind == "filter" {
			missing = r.Filter(fid) == nil
		} else {
			missing = r.Group(fid) == nil
		}
		if missing {
			notes = append(notes, fmt.Sprintf("链式代理「%s」的前置组已删除,该条已禁用", ch.Name))
			ch.FrontGroupID = ""
			ch.Disabled = true
		}
	}

	// 分流规则:出口不存在则禁用
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		ok := true
		switch {
		case p.Target == "direct" || p.Target == "block" || p.Target == SelfTarget:
		case strings.HasPrefix(p.Target, "group:"):
			ok = r.Group(strings.TrimPrefix(p.Target, "group:")) != nil
		case strings.HasPrefix(p.Target, "chain:"):
			ok = r.Chain(strings.TrimPrefix(p.Target, "chain:")) != nil
		case strings.HasPrefix(p.Target, "filter:"):
			ok = r.Filter(strings.TrimPrefix(p.Target, "filter:")) != nil
		case strings.HasPrefix(p.Target, "node:"):
			ok = r.Node(strings.TrimPrefix(p.Target, "node:")) != nil
		default:
			ok = false
		}
		if !ok && p.Enabled {
			notes = append(notes, fmt.Sprintf("分流规则「%s」的出口已删除,该规则已禁用", p.Name))
			p.Enabled = false
		}
	}
	return notes
}

// EnsureDefaultGroups 保证至少有一个可用的手动选择组和前置组,首次安装后即可用。
// 返回新建的组;已有符合要求的组则返回 nil。
func EnsureDefaultGroups(cfg *Config, newID func() string) (created []Group) {
	hasSelect, hasFront := false, false
	for _, g := range cfg.Groups {
		if g.Type != GroupSelect {
			continue
		}
		hasSelect = true
		// 不含链式代理的 select 组可以作为前置
		hasChainMember := false
		for _, m := range g.Members {
			if strings.HasPrefix(m, "chain:") {
				hasChainMember = true
			}
		}
		if !hasChainMember {
			hasFront = true
		}
	}
	// 所有已启用节点,按添加顺序
	var nodeMembers []string
	for _, n := range cfg.Nodes {
		if !n.Disabled {
			nodeMembers = append(nodeMembers, "node:"+n.ID)
		}
	}
	if !hasSelect {
		g := Group{ID: newID(), Name: "手动选择", Type: GroupSelect, Members: nodeMembers}
		cfg.Groups = append(cfg.Groups, g)
		created = append(created, g)
	}
	if !hasFront {
		g := Group{ID: newID(), Name: "前置线路", Type: GroupSelect, Members: append([]string{}, nodeMembers...)}
		cfg.Groups = append(cfg.Groups, g)
		created = append(created, g)
	}
	return created
}

// AdoptNewMembers 把新加入的节点自动并入"空的手动选择组"。
//
// 动机:首装时建的默认组在节点加入前是空的,之后订阅导入几十个节点,
// 用户会发现"节点有了但组里没有、而兜底直接指向某个单节点",很反直觉。
// 这里只对**成员为空**的 select 组自动补成员,已有成员的组绝不改动
// (用户的手动选择必须被尊重)。
func AdoptNewMembers(cfg *Config) []string {
	var notes []string
	var nodeIDs []string
	for _, n := range cfg.Nodes {
		if !n.Disabled {
			nodeIDs = append(nodeIDs, "node:"+n.ID)
		}
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		if g.Type != GroupSelect || g.Disabled || len(g.Members) > 0 {
			continue
		}
		// 前置组不放链式代理,这里只填普通节点,天然安全
		g.Members = append([]string{}, nodeIDs...)
		notes = append(notes, fmt.Sprintf("节点组「%s」已自动加入 %d 个节点", g.Name, len(nodeIDs)))
	}
	return notes
}

// FrontGroupFor 返回适合做前置的组(优先名为"前置线路"的),没有则返回第一个可选组。
func (r *Resolver) FrontGroupFor() *Group {
	var fallback *Group
	for _, g := range r.groups {
		if g.Disabled || g.Type == GroupDynamic {
			continue
		}
		if g.Name == "前置线路" {
			return g
		}
		if fallback == nil {
			fallback = g
		}
	}
	return fallback
}

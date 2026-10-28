// Package config(续):筛选系统。
//
// SMBox 的核心差异点是"筛选"(filter):把几百上千个订阅节点按名称关键词
// 自动归类成若干组(如 "🇯🇵 日本 173 个节点"、"CFNEW 141 个节点"),
// 分流规则再引用这些筛选,而不是引用单个节点。
//
// 我们保留原有的 NodeGroup(手动/自动/动态组),另外引入 Filter:
//   - 一个 Filter 可以匹配多个关键词,也支持正则;
//   - 支持"排除词",避免 "日本" 误收 "日本 专线-备用" 这类;
//   - 支持按地区代码自动识别国旗 country 字段(便于前端分组显示);
//   - 节点只要名称命中即自动入选,订阅更新后**自动跟随**,不需要手工维护。

package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Filter 一条节点筛选规则。
type Filter struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Keywords 关键词,任一命中即入选(OR)。
	Keywords []string `json:"keywords"`
	// KeywordsAnd 必须同时包含的关键词(AND),用于精确定位。
	KeywordsAnd []string `json:"keywords_and,omitempty"`
	// Exclude 排除关键词,命中任何一个则不入选。
	Exclude []string `json:"exclude,omitempty"`
	// Regex 正则表达式(可选),与关键词是 AND 关系。
	Regex string `json:"regex,omitempty"`
	// Region 地区代码(如 jp/us/hk),仅用于界面分组与排序。
	Region string `json:"region,omitempty"`
	// Icon 用户挑选的 emoji 图标。它会拼进显示名与内核出站 tag ——
	// 内核面板(zashboard)只认出站名文字,emoji 是唯一能透传到
	// 所有面板的"图标"(品牌 logo 图片出不了 boxd 单文件)。
	Icon string `json:"icon,omitempty"`
	// Sort 排序方式:name / region / none。
	Sort string `json:"sort,omitempty"`
	// Mode 组类型:select(手动选择) / urltest(自动择优)。
	// 与 SMBox 一致:筛选本身就是一个出站组,所以必须能选类型。
	Mode string `json:"mode,omitempty"`
	// SubIDs 限定只从这些订阅里筛选;为空表示所有订阅 + 手工节点。
	SubIDs []string `json:"sub_ids,omitempty"`
	// MaxNodes 最多收录多少个节点,0 表示不限。
	MaxNodes int `json:"max_nodes,omitempty"`
	// 仅统计,不生成内核出站(用于"只看统计"的场景)。
	CountOnly bool `json:"count_only,omitempty"`
}

// Tag 是配置内部的引用标识(规则用它指向筛选),稳定不随改名变化。
func (f Filter) Tag() string { return "filter:" + f.ID }

// OutboundTag 是筛选组写到内核配置里的 tag,直接用名称,
// 以便在内核面板里与面板显示一致。
// Display 显示名 = 图标 + 名称。未选图标时就是名称本身。
func (f Filter) Display() string {
	if strings.TrimSpace(f.Icon) != "" {
		return strings.TrimSpace(f.Icon) + " " + f.Name
	}
	return f.Name
}

func (f Filter) OutboundTag() string { return f.Display() }

// GroupType 返回筛选组在内核里的类型,默认 select(与 SMBox 一致)。
func (f Filter) GroupType() string {
	if f.Mode == "urltest" {
		return "urltest"
	}
	return "select"
}

// Match 判断节点名是否命中该筛选。
func (f Filter) Match(name string) bool {
	lower := strings.ToLower(name)
	// 排除优先
	for _, ex := range f.Exclude {
		if ex = strings.TrimSpace(ex); ex != "" && strings.Contains(lower, strings.ToLower(ex)) {
			return false
		}
	}
	// AND 关键词必须全部包含
	for _, kw := range f.KeywordsAnd {
		if kw = strings.TrimSpace(kw); kw != "" && !strings.Contains(lower, strings.ToLower(kw)) {
			return false
		}
	}
	// 正则
	if f.Regex != "" {
		re, err := regexp.Compile(f.Regex)
		if err != nil {
			return false
		}
		if !re.MatchString(name) {
			return false
		}
	}
	// OR 关键词:没有配置任何 OR 关键词时,只要有 AND 或 Regex 就算命中
	var ors []string
	for _, kw := range f.Keywords {
		if kw = strings.TrimSpace(kw); kw != "" {
			ors = append(ors, kw)
		}
	}
	if len(ors) == 0 {
		return len(f.KeywordsAnd) > 0 || f.Regex != ""
	}
	for _, kw := range ors {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// Validate 校验筛选配置。
func (f *Filter) Validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("筛选名称不能为空")
	}
	if len(f.Keywords) == 0 && len(f.KeywordsAnd) == 0 && f.Regex == "" {
		return fmt.Errorf("筛选「%s」至少要有一个关键词或正则", f.Name)
	}
	if f.Regex != "" {
		if _, err := regexp.Compile(f.Regex); err != nil {
			return fmt.Errorf("筛选「%s」的正则无效: %w", f.Name, err)
		}
	}
	if f.MaxNodes < 0 {
		return fmt.Errorf("筛选「%s」的最大节点数不能为负", f.Name)
	}
	switch f.Mode {
	case "", "select", "urltest":
	default:
		return fmt.Errorf("筛选「%s」的组类型只能是 select 或 urltest", f.Name)
	}
	return nil
}

// Matches 返回命中该筛选的节点(保持配置顺序,受 MaxNodes 限制)。
func (f Filter) Matches(nodes []Node) []Node {
	var out []Node
	// 订阅限定:只保留来自指定订阅的节点
	var allow map[string]bool
	if len(f.SubIDs) > 0 {
		allow = make(map[string]bool, len(f.SubIDs))
		for _, id := range f.SubIDs {
			allow["sub:"+id] = true
		}
	}
	for _, n := range nodes {
		if n.Disabled {
			continue
		}
		if allow != nil && !allow[n.Source] {
			continue
		}
		if !f.Match(n.Name) {
			continue
		}
		out = append(out, n)
		if f.MaxNodes > 0 && len(out) >= f.MaxNodes {
			break
		}
	}
	switch f.Sort {
	case "name":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	case "region":
		sort.SliceStable(out, func(i, j int) bool {
			ri, rj := GuessRegion(out[i].Name), GuessRegion(out[j].Name)
			if ri != rj {
				return ri < rj
			}
			return out[i].Name < out[j].Name
		})
	}
	return out
}

// regionTable 常见国家/地区识别表。顺序有意义:先匹配更具体的词。
// 这些词同时用于"节点名 → 地区"的推断,以及界面上的国旗分组展示。
var regionTable = []struct {
	Code   string
	Flag   string
	Names  []string
}{
	{"hk", "🇭🇰", []string{"香港", "hongkong", "hong kong", "hk", "深港", "沪港"}},
	{"tw", "🇹🇼", []string{"台湾", "taiwan", "tw", "台北", "彰化"}},
	{"jp", "🇯🇵", []string{"日本", "japan", "jp", "东京", "大阪", "埼玉", "沪日", "深日"}},
	{"kr", "🇰🇷", []string{"韩国", "korea", "kr", "首尔", "춘천", "春川"}},
	{"sg", "🇸🇬", []string{"新加坡", "singapore", "sg", "狮城"}},
	{"us", "🇺🇸", []string{"美国", "united states", "usa", "us", "洛杉矶", "圣何塞", "西雅图", "芝加哥", "纽约", "达拉斯", "硅谷"}},
	{"gb", "🇬🇧", []string{"英国", "united kingdom", "uk", "gb", "伦敦"}},
	{"de", "🇩🇪", []string{"德国", "germany", "de", "法兰克福"}},
	{"fr", "🇫🇷", []string{"法国", "france", "fr", "巴黎"}},
	{"nl", "🇳🇱", []string{"荷兰", "netherlands", "nl", "阿姆斯特丹"}},
	{"ru", "🇷🇺", []string{"俄罗斯", "russia", "ru", "莫斯科"}},
	{"ca", "🇨🇦", []string{"加拿大", "canada", "ca", "多伦多", "蒙特利尔"}},
	{"au", "🇦🇺", []string{"澳大利亚", "australia", "au", "悉尼"}},
	{"in", "🇮🇳", []string{"印度", "india", "in", "孟买"}},
	{"tr", "🇹🇷", []string{"土耳其", "turkey", "tr", "伊斯坦布尔"}},
	{"ar", "🇦🇷", []string{"阿根廷", "argentina", "ar"}},
	{"br", "🇧🇷", []string{"巴西", "brazil", "br", "圣保罗"}},
	{"th", "🇹🇭", []string{"泰国", "thailand", "th", "曼谷"}},
	{"vn", "🇻🇳", []string{"越南", "vietnam", "vn"}},
	{"my", "🇲🇾", []string{"马来西亚", "malaysia", "my", "吉隆坡"}},
	{"ph", "🇵🇭", []string{"菲律宾", "philippines", "ph", "马尼拉"}},
	{"id", "🇮🇩", []string{"印尼", "indonesia", "id", "雅加达"}},
	{"ae", "🇦🇪", []string{"阿联酋", "uae", "迪拜", "dubai"}},
	{"ch", "🇨🇭", []string{"瑞士", "switzerland", "ch", "苏黎世"}},
	{"se", "🇸🇪", []string{"瑞典", "sweden", "se", "斯德哥尔摩"}},
	{"pl", "🇵🇱", []string{"波兰", "poland", "pl", "华沙"}},
	{"fi", "🇫🇮", []string{"芬兰", "finland", "fi", "赫尔辛基"}},
	{"cn", "🇨🇳", []string{"中国", "china", "回国", "北京", "上海", "广州", "深圳", "杭州", "成都"}},
}

// GuessRegion 从节点名推断地区代码,识别不出返回 "other"。
func GuessRegion(name string) string {
	lower := strings.ToLower(name)
	for _, r := range regionTable {
		for _, kw := range r.Names {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return r.Code
			}
		}
	}
	return "other"
}

// RegionFlag 返回地区对应的国旗 emoji,未知返回空串。
func RegionFlag(code string) string {
	for _, r := range regionTable {
		if r.Code == code {
			return r.Flag
		}
	}
	return ""
}

// RegionName 返回地区中文名。
func RegionName(code string) string {
	names := map[string]string{
		"hk": "香港", "tw": "台湾", "jp": "日本", "kr": "韩国", "sg": "新加坡",
		"us": "美国", "gb": "英国", "de": "德国", "fr": "法国", "nl": "荷兰",
		"ru": "俄罗斯", "ca": "加拿大", "au": "澳大利亚", "in": "印度",
		"tr": "土耳其", "ar": "阿根廷", "br": "巴西", "th": "泰国", "vn": "越南",
		"my": "马来西亚", "ph": "菲律宾", "id": "印尼", "ae": "阿联酋",
		"ch": "瑞士", "se": "瑞典", "pl": "波兰", "fi": "芬兰", "cn": "中国",
	}
	if n, ok := names[code]; ok {
		return n
	}
	return "其他"
}

// FilterStats 一个筛选的统计信息,用于仪表盘展示。
type FilterStats struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Region  string `json:"region"`
	Flag    string `json:"flag"`
	Count   int    `json:"count"`
	Enabled bool   `json:"enabled"`
}

// ComputeFilterStats 计算所有筛选命中的节点数量。
func ComputeFilterStats(cfg *Config) []FilterStats {
	out := make([]FilterStats, 0, len(cfg.Filters))
	for _, f := range cfg.Filters {
		st := FilterStats{
			ID: f.ID, Name: f.Name, Region: f.Region,
			Flag: RegionFlag(f.Region), Enabled: f.Enabled,
			Count: len(f.Matches(cfg.Nodes)),
		}
		out = append(out, st)
	}
	// 数量多的排前面,和 SMBox 的展示习惯一致
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AutoFilters 根据现有节点自动生成一组地区筛选。
// 用于"一键生成地区分组":把订阅里出现的地区都建出来。
func AutoFilters(cfg *Config, newID func() string) []Filter {
	counts := map[string]int{}
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		counts[GuessRegion(n.Name)]++
	}
	// 已有筛选的名称集合,避免重复创建
	existing := map[string]bool{}
	for _, f := range cfg.Filters {
		existing[f.Region] = true
	}
	var created []Filter
	for code, cnt := range counts {
		if code == "other" || cnt == 0 || existing[code] {
			continue
		}
		// 名称只放地区中文名:国旗由前端按 region 字段统一渲染,
		// 否则会出现 "🇯🇵 🇯🇵 日本" 这种重复。
		name := RegionName(code)
		// 用地区的中英文名做关键词,覆盖大多数命名方式
		var kws []string
		for _, r := range regionTable {
			if r.Code == code {
				kws = append(kws, r.Names...)
				break
			}
		}
		f := Filter{
			ID: newID(), Name: name, Enabled: true,
			Keywords: kws, Region: code, Sort: "name",
		}
		cfg.Filters = append(cfg.Filters, f)
		created = append(created, f)
	}
	sort.Slice(created, func(i, j int) bool { return created[i].Name < created[j].Name })
	return created
}

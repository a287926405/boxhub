// Package config(续):内置规则模板。
//
// 这批模板的地址与分组方式**直接对齐 SMBox**:从它生成的 sing-box 配置与
// smbox.db 的 rules 表里提取出来的真实值,包括:
//   - 每个分类使用哪些 .srs / .json 规则集
//   - 哪些走代理(nodeList)、哪些直连(direct)
//
// 这样用户"一键添加"得到的分流效果,与 SMBox 开箱即用的效果一致,
// 而不是一个需要自己逐个填域名的空壳。

package config

// SelfTarget 表示"路由到规则自己生成的同名组"。
// 这样配置里能一眼区分"直连"和"走这个规则自己的选线组"。
const SelfTarget = "self"

// RuleTemplate 一条预置规则。
type RuleTemplate struct {
	// Key 稳定标识,用于幂等添加。
	Key string `json:"key"`
	// Name 规则名,同时作为内核出站组名(因此必须唯一)。
	Name string `json:"name"`
	// Desc 一句话说明。
	Desc string `json:"desc"`
	// Target direct / block,或留空表示"走代理组"
	Target string `json:"target,omitempty"`
	// AsGroup 是否生成同名选线组(走代理的规则通常为 true)
	AsGroup bool `json:"as_group"`
	// RuleSets 规则集地址列表
	RuleSets []string `json:"rule_sets"`
	// DomainSuffix 附加的域名后缀(部分规则集里没有的目标)
	DomainSuffix []string `json:"domain_suffix,omitempty"`
	// Sort 建议的插入顺序(小的排前面)
	Sort int `json:"sort"`
	// Default 是否默认勾选
	Default bool `json:"default"`
}

// 规则集基础地址。用 meta-rules-dat 的 sing 分支,与 SMBox 相同。
const (
	rsBase     = "https://github.com/MetaCubeX/meta-rules-dat/raw/sing/geo/"
	rsBaseHead = "https://github.com/MetaCubeX/meta-rules-dat/raw/refs/heads/sing/geo/"
)

// geoSite / geoIP 拼地址的小工具,减少复制粘贴出错。
func site(name string) string { return rsBase + "geosite/" + name + ".srs" }
func geoip(name string) string { return rsBase + "geoip/" + name + ".srs" }
func siteHead(name string) string { return rsBaseHead + "geosite/" + name + ".srs" }

// RuleTemplates 返回内置的常用规则模板(顺序即建议插入顺序)。
//
// 顺序设计上遵循"先直连、后代理"的惯例:
//   1. 本地/私网直连(默认规则里已有)
//   2. 国内 CDN 直连(避免国内站点被绕一圈)
//   3. 中国大陆直连
//   4. 常见被墙/需要代理的服务
//   5. 其余境外流量走代理(由兜底处理,这里提供一个可选的 !CN 规则)
func RuleTemplates() []RuleTemplate {
	return []RuleTemplate{
		{
			Key: "cn-cdn", Name: "CN-CDN", Sort: 10, Default: true,
			Desc: "国内 CDN/应用商店直连(Apple、Steam、Microsoft 的国内节点)",
			Target: "direct",
			RuleSets: []string{
				rsBaseHead + "geosite/apple@cn.json",
				rsBaseHead + "geosite/steam@cn.json",
				rsBaseHead + "geosite/microsoft@cn.json",
			},
		},
		{
			Key: "cn", Name: "CN", Sort: 20, Default: true,
			Desc: "中国大陆站点与 IP 直连",
			Target: "direct",
			RuleSets: []string{geoip("cn"), site("cn")},
		},
		{
			Key: "youtube", Name: "Youtube", Sort: 30, Default: true,
			Desc: "YouTube(可单独指定线路)",
			AsGroup:  true,
			RuleSets: []string{site("youtube")},
		},
		{
			Key: "telegram", Name: "Telegram", Sort: 40, Default: true,
			Desc: "Telegram、TikTok(含 IP 段,避免走 CDN 直连)",
			AsGroup:  true,
			RuleSets: []string{geoip("telegram"), site("telegram"), siteHead("tiktok")},
		},
		{
			Key: "netflix", Name: "Netflix", Sort: 50, Default: true,
			Desc: "Netflix(建议单独选一个原生 IP 线路)",
			AsGroup:  true,
			RuleSets: []string{geoip("netflix"), site("netflix")},
		},
		{
			Key: "spotify", Name: "Spotify", Sort: 60, Default: true,
			Desc: "Spotify",
			AsGroup:  true,
			RuleSets: []string{site("spotify")},
		},
		{
			Key: "ai", Name: "AI", Sort: 70, Default: true,
			Desc: "OpenAI / Claude / Gemini 等 AI 服务",
			AsGroup: true,
			RuleSets: []string{
				"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/refs/heads/sing/geo/geosite/category-ai-!cn.json",
			},
			// 规则集里偶尔漏掉的目标,补几个常用域名
			DomainSuffix: []string{"openai.com", "anthropic.com", "claude.ai", "gemini.google.com"},
		},
		{
			Key: "ins", Name: "INS", Sort: 80, Default: true,
			Desc: "Instagram / Facebook",
			AsGroup:  true,
			RuleSets: []string{
				siteHead("instagram"),
				headIP("facebook"),
				siteHead("facebook"),
			},
			DomainSuffix: []string{"fbsbx.com"},
		},
		{
			Key: "google", Name: "Google", Sort: 90, Default: true,
			Desc: "Google 全家桶",
			AsGroup:  true,
			RuleSets: []string{site("google")},
		},
		{
			Key: "github", Name: "Github", Sort: 100, Default: true,
			Desc: "GitHub",
			AsGroup:  true,
			RuleSets: []string{site("github")},
		},
		{
			Key: "steam", Name: "Steam", Sort: 110, Default: false,
			Desc: "Steam 商店与社区",
			AsGroup:  true,
			RuleSets: []string{site("steam")},
		},
		{
			Key: "notcn", Name: "!CN", Sort: 900, Default: false,
			Desc: "所有非中国大陆目标走代理(放在最后兜底前)",
			AsGroup:  true,
			RuleSets: []string{siteHead("geolocation-!cn")},
		},
	}
}

// headIP 是 geoip 目录下 refs/heads 形式的地址。
func headIP(name string) string { return rsBaseHead + "geoip/" + name + ".srs" }

// ApplyTemplate 把一条模板转成 Policy。
// 规则集地址统一加 "srs:" 前缀之外,保持原样由 render 层判断格式;
// 这里只负责把结构填好,不做网络请求。
func ApplyTemplate(t RuleTemplate, newID func() string) Policy {
	p := Policy{
		ID:      newID(),
		Name:    t.Name,
		Enabled: true,
		AsGroup: t.AsGroup,
		Target:  t.Target,
		Match: Match{
			RuleSets:     append([]string{}, t.RuleSets...),
			DomainSuffix: append([]string{}, t.DomainSuffix...),
		},
	}
	if p.Target == "" {
		// 走代理:规则会路由到自己那个同名组(AsGroup),
		// 用 self: 前缀显式表达,避免被误认为直连。
		p.Target = SelfTarget
	}
	return p
}

// AlreadyHasTemplate 判断是否已经添加过某条模板(按规则名与规则集判断)。
func AlreadyHasTemplate(cfg *Config, t RuleTemplate) bool {
	for _, p := range cfg.Policies {
		if p.Name == t.Name {
			return true
		}
	}
	// 名称被改过的情况:看规则集是否高度重合
	for _, p := range cfg.Policies {
		if len(p.Match.RuleSets) == 0 {
			continue
		}
		hit := 0
		for _, want := range t.RuleSets {
			for _, has := range p.Match.RuleSets {
				if want == has {
					hit++
					break
				}
			}
		}
		if hit > 0 && hit == len(t.RuleSets) {
			return true
		}
	}
	return false
}

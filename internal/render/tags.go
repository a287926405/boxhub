package render

// 出站 tag 分配:内核面板(metacubexd)里显示的就是这些 tag,
// 用 node:a1b2c3 这种内部 ID 用户根本认不出哪个是哪个节点。
//
// 规则:
//   - 节点、链式代理、节点组、筛选组都用**可读名称**作为内核出站 tag;
//   - 配置内部引用仍然是 node:<id> / chain:<id> / group:<id> / filter:<id>,
//     改名不会让节点组、分流规则的引用错位(引用认 ID,显示认名称);
//   - 重名自动加 ID 尾缀区分:两个订阅可能有同名节点,
//     sing-box 遇到重复 tag 会直接 FATAL,所以必须保证全局唯一。

import (
	"fmt"
	"strings"

	"boxhub/internal/config"
)

// OutboundTags 返回「内部引用 → 内核出站 tag」映射。
//
// 分配顺序固定(组 > 筛选 > 链式 > 节点),保证同一份配置每次生成的 tag 一致,
// 不会因 Go map 遍历顺序导致内核反复重启。
func OutboundTags(cfg *config.Config) map[string]string {
	used := map[string]bool{}
	// 保留名:兜底出站、直连、总出口组,任何人都不能占用
	for _, r := range []string{"direct", "block", "Proxy", "➜ Direct"} {
		used[r] = true
	}
	out := map[string]string{}
	claim := func(ref, name, id string) {
		tag := strings.TrimSpace(name)
		if tag == "" {
			tag = ref // 名称为空的极端情况退回引用,保证不为空串
		}
		if used[tag] {
			tag = tag + " " + tailOf(id)
			for i := 2; used[tag]; i++ {
				tag = strings.TrimSpace(name) + " " + tailOf(id) + "-" + fmt.Sprint(i)
			}
		}
		used[tag] = true
		out[ref] = tag
	}
	for _, g := range cfg.Groups {
		if g.Disabled {
			continue
		}
		claim(g.Tag(), g.Name, g.ID)
	}
	for _, f := range cfg.Filters {
		if !f.Enabled || f.CountOnly {
			continue
		}
		claim(f.Tag(), f.Display(), f.ID)
	}
	for _, c := range cfg.Chains {
		if c.Disabled {
			continue
		}
		claim(c.Tag(), c.Name, c.ID)
	}
	for _, n := range cfg.Nodes {
		if n.Disabled {
			continue
		}
		claim("node:"+n.ID, n.Name, n.ID)
	}
	return out
}

// tailOf 取 ID 尾部 4 位做区分后缀(稳定、短、可读)。
func tailOf(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 4 {
		return id
	}
	return id[len(id)-4:]
}

// Tag 查一个内部引用对应的出站名;不存在返回原引用(交给内核报错,便于排查)。
func (t tags) Tag(ref string) string {
	if v, ok := t[ref]; ok {
		return v
	}
	return ref
}

type tags map[string]string

// tagsOf 内部使用:Build 各阶段共享同一份映射。
func tagsOf(cfg *config.Config) tags { return tags(OutboundTags(cfg)) }

// OutboundTagFor 对外助手:把一个内部引用(node:/chain:/group:/filter:)
// 转成它在内核配置里的出站名。API 层探测/切换时要用它——
// 拿内部引用去调 Clash API 会得到 404 Resource not found。
func OutboundTagFor(cfg *config.Config, ref string) string {
	return tagsOf(cfg).Tag(ref)
}

// DefaultProxyTagFor 返回当前兜底出口的内核出站名。
func DefaultProxyTagFor(cfg *config.Config) string {
	r := config.NewResolver(cfg)
	ref := DefaultProxyTarget(cfg, r)
	return OutboundTagFor(cfg, ref)
}

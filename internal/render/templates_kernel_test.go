package render_test

// 验证"应用全部规则模板"后的配置能被真实内核加载。
//
// 这一步很关键:模板里的规则集地址来自 meta-rules-dat,
// 内核启动时会**实际下载**这些 .srs/.json。地址写错、格式标错、
// 或者某个规则集在 sing-box 里不被支持,都会让内核直接起不来,
// 而面板上只看到"内核未运行"。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func TestAllTemplatesRunOnKernel(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("未找到 sing-box")
	}
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.Inbounds.ClashAPI = true
	cfg.Inbounds.ClashAPIPort = freePort(t) + 1
	cfg.Inbounds.AllowLAN = false
	cfg.DNS.FakeIP = false
	cfg.Nodes = []config.Node{
		{ID: "n1", Name: "日本 01", Type: "vless", Server: "1.1.1.1", Port: 443,
			UUID: "11111111-2222-3333-4444-555555555555"},
	}
	cfg.Filters = []config.Filter{
		{ID: "f1", Name: "日本", Enabled: true, Keywords: []string{"日本"}, Mode: "select"},
	}
	id := 0
	newID := func() string { id++; return "p" + string(rune('a'+id)) }
	for _, tp := range config.RuleTemplates() {
		cfg.Policies = append(cfg.Policies, config.ApplyTemplate(tp, newID))
	}
	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("校验失败: %v", e)
		}
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	raw, err := sb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 内核要下载十几个规则集,给足时间
	alive, out := runKernel(t, bin, path, 12*time.Second)
	if !alive {
		t.Fatalf("应用全部模板后内核无法启动(多半是规则集地址或格式问题):\n%s", out)
	}
	if strings.Contains(out, "FATAL") {
		t.Fatalf("内核启动出现致命错误:\n%s", out)
	}
	t.Logf("全部 %d 条模板规则已通过内核校验", len(cfg.Policies))
}

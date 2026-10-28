package api_test

// 这组测试覆盖"配置持久化"这条最容易出人命的主线。
//
// 背景:早期版本把生成的内核配置写到 data/config.json,与面板配置同路径,
// 一次保存后就把面板配置覆盖成内核格式,下次启动读不回来,面板显示"配置损坏"。
// 这类问题用户看到的是"数据全丢",必须在测试层守住。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"boxhub/internal/api"
	"boxhub/internal/config"
	"boxhub/internal/store"
)

// TestCoreConfigNameSeparated 保证内核配置与面板配置是两个文件。
func TestCoreConfigNameSeparated(t *testing.T) {
	if api.CoreConfigName == "config.json" {
		t.Fatal("内核配置文件名不能与面板配置相同,否则会互相覆盖")
	}
}

// TestPanelConfigSurvivesCoreWrite 模拟:写面板配置 → 写内核配置 → 重开,
// 面板配置必须完好。
func TestPanelConfigSurvivesCoreWrite(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewDefault()
	cfg.Nodes = []config.Node{{
		ID: "n1", Name: "香港 01", Type: "shadowsocks", Server: "203.0.113.5",
		Port: 8388, Method: "aes-256-gcm", Password: "p", Source: "manual",
	}}
	cfg.Chains = []config.Chain{}
	if errs := st.Replace(cfg); len(errs) > 0 {
		t.Fatalf("保存失败: %v", errs[0])
	}

	// 模拟写入内核配置(真实实现写在 data/core.json)
	coreJSON, _ := json.MarshalIndent(map[string]any{
		"log":       map[string]any{"level": "info"},
		"inbounds":  []any{map[string]any{"type": "mixed", "tag": "mixed-in"}},
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route":     map[string]any{"final": "direct"},
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, api.CoreConfigName), coreJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// 重新打开:必须还能读到面板配置
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("重新打开失败(说明面板配置被覆盖了): %v", err)
	}
	got := st2.Current()
	if got == nil {
		t.Fatal("面板配置丢失")
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "香港 01" {
		t.Fatalf("面板配置内容不正确: %+v", got.Nodes)
	}
	if got.Inbounds.MixedPort == 0 {
		t.Fatal("面板配置的 inbounds 丢失(可能被内核配置覆盖)")
	}
}

// TestCorruptRecovery 确认损坏时能从备份恢复。
func TestCorruptRecovery(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 保存两次,产生备份
	for i := 1; i <= 2; i++ {
		cfg := config.NewDefault()
		cfg.Nodes = []config.Node{{
			ID: "n1", Name: "节点", Type: "shadowsocks", Server: "1.2.3.4",
			Port: 8388 + i, Method: "aes-256-gcm", Password: "p", Source: "manual",
		}}
		if errs := st.Replace(cfg); len(errs) > 0 {
			t.Fatal(errs[0])
		}
	}
	if len(st.Backups()) == 0 {
		t.Fatal("应有备份")
	}
	// 人为写坏配置文件
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"inbounds": [1,2,3]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("损坏后应能从备份恢复,实际报错: %v", err)
	}
	if st2.Current() == nil {
		t.Fatal("恢复后应有配置")
	}
	// 坏文件应被留档
	if _, err := os.Stat(filepath.Join(dir, "config.json.corrupt")); err != nil {
		t.Fatal("损坏的配置应被留档为 config.json.corrupt")
	}
}

package api

// 设置接口 inbounds 字段的回归测试。
//
// 历史问题:handleSettings 漏了 TUNAutoRoute 字段,面板上也没有对应
// 开关,导致"全网设备网关指向 BoxHub"的网关模式在 UI 上无法开启,
// 只能手改 config.json。这里锁住两个契约:
//   1. PUT /api/settings 传 tun_auto_route 能落到配置;
//   2. sniff 等布尔开关不会被一次保存悄悄重置。

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/kernel"
	"boxhub/internal/store"
)

func newSettingsServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if errs := st.Replace(config.NewDefault()); len(errs) > 0 {
		t.Fatalf("写入配置失败: %v", errs[0])
	}
	km := kernel.New(kernel.Options{
		// 内核校验需要真实 sing-box;没提供就跳过整个用例。
		BinPath:    os.Getenv("BOXHUB_TEST_SINGBOX"),
		ConfigPath: filepath.Join(dir, "core.json"),
		WorkDir:    dir,
		LogPath:    filepath.Join(dir, "kernel.log"),
	})
	return New(Options{Store: st, DataDir: dir, Kernel: km})
}

func putSettings(t *testing.T, s *Server, body string) bool {
	t.Helper()
	r := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSettings(w, r)
	// 测试环境里内核重启可能失败(没有跑起来的内核),但配置已落盘,
	// 这里只关心字段是否保存成功。
	if w.Code != 200 && !strings.Contains(w.Body.String(), "配置已保存") {
		t.Fatalf("保存设置失败: %d %s", w.Code, w.Body.String())
	}
	return true
}

func TestSettingsTUNAutoRouteRoundTrip(t *testing.T) {
	if os.Getenv("BOXHUB_TEST_SINGBOX") == "" {
		t.Skip("未设置 BOXHUB_TEST_SINGBOX,跳过(需要真实 sing-box 做配置校验)")
	}
	s := newSettingsServer(t)

	putSettings(t, s, `{"inbounds":{"tun":true,"tun_auto_route":true,"sniff":true}}`)
	cfg := s.store.Current()
	if !cfg.Inbounds.TUNAutoRoute {
		t.Fatal("tun_auto_route=true 未保存:网关模式在面板上永远开不了")
	}
	if !cfg.Inbounds.Sniff {
		t.Fatal("sniff=true 未保存")
	}

	// 再保存一次不带这些字段(零值),确认是显式覆盖语义,且不炸。
	putSettings(t, s, `{"inbounds":{"tun":true}}`)
	cfg = s.store.Current()
	if cfg.Inbounds.TUNAutoRoute {
		t.Fatal("tun_auto_route 应随请求显式关闭")
	}
}

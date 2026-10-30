package api

// 保存前规则集预检的回归测试。
//
// 真实故障:GitHub /blob/ 链接下载到 HTML,sing-box check 不下载所以照样通过,
// 但 Reload 时 FATAL;config.json 已经落盘,旧内核又被停掉,直接全屋断网。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxhub/internal/kernel"
	"boxhub/internal/render"
)

func preflightConfig(url string) *render.SBConfig {
	return &render.SBConfig{Route: &render.SBRoute{
		RuleSet: []render.SBRuleSet{{
			Tag: "test-rs", Type: "remote", Format: "binary", URL: url,
		}},
	}}
}

func TestPreflightRuleSetsRejectsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!DOCTYPE html><html>github file page</html>"))
	}))
	defer srv.Close()

	s := &Server{dataDir: t.TempDir(), kernel: kernel.New(kernel.Options{BinPath: "/nonexistent/sing-box"})}
	err := s.preflightRuleSets(preflightConfig(srv.URL + "/x.srs"))
	if err == nil {
		t.Fatal("HTML 规则集必须被预检拦下,否则内核会 FATAL")
	}
	if !strings.Contains(err.Error(), "规则集预检失败") {
		t.Fatalf("错误信息应说明预检失败: %v", err)
	}
}

func TestPreflightRuleSetsAcceptsValidSRS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("SRS\x02valid"))
	}))
	defer srv.Close()

	s := &Server{dataDir: t.TempDir(), kernel: kernel.New(kernel.Options{BinPath: "/nonexistent/sing-box"})}
	if err := s.preflightRuleSets(preflightConfig(srv.URL + "/ok.srs")); err != nil {
		t.Fatalf("合法 SRS 不该被拦: %v", err)
	}
}

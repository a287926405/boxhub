package api

// 内核面板(zashboard)入口地址的回归测试。
//
// 这里是"内核面板打不开"最集中的地方,两个契约必须守住:
//   1. 后端参数走 **hash 之前的 query string**(zashboard 的 setup 页读
//      window.location.search;写在 #/setup? 后面它拿不到);
//   2. 入口 URL 必须带上面板自己的端口。只写 host 的话,面板跑在
//      3036(80 被占用时的回退端口)或任何非默认端口时,iframe 会去请求
//      http://host/ui/ 而不是 http://host:3036/ui/,面板直接 404。

import (
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/store"
)

func newKernelUIServer(t *testing.T, secret string) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewDefault()
	cfg.Inbounds.ClashAPISecret = secret
	if errs := st.Replace(cfg); len(errs) > 0 {
		t.Fatalf("写入配置失败: %v", errs[0])
	}
	s := New(Options{
		Store:     st,
		DataDir:   dir,
		ClashAddr: func() string { return "http://127.0.0.1:9090" },
		ClashSecret: func() string { return secret },
	})
	return s
}

func kernelUIURL(t *testing.T, s *Server, host string, tlsConn bool) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/kernel-ui-url", nil)
	r.Host = host
	if tlsConn {
		r.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	s.handleKernelUIURL(w, r)
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, w.Body.String())
	}
	return out.URL
}

func TestKernelUIURLKeepsPanelPort(t *testing.T) {
	s := newKernelUIServer(t, "")

	// 非默认端口(例如 80 被占用后回退的 3036):入口必须带端口
	got := kernelUIURL(t, s, "10.10.10.45:3036", false)
	if !strings.Contains(got, "//10.10.10.45:3036/ui/") {
		t.Errorf("入口地址丢了端口,iframe 会打到 80: %s", got)
	}
	if !strings.Contains(got, "hostname=10.10.10.45") || !strings.Contains(got, "port=3036") {
		t.Errorf("后端参数不对: %s", got)
	}

	// 默认 80:host 不该写成 host:80,但参数里 port=80 要在
	got = kernelUIURL(t, s, "10.10.10.45", false)
	if !strings.HasPrefix(got, "http://10.10.10.45/ui/") {
		t.Errorf("80 端口不该出现在 host 里: %s", got)
	}
	if !strings.Contains(got, "port=80") {
		t.Errorf("后端参数缺 port=80: %s", got)
	}
}

func TestKernelUIURLContract(t *testing.T) {
	s := newKernelUIServer(t, "s3cr3t")
	got := kernelUIURL(t, s, "boxhub.lan", false)

	// 参数必须在 hash 之前
	q := got[strings.Index(got, "?")+1:]
	hashAt := strings.Index(q, "#")
	if hashAt < 0 {
		t.Fatalf("没有 hash 段: %s", got)
	}
	search := q[:hashAt]
	if !strings.Contains(search, "hostname=boxhub.lan") || !strings.Contains(search, "secondaryPath=%2Fclash") {
		t.Errorf("query 段缺少后端参数: %s", search)
	}
	if strings.Contains(q[hashAt:], "hostname=") {
		t.Errorf("参数被写到 hash 之后,zashboard 读不到: %s", got)
	}
	// 密钥与同源反代路径
	if !strings.Contains(search, "secret=s3cr3t") {
		t.Errorf("缺少 secret: %s", got)
	}
	if !strings.Contains(got, "/ui/") || !strings.Contains(search, "disableUpgradeCore=1") {
		t.Errorf("入口路径或 disableUpgradeCore 不对: %s", got)
	}
	// 不该再引 metacubexd 的 config.js 方案
	if strings.Contains(got, "config.js") {
		t.Errorf("不应出现 metacubexd 的 config.js 契约: %s", got)
	}
}

func TestKernelUIURLHTTPSUses443(t *testing.T) {
	s := newKernelUIServer(t, "")
	got := kernelUIURL(t, s, "panel.example.com", true)
	if !strings.HasPrefix(got, "https://panel.example.com/ui/") {
		t.Errorf("https 入口不对: %s", got)
	}
	if !strings.Contains(got, "protocol=https") || !strings.Contains(got, "port=443") {
		t.Errorf("https 参数不对: %s", got)
	}
}

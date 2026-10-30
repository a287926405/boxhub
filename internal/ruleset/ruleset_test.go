package ruleset

// 本地规则集缓存的单元测试。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileNameUniquePerURL(t *testing.T) {
	a := FileName("https://example.com/geo/geosite/instagram.srs", "binary")
	b := FileName("https://example.com/geo/geosite/youtube.srs", "binary")
	if a == b {
		t.Fatal("不同 URL 得到同一个文件名,缓存会互相覆盖")
	}
	if got := FileName("https://x/a.json", "source"); filepath.Ext(got) != ".json" {
		t.Fatalf("source 格式应以 .json 结尾,得到 %s", got)
	}
	if got := FileName("https://x/a.srs", "binary"); filepath.Ext(got) != ".srs" {
		t.Fatalf("binary 格式应以 .srs 结尾,得到 %s", got)
	}
}

func TestLocalPathAndNeedFetch(t *testing.T) {
	dir := t.TempDir()
	u := "https://example.com/a.srs"
	if p := LocalPath(dir, u, "binary"); p != "" {
		t.Fatal("还没下载就返回了路径")
	}
	if !NeedFetch(dir, u, "binary") {
		t.Fatal("文件不存在时应需要下载")
	}

	f := filepath.Join(dir, FileName(u, "binary"))
	if err := os.WriteFile(f, []byte("SRS\x02cached-body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := LocalPath(dir, u, "binary"); p != f {
		t.Fatalf("应返回 %s,得到 %q", f, p)
	}
	if NeedFetch(dir, u, "binary") {
		t.Fatal("文件刚写入不该判定为过期")
	}
}

// 空文件视为无效(下载中断会留下 0 字节文件)。
func TestEmptyFileTreatedAsMissing(t *testing.T) {
	dir := t.TempDir()
	u := "https://example.com/b.srs"
	f := filepath.Join(dir, FileName(u, "binary"))
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if p := LocalPath(dir, u, "binary"); p != "" {
		t.Fatal("0 字节文件不该当作有效缓存")
	}
	if !NeedFetch(dir, u, "binary") {
		t.Fatal("0 字节文件应需要重新下载")
	}
}

func TestEnsureDownloadsAndCaches(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("SRS\x02rule-set-body"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	refs := []Ref{
		{URL: srv.URL + "/a.srs", Format: "binary"},
		{URL: srv.URL + "/b.srs", Format: "binary"},
		{URL: srv.URL + "/a.srs", Format: "binary"}, // 重复项要去重
	}
	res := Ensure(context.Background(), dir, refs, "", nil)
	if res.Failed != 0 {
		t.Fatalf("下载失败: %v", res.Errors)
	}
	if res.Downloaded != 2 {
		t.Fatalf("应下载 2 个(去重后),实际 %d", res.Downloaded)
	}
	if hits != 2 {
		t.Fatalf("服务端应被请求 2 次,实际 %d", hits)
	}
	for _, r := range refs[:2] {
		p := LocalPath(dir, r.URL, r.Format)
		if p == "" {
			t.Fatalf("%s 没有落地", r.URL)
		}
		b, _ := os.ReadFile(p)
		if string(b) != "SRS\x02rule-set-body" {
			t.Fatalf("内容不对: %q", b)
		}
	}

	// 再跑一次不应重复下载
	res2 := Ensure(context.Background(), dir, refs, "", nil)
	if res2.Downloaded != 0 {
		t.Fatalf("缓存有效时不该重复下载,实际 %d", res2.Downloaded)
	}
}

func TestEnsureHandlesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	res := Ensure(context.Background(), dir, []Ref{{URL: srv.URL + "/x.srs", Format: "binary"}}, "", nil)
	if res.Failed != 1 {
		t.Fatalf("404 应记为失败,实际 Failed=%d", res.Failed)
	}
	if len(res.Errors) == 0 {
		t.Fatal("失败时应给出原因")
	}
	if LocalPath(dir, srv.URL+"/x.srs", "binary") != "" {
		t.Fatal("失败时不该留下缓存文件")
	}
}

// 目录为空时直接返回,不 panic。
func TestEnsureNoopWithoutDir(t *testing.T) {
	res := Ensure(context.Background(), "", []Ref{{URL: "https://example.com/a.srs"}}, "", nil)
	if res.Downloaded != 0 || res.Failed != 0 {
		t.Fatal("没有目录时应当什么都不做")
	}
}

// GitHub /blob/ 网页返回的是 HTML,绝不能落盘成 .srs。
func TestEnsureRejectsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!DOCTYPE html><html><body>not a rule set</body></html>"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := srv.URL + "/cloudflare.srs"
	res := Ensure(context.Background(), dir, []Ref{{URL: u, Format: "binary"}}, "", nil)
	if res.Failed != 1 {
		t.Fatalf("HTML 应记为失败,实际 Failed=%d", res.Failed)
	}
	if LocalPath(dir, u, "binary") != "" {
		t.Fatal("HTML 不该被缓存成规则集")
	}
}

// 已经缓存了坏文件(旧版本留下的)时,LocalPath 必须拒绝,
// 否则渲染会一直用 type:local 指向坏文件,内核永远起不来。
func TestLocalPathIgnoresInvalidCache(t *testing.T) {
	dir := t.TempDir()
	u := "https://example.com/bad.srs"
	f := filepath.Join(dir, FileName(u, "binary"))
	if err := os.WriteFile(f, []byte("<!DOCTYPE html><html>oops</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := LocalPath(dir, u, "binary"); p != "" {
		t.Fatalf("坏缓存不该被当作可用规则集: %s", p)
	}
	if !NeedFetch(dir, u, "binary") {
		t.Fatal("坏缓存应触发重新下载")
	}
}

func TestValidateFileSource(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"version":2,"rules":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(good, "source"); err != nil {
		t.Fatalf("合法 JSON 规则集不应报错: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`<!DOCTYPE html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(bad, "source"); err == nil {
		t.Fatal("HTML 伪装成 JSON 应报错")
	}
}

// 直连失败(404)时必须自动换镜像重试,且缓存键仍是原始 URL。
func TestEnsureFallsBackToMirror(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusNotFound)
	}))
	defer direct.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// gh-proxy 形态:路径里带完整原始 URL
		if !strings.Contains(r.URL.RequestURI(), "github.com") {
			t.Errorf("镜像收到的不是前缀拼接地址: %s", r.URL.RequestURI())
		}
		w.Write([]byte("SRS\x02via-mirror"))
	}))
	defer mirror.Close()

	dir := t.TempDir()
	raw := direct.URL + "/github.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/cn.srs"
	res := Ensure(context.Background(), dir, []Ref{{URL: raw, Format: "binary"}}, "", []string{mirror.URL})
	if res.Downloaded != 1 || res.Failed != 0 {
		t.Fatalf("镜像回退应成功,实际 %+v %v", res, res.Errors)
	}
	p := LocalPath(dir, raw, "binary")
	if p == "" {
		t.Fatal("缓存键必须是原始 URL,而不是镜像地址")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "SRS\x02via-mirror" {
		t.Fatalf("内容不对: %q", b)
	}
}

func TestMirrorCandidates(t *testing.T) {
	got := MirrorCandidates("https://github.com/a/b.srs", []string{" https://m1.example ", "", "not-http", "https://m2.example/"})
	want := []string{
		"https://github.com/a/b.srs",
		"https://m1.example/https://github.com/a/b.srs",
		"https://m2.example/https://github.com/a/b.srs",
	}
	if len(got) != len(want) {
		t.Fatalf("候选数不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("候选[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

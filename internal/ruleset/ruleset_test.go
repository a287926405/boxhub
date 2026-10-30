package ruleset

// 本地规则集缓存的单元测试。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
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
		w.Write([]byte("rule-set-body"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	refs := []Ref{
		{URL: srv.URL + "/a.srs", Format: "binary"},
		{URL: srv.URL + "/b.srs", Format: "binary"},
		{URL: srv.URL + "/a.srs", Format: "binary"}, // 重复项要去重
	}
	res := Ensure(context.Background(), dir, refs, "")
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
		if string(b) != "rule-set-body" {
			t.Fatalf("内容不对: %q", b)
		}
	}

	// 再跑一次不应重复下载
	res2 := Ensure(context.Background(), dir, refs, "")
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
	res := Ensure(context.Background(), dir, []Ref{{URL: srv.URL + "/x.srs", Format: "binary"}}, "")
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
	res := Ensure(context.Background(), "", []Ref{{URL: "https://example.com/a.srs"}}, "")
	if res.Downloaded != 0 || res.Failed != 0 {
		t.Fatal("没有目录时应当什么都不做")
	}
}

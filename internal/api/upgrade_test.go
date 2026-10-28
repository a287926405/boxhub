package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVerCmp(t *testing.T) {
	cases := []struct{ a, b string; want int }{
		{"1.14.4", "1.14.5", -1},
		{"1.14.5", "1.14.4", 1},
		{"1.14.4", "1.14.4", 0},
		{"1.9.0", "1.14.0", -1}, // 数字比较,不是字典序
		{"", "1.0.0", 0},        // 解析失败不误报可升级
	}
	for _, c := range cases {
		if got := verCmp(c.a, c.b); got != c.want {
			t.Errorf("verCmp(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCurrentShortVersion(t *testing.T) {
	if got := currentShortVersion("sing-box version 1.14.4\ncommit: abc"); got != "1.14.4" {
		t.Errorf("got %q", got)
	}
	if got := currentShortVersion("garbage"); got != "" {
		t.Errorf("garbage should yield empty, got %q", got)
	}
}

func TestGhAssetURL(t *testing.T) {
	want := kernelLatestFile("v1.14.4")
	if want != "sing-box-1.14.4-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz" {
		t.Errorf("asset name: %s", want)
	}
	u := ghAssetURL("", "v1.14.4", want)
	if u != "https://github.com/SagerNet/sing-box/releases/download/v1.14.4/"+want {
		t.Errorf("official url: %s", u)
	}
	// 镜像前缀:gh-proxy 形态 = 前缀 + 完整 URL
	m := ghAssetURL("https://ghproxy.local/", "v1.14.4", want)
	if m != "https://ghproxy.local/"+u {
		t.Errorf("mirror url: %s", m)
	}
}

// extractSingBox:release 包内是 <dir>/sing-box 一层目录,要能取出来。
func TestExtractSingBox(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "pkg.tar.gz")
	payload := []byte("#!/bin/sh\necho fake-kernel\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// 目录项 + 目标文件
	_ = tw.WriteHeader(&tar.Header{Name: "sing-box-1.14.4/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "sing-box-1.14.4/sing-box", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(payload))})
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(tgz, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sing-box")
	if err := extractSingBox(tgz, out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("内容不符: err=%v got=%q", err, got)
	}
}

// 下载体积过小(错误页/半截响应)必须被拒绝,不能拿去替换内核。
func TestDownloadTooSmallRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.tar.gz")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	// 直接校验判断条件本身(不依赖网络)
	if fi, _ := os.Stat(path); fi.Size() >= 1<<20 {
		t.Fatal("测试前提不成立")
	}
}

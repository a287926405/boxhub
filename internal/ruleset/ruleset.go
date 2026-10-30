// Package ruleset 负责把远程规则集预取到本地磁盘。
//
// 背景(真实故障):
//
//	sing-box 启动时会先下载所有 remote 规则集,下载失败直接 FATAL,
//	整个内核起不来 —— 表现为"服务 active 但内核没跑,全屋断网"。
//	国内直连 github.com 基本不通,只能让下载走代理;可代理又依赖内核,
//	一旦节点全挂,内核会因为下载不到规则集而永远起不来(连直连也救不回来)。
//
// 解法:把规则集缓存成本地文件,渲染时优先引用本地路径。
//   - 文件在 → 内核启动零网络依赖,秒起;
//   - 文件缺 → 仍然退回远程 URL(行为与以前一致,不会更差);
//   - 补齐放在后台做,不阻塞面板保存设置。
package ruleset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxAge 本地文件多久算过期。过期不删,只是后台会重新拉一次。
const MaxAge = 24 * time.Hour

// downloadTimeout 单个规则集的下载上限。
const downloadTimeout = 90 * time.Second

// concurrency 同时下载的数量。规则集动辄几 MB,并发太高会把代理打满。
const concurrency = 4

// FileName 返回某个 URL 对应的本地文件名。
//
// 用 URL 的哈希而不是可读名:不同规则集可能同名(instagram.srs / geosite/instagram.srs),
// 哈希能保证唯一,而且改 tag 方案时缓存不会失效。
func FileName(rawURL, format string) string {
	sum := sha256.Sum256([]byte(rawURL))
	ext := "srs"
	if strings.EqualFold(format, "source") {
		ext = "json"
	}
	return hex.EncodeToString(sum[:8]) + "." + ext
}

// LocalPath 返回已存在的本地文件路径;不存在返回空串。
func LocalPath(dir, rawURL, format string) string {
	if dir == "" || rawURL == "" {
		return ""
	}
	p := filepath.Join(dir, FileName(rawURL, format))
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		return p
	}
	return ""
}

// NeedFetch 判断这个 URL 是否需要(重新)下载。
func NeedFetch(dir, rawURL, format string) bool {
	p := filepath.Join(dir, FileName(rawURL, format))
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		return true
	}
	return time.Since(st.ModTime()) > MaxAge
}

// Ref 一条待下载的规则集。
type Ref struct {
	URL    string
	Format string // binary / source
}

// Result 预取结果。
type Result struct {
	Downloaded int
	Failed     int
	Errors     []string
}

// Ensure 并发下载缺失/过期的规则集。
//
// proxyURL 形如 "http://127.0.0.1:7890";为空表示直连。
// 任何单个失败都只记录,不返回错误 —— 调用方按"本地有就用、没有就退回远程"处理。
func Ensure(ctx context.Context, dir string, refs []Ref, proxyURL string) Result {
	var res Result
	if dir == "" || len(refs) == 0 {
		return res
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Errors = append(res.Errors, "创建目录失败: "+err.Error())
		return res
	}

	client := newClient(proxyURL)

	// 去重
	seen := map[string]bool{}
	var todo []Ref
	for _, r := range refs {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		if NeedFetch(dir, r.URL, r.Format) {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return res
	}

	sem := make(chan struct{}, concurrency)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, r := range todo {
		wg.Add(1)
		go func(r Ref) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := fetchOne(ctx, client, dir, r); err != nil {
				mu.Lock()
				res.Failed++
				if len(res.Errors) < 5 {
					res.Errors = append(res.Errors, filepath.Base(r.URL)+": "+err.Error())
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			res.Downloaded++
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return res
}

func newClient(proxyURL string) *http.Client {
	tr := &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 4,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: downloadTimeout}
}

// fetchOne 下载到临时文件再改名,避免下到一半的内核读到半截文件。
func fetchOne(ctx context.Context, client *http.Client, dir string, r Ref) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "BoxHub-RuleSet")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	dest := filepath.Join(dir, FileName(r.URL, r.Format))
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 64<<20)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

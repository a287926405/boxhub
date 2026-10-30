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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// maxRuleSetSize 单个规则集文件上限,防止把磁盘写满。
const maxRuleSetSize = 64 << 20

// DefaultMirrors GitHub 直连失败时按序尝试的镜像前缀(gh-proxy 形态:
// 前缀 + 完整 URL)。公共镜像存活变动频繁,实测(2026-10):
//   - gh-proxy.com   可用,支持 raw / releases 大文件 / api.github.com
//   - gh-proxy.net   可用(302 跳转)
//   - gh.ddlc.top    可用但有速率限制
//
// 环境变量 BOXHUB_GH_MIRROR(逗号/空格分隔)可整体覆盖或前置。
var DefaultMirrors = []string{
	"https://gh-proxy.com",
	"https://gh-proxy.net",
	"https://gh.ddlc.top",
}

// MirrorCandidates 返回一个 URL 的下载候选:原始地址在前(缓存键不变),
// 之后依次是各镜像前缀拼出的地址。
func MirrorCandidates(rawURL string, mirrors []string) []string {
	out := make([]string, 0, 1+len(mirrors))
	out = append(out, rawURL)
	for _, m := range mirrors {
		m = strings.TrimRight(strings.TrimSpace(m), "/")
		if m == "" || !strings.HasPrefix(m, "http") {
			continue
		}
		out = append(out, m+"/"+rawURL)
	}
	return out
}

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

// ValidateFile 检查一个本地文件是否真的是 sing-box 规则集。
//
// 真实故障:GitHub 的 /blob/ 网页链接会返回 200 + HTML,旧代码只看文件
// 大小,于是把 HTML 当 .srs 缓存下来;之后每次启动都渲染成 type:local,
// 内核固定报 invalid sing-box rule-set file,而且删掉配置也没用 ——
// 必须先删缓存文件。所以缓存命中前必须验证内容。
func ValidateFile(path, format string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return errors.New("空文件")
	}

	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if looksLikeHTML(head) {
		return errors.New("下载到的是网页(HTML),不是规则集文件;GitHub 链接请用 raw 地址")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if strings.EqualFold(format, "source") {
		var doc struct {
			Version int             `json:"version"`
			Rules   json.RawMessage `json:"rules"`
		}
		if err := json.NewDecoder(io.LimitReader(f, maxRuleSetSize)).Decode(&doc); err != nil {
			return fmt.Errorf("JSON 解析失败: %w", err)
		}
		if doc.Version <= 0 {
			return errors.New("不是有效的 sing-box 源格式规则集(缺少 version)")
		}
		if len(doc.Rules) == 0 {
			return errors.New("不是有效的 sing-box 源格式规则集(缺少 rules)")
		}
		return nil
	}

	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("文件太短,读不到规则集头: %w", err)
	}
	if !bytes.Equal(hdr[:3], []byte("SRS")) {
		return fmt.Errorf("不是 sing-box 二进制规则集(文件头为 %q)", hdr[:3])
	}
	if hdr[3] == 0 {
		return errors.New("规则集版本号为 0,文件无效")
	}
	return nil
}

func looksLikeHTML(head []byte) bool {
	t := bytes.TrimSpace(head)
	if len(t) == 0 {
		return false
	}
	return t[0] == '<' ||
		bytes.HasPrefix(bytes.ToLower(t), []byte("<!doctype")) ||
		bytes.HasPrefix(bytes.ToLower(t), []byte("<html"))
}

// LocalPath 返回已存在且内容有效的本地文件路径;不存在/损坏返回空串。
func LocalPath(dir, rawURL, format string) string {
	if dir == "" || rawURL == "" {
		return ""
	}
	p := filepath.Join(dir, FileName(rawURL, format))
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		if err := ValidateFile(p, format); err == nil {
			return p
		}
	}
	return ""
}

// NeedFetch 判断这个 URL 是否需要(重新)下载。损坏的缓存也视为需要下载。
func NeedFetch(dir, rawURL, format string) bool {
	p := filepath.Join(dir, FileName(rawURL, format))
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 {
		return true
	}
	if err := ValidateFile(p, format); err != nil {
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
// mirrors 是镜像前缀候选:直连失败时自动换镜像重试(国内直连 GitHub
// 不通是常态,镜像前缀 + 完整 URL 是目前最稳的路子)。
// 任何单个失败都只记录,不返回错误 —— 调用方按"本地有就用、没有就退回远程"处理。
func Ensure(ctx context.Context, dir string, refs []Ref, proxyURL string, mirrors []string) Result {
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
			if err := fetchOne(ctx, client, dir, r, mirrors); err != nil {
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
// 直连失败(超时/404/HTML)自动依次尝试镜像候选,缓存文件名始终以原始
// URL 为键,镜像只影响传输路径。
func fetchOne(ctx context.Context, client *http.Client, dir string, r Ref, mirrors []string) error {
	var errs []string
	for _, u := range MirrorCandidates(r.URL, mirrors) {
		err := fetchFrom(ctx, client, dir, r, u)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		errs = append(errs, u+": "+err.Error())
	}
	if len(errs) == 0 {
		return errors.New("没有可用的下载候选")
	}
	// 只保留最后一条完整错误 + 候选数量,别把面板刷爆
	last := errs[len(errs)-1]
	if i := strings.Index(last, " "); i > 0 {
		last = last[i+1:]
	}
	return fmt.Errorf("%w(已尝试 %d 个源)", errors.New(last), len(errs))
}

func fetchFrom(ctx context.Context, client *http.Client, dir string, r Ref, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "text/html") {
		return errors.New("返回的是网页(HTML),不是规则集文件;GitHub 链接请用 raw 地址")
	}

	dest := filepath.Join(dir, FileName(r.URL, r.Format))
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxRuleSetSize+1))
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if n > maxRuleSetSize {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("文件超过 %d MB,拒绝缓存", maxRuleSetSize>>20)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// 落盘前先校验:防止把 HTML/404 页面当成规则集缓存下来。
	if err := ValidateFile(tmp, r.Format); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("下载内容无效: %w", err)
	}
	return os.Rename(tmp, dest)
}

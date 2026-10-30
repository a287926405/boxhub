package api

// sing-box 内核在线检测与升级。
//
// 思路(SMBox 同款):查 GitHub 最新 release → 下载与当前系统同 OS/架构的
// tar.gz → 解出 sing-box → **先用新版本号跑一次验证可执行**(架构错下载
// 当场暴露)→ 备份旧文件后原子替换(rename)→ 重启内核;重启失败自动回滚。
//
// 容器/内网机器到 GitHub 可能慢或不通:支持自定义镜像前缀(mirror),
// 下载失败给出可操作提示,不影响面板其它功能。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"

	"boxhub/internal/ruleset"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	singboxRepo   = "SagerNet/sing-box"
	downloadLimit = 160 << 20 // 160MB 上限,防异常大文件
	checkCacheTTL = 10 * time.Minute
)

// upgProg 下载进度(供前端轮询显示"已下多少/共多少",
// 慢网络下用户能分辨"在爬"还是"卡死")。
type upgProg struct {
	mu         sync.Mutex
	stage      string
	downloaded int64
	total      int64
	at         time.Time
}

var upg = &upgProg{}

func (u *upgProg) setStage(v string) {
	u.mu.Lock()
	u.stage, u.at = v, time.Now()
	u.mu.Unlock()
}

func (u *upgProg) setProgress(done, total int64) {
	u.mu.Lock()
	u.downloaded, u.total, u.at = done, total, time.Now()
	u.mu.Unlock()
}

func (u *upgProg) snapshot() (string, int64, int64, time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.stage, u.downloaded, u.total, u.at
}

var (
	upgradeMu  sync.Mutex // 同一时间只允许一次升级
	checkCache struct {
		sync.Mutex
		at   time.Time
		data map[string]any
	}
	versionRe = regexp.MustCompile(`(\d+\.\d+\.\d+)`)
)

// ghAssetURL 按镜像前缀拼 release 下载地址。
// mirror 为空 → GitHub 官方;形如 https://ghproxy.local/ → 前缀拼接。
func ghAssetURL(mirror, tag, file string) string {
	base := "https://github.com/" + singboxRepo + "/releases/download/" + tag + "/" + file
	if m := strings.Trim(strings.TrimSpace(mirror), "/"); m != "" {
		// 常见 gh-proxy 形态:前缀 + 完整 github URL
		if strings.HasPrefix(m, "http") {
			return m + "/" + base
		}
	}
	return base
}

// ghURLCandidates 返回一个 GitHub 地址的下载候选:显式指定的 mirror 最优先,
// 其次直连(境外机器一次命中),最后内置镜像(国内直连不通的兜底)。
// mirror 传 "" 表示用户没有指定,直接从直连开始。
func ghURLCandidates(mirror, rawURL string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p != "" && !strings.HasPrefix(p, "http") {
			return
		}
		u := rawURL
		if p != "" {
			u = p + "/" + rawURL
		}
		if seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	if m := strings.TrimSpace(mirror); m != "" {
		add(m)
	}
	add("")
	for _, m := range ruleset.DefaultMirrors {
		add(m)
	}
	return out
}

// verCmp 比较 x.y.z;返回 -1/0/1。解析失败按相等处理(不误报可升级)。
func verCmp(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		x, e1 := strconv.Atoi(pa[i])
		y, e2 := strconv.Atoi(pb[i])
		if e1 != nil || e2 != nil {
			continue
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func currentShortVersion(full string) string {
	if m := versionRe.FindString(full); m != "" {
		return m
	}
	return ""
}

// kernelLatestFile 当前平台期望的 release 文件名。
func kernelLatestFile(tag string) string {
	return fmt.Sprintf("sing-box-%s-%s-%s.tar.gz", strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// handleKernelUpdate 检测最新版本。?mirror=xx 走代理站。
func (s *Server) handleKernelUpdate(w http.ResponseWriter, r *http.Request) {
	mirror := r.URL.Query().Get("mirror")
	cur := currentShortVersion(s.kernel.Version())

	// 缓存:10 分钟内同参数不重复打 GitHub API(匿名限流 60 次/小时)
	cacheKey := mirror
	checkCache.Lock()
	if checkCache.data != nil && checkCache.at.After(time.Now().Add(-checkCacheTTL)) {
		if k, _ := checkCache.data["cache_key"].(string); k == cacheKey {
			out := map[string]any{}
			for kk, vv := range checkCache.data {
				if kk != "cache_key" {
					out[kk] = vv
				}
			}
			out["cached"] = true
			checkCache.Unlock()
			writeOK(w, out)
			return
		}
	}
	checkCache.Unlock()

	// gh-proxy 类镜像也能代理 api.github.com(实测);候选链:
	// 用户镜像 → 直连 → 内置镜像,任一拿到结果即止。
	apiURL := "https://api.github.com/repos/" + singboxRepo + "/releases/latest"
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	var (
		resp *http.Response
		last error
	)
	for _, u := range ghURLCandidates(mirror, apiURL) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		req.Header.Set("User-Agent", "BoxHub/update-check")
		resp, err = http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == 200 {
			break
		}
		if resp != nil {
			resp.Body.Close()
			last = fmt.Errorf("HTTP %d (%s)", resp.StatusCode, u)
		} else {
			last = fmt.Errorf("%v (%s)", err, u)
		}
		resp = nil
		if ctx.Err() != nil {
			break
		}
	}
	if resp == nil {
		if last == nil {
			last = errors.New("没有可用的下载源")
		}
		writeErr(w, 502, "无法访问 GitHub API(镜像与直连都失败): "+last.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		writeErr(w, 502, fmt.Sprintf("GitHub API 返回 %d(可能触发匿名限流,稍后再试)", resp.StatusCode))
		return
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rel); err != nil {
		writeErr(w, 502, "解析 release 失败: "+err.Error())
		return
	}
	want := kernelLatestFile(rel.TagName)
	found := false
	for _, a := range rel.Assets {
		if a.Name == want {
			found = true
		}
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	avail := cur != "" && latest != "" && verCmp(cur, latest) < 0
	out := map[string]any{
		"current": cur, "latest": latest, "available": avail,
		"asset": want, "asset_ok": found, "platform": runtime.GOOS + "/" + runtime.GOARCH,
	}
	checkCache.Lock()
	dataCopy := map[string]any{}
	for k, v := range out {
		dataCopy[k] = v
	}
	dataCopy["cache_key"] = cacheKey
	checkCache.data = dataCopy
	checkCache.at = time.Now()
	checkCache.Unlock()
	writeOK(w, out)
}

// handleKernelUpgrade 下载并替换内核,成功后重启;失败回滚旧文件。
func (s *Server) handleKernelUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "仅支持 POST")
		return
	}
	var body struct {
		Mirror string `json:"mirror"`
		Tag    string `json:"tag"` // 指定版本(默认取检测到的 latest)
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	fromVer := s.kernel.Version()
	if !upgradeMu.TryLock() {
		writeErr(w, 409, "已有升级任务在进行")
		return
	}
	defer upgradeMu.Unlock()

	// 目标版本:优先请求指定;否则用检测缓存的 latest
	tag := strings.TrimSpace(body.Tag)
	if tag == "" {
		checkCache.Lock()
		if checkCache.data != nil {
			tag = "v" + fmt.Sprint(checkCache.data["latest"])
		}
		checkCache.Unlock()
	}
	if tag == "v" || tag == "" {
		writeErr(w, 400, "尚未检测可用新版本(先点“检测更新”)")
		return
	}
	file := kernelLatestFile(tag)
	rawURL := "https://github.com/" + singboxRepo + "/releases/download/" + tag + "/" + file

	tmpDir, err := os.MkdirTemp("", "boxhub-upgrade")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)

	// 下载走候选链:用户镜像 → 直连 → 内置镜像。内核包几十 MB,
	// 国内直连 releases 普遍不通,镜像前缀是主要通路。
	s.reportUpgrade("正在下载 " + file)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var derr error
	for _, u := range ghURLCandidates(body.Mirror, rawURL) {
		s.reportUpgrade("正在下载 " + file + " (" + u + ")")
		derr = downloadTo(ctx, u, filepath.Join(tmpDir, file))
		if derr == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if derr != nil {
		writeErr(w, 502, "下载失败(已尝试镜像与直连): "+derr.Error()+" — 内网可填镜像地址")
		return
	}

	s.reportUpgrade("正在解压")
	binOut := filepath.Join(tmpDir, "sing-box")
	if err := extractSingBox(filepath.Join(tmpDir, file), binOut); err != nil {
		writeErr(w, 500, "解压失败: "+err.Error())
		return
	}
	if err := os.Chmod(binOut, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	s.reportUpgrade("正在验证新内核")
	vctx, vcancel := context.WithTimeout(context.Background(), 15*time.Second)
	out, err := exec.CommandContext(vctx, binOut, "version").CombinedOutput()
	vcancel()
	if err != nil {
		writeErr(w, 400, fmt.Sprintf("新内核无法运行(架构不匹配?): %v %s", err, firstLine(string(out))))
		return
	}
	newVer := currentShortVersion(string(out))
	if newVer == "" {
		writeErr(w, 400, "新版本号解析失败,已放弃")
		return
	}

	// 替换:备份旧文件 → 原子 rename → 重启;失败回滚
	binPath := s.kernel.BinPath()
	if binPath == "" {
		writeErr(w, 400, "无法定位当前内核文件")
		return
	}
	oldBackup := binPath + ".prev"
	s.reportUpgrade("正在替换内核文件并重启")
	if err := os.Rename(binPath, oldBackup); err != nil {
		writeErr(w, 500, "备份旧内核失败: "+err.Error())
		return
	}
	if err := os.Rename(binOut, binPath); err != nil {
		_ = os.Rename(oldBackup, binPath)
		_ = s.kernel.Restart()
		writeErr(w, 500, "替换失败已回滚: "+err.Error())
		return
	}
	if err := s.kernel.Restart(); err != nil {
		// 新内核起不来:回滚旧文件并恢复运行,把真实错误带给用户
		_ = s.kernel.Stop()
		_ = os.Remove(binPath)
		_ = os.Rename(oldBackup, binPath)
		_ = s.kernel.Restart()
		writeErr(w, 500, "新内核启动失败,已回滚到旧版本: "+err.Error())
		return
	}
	// 成功:更新内存中的版本描述,清掉检测缓存
	s.kernel.SetVersion(newVer)
	checkCache.Lock()
	checkCache.data = nil
	checkCache.Unlock()
	writeOK(w, map[string]any{"from": fromVer, "to": newVer, "restarted": true})
}

func downloadTo(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 32*1024)
	for {
		nr, rerr := io.ReadFull(resp.Body, buf)
		if nr > 0 {
			if _, werr := f.Write(buf[:nr]); werr != nil {
				return werr
			}
			done += int64(nr)
			// 每 256KB 更新一次,避免高频加锁
			if done%(256*1024) < int64(nr) {
				upg.setProgress(done, total)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	n := done
	upg.setProgress(done, total)
	if n < 1<<20 {
		return fmt.Errorf("文件过小(%d 字节),疑似错误响应", n)
	}
	return nil
}

// extractSingBox 从 tar.gz 里取名为 sing-box 的文件(release 内有一层目录)。
func extractSingBox(tgzPath, outPath string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != "sing-box" {
			continue
		}
		if h.Size > downloadLimit {
			return fmt.Errorf("sing-box 文件异常大: %d", h.Size)
		}
		out, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, tr); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("压缩包里没有 sing-box 文件")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// reportUpgrade 记录当前升级阶段,供前端轮询展示进度。
func (s *Server) reportUpgrade(stage string) { upg.setStage(stage) }

func (s *Server) handleKernelUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	stage, done, total, at := upg.snapshot()
	busy := upgradeMu.TryLock()
	if busy {
		upgradeMu.Unlock()
	}
	writeOK(w, map[string]any{
		"stage": stage, "downloaded": done, "total": total,
		"at": at.Format(time.RFC3339), "running": !busy,
	})
}

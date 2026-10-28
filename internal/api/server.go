// Package api 提供面板使用的 HTTP 接口。
//
// 设计原则:
//   - **保存即生效**:任何影响内核的改动都会依次执行「结构校验 → 生成配置 →
//     内核 check → 热重启」。内核 check 失败时**不落盘、不重启**,旧配置继续
//     服务,面板收到具体错误原因。
//   - 所有错误都用中文回显,面板直接展示。
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/kernel"
	"boxhub/internal/render"
	"boxhub/internal/store"
	"boxhub/internal/sub"
)

// Server 是 API 服务。
type Server struct {
	store     *store.Store
	kernel    *kernel.Manager
	dataDir   string
	clashAddr func() string
	clashSec  func() string
	static    http.Handler
	clashUI   http.Handler

	mu        sync.Mutex
	lastGen   []byte
	lastErr   string
	lastGenAt time.Time
	probes    map[string]config.ProbeResult

	// probeMu 串行化"临时切换组"的探测,避免两个探测互相覆盖选择
	probeMu sync.Mutex
	// dnsTrack 增量扫描内核日志聚合 DNS 统计(日志尾部窗口会被
	// outbound 行挤空,已复现两次"隔会儿就没了",见 dns_tracker.go)
	dnsTrack *dnsTracker

	// 流量速率计算:保存上一次的累计读数
	trafficMu     sync.Mutex
	lastTrafficUp   int64
	lastTrafficDown int64
	lastTrafficAt   time.Time
}

// CoreConfigName 是由面板配置**生成**的内核配置文件名。
//
// 它必须与面板配置文件(config.json)分开:早期版本两者都用 config.json,
// 导致内核配置把面板配置覆盖掉,下次启动直接读不回来(表现为"配置损坏")。
const CoreConfigName = "core.json"

// Options 构造参数。
type Options struct {
	Store   *store.Store
	Kernel  *kernel.Manager
	DataDir string
	// ClashAddr 返回本地 Clash API 地址(随配置变化)。
	ClashAddr func() string
	// ClashSecret 返回 Clash API 密钥。
	ClashSecret func() string
	// Static 自研面板静态文件。
	Static http.Handler
	// ClashUI metacubexd 静态文件(可为 nil)。
	ClashUI http.Handler
}

// New 创建服务。
func New(o Options) *Server {
	return &Server{
		store:     o.Store,
		dnsTrack:  newDNSTracker(filepath.Join(o.DataDir, "kernel.log")),
		kernel:    o.Kernel,
		dataDir:   o.DataDir,
		clashAddr: o.ClashAddr,
		clashSec:  o.ClashSecret,
		static:    o.Static,
		clashUI:   o.ClashUI,
		probes:    map[string]config.ProbeResult{},
	}
}

// Handler 返回带认证的根路由。
func (s *Server) Handler(auth *Auth) http.Handler {
	mux := http.NewServeMux()

	// ---- 未认证接口 ----
	mux.HandleFunc("/api/login", s.handleLogin(auth))
	mux.HandleFunc("/api/logout", s.handleLogout(auth))
	mux.HandleFunc("/api/session", s.handleSession(auth))

	// ---- 需要认证的接口 ----
	authed := http.NewServeMux()
	authed.HandleFunc("/api/overview", s.handleDashboard)
	authed.HandleFunc("/api/dashboard", s.handleDashboard)
	authed.HandleFunc("/api/kernel-ui-url", s.handleKernelUIURL)
	authed.HandleFunc("/api/kernel/update", s.handleKernelUpdate)
	authed.HandleFunc("/api/kernel/upgrade", s.handleKernelUpgrade)
	authed.HandleFunc("/api/kernel/upgrade/status", s.handleKernelUpgradeStatus)
	authed.HandleFunc("/api/logs", s.handleLogs)
	authed.HandleFunc("/api/speedtest", s.handleSpeedtest)
	authed.HandleFunc("/api/traffic", s.handleTraffic)
	authed.HandleFunc("/api/dns/stats", s.handleDNSStats)
	authed.HandleFunc("/api/config", s.handleConfig)
	authed.HandleFunc("/api/validate", s.handleValidate)
	authed.HandleFunc("/api/render", s.handleRender)

	authed.HandleFunc("/api/nodes", s.handleNodes)
	authed.HandleFunc("/api/nodes/", s.handleNodeByID)
	authed.HandleFunc("/api/subscriptions", s.handleSubscriptions)
	authed.HandleFunc("/api/subscriptions/", s.handleSubscriptionByID)

	authed.HandleFunc("/api/groups", s.handleGroups)
	authed.HandleFunc("/api/groups/", s.handleGroupByID)
	authed.HandleFunc("/api/chains", s.handleChains)
	authed.HandleFunc("/api/chains/", s.handleChainByID)
	authed.HandleFunc("/api/filters", s.handleFilters)
	// auto 必须注册在通配之前的意义上更具体,ServeMux 会优先选它
	authed.HandleFunc("/api/filters/auto", s.handleAutoFilters)
	authed.HandleFunc("/api/filters/", s.handleFilterByID)
	authed.HandleFunc("/api/condition-types", s.handleConditionTypes)
	authed.HandleFunc("/api/rule-templates", s.handleRuleTemplates)
	authed.HandleFunc("/api/rule-templates/apply", s.handleApplyTemplates)
	authed.HandleFunc("/api/policies", s.handlePolicies)
	authed.HandleFunc("/api/policies/", s.handlePolicyByID)

	authed.HandleFunc("/api/settings", s.handleSettings)
	authed.HandleFunc("/api/kernel/", s.handleKernel)
	authed.HandleFunc("/api/proxy/", s.handleProxy)
	authed.HandleFunc("/api/backups", s.handleBackups)
	authed.HandleFunc("/api/backups/", s.handleBackupByName)

	mux.Handle("/api/", auth.Middleware(authed))
	// 把 Clash API 反代到面板同源路径下:metacubexd 在 /ui/ 下运行时,
	// 直接连 9090 是跨域请求(浏览器会拦),反代后同源即可。
	mux.Handle("/clash/", s.clashProxy())
	// metacubexd 是独立的前端应用:它自己会连 Clash API(带密钥),不依赖面板会话,
	// 所以不能套 boxhub_session 认证,否则浏览器直接打开 /ui/ 只会拿到 401。
	mux.Handle("/ui/", http.StripPrefix("/ui/", uiNoCache(s.clashUIHandler())))
	mux.Handle("/", s.staticHandler())
	return mux
}

// clashProxy 把 /clash/* 转发到本地 Clash API。
//
// metacubexd 的默认后端地址可配成 window.location.origin + "/clash",
// 这样浏览器只与面板端口通信,不涉及跨域和额外的防火墙放行。
func (s *Server) clashProxy() http.Handler {
	target := func() string { return s.clashAddr() }
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			base := target()
			u, err := url.Parse(base)
			if err != nil {
				return
			}
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			// /clash/proxies -> /proxies
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/clash")
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			r.Host = u.Host
			// 注入密钥:前端不必知道 secret,也避免它被写死在页面里
			if sec := s.clashSec(); sec != "" {
				r.Header.Set("Authorization", "Bearer "+sec)
			}
			// 同源请求,去掉可能引起上游困惑的 Origin
			r.Header.Del("Origin")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":"内核未运行或 Clash API 不可达: %s"}`, strings.ReplaceAll(err.Error(), `"`, `'`))
		},
	}
	return proxy
}

func (s *Server) clashUIHandler() http.Handler {
	if s.clashUI == nil {
		// 没装 metacubexd 时给一段可读的说明,而不是空白 404。
		// 内网环境常见:安装时下载不通被跳过。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>内核面板未安装</title>
<style>body{font:14px/1.7 -apple-system,"PingFang SC","Microsoft YaHei",system-ui,sans-serif;
background:#0e1116;color:#e6edf3;display:grid;place-items:center;height:100vh;margin:0}
.box{max-width:600px;padding:28px;border:1px solid #2a323d;border-radius:12px;background:#161b22}
h2{margin:0 0 12px;font-size:18px}code{background:#1c232d;padding:2px 6px;border-radius:4px;font-size:12px}
p{color:#8b949e}ul{color:#8b949e;padding-left:20px}li{margin:6px 0}a{color:#2f81f7}</style>
</head><body><div class="box">
<h2>内核面板(metacubexd)未安装</h2>
<p>BoxHub 控制台本身可以正常使用,只是少了这个可视化内核面板。</p>
<p><b>原因</b>:安装时下载 metacubexd 失败(内网环境访问 GitHub 受限很常见),脚本自动跳过了它。</p>
<p><b>怎么补装</b>:在能上网的机器上下载
<code>https://github.com/MetaCubeX/metacubexd/releases/latest/download/compressed-dist.tgz</code>,
解压到 <code>/opt/boxhub/data/ui/</code> 后刷新本页。</p>
<p><b>不装也可以</b>:下面这些能直接看到内核信息 ——</p>
<ul>
<li><a href="/">返回 BoxHub 控制台</a>(概览页有实时连接数与流量)</li>
<li>Clash API 原始接口:<code>curl http://127.0.0.1:9090/proxies</code></li>
</ul>
</div></body></html>`)
		})
	}
	return s.clashUI
}

func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.NotFoundHandler()
	}
	return s.static
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// contextWithTimeout 基于请求上下文派生带超时的 context。
func contextWithTimeout(r *http.Request, seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func writeOK(w http.ResponseWriter, v any) {
	if v == nil {
		v = map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		m["ok"] = true
		writeJSON(w, 200, m)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "data": v})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeErr(w, 405, "请求方法不支持")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, 400, "读取请求失败")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return false
	}
	return true
}

func (s *Server) clash() *ClashClient {
	return NewClashClient(s.clashAddr(), s.clashSec())
}

// ---------------------------------------------------------------------------
// 核心:保存并应用配置
// ---------------------------------------------------------------------------

// applyResult 描述一次"保存 + 应用"的结果。
type applyResult struct {
	Restarted bool     `json:"restarted"`
	Warnings  []string `json:"warnings,omitempty"`
}

// apply 是**所有写操作的唯一出口**。流程:
//
//  1. 结构校验(引用完整性、循环依赖)
//  2. 生成 sing-box 配置
//  3. 内核 check(字段拼写、出站依赖环、规则集引用)
//  4. 通过 → 写盘 + 热重启内核
//     失败 → 全部回滚,内存与磁盘保持旧配置
func (s *Server) apply(mutate func(cfg *config.Config) error) (*applyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 在副本上改动
	cur := s.store.Current()
	if cur == nil {
		cur = config.NewDefault()
	}
	var (
		next     *config.Config
		warns    []string
		buildErr error
	)
	// 用 store 的 Update 做锁内读改写,但需要先做内核校验再落盘,
	// 所以这里自己实现:先算新配置,校验通过后调用 Replace。
	next = cur
	if err := mutate(next); err != nil {
		return nil, err
	}
	if errs := config.ValidateAll(next); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return nil, errors.New(strings.Join(msgs, "; "))
	}

	sb, buildErr := s.build(next)
	if buildErr != nil {
		return nil, buildErr
	}
	raw, err := sb.Marshal()
	if err != nil {
		return nil, err
	}

	// 写到临时文件并用内核校验
	tmpPath := filepath.Join(s.dataDir, "config.check.json")
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(tmpPath)
	if err := s.kernel.Check(tmpPath); err != nil {
		s.lastErr = err.Error()
		return nil, fmt.Errorf("内核校验未通过,配置未保存:\n%s", err.Error())
	}

	// 落盘
	if errs := s.store.Replace(next); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return nil, errors.New(strings.Join(msgs, "; "))
	}
	// 写正式配置并热重启
	finalPath := filepath.Join(s.dataDir, "core.json")
	if err := os.WriteFile(finalPath, raw, 0o600); err != nil {
		return nil, err
	}
	s.lastGen = raw
	s.lastGenAt = time.Now()
	res := &applyResult{Warnings: warns}
	if err := s.kernel.Reload(finalPath); err != nil {
		// 到这里配置已经校验过,失败多半是端口被占用等运行期问题
		s.lastErr = err.Error()
		return nil, fmt.Errorf("配置已保存,但内核重启失败: %w", err)
	}
	res.Restarted = true
	s.lastErr = ""
	return res, nil
}

// build 生成内核配置对象。
func (s *Server) build(cfg *config.Config) (*render.SBConfig, error) {
	return render.Build(cfg, render.Context{
		CacheFile:  filepath.Join(s.dataDir, "cache.db"),
		ClashUI:    s.clashUIPath(),
		RuleSetDir: filepath.Join(s.dataDir, "rules"),
	})
}

func (s *Server) clashUIPath() string {
	p := filepath.Join(s.dataDir, "ui")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ApplyStartup 在进程启动时生成配置并启动内核。
func (s *Server) ApplyStartup() error {
	cfg := s.store.Current()
	if cfg == nil {
		cfg = config.NewDefault()
		// 首次启动:写入默认配置并建默认组
		cfg.Groups = nil
		config.EnsureDefaultGroups(cfg, func() string { return store.RandomID() })
		if errs := s.store.Replace(cfg); len(errs) > 0 {
			return errors.New(errs[0].Error())
		}
	}
	sb, err := s.build(cfg)
	if err != nil {
		return err
	}
	raw, err := sb.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dataDir, "core.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastGen = raw
	s.lastGenAt = time.Now()
	s.mu.Unlock()
	// 内核启动失败不应阻塞面板:面板要能进去改配置修问题
	if err := s.kernel.Start(); err != nil {
		s.lastErr = err.Error()
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

func (s *Server) handleLogin(auth *Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		// 首次使用时没有密码:这一条请求就是"设置密码 + 登录"。
		// 之后必须校验密码,不再允许用登录接口改密码。
		if !auth.HasPassword() {
			clientIP := ClientIP(r)
			if !isLocalIP(clientIP) {
				writeErr(w, 403, "首次设置密码请在本机访问面板完成")
				return
			}
			if err := auth.SetPassword(req.Password); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			token, err := auth.Issue()
			if err != nil {
				writeErr(w, 500, "签发会话失败")
				return
			}
			setSessionCookie(w, token)
			writeOK(w, map[string]any{"first_time": true})
			return
		}
		if !auth.CheckPassword(req.Password) {
			writeErr(w, 401, "密码错误")
			return
		}
		token, err := auth.Issue()
		if err != nil {
			writeErr(w, 500, "签发会话失败")
			return
		}
		setSessionCookie(w, token)
		writeOK(w, nil)
	}
}

func (s *Server) handleLogout(auth *Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("boxhub_session"); err == nil {
			auth.Revoke(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "boxhub_session", Value: "", Path: "/", MaxAge: -1})
		writeOK(w, nil)
	}
}

func (s *Server) handleSession(auth *Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.LoggedIn(r) {
			writeOK(w, map[string]any{"logged_in": true})
			return
		}
		writeOK(w, map[string]any{"logged_in": false, "needs_setup": !auth.HasPassword()})
	}
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	st := s.kernel.Status()
	out := map[string]any{
		"kernel": map[string]any{
			"running":    st.Running,
			"pid":        st.PID,
			"version":    st.Version,
			"started_at": st.StartedAt,
			"last_error": st.LastError,
			"restarts":   st.Restarts,
		},
		"counts": map[string]int{
			"nodes":         len(cfg.Nodes),
			"groups":        len(cfg.Groups),
			"chains":        len(cfg.Chains),
			"policies":      len(cfg.Policies),
			"subscriptions": len(cfg.Subscriptions),
		},
		"updated_at": cfg.UpdatedAt,
	}
	// 实时数据:拿不到不影响页面
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	cl := s.clash()
	if v, err := cl.Version(ctx); err == nil {
		out["kernel"].(map[string]any)["api_version"] = v
		if cs, err := cl.Connections(ctx); err == nil {
			var up, down int64
			for _, c := range cs {
				up += c.Upload
				down += c.Download
			}
			out["connections"] = map[string]any{
				"count": len(cs), "upload": up, "download": down,
			}
		}
	} else {
		out["kernel"].(map[string]any)["api_error"] = err.Error()
	}
	s.mu.Lock()
	if s.lastErr != "" {
		out["last_error"] = s.lastErr
	}
	s.mu.Unlock()
	writeOK(w, out)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeOK(w, map[string]any{"config": s.store.Current()})
		return
	}
	// 整体替换(导入)
	var req struct {
		Config *config.Config `json:"config"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Config == nil {
		writeErr(w, 400, "缺少配置内容")
		return
	}
	res, err := s.apply(func(cfg *config.Config) error {
		*cfg = *req.Config
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, res)
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	issues := []string{}
	for _, e := range config.ValidateAll(cfg) {
		issues = append(issues, e.Error())
	}
	writeOK(w, map[string]any{"issues": issues})
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	sb, err := s.build(cfg)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	raw, err := sb.Marshal()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.mu.Lock()
	lastErr := s.lastErr
	s.mu.Unlock()
	writeOK(w, map[string]any{
		"config":     json.RawMessage(raw),
		"last_error": lastErr,
	})
}

// ---------------------------------------------------------------------------
// 通用 CRUD 辅助
// ---------------------------------------------------------------------------

// mutate 是带写锁的配置改动 + 应用。所有实体路由共用。
func (s *Server) mutate(fn func(cfg *config.Config) error) (*applyResult, error) {
	return s.apply(fn)
}

func genID() string { return "x" + store.RandomID() }

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.store.Current()
		writeOK(w, map[string]any{"nodes": cfg.Nodes})
	case http.MethodPost:
		var n config.Node
		if !readJSON(w, r, &n) {
			return
		}
		if n.Source == "" {
			n.Source = "manual"
		}
		if n.ID == "" {
			n.ID = "n-" + store.RandomID()
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			if findNode(cfg, n.ID) != nil {
				return errors.New("节点 ID 重复")
			}
			cfg.Nodes = append(cfg.Nodes, n)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res, "node": n})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

func (s *Server) handleNodeByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if rest == "parse" {
		s.handleNodeParse(w, r)
		return
	}
	id := rest
	if id == "" {
		writeErr(w, 400, "缺少节点 ID")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var n config.Node
		if !readJSON(w, r, &n) {
			return
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			dst := findNode(cfg, id)
			if dst == nil {
				return errors.New("节点不存在")
			}
			n.ID = id
			if n.Source == "" {
				n.Source = dst.Source
			}
			*dst = n
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		res, err := s.mutate(func(cfg *config.Config) error {
			idx := -1
			for i := range cfg.Nodes {
				if cfg.Nodes[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				return errors.New("节点不存在")
			}
			cfg.Nodes = append(cfg.Nodes[:idx], cfg.Nodes[idx+1:]...)
			config.CleanupDangling(cfg)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

func findNode(cfg *config.Config, id string) *config.Node {
	for i := range cfg.Nodes {
		if cfg.Nodes[i].ID == id {
			return &cfg.Nodes[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 订阅
// ---------------------------------------------------------------------------

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	if r.Method == http.MethodGet {
		writeOK(w, map[string]any{"subscriptions": cfg.Subscriptions})
		return
	}
	var req struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id := "s-" + store.RandomID()
	sub1 := config.Subscription{ID: id, Name: req.Name, URL: req.URL, Enabled: true}
	cfg.Subscriptions = append(cfg.Subscriptions, sub1)
	if errs := s.store.Replace(cfg); len(errs) > 0 {
		writeErr(w, 400, errs[0].Error())
		return
	}
	// 立即拉取一次
	if err := s.refreshSubscription(id); err != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "warning": "订阅已添加,但首次拉取失败: " + err.Error()})
		return
	}
	writeOK(w, nil)
}

func (s *Server) handleSubscriptionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if action == "refresh" {
		if err := s.refreshSubscription(id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
		return
	}
	switch r.Method {
	case http.MethodPut:
		// 编辑订阅:名称/地址/UA/关键词过滤/更新间隔/启停。
		// URL 变了不重建节点 ID —— 节点 ID 只由 订阅ID+服务器+端口+名称 决定,
		// 换订阅地址后照常按稳定 ID 对账,节点组与筛选里的选择不会错位。
		var req struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			UserAgent      string `json:"user_agent"`
			Filter         string `json:"filter"`
			Enabled        *bool  `json:"enabled"`
			UpdateInterval int    `json:"update_interval"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeErr(w, 400, "名称不能为空")
			return
		}
		if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
			writeErr(w, 400, "订阅地址必须以 http:// 或 https:// 开头")
			return
		}
		oldURL := ""
		res, err := s.mutate(func(cfg *config.Config) error {
			for i := range cfg.Subscriptions {
				if cfg.Subscriptions[i].ID == id {
					oldURL = cfg.Subscriptions[i].URL
					sub := &cfg.Subscriptions[i]
					sub.Name = strings.TrimSpace(req.Name)
					sub.URL = strings.TrimSpace(req.URL)
					sub.UserAgent = req.UserAgent
					sub.Filter = strings.TrimSpace(req.Filter)
					sub.UpdateInterval = req.UpdateInterval
					if req.Enabled != nil {
						sub.Enabled = *req.Enabled
					}
					return nil
				}
			}
			return errors.New("订阅不存在")
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		// 地址变了:立即重新拉取,让新地址的内容马上生效(失败不回滚编辑,
		// 错误会记在订阅上,面板可见,用户可以再手动刷新)
		if oldURL != strings.TrimSpace(req.URL) {
			if rerr := s.refreshSubscription(id); rerr != nil {
				writeOK(w, map[string]any{"result": res, "warning": "订阅已保存,但用新地址拉取失败: " + rerr.Error()})
				return
			}
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		cfg := s.store.Current()
		idx := -1
		for i := range cfg.Subscriptions {
			if cfg.Subscriptions[i].ID == id {
				idx = i
			}
		}
		if idx < 0 {
			writeErr(w, 404, "订阅不存在")
			return
		}
		cfg.Subscriptions = append(cfg.Subscriptions[:idx], cfg.Subscriptions[idx+1:]...)
		// 删掉该订阅带来的节点
		kept := cfg.Nodes[:0]
		for _, n := range cfg.Nodes {
			if n.Source != "sub:"+id {
				kept = append(kept, n)
			}
		}
		cfg.Nodes = kept
		config.CleanupDangling(cfg)
		if errs := s.store.Replace(cfg); len(errs) > 0 {
			writeErr(w, 400, errs[0].Error())
			return
		}
		_, _ = s.apply(func(c *config.Config) error { return nil })
		writeOK(w, nil)
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

// StartTrafficSampler 后台按固定间隔采样流量,保证即使面板没人打开,
// 短连接的字节数也会被累计进账本(采样间隔内结束的长连接不会漏算;
// 间隔内完成的极短连接仍可能漏,这是轮询方式的固有代价)。
func (s *Server) StartTrafficSampler(ctx context.Context) {
	go func() {
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = s.sampleTraffic(sctx)
				cancel()
			}
		}
	}()
}

// RefreshSubscription 供定时任务调用(导出给 main 用)。
func (s *Server) RefreshSubscription(id string) error { return s.refreshSubscription(id) }

// refreshSubscription 拉取并解析订阅,把节点合并进配置。
//
// 合并策略(很重要):以「订阅 + 服务器 + 端口 + 名称」生成的稳定 ID 为准,
// 已存在的节点**更新**而不是重新添加,这样节点组里的选择不会因为订阅刷新而错位;
// 订阅里消失的节点会被移除,并在移除时清理组内悬空引用。
func (s *Server) refreshSubscription(subID string) error {
	cfg := s.store.Current()
	var target *config.Subscription
	for i := range cfg.Subscriptions {
		if cfg.Subscriptions[i].ID == subID {
			target = &cfg.Subscriptions[i]
			break
		}
	}
	if target == nil {
		return errors.New("订阅不存在")
	}
	cfg2 := s.store.Current()
	var t2 *config.Subscription
	for i := range cfg2.Subscriptions {
		if cfg2.Subscriptions[i].ID == subID {
			t2 = &cfg2.Subscriptions[i]
		}
	}
	body, err := fetchSubscription(t2)
	if err != nil {
		t2.LastError = err.Error()
		t2.LastUpdate = time.Now()
		_ = s.store.Replace(cfg2)
		return err
	}
	res, err := sub.ParseSubscription(body, subID, t2.Name)
	if err != nil {
		t2.LastError = err.Error()
		t2.LastUpdate = time.Now()
		_ = s.store.Replace(cfg2)
		return err
	}
	nodes := sub.Dedup(sub.MatchFilter(res.Nodes, t2.Filter))

	_, err = s.apply(func(c *config.Config) error {
		var subPtr *config.Subscription
		for i := range c.Subscriptions {
			if c.Subscriptions[i].ID == subID {
				subPtr = &c.Subscriptions[i]
			}
		}
		if subPtr == nil {
			return errors.New("订阅已被删除")
		}
		// 索引现有订阅节点
		existing := map[string]*config.Node{}
		var manual []config.Node
		for i := range c.Nodes {
			n := c.Nodes[i]
			if n.Source == "sub:"+subID {
				existing[n.ID] = &c.Nodes[i]
			} else {
				manual = append(manual, n)
			}
		}
		merged := make([]config.Node, 0, len(manual)+len(nodes))
		merged = append(merged, manual...)
		for _, n := range nodes {
			if old, ok := existing[n.ID]; ok {
				// 保留用户的启用/禁用状态
				n.Disabled = old.Disabled
				delete(existing, n.ID)
			}
			merged = append(merged, n)
		}
		c.Nodes = merged
		subPtr.LastUpdate = time.Now()
		subPtr.LastError = ""
		config.CleanupDangling(c)
		// 空组自动吸收新节点(已有成员的组不动)
		config.AdoptNewMembers(c)
		return nil
	})
	return err
}

// fetchSubscription 拉取订阅正文,处理 UA 与流量信息头。
func fetchSubscription(s *config.Subscription) ([]byte, error) {
	req, err := http.NewRequest("GET", s.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("订阅地址无效: %w", err)
	}
	ua := s.UserAgent
	if ua == "" {
		// 常见机场按 UA 返回 Clash 配置,用通用 UA 兼容性最好
		ua = "Clash/v1.18.0"
	}
	req.Header.Set("User-Agent", ua)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	// 流量信息头(机场自定义,能拿到就展示)
	parseTrafficHeaders(&s.Info, resp.Header)
	return body, nil
}

func parseTrafficHeaders(info *config.SubscriptionInfo, h http.Header) {
	get := func(k string) int64 {
		v := h.Get(k)
		if v == "" {
			return 0
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	up := get("Subscription-Userinfo-Upload") // 非标准,多数机场用 subscription-userinfo
	if v := h.Get("Subscription-Userinfo"); v != "" {
		for _, part := range strings.Split(v, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
			switch strings.TrimSpace(kv[0]) {
			case "upload":
				info.Upload = n
			case "download":
				info.Download = n
			case "total":
				info.Total = n
			case "expire":
				if n > 0 {
					info.Expire = time.Unix(n, 0)
				}
			}
		}
	}
	_ = up
}

// ---------------------------------------------------------------------------
// 节点组
// ---------------------------------------------------------------------------

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	if r.Method == http.MethodGet {
		resolver := config.NewResolver(cfg)
		type groupView struct {
			config.Group
			MemberCount int `json:"member_count"`
		}
		var views []groupView
		for _, g := range cfg.Groups {
			cnt := len(g.Members)
			if g.Type == config.GroupDynamic {
				cnt = 0 // 由前端预览
			}
			_ = resolver
			views = append(views, groupView{Group: g, MemberCount: cnt})
		}
		writeOK(w, map[string]any{"groups": views})
		return
	}
	var g config.Group
	if !readJSON(w, r, &g) {
		return
	}
	if g.ID == "" {
		g.ID = "g-" + store.RandomID()
	}
	if g.Type == "" {
		g.Type = config.GroupSelect
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		cfg.Groups = append(cfg.Groups, g)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "group": g})
}

func (s *Server) handleGroupByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	switch r.Method {
	case http.MethodPut:
		var g config.Group
		if !readJSON(w, r, &g) {
			return
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					g.ID = id
					cfg.Groups[i] = g
					return nil
				}
			}
			return errors.New("节点组不存在")
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		res, err := s.mutate(func(cfg *config.Config) error {
			idx := -1
			for i := range cfg.Groups {
				if cfg.Groups[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				return errors.New("节点组不存在")
			}
			cfg.Groups = append(cfg.Groups[:idx], cfg.Groups[idx+1:]...)
			config.CleanupDangling(cfg)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

// ---------------------------------------------------------------------------
// 链式代理
// ---------------------------------------------------------------------------

func (s *Server) handleChains(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	if r.Method == http.MethodGet {
		resolver := config.NewResolver(cfg)
		type chainView struct {
			config.Chain
			FrontGroupName string   `json:"front_group_name"`
			References     []string `json:"references"`
		}
		var views []chainView
		for _, ch := range cfg.Chains {
			v := chainView{Chain: ch, References: config.ChainReferences(cfg, ch.ID)}
			if k, fid := ch.FrontRef(); k == "filter" {
				if f := resolver.Filter(fid); f != nil {
					v.FrontGroupName = f.Name
				}
			} else if g := resolver.Group(fid); g != nil {
				v.FrontGroupName = g.Name
			}
			if p, ok := s.probes[ch.ID]; ok {
				v.Probe = &p
			}
			views = append(views, v)
		}
		writeOK(w, map[string]any{
			"chains":        views,
			"front_options": frontOptionViews(resolver, ""),
		})
		return
	}
	var ch config.Chain
	if !readJSON(w, r, &ch) {
		return
	}
	if ch.ID == "" {
		ch.ID = "c-" + store.RandomID()
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		cfg.Chains = append(cfg.Chains, ch)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "chain": ch})
}

func (s *Server) handleChainByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/chains/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case id == "options":
		// 编辑某条链式代理时,前置组选项要按"它自己"来过滤:
		// 新建时不允许选含链式代理的组,但编辑时它自己的前置组(哪怕含别的链)
		// 必须仍然可选,否则用户一打开编辑框,原来的正确选择就"消失"了。
		s.handleChainOptions(w, r)
		return
	case action == "probe":
		res := s.probeChain(r.Context(), id)
		writeOK(w, map[string]any{"probe": res})
		return
	case action == "references":
		cfg := s.store.Current()
		writeOK(w, map[string]any{"references": config.ChainReferences(cfg, id)})
		return
	}
	switch r.Method {
	case http.MethodPut:
		var ch config.Chain
		if !readJSON(w, r, &ch) {
			return
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			for i := range cfg.Chains {
				if cfg.Chains[i].ID == id {
					ch.ID = id
					ch.Probe = cfg.Chains[i].Probe
					cfg.Chains[i] = ch
					return nil
				}
			}
			return errors.New("链式代理不存在")
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		res, err := s.mutate(func(cfg *config.Config) error {
			idx := -1
			for i := range cfg.Chains {
				if cfg.Chains[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				return errors.New("链式代理不存在")
			}
			cfg.Chains = append(cfg.Chains[:idx], cfg.Chains[idx+1:]...)
			config.CleanupDangling(cfg)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		s.mu.Lock()
		delete(s.probes, id)
		s.mu.Unlock()
		writeOK(w, map[string]any{"result": res})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	if r.Method == http.MethodGet {
		writeOK(w, map[string]any{"policies": cfg.Policies})
		return
	}
	var p config.Policy
	if !readJSON(w, r, &p) {
		return
	}
	if p.ID == "" {
		p.ID = "p-" + store.RandomID()
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		cfg.Policies = append(cfg.Policies, p)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "policy": p})
}

func (s *Server) handlePolicyByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/policies/")
	switch r.Method {
	case http.MethodPut:
		var p config.Policy
		if !readJSON(w, r, &p) {
			return
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			for i := range cfg.Policies {
				if cfg.Policies[i].ID == id {
					p.ID = id
					cfg.Policies[i] = p
					return nil
				}
			}
			return errors.New("分流规则不存在")
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		res, err := s.mutate(func(cfg *config.Config) error {
			idx := -1
			for i := range cfg.Policies {
				if cfg.Policies[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				return errors.New("分流规则不存在")
			}
			cfg.Policies = append(cfg.Policies[:idx], cfg.Policies[idx+1:]...)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Panel    *config.PanelSettings   `json:"panel"`
		DNS      *config.DNSSettings     `json:"dns"`
		Inbounds *config.InboundSettings `json:"inbounds"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		// 面板设置:只覆盖明确传上来的字段
		if req.Panel != nil {
			p := req.Panel
			if p.Language != "" {
				cfg.Panel.Language = p.Language
			}
			if p.Theme != "" {
				cfg.Panel.Theme = p.Theme
			}
			if p.SubscriptionUserAgent != "" {
				cfg.Panel.SubscriptionUserAgent = p.SubscriptionUserAgent
			}
			if p.SubscriptionInterval != 0 {
				cfg.Panel.SubscriptionInterval = p.SubscriptionInterval
			}
		}
		if req.DNS != nil {
			d := req.DNS
			if len(d.DirectServers) > 0 {
				cfg.DNS.DirectServers = d.DirectServers
			}
			if len(d.ProxyServers) > 0 {
				cfg.DNS.ProxyServers = d.ProxyServers
			}
			if d.FinalServer != "" {
				cfg.DNS.FinalServer = d.FinalServer
			}
			if d.Strategy != "" {
				cfg.DNS.Strategy = d.Strategy
			}
			// 布尔开关需要显式传值;策略:只有传了任意 DNS 字段才更新
			cfg.DNS.Hijack = d.Hijack
			cfg.DNS.FakeIP = d.FakeIP
			if d.Mode != "" {
				cfg.DNS.Mode = d.Mode
			}
			// 例外设备允许清空,所以无条件覆盖
			cfg.DNS.ModeExceptions = d.ModeExceptions
		}
		if req.Inbounds != nil {
			ib := req.Inbounds
			if ib.MixedPort > 0 {
				cfg.Inbounds.MixedPort = ib.MixedPort
			}
			if ib.MixedListen != "" {
				cfg.Inbounds.MixedListen = ib.MixedListen
			}
			if ib.ClashAPIPort > 0 {
				cfg.Inbounds.ClashAPIPort = ib.ClashAPIPort
			}
			if ib.ClashAPISecret != "" {
				cfg.Inbounds.ClashAPISecret = ib.ClashAPISecret
			}
			if ib.TUNStack != "" {
				cfg.Inbounds.TUNStack = ib.TUNStack
			}
			cfg.Inbounds.Mixed = ib.Mixed
			cfg.Inbounds.TUN = ib.TUN
			cfg.Inbounds.AllowLAN = ib.AllowLAN
			cfg.Inbounds.BlockQUIC = ib.BlockQUIC
			cfg.Inbounds.ClashAPI = ib.ClashAPI
			cfg.Inbounds.Sniff = ib.Sniff
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, res)
}

// ---------------------------------------------------------------------------
// 内核与代理操作
// ---------------------------------------------------------------------------

func (s *Server) handleKernel(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/kernel/")
	switch action {
	case "status":
		writeOK(w, s.kernel.Status())
	case "start":
		if err := s.kernel.Start(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
	case "stop":
		if err := s.kernel.Stop(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
	case "restart":
		cfg := s.store.Current()
		sb, err := s.build(cfg)
		if err == nil {
			var raw []byte
			if raw, err = sb.Marshal(); err == nil {
				p := filepath.Join(s.dataDir, "core.json")
				if err = os.WriteFile(p, raw, 0o600); err == nil {
					err = s.kernel.Reload(p)
				}
			}
		}
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
	case "log":
		n := 200
		if v := r.URL.Query().Get("lines"); v != "" {
			if x, err := strconv.Atoi(v); err == nil {
				n = x
			}
		}
		lines, err := s.kernel.TailLog(n)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"lines": lines})
	case "log/clear":
		if err := s.kernel.ClearLog(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
	default:
		writeErr(w, 404, "未知的内核操作")
	}
}

// handleProxy 提供经节点组切换出站的能力(对应面板上的"选择节点")。
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/proxy/")
	if rest == "select" {
		var req struct {
			Group  string `json:"group"`
			Member string `json:"member"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.clash().Select(ctx, req.Group, req.Member); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil)
		return
	}
	if rest == "list" {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		proxies, err := s.clash().Proxies(ctx)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"proxies": proxies})
		return
	}
	if rest == "delay" {
		query := r.URL.Query()
		target := query.Get("name")
		if target == "" {
			writeErr(w, 400, "缺少出站名")
			return
		}
		testURL := query.Get("url")
		if testURL == "" {
			testURL = "https://www.gstatic.com/generate_204"
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		res := s.clash().Delay(ctx, target, testURL, 8*time.Second)
		writeOK(w, res)
		return
	}
	// 代理列表与当前选择
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	proxies, err := s.clash().Proxies(ctx)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"proxies": proxies})
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{"backups": s.store.Backups()})
}

func (s *Server) handleBackupByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/backups/")
	if r.Method != http.MethodPost {
		writeErr(w, 405, "请求方法不支持")
		return
	}
	if err := s.store.RestoreBackup(name); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := s.apply(func(c *config.Config) error { return nil }); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, nil)
}

// SubFS 允许把内嵌资源作为静态文件目录。
func SubFS(f embed.FS, dir string) http.Handler {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// handleNodeParse 解析用户粘贴的分享链接并加入节点。
// 支持一次多行(每行一条),解析失败的逐条回报,不会因为一条坏链接整批失败。
func (s *Server) handleNodeParse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var parsed []config.Node
	var skipped []string
	for _, line := range strings.Split(req.Text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := sub.ParseLink(line)
		if err != nil {
			skipped = append(skipped, abbreviateStr(line)+": "+err.Error())
			continue
		}
		if req.Name != "" && len(strings.Split(strings.TrimSpace(req.Text), "\n")) == 1 {
			n.Name = req.Name
		}
		if n.Name == "" {
			n.Name = n.Server
		}
		n.Source = "manual"
		n.ID = "n-" + store.RandomID()
		parsed = append(parsed, n)
	}
	if len(parsed) == 0 {
		msg := "没有解析出任何节点"
		if len(skipped) > 0 {
			msg += ":" + strings.Join(skipped, ";")
		}
		writeErr(w, 400, msg)
		return
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		cfg.Nodes = append(cfg.Nodes, parsed...)
		config.AdoptNewMembers(cfg)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "added": len(parsed), "skipped": skipped})
}

func abbreviateStr(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:37] + "..."
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: "boxhub_session", Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

// isLocalIP 判断请求是否来自本机。
func isLocalIP(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	return p.IsLoopback() || p.IsPrivate()
}

// ---------------------------------------------------------------------------
// 筛选
// ---------------------------------------------------------------------------

func (s *Server) handleFilters(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	if r.Method == http.MethodGet {
		// 带上统计结果,前端直接渲染"筛选数 24 / 各筛选多少节点"
		type view struct {
			config.Filter
			Count      int      `json:"count"`
			References []string `json:"references"`
			Flag       string   `json:"flag"`
			RegionName string   `json:"region_name"`
		}
		var out []view
		for _, f := range cfg.Filters {
			out = append(out, view{
				Filter:     f,
				Count:      len(f.Matches(cfg.Nodes)),
				References: config.FilterReferences(cfg, f.ID),
				Flag:       config.RegionFlag(f.Region),
				RegionName: config.RegionName(f.Region),
			})
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Name < out[j].Name
		})
		writeOK(w, map[string]any{
			"filters": out,
			"stats":   config.ComputeFilterStats(cfg),
			"total":   len(cfg.Nodes),
		})
		return
	}
	var f config.Filter
	if !readJSON(w, r, &f) {
		return
	}
	if f.ID == "" {
		f.ID = "f-" + store.RandomID()
	}
	if f.Sort == "" {
		f.Sort = "name"
	}
	res, err := s.mutate(func(cfg *config.Config) error {
		cfg.Filters = append(cfg.Filters, f)
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "filter": f})
}

func (s *Server) handleFilterByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/filters/")
	if rest == "auto" {
		s.handleAutoFilters(w, r)
		return
	}
	id := rest
	switch r.Method {
	case http.MethodGet:
		// 返回该筛选命中的节点明细,便于前端预览
		cfg := s.store.Current()
		var f *config.Filter
		for i := range cfg.Filters {
			if cfg.Filters[i].ID == id {
				f = &cfg.Filters[i]
			}
		}
		if f == nil {
			writeErr(w, 404, "筛选不存在")
			return
		}
		matched := f.Matches(cfg.Nodes)
		writeOK(w, map[string]any{"filter": f, "nodes": matched, "count": len(matched)})
	case http.MethodPut:
		var f config.Filter
		if !readJSON(w, r, &f) {
			return
		}
		res, err := s.mutate(func(cfg *config.Config) error {
			for i := range cfg.Filters {
				if cfg.Filters[i].ID == id {
					f.ID = id
					cfg.Filters[i] = f
					return nil
				}
			}
			return errors.New("筛选不存在")
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	case http.MethodDelete:
		res, err := s.mutate(func(cfg *config.Config) error {
			idx := -1
			for i := range cfg.Filters {
				if cfg.Filters[i].ID == id {
					idx = i
				}
			}
			if idx < 0 {
				return errors.New("筛选不存在")
			}
			cfg.Filters = append(cfg.Filters[:idx], cfg.Filters[idx+1:]...)
			config.CleanupDangling(cfg)
			return nil
		})
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, map[string]any{"result": res})
	default:
		writeErr(w, 405, "请求方法不支持")
	}
}

// handleAutoFilters 按现有节点的地区分布自动生成筛选(一键生成地区分组)。
func (s *Server) handleAutoFilters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "请求方法不支持")
		return
	}
	var created []config.Filter
	res, err := s.mutate(func(cfg *config.Config) error {
		created = config.AutoFilters(cfg, func() string { return "f-" + store.RandomID() })
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "created": created, "count": len(created)})
}

// handleDashboard 是 SMBox 风格仪表盘的数据源:
// 设备信息 / 硬件信息 / 内核状态 / 各类数量统计 / 筛选统计。
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	st := s.kernel.Status()

	// CPU 采样要 200ms,和其余数据并行做,避免拖慢页面
	type cpures struct{ v float64 }
	cpuCh := make(chan cpures, 1)
	go func() { cpuCh <- cpures{cpuPercent()} }()

	si := collectSysInfo()
	si.CPUPercent = (<-cpuCh).v

	// 实时流量:从 Clash API 取连接累计值,再算一次速率
	var traffic map[string]any
	ctx, cancel := contextWithTimeout(r, 3)
	defer cancel()
	cl := s.clash()
	connUp, connDown := int64(0), int64(0)
	connCount := 0
	if cs, err := cl.Connections(ctx); err == nil {
		connCount = len(cs)
		for _, c := range cs {
			connUp += c.Upload
			connDown += c.Download
		}
	}
	rateUp, rateDown := s.trafficRate(connUp, connDown)
	traffic = map[string]any{
		"connections": connCount,
		"upload":      connUp,
		"download":    connDown,
		"rate_up":     rateUp,
		"rate_down":   rateDown,
	}

	out := map[string]any{
		"system": si,
		"kernel": map[string]any{
			"running":    st.Running,
			"pid":        st.PID,
			"version":    st.Version,
			"started_at": st.StartedAt,
			"last_error": st.LastError,
			"restarts":   st.Restarts,
		},
		"counts": map[string]int{
			"nodes":         len(cfg.Nodes),
			"subscriptions": len(cfg.Subscriptions),
			"filters":       len(cfg.Filters),
			"rules":         len(cfg.Policies),
			"groups":        len(cfg.Groups),
			"chains":        len(cfg.Chains),
		},
		"filter_stats": config.ComputeFilterStats(cfg),
		"traffic":      traffic,
		"updated_at":   cfg.UpdatedAt,
	}
	s.mu.Lock()
	if s.lastErr != "" {
		out["last_error"] = s.lastErr
	}
	s.mu.Unlock()
	writeOK(w, out)
}

// trafficRate 计算上传/下载速率(字节/秒)。samples 保存上一次的累计值。
func (s *Server) trafficRate(up, down int64) (rateUp, rateDown int64) {
	s.trafficMu.Lock()
	defer s.trafficMu.Unlock()
	now := time.Now()
	if !s.lastTrafficAt.IsZero() {
		elapsed := now.Sub(s.lastTrafficAt).Seconds()
		if elapsed > 0.2 {
			// 累计值可能因为连接关闭而变小,负数一律按 0 处理
			du := up - s.lastTrafficUp
			dd := down - s.lastTrafficDown
			if du > 0 {
				rateUp = int64(float64(du) / elapsed)
			}
			if dd > 0 {
				rateDown = int64(float64(dd) / elapsed)
			}
		}
	}
	s.lastTrafficUp, s.lastTrafficDown, s.lastTrafficAt = up, down, now
	return rateUp, rateDown
}

// handleLogs 返回内核日志,支持按级别过滤。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 300
	if v := r.URL.Query().Get("lines"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	lines, err := s.kernel.TailLog(n)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 级别过滤:error / warn / info / all
	level := r.URL.Query().Get("level")
	if level != "" && level != "all" {
		keep := make([]string, 0, len(lines))
		upper := strings.ToUpper(level)
		for _, l := range lines {
			if strings.Contains(strings.ToUpper(l), upper) {
				keep = append(keep, l)
			}
		}
		lines = keep
	}
	writeOK(w, map[string]any{"lines": lines, "count": len(lines)})
}

// handleDNSStats 统计内核日志里的 DNS 查询情况。
//
// sing-box 的 Clash API 不提供 DNS 统计接口,但它的日志里会记录 DNS 交换:
//
//	INFO dns: exchanged A www.example.com. 600 IN A 1.2.3.4
//	ERROR dns: lookup failed for xxx: ...
//
// 这里解析最近日志做聚合,给面板一个"最近 N 分钟内"的概览。
// 代价是不精确(受日志级别与行数限制),但对排查"DNS 是否异常"已经够用。
func (s *Server) handleDNSStats(w http.ResponseWriter, r *http.Request) {
	stats := dnsStats{Upstreams: map[string]*dnsUpstream{}}
	const name = "代理+直连合计"
	up := &dnsUpstream{Name: name}
	stats.Upstreams[name] = up

	if err := s.dnsTrack.poll(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var since time.Time
	total, success, failed, nxd, avgMs, since := s.dnsTrack.snapshot()
	stats.Total, stats.Success, stats.Failed, stats.NXDomain = total, success, failed, nxd
	stats.AvgMs = avgMs
	up.Total, up.Success, up.Failed, up.NXDomain = total, success, failed, nxd
	up.AvgMs = avgMs
	if up.Total > 0 {
		up.Rate = float64(up.Success) / float64(up.Total) * 100
	}
	if total > 0 {
		stats.Available = true
	}
	sinceStr := ""
	if !since.IsZero() {
		sinceStr = since.Format("2006-01-02 15:04:05")
	}
	writeOK(w, map[string]any{
		"available": stats.Available,
		"since":     sinceStr,
		"total":     stats.Total,
		"success":   stats.Success,
		"failed":    stats.Failed,
		"nxdomain":  stats.NXDomain,
		"avg_ms":    stats.AvgMs,
		"upstreams": []map[string]any{{
			"name": up.Name, "total": up.Total, "success": up.Success,
			"failed": up.Failed, "nxdomain": up.NXDomain,
			"avg_ms": up.AvgMs, "rate": up.Rate,
		}},
	})
}

type dnsStats struct {
	Available bool
	Total     int
	Success   int
	Failed    int
	NXDomain  int
	AvgMs     float64
	Upstreams map[string]*dnsUpstream
}

type dnsUpstream struct {
	Name              string
	Total, Success    int
	Failed, NXDomain  int
	AvgMs, Rate       float64
	durSum            float64
	durCnt            int
}

// parseLogTime 解析内核日志行开头的时间戳。
// 格式: "+0800 2026-10-27 15:04:05 INFO ..." 或带连接ID的
// "+0000 2026-10-27 07:10:01 INFO [123 45ms] ..."
func parseLogTime(line string) (time.Time, bool) {
	// 跳过时区偏移字段(+0800)
	i := strings.IndexByte(line, ' ')
	if i <= 0 || len(line) < i+20 {
		return time.Time{}, false
	}
	rest := line[i+1:]
	tsPart := rest[:19] // 2006-01-02 15:04:05
	// 日志时间戳是本地时区(内核进程环境),按本地时区解析
	ts, err := time.ParseInLocation("2006-01-02 15:04:05", tsPart, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// parseMs 从日志行里提取形如 "123ms" 的耗时。
func parseMs(line string) (float64, bool) {
	i := strings.Index(line, "ms]")
	if i < 0 {
		return 0, false
	}
	// 形如 "[1234 456ms]" —— 取 ] 与 ms 之间的数字
	j := strings.LastIndex(line[:i], " ")
	if j < 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[j+1:i]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// 规则模板
// ---------------------------------------------------------------------------

// handleRuleTemplates 返回内置模板清单,并标注哪些已经添加过。
func (s *Server) handleRuleTemplates(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Current()
	type view struct {
		config.RuleTemplate
		Added bool `json:"added"`
	}
	var out []view
	for _, t := range config.RuleTemplates() {
		out = append(out, view{RuleTemplate: t, Added: config.AlreadyHasTemplate(cfg, t)})
	}
	writeOK(w, map[string]any{"templates": out})
}

// handleApplyTemplates 批量应用模板。按 key 列表添加,已存在的自动跳过。
func (s *Server) handleApplyTemplates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []string `json:"keys"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Keys) == 0 {
		writeErr(w, 400, "请选择要添加的规则")
		return
	}
	want := map[string]bool{}
	for _, k := range req.Keys {
		want[k] = true
	}
	tpls := config.RuleTemplates()
	var added []string
	var skipped []string
	res, err := s.mutate(func(cfg *config.Config) error {
		for _, t := range tpls {
			if !want[t.Key] {
				continue
			}
			if config.AlreadyHasTemplate(cfg, t) {
				skipped = append(skipped, t.Name)
				continue
			}
			cfg.Policies = append(cfg.Policies, config.ApplyTemplate(t, func() string {
				return "p-" + store.RandomID()
			}))
			added = append(added, t.Name)
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, map[string]any{"result": res, "added": added, "skipped": skipped})
}

// frontOptionViews 计算某个链式代理(空串=新建)可选的前置。
// 包含节点组与筛选组(筛选组按名称自动收编节点,同样可做前置);
// 返回的是 ref("group:<id>"/"filter:<id>"),前端保存时原样回传。
func frontOptionViews(resolver *config.Resolver, chainID string) []config.FrontOption {
	return resolver.FrontChoices(chainID)
}

// handleChainOptions 返回"编辑某条链式代理"视角下的前置组候选。
func (s *Server) handleChainOptions(w http.ResponseWriter, r *http.Request) {
	forChain := r.URL.Query().Get("for")
	cfg := s.store.Current()
	resolver := config.NewResolver(cfg)
	// 校验 for 指向的链存在,否则按新建处理
	if forChain != "" && resolver.Chain(forChain) == nil {
		forChain = ""
	}
	writeOK(w, map[string]any{"front_options": frontOptionViews(resolver, forChain)})
}

// handleConditionTypes 返回条件类型下拉(与 SMBox 编辑弹窗一致)。
func (s *Server) handleConditionTypes(w http.ResponseWriter, r *http.Request) {
	type opt struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	}
	var out []opt
	for _, t := range config.ConditionTypeLabels {
		out = append(out, opt{string(t.Type), t.Name})
	}
	writeOK(w, map[string]any{"types": out})
}

// handleKernelUIURL 返回内核面板(zashboard)的浏览器可达地址。
//
// 为什么用 9090 直连而不是走面板 /ui/ + /clash 反代:
// zashboard 的后端地址写死为 http://<host>:9090(不像 metacubexd 有 config.js),
// 从 80 端口页面跨源调 9090 会被 CORS 拦(sing-box 无 allow_cors 配置项,实测)。
// 内核自己在 external_ui 目录存在时直接 serve /ui/,页面与 API 同源,零问题。
func (s *Server) handleKernelUIURL(w http.ResponseWriter, r *http.Request) {
	// 内核面板走**同源路径**:页面在 <本面板 origin>/ui/,API 用 /clash 反代。
	// 曾经的 9090 直连方案有两个坑:
	//   1) zashboard 是 PWA,浏览器里的旧 Service Worker 会拦截 /ui 返回
	//      失效缓存(实测 Mac 上 404 白屏,而 curl 直连一切正常);
	//   2) SW 注册脚本在**跨源 iframe** 里抛 SecurityError,整页白屏。
	// 同源没有跨源限制,面板启动脚本(core.js)还会顺手清掉本域历史 SW。
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.HasPrefix(host, "[") {
		host = host[:i]
	}
	proto := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "https"
	}
	port := "80"
	if proto == "https" {
		port = "443"
	}
	if _, ap, err := net.SplitHostPort(r.Host); err == nil {
		port = ap
	}
	secret := ""
	if cfg := s.store.Current(); cfg != nil {
		secret = cfg.Inbounds.ClashAPISecret
	}
	// ?v=index.html 的 mtime:浏览器对"没有 Cache-Control 的静态文件"
	// 会按 Last-Modified 做启发式长缓存 —— 换面板文件后旧 index 仍命中缓存,
	// 里面引用的却是不存在的资源(metacubexd 的 _nuxt/*.js),表现为
	// "内核面板打不开"(500/白屏)。入口 URL 带 mtime 版本,换文件即失效。
	//
	// zashboard 的后端参数实测必须放在 **hash 前的 query** 里
	// (location.search);README 示例写在 #/setup? 后,放那里不生效。
	vtag := s.uiVersionTag()
	u := fmt.Sprintf("%s://%s/ui/?v=%s&disableUpgradeCore=1&protocol=%s&hostname=%s&port=%s&secondaryPath=%s#/setup",
		proto, host, vtag, proto, host, port, "%2Fclash")
	if secret != "" {
		u += "&secret=" + url.QueryEscape(secret)
	}
	writeOK(w, map[string]any{"url": u})
}

// uiNoCache 给内核面板的入口 HTML 关掉缓存,哈希资源长缓存。
// (原实现裸用 http.FileServer → 无 Cache-Control → 浏览器启发式缓存
// 旧 index,升级换面板后旧入口引用失效资源,"内核面板打不开")
func uiNoCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "" || p == "/" || p == "index.html" || strings.HasSuffix(p, ".html") {
			w.Header().Set("Cache-Control", "no-cache") // 每次都回源验证
		} else {
			w.Header().Set("Cache-Control", "public, max-age=604800")
		}
		next.ServeHTTP(w, r)
	})
}

// uiVersionTag 返回 data/ui/index.html 的修改时间戳,用作入口 URL 缓存版本。
func (s *Server) uiVersionTag() string {
	if st, err := os.Stat(filepath.Join(s.dataDir, "ui", "index.html")); err == nil {
		return strconv.FormatInt(st.ModTime().Unix(), 10)
	}
	return "0"
}

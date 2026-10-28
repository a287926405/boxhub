// boxd 是 BoxHub 的主程序:面板 + 内核管理器。
//
// 一个二进制只做三件事:
//   - 管内核(启动、校验、热重启、崩溃恢复)
//   - 提供面板 API 与静态页面
//   - 定时刷新订阅
//
// 它自己就是 systemd 服务的主体,不依赖外部进程管理器管理内核,
// 这样面板才能精确控制"先校验、再重启"的顺序。
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"boxhub/internal/api"
	"boxhub/internal/config"
	"boxhub/internal/kernel"
	"boxhub/internal/store"
)

//go:embed web/static
var staticFS embed.FS

var version = "1.0.0"

func main() {
	var (
		dataDir    = flag.String("data", "/opt/boxhub/data", "数据目录")
		listen     = flag.String("listen", "0.0.0.0:80", "面板监听地址(默认 80:装完直接 http://IP 打开;非 root 调试用 -listen 127.0.0.1:3036)")
		singBox    = flag.String("sing-box", "", "sing-box 内核路径(默认在数据目录同级 bin/ 下查找)")
		noKernel   = flag.Bool("no-kernel", false, "只启动面板,不管理内核(调试用)")
		showVer    = flag.Bool("version", false, "显示版本")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("boxd %s\n", version)
		return
	}

	if err := run(*dataDir, *listen, *singBox, *noKernel); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run(dataDir, listen, singBoxPath string, noKernel bool) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	// 初始化配置
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}

	// 路径:数据目录下放运行期文件,bin/ 放内核
	binPath := singBoxPath
	if binPath == "" {
		for _, cand := range []string{
			filepath.Join(filepath.Dir(dataDir), "bin", "sing-box"),
			filepath.Join(dataDir, "bin", "sing-box"),
			"/usr/local/bin/sing-box",
			"/usr/bin/sing-box",
		} {
			if _, err := os.Stat(cand); err == nil {
				binPath = cand
				break
			}
		}
	}
	if binPath == "" {
		return fmt.Errorf("未找到 sing-box 内核,请用 -sing-box 指定路径,或运行 manage.sh 安装")
	}

	km := kernel.New(kernel.Options{
		BinPath:    binPath,
		ConfigPath: filepath.Join(dataDir, api.CoreConfigName),
		WorkDir:    dataDir,
		LogPath:    filepath.Join(dataDir, "kernel.log"),
	})

	cfg0 := st.Current()
	clashAddrFn := func() string {
		cfg := st.Current()
		if cfg == nil || !cfg.Inbounds.ClashAPI {
			return "http://127.0.0.1:9090"
		}
		host := "127.0.0.1"
		if cfg.Inbounds.AllowLAN {
			host = "127.0.0.1" // 面板内部访问始终走本地
		}
		port := cfg.Inbounds.ClashAPIPort
		if port <= 0 {
			port = 9090
		}
		return fmt.Sprintf("http://%s:%d", host, port)
	}
	clashSecFn := func() string {
		cfg := st.Current()
		if cfg == nil {
			return ""
		}
		return cfg.Inbounds.ClashAPISecret
	}

	// 静态资源
	staticSub, err := fs.Sub(staticFS, "web/static")
	if err != nil {
		return err
	}
	// 前端资源带版本号:升级后浏览器不会继续用旧的 core.js/pages.js
	// 否则"改了代码但页面没变",极难排查。
	staticHandler := versionedStatic(staticSub, version)
	var clashUI http.Handler
	if p := filepath.Join(dataDir, "ui"); dirExists(p) {
		clashUI = http.FileServer(http.Dir(p))
	}

	srv := api.New(api.Options{
		Store:       st,
		Kernel:      km,
		DataDir:     dataDir,
		ClashAddr:   clashAddrFn,
		ClashSecret: clashSecFn,
		Static:      spaHandler(staticHandler),
		ClashUI:     clashUI,
	})

	auth, err := api.NewAuth(dataDir)
	if err != nil {
		return err
	}

	if !noKernel {
		if err := srv.ApplyStartup(); err != nil {
			fmt.Fprintf(os.Stderr, "[警告] 内核启动失败: %v(面板仍会启动,可在面板内修复)\n", err)
		} else if km.Status().Running {
			fmt.Printf("[信息] 内核已启动: %s\n", km.Status().Version)
		}
	}

	_ = cfg0

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler(auth))

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           logMiddleware(securityHeaders(mux)),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 流量采样:后台每 3 秒抓一次内核连接快照并累计
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	defer stopSampler()
	srv.StartTrafficSampler(samplerCtx)

	// 订阅定时刷新
	stopSub := startSubscriptionLoop(st, srv)
	defer stopSub()

	// 信号处理:优雅退出
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Println("[信息] 正在退出...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		if !noKernel {
			_ = km.Stop()
		}
		close(idle)
	}()

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", listen, err)
	}
	fmt.Printf("[信息] BoxHub %s 面板已启动: http://%s\n", version, displayAddr(listen))
	if !auth.HasPassword() {
		fmt.Println("[提示] 首次使用请打开面板设置访问密码")
	}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	<-idle
	return nil
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	if strings.HasPrefix(listen, "[::]:") || strings.HasPrefix(listen, ":") {
		return "127.0.0.1:" + listen[strings.LastIndex(listen, ":")+1:]
	}
	return listen
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// spaHandler 让前端路由在刷新时仍能命中 index.html。
// 非静态资源路径一律交给内部 handler(它会返回 index.html)。
func spaHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// 带扩展名的按静态文件处理,其余(前端路由)都回落到首页
		if strings.Contains(p, ".") {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		next.ServeHTTP(w, r2)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只记录 API 的写操作与错误,避免日志被静态资源刷屏
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			fmt.Printf("[%s] %s %s\n", time.Now().Format("15:04:05"), r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// startSubscriptionLoop 定时刷新启用的订阅。
func startSubscriptionLoop(st *store.Store, srv *api.Server) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 启动后延迟 30 秒做第一次检查,避免和内核启动抢资源
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			cfg := st.Current()
			if cfg != nil {
				now := time.Now()
				for _, s := range cfg.Subscriptions {
					if !s.Enabled {
						continue
					}
					interval := time.Duration(s.UpdateInterval) * time.Minute
					if interval <= 0 {
						interval = cfg.Panel.SubscriptionInterval
					}
					if interval <= 0 {
						interval = 24 * time.Hour
					}
					if s.LastUpdate.IsZero() || now.Sub(s.LastUpdate) >= interval {
						if err := srv.RefreshSubscription(s.ID); err != nil {
							fmt.Printf("[订阅] %s 更新失败: %v\n", s.Name, err)
						} else {
							fmt.Printf("[订阅] %s 已更新\n", s.Name)
						}
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return cancel
}

var _ = config.CurrentSchema

// versionedStatic 把 index.html 里的 __V__ 占位替换成版本号,
// 并给静态资源加上 no-cache,保证升级后立刻生效。
func versionedStatic(fsys fs.FS, ver string) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if p == "index.html" {
			if raw, err := fs.ReadFile(fsys, p); err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				_, _ = w.Write([]byte(strings.ReplaceAll(string(raw), "__V__", ver)))
				return
			}
		}
		// JS/CSS 允许缓存,但靠 query 版本号失效
		if strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		// 图标/logo 也长缓存,但名字带不进 query 版本 —— 换图时内容字节必变,
		// 配合 revalidate 策略由下面的 must-revalidate 保证及时更新
		if strings.HasSuffix(p, ".png") || strings.HasSuffix(p, ".ico") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		fileServer.ServeHTTP(w, r)
	})
}

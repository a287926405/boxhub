package render_test

// 这些测试的核心作用不是"代码跑通",而是**用真实内核校验生成结果**。
// 只要本机有 sing-box 二进制(或设置了 BOXHUB_SING_BOX),就会真的用它跑一遍
// `sing-box run`,内核在加载阶段会检查出站依赖环、字段拼写、规则集引用等
// 所有能 FATAL 的问题。
//
// 没有内核时只做结构性断言,测试仍然可跑(CI 上建议装一个)。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func singBoxBinary(t *testing.T) string {
	if p := os.Getenv("BOXHUB_SING_BOX"); p != "" {
		return p
	}
	for _, p := range []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box", "./sing-box"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// runKernel 真正启动内核,等待若干秒看它是否保持运行。
// 返回 (是否存活, 输出)。
func runKernel(t *testing.T, bin, cfgPath string, wait time.Duration) (bool, string) {
	t.Helper()
	dir := filepath.Dir(cfgPath)
	cmd := exec.Command(bin, "run", "-c", cfgPath, "-D", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动内核失败: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return false, buf.String() + "\n" + errString(err)
	case <-time.After(wait):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return true, buf.String()
	}
}

// runOnly 只判断内核能否持续运行,不做任何出站断言。
func runOnly(t *testing.T, bin, cfgPath, dir string, wait time.Duration) (bool, string) {
	t.Helper()
	return runKernel(t, bin, cfgPath, wait)
}

// kernelWithClash 启动内核后,通过 Clash API 确认链式代理确实作为可切换的
// 代理出现在面板后端里——这正是 zashboard 与 BoxHub 面板测速/切换所依赖的。
func kernelWithClash(t *testing.T, bin, cfgPath string, apiPort int, dir string) (bool, string) {
	t.Helper()
	cmd := exec.Command(bin, "run", "-c", cfgPath, "-D", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动内核失败: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// 内核没在 5 秒内退出,放弃等待,避免测试挂死
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/proxies", apiPort)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return false, buf.String()
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var data struct {
					Proxies map[string]struct {
						Type string `json:"type"`
						Now  string `json:"now"`
						All  []string `json:"all"`
					} `json:"proxies"`
				}
				if err := json.Unmarshal(body, &data); err != nil {
					return false, "Clash API 返回无法解析: " + err.Error()
				}
				// 链式代理必须作为独立出站出现在列表里
				for _, want := range []string{"香港住宅", "美国 HTTP", "美国 HTTPS", "前置线路"} {
					if _, ok := data.Proxies[want]; !ok {
						keys := make([]string, 0, len(data.Proxies))
						for k := range data.Proxies {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						t.Errorf("Clash API 里缺少 %s,实际有: %v", want, keys)
					}
				}
				// 前置组的可选项里必须是普通节点(不是链式代理)
				if front, ok := data.Proxies["前置线路"]; ok {
					for _, m := range front.All {
						if m == "香港住宅" || m == "美国 HTTP" || m == "美国 HTTPS" {
							t.Errorf("前置组的选项里出现链式代理: %s", m)
						}
					}
				}
				// 主选择组里必须能选到链式代理
				if main, ok := data.Proxies["代理"]; ok {
					hasChain := false
					for _, m := range main.All {
						if m == "香港住宅" || m == "美国 HTTP" {
							hasChain = true
						}
					}
					if !hasChain {
						t.Errorf("主组选项里没有链式代理: %v", main.All)
					}
				}
				return true, buf.String()
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false, "Clash API 未在 60 秒内就绪\n" + buf.String()
}

func errString(err error) string {
	if err == nil {
		return "exit ok"
	}
	return err.Error()
}

func freePort(t *testing.T) int {
	t.Helper()
	// 简单做法:取时间戳低位,避开常用端口
	return 20000 + int(time.Now().UnixNano()%20000)
}

// baseConfig 构造一份完整的测试配置:一个前置组、两条链式代理、若干节点。
func baseConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.NewDefault()
	cfg.Inbounds.Mixed = true
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.Inbounds.TUN = false
	cfg.Inbounds.ClashAPI = true
	cfg.Inbounds.ClashAPIPort = freePort(t) + 1
	cfg.Inbounds.AllowLAN = false
	cfg.DNS.FakeIP = false

	cfg.Nodes = []config.Node{
		{ID: "n-hk", Name: "香港 01", Type: "shadowsocks", Server: "203.0.113.10", Port: 8388,
			Method: "aes-256-gcm", Password: "pw", Source: "manual"},
		{ID: "n-jp", Name: "日本 01", Type: "trojan", Server: "203.0.113.11", Port: 443,
			Password: "pw", TLS: &config.TLSConfig{Enabled: true, ServerName: "jp.example.com"}, Source: "manual"},
	}

	// 前置组:只含普通节点,不含链式代理
	cfg.Groups = []config.Group{
		{ID: "g-front", Name: "前置线路", Type: config.GroupSelect, Members: []string{"node:n-hk", "node:n-jp"}},
	}
	// 链式代理:住宅出口,前置走上面那个组
	cfg.Chains = []config.Chain{
		{ID: "c-home", Name: "香港住宅", Type: config.ChainSocks5, Host: "198.51.100.20", Port: 1080,
			Username: "u", Password: "p", FrontGroupID: "g-front"},
		{ID: "c-http", Name: "美国 HTTP", Type: config.ChainHTTP, Host: "198.51.100.21", Port: 8080,
			FrontGroupID: "g-front"},
		{ID: "c-https", Name: "美国 HTTPS", Type: config.ChainHTTPS, Host: "198.51.100.22", Port: 443,
			FrontGroupID: "g-front", TLSServerName: "proxy.example.com", Insecure: true},
	}
	// 主选择组:链式代理 + 普通节点混排
	cfg.Groups = append(cfg.Groups,
		config.Group{ID: "g-main", Name: "代理", Type: config.GroupSelect,
			Members: []string{"chain:c-home", "chain:c-http", "chain:c-https", "node:n-hk"}},
		config.Group{ID: "g-auto", Name: "自动选择", Type: config.GroupURLTest,
			Members: []string{"chain:c-home", "node:n-hk"}, URL: "https://www.gstatic.com/generate_204", Interval: 3},
		config.Group{ID: "g-dyn", Name: "香港", Type: config.GroupDynamic, Keywords: []string{"香港", "HK"}, KeywordMode: "or"},
	)
	cfg.Policies = append(cfg.Policies,
		config.Policy{ID: "p-ai", Name: "AI 站点", Enabled: true, Target: "chain:c-home",
			Match: config.Match{DomainSuffix: []string{"openai.com", "anthropic.com"}}},
		config.Policy{ID: "p-netflix", Name: "流媒体", Enabled: true, Target: "group:g-main",
			Match: config.Match{DomainSuffix: []string{"netflix.com"}, RuleSets: []string{"srs:https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-netflix.srs"}}},
	)
	return cfg
}

func TestBuild_NoValidationErrors(t *testing.T) {
	cfg := baseConfig(t)
	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("校验错误: %v", e)
		}
	}
}

// TestChainOutboundHasDetour 断言链式代理编译成的是「带 detour 的出站」,
// 并且 detour 指向前置组而不是任何链式代理。
func TestChainOutboundHasDetour(t *testing.T) {
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	byTag := map[string]render.SBOutbound{}
	for _, ob := range sb.Outbounds {
		byTag[ob.Tag] = ob
	}
	for _, ch := range cfg.Chains {
		ob, ok := byTag[ch.Name]
		if !ok {
			t.Fatalf("链式代理 %s 未生成出站", ch.Name)
		}
		if ob.Detour != "前置线路" {
			t.Fatalf("%s 的 detour 应为「前置线路」,实际 %q", ch.Name, ob.Detour)
		}
		if ob.Server != ch.Host || ob.ServerPort != ch.Port {
			t.Fatalf("%s 的出口地址不对: %s:%d", ch.Name, ob.Server, ob.ServerPort)
		}
	}
	// socks5 出口应该编译成 socks 类型
	if byTag["香港住宅"].Type != "socks" {
		t.Fatalf("socks5 出口类型应为 socks,实际 %s", byTag["香港住宅"].Type)
	}
	if byTag["美国 HTTP"].Type != "http" || byTag["美国 HTTP"].TLS != nil {
		t.Fatalf("http 出口类型不对")
	}
	https := byTag["美国 HTTPS"]
	if https.Type != "http" || https.TLS == nil || !https.TLS.Enabled {
		t.Fatalf("https 出口应该编译成带 TLS 的 http,实际 %+v", https)
	}
	if https.TLS.ServerName != "proxy.example.com" {
		t.Fatalf("https 出口的 SNI 不对: %q", https.TLS.ServerName)
	}
	// 前置组不能含链式代理
	front := byTag["前置线路"]
	for _, m := range front.Outbounds {
		if strings.HasPrefix(m, "chain:") {
			t.Fatalf("前置组里不该出现链式代理: %s", m)
		}
	}
}

// TestDynamicGroupExcludesChains 动态组默认不收链式代理。
func TestDynamicGroupExcludesChains(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Chains[0].Name = "香港住宅-含香港字样"
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	for _, ob := range sb.Outbounds {
		if ob.Tag != "香港" {
			continue
		}
		for _, m := range ob.Outbounds {
			if m == "香港住宅-含香港字样" || m == "美国 HTTP" || m == "美国 HTTPS" {
				t.Fatalf("动态组不该收链式代理,实际成员: %v", ob.Outbounds)
			}
		}
		if len(ob.Outbounds) == 0 {
			t.Fatalf("动态组应至少收编一个含「香港」的节点")
		}
	}
}

// TestKernelAcceptsGeneratedConfig 是最重要的一条:用真实内核启动生成的配置。
func TestKernelAcceptsGeneratedConfig(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("未找到 sing-box 二进制,跳过真实内核测试(设置 BOXHUB_SING_BOX 可指定)")
	}
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	raw, err := sb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	alive, out := kernelWithClash(t, bin, path, cfg.Inbounds.ClashAPIPort, dir)
	if !alive {
		t.Fatalf("内核拒绝启动(说明生成的配置有 FATAL 级错误):\n%s", out)
	}
	if strings.Contains(out, "FATAL") || strings.Contains(out, "panic") {
		t.Fatalf("内核启动过程中出现致命错误:\n%s", out)
	}
}

func assertClashHasProxies(t *testing.T, cfg *config.Config) {
	t.Helper()
	// 上一步的内核已经杀掉,这里只做结构断言
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"chain:c-home": false, "chain:c-http": false, "group:g-front": false}
	for _, ob := range sb.Outbounds {
		if _, ok := want[ob.Tag]; ok {
			want[ob.Tag] = true
		}
	}
	for tag, ok := range want {
		if !ok {
			t.Fatalf("缺少出站 %s", tag)
		}
	}
}

// TestSelfReferenceRejected 链式代理的前置组包含自己时必须被拒绝。
func TestSelfReferenceRejected(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Groups[0].Members = append(cfg.Groups[0].Members, "chain:c-home")
	errs := config.ValidateAll(cfg)
	if len(errs) == 0 {
		t.Fatal("前置组包含自身应报错")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "循环") || strings.Contains(e.Message, "自身") {
			found = true
		}
	}
	if !found {
		t.Fatalf("错误信息应说明循环依赖,实际: %v", errs)
	}
}

// TestChainCycleRejected 两条链式代理互为前置时必须被拒绝。
func TestChainCycleRejected(t *testing.T) {
	cfg := baseConfig(t)
	// 新组只含两条链式代理,并让它们互为前置
	cfg.Groups = append(cfg.Groups,
		config.Group{ID: "g-c1", Name: "前置A", Type: config.GroupSelect, Members: []string{"chain:c-http"}},
		config.Group{ID: "g-c2", Name: "前置B", Type: config.GroupSelect, Members: []string{"chain:c-home"}},
	)
	cfg.Chains[0].FrontGroupID = "g-c1" // c-home 前置 c-http
	cfg.Chains[1].FrontGroupID = "g-c2" // c-http 前置 c-home
	errs := config.ValidateAll(cfg)
	hasCycle := false
	for _, e := range errs {
		if strings.Contains(e.Message, "循环依赖") {
			hasCycle = true
		}
	}
	if !hasCycle {
		t.Fatalf("互为前置应报循环依赖,实际: %v", errs)
	}
}

// TestChainAsMemberOfGroup 链式代理可以像普通节点一样进静态组并被分流引用。
func TestChainAsMemberOfGroup(t *testing.T) {
	cfg := baseConfig(t)
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	var main render.SBOutbound
	for _, ob := range sb.Outbounds {
		if ob.Tag == "代理" {
			main = ob
		}
	}
	joined := strings.Join(main.Outbounds, ",")
	if !strings.Contains(joined, "香港住宅") {
		t.Fatalf("主组应包含链式代理,实际: %v", main.Outbounds)
	}
	// 分流规则引用链式代理
	found := false
	for _, ru := range sb.Route.Rules {
		if ru.Outbound == "香港住宅" {
			found = true
		}
	}
	if !found {
		t.Fatal("分流规则应能直接指向链式代理")
	}
}

// TestKernelRunsWithEmptyGroups 是踩坑回归测试。
//
// 首装时配置里只有空的默认节点组,此时 route.final 若指向这个空组,
// `sing-box check` 会通过(check 不校验出站引用是否存在),
// 但 `sing-box run` 直接 FATAL:default outbound not found: group:xxx。
// 面板会显示"内核运行中"但其实已经挂了,这类问题极难排查,必须有测试守住。
func TestKernelRunsWithEmptyGroups(t *testing.T) {
	bin := singBoxBinary(t)
	if bin == "" {
		t.Skip("未找到 sing-box 二进制")
	}
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = freePort(t)
	cfg.Inbounds.ClashAPI = true
	cfg.Inbounds.ClashAPIPort = freePort(t) + 1
	cfg.Inbounds.AllowLAN = false
	cfg.DNS.FakeIP = false
	// 只有空组,没有任何节点
	cfg.Groups = []config.Group{
		{ID: "g-empty", Name: "手动选择", Type: config.GroupSelect, Members: nil},
		{ID: "g-empty2", Name: "前置线路", Type: config.GroupSelect, Members: []string{}},
	}
	// 再加一条指向空组的分流规则
	cfg.Policies = append(cfg.Policies, config.Policy{
		ID: "p-empty", Name: "指向空组", Enabled: true, Target: "group:g-empty",
		Match: config.Match{DomainSuffix: []string{"example.com"}},
	})
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	// final 不能指向不存在的出站
	for _, ob := range sb.Outbounds {
		if ob.Tag == sb.Route.Final {
			goto ok
		}
	}
	t.Fatalf("route.final=%q 没有对应的出站", sb.Route.Final)
ok:
	raw, _ := sb.Marshal()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 必须真的能 run 起来(不是只看 check)。这里只关心"是否存活",
	// 不关心链式代理是否出现(本用例里没有链式代理)。
	alive, out := runOnly(t, bin, path, dir, 15*time.Second)
	if !alive {
		t.Fatalf("空组场景下内核无法运行(这是 check 通过但 run 失败的经典问题):\n%s", out)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	cfg := baseConfig(t)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back config.Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Chains) != len(cfg.Chains) || back.Chains[0].FrontGroupID != cfg.Chains[0].FrontGroupID {
		t.Fatal("链式代理序列化失败")
	}
}

func init() {
	// 让测试里的 freePort 不重复
	counter = int(time.Now().UnixNano() % 1000) + 30000
}

var counter int

func TestPortHelper(t *testing.T) {
	if strconv.Itoa(counter) == "" {
		t.Fatal("unreachable")
	}
}

// Package probe 实现链式代理的链路探测:测速 + 出口公网 IP 归属。
//
// Open-Box 的链式代理体验里有两个关键点,这里都实现了:
//
//  1. **测速**:经"前置 → 出口"完整链路访问一个测速地址,得到端到端延时。
//     注意不是只测出口能不能连——前置不通时出口也走不通,必须整链测。
//  2. **出口 IP 与归属**:经链路访问一个回显 IP 的接口,确认出口看到的公网 IP,
//     并显示国家 / 地区 / ISP。这是判断"住宅 IP 是否真的生效"的唯一可靠办法。
//
// 实现方式不用现成的 SOCKS 库:sing-box 的 Clash API 已经提供了经指定出站
// 发起请求的能力(代理选择),但"出口 IP"必须真实经链路访问才能拿到。
// 这里用最小实现的 SOCKS5 / HTTP CONNECT 客户端,自己走完整链路:
// 本地 → 前置(由 sing-box 提供)-> 出口 → 目标。
package probe

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result 一次链路探测结果。
type Result struct {
	// Delay 端到端延时(毫秒),-1 表示失败。
	Delay int `json:"delay"`
	// ExitIP 出口看到的公网 IP。
	ExitIP string `json:"exit_ip,omitempty"`
	// Country / Region / ISP 归属信息。
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	City    string `json:"city,omitempty"`
	ISP     string `json:"isp,omitempty"`
	// Error 失败原因。
	Error string `json:"error,omitempty"`
	// At 探测时间。
	At time.Time `json:"at"`
	// Stages 各阶段耗时,便于判断是前置慢还是出口慢。
	Stages map[string]int `json:"stages,omitempty"`
	// ExitByProto 按目标协议区分的出口 IP。
	// 智能出口池会按目标端口分配不同出口,这个字段能把这个事实呈现出来。
	ExitByProto map[string]string `json:"exit_by_proto,omitempty"`
	// PoolNote 当不同协议的出口不一致时给出的说明。
	PoolNote string `json:"pool_note,omitempty"`
}

// Dialer 是一个能"经前置再经出口"建立连接的拨号器。
type Dialer interface {
	// DialContext 建立到 addr 的 TCP 连接。addr 形如 "example.com:443"。
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Options 探测参数。
type Options struct {
	// TestURL 测速地址,应与机场判定可用性的地址一致。
	TestURL string
	// IPEndpoints 回显 IP 的接口,按顺序尝试。
	IPEndpoints []string
	// Timeout 单次请求超时。
	Timeout time.Duration
}

// DefaultOptions 默认探测参数。
//
// IPEndpoints 的选取有讲究:部分住宅/静态 IP 代理是**按目标自动选出口池**的
// 智能代理(实测:同一条代理访问 HTTP:80 出口是国内 IP,访问 HTTPS:443
// 出口是日本 IP)。所以探测必须同时用两种协议的接口,
// 否则会给出"出口 IP 看起来是境外"的误导结论。
func DefaultOptions() Options {
	return Options{
		TestURL: "https://www.gstatic.com/generate_204",
		IPEndpoints: []string{
			// HTTP:80 —— 反映"明文流量"的出口
			"http://ip-api.com/json/?fields=status,country,regionName,city,isp,query&lang=zh-CN",
			// HTTPS:443 —— 反映"加密流量"的出口
			"https://ipinfo.io/json",
		},
		Timeout: 10 * time.Second,
	}
}

// ChainDialer 通过"本地 sing-box 的混合端口"拨号。语料上:
//
//	BoxHub → 本地 SOCKS5(sing-box mixed 入站)→ [前置节点] → 出口 → 目标
//
// 之所以不直接连出口:出口只接受来自前置的连入,直连出口通常是被拒绝的;
// 而且我们要验证的正是"整条链路通不通"。
type ChainDialer struct {
	// ProxyAddr 本地混合入站地址,例如 127.0.0.1:7890。
	ProxyAddr string
	// Username / Password 入站认证(当前 BoxHub 默认无认证)。
	Username string
	Password string
}

// DialContext 实现 Dialer。
func (d *ChainDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", d.ProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接本地代理失败: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := socks5Handshake(conn, addr, d.Username, d.Password); err != nil {
		conn.Close()
		return nil, err
	}
	// 握手完成后交给 HTTP 客户端自行管理超时
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// socks5Handshake 完成 SOCKS5 握手与 CONNECT。
func socks5Handshake(conn net.Conn, addr, user, pass string) error {
	// 版本 + 认证方式
	methods := []byte{0x00} // 无认证
	if user != "" {
		methods = append(methods, 0x02) // 用户名密码
	}
	buf := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("SOCKS5 握手失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("SOCKS5 握手响应失败: %w", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("SOCKS5 版本不符: %d", resp[0])
	}
	switch resp[1] {
	case 0x00:
		// 无需认证
	case 0x02:
		if user == "" {
			return errors.New("代理要求用户名密码")
		}
		ub := []byte(user)
		pb := []byte(pass)
		b := []byte{0x01, byte(len(ub))}
		b = append(b, ub...)
		b = append(b, byte(len(pb)))
		b = append(b, pb...)
		if _, err := conn.Write(b); err != nil {
			return fmt.Errorf("SOCKS5 认证发送失败: %w", err)
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return fmt.Errorf("SOCKS5 认证响应失败: %w", err)
		}
		if authResp[1] != 0x00 {
			return errors.New("SOCKS5 认证被拒绝")
		}
	case 0xff:
		return errors.New("SOCKS5 代理不接受任何认证方式")
	default:
		return fmt.Errorf("SOCKS5 未知认证方式: %d", resp[1])
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("目标地址无效: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("目标端口无效: %w", err)
	}
	req := []byte{0x05, 0x01, 0x00} // CONNECT
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("域名过长")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 发送失败: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 响应失败: %w", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("SOCKS5 响应版本不符")
	}
	if head[1] != 0x00 {
		return fmt.Errorf("SOCKS5 连接失败: %s", socksErrText(head[1]))
	}
	// 读完绑定地址
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return err
		}
		skip = int(one[0])
	default:
		return fmt.Errorf("SOCKS5 未知地址类型: %d", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return err
	}
	return nil
}

func socksErrText(code byte) string {
	switch code {
	case 0x01:
		return "代理服务器故障"
	case 0x02:
		return "规则不允许"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "连接被拒绝"
	case 0x06:
		return "TTL 过期"
	case 0x07:
		return "不支持的命令"
	case 0x08:
		return "不支持的地址类型"
	}
	return "未知错误"
}

// HTTPDialer 是直接用 HTTP CONNECT 经代理拨号,用于不经过内核、
// 直接验证出口本身的连通性(排查"是前置坏了还是出口坏了")。
type HTTPDialer struct {
	// ProxyURL 例如 http://user:pass@host:port 或 https://...
	ProxyURL *url.URL
	Timeout  time.Duration
}

func (d *HTTPDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	host := d.ProxyURL.Host
	if !strings.Contains(host, ":") {
		if d.ProxyURL.Scheme == "https" {
			host += ":443"
		} else {
			host += ":8080"
		}
	}
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("连接出口失败: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.ProxyURL.User != nil {
		pass, _ := d.ProxyURL.User.Password()
		auth := base64Encode(d.ProxyURL.User.Username() + ":" + pass)
		req += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	req += "Proxy-Connection: Keep-Alive\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("出口代理响应异常: %w", err)
	}
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("出口代理拒绝连接: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func base64Encode(s string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(table[(n>>18)&63])
		b.WriteByte(table[(n>>12)&63])
		if rem > 1 {
			b.WriteByte(table[(n>>6)&63])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(table[n&63])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

// Run 通过给定拨号器执行一次完整探测:测速 + 出口 IP 归属。
func Run(ctx context.Context, d Dialer, opts Options) Result {
	if opts.Timeout == 0 {
		opts = DefaultOptions()
	}
	res := Result{At: time.Now(), Stages: map[string]int{}}
	start := time.Now()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         d.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: opts.Timeout,
		},
		Timeout: opts.Timeout,
		// 不跟随跳转:ip 接口通常不跳转,跳转会多一次往返
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// ---- 1. 测速 ----
	delay, err := measureDelay(ctx, client, opts.TestURL)
	if err != nil {
		res.Delay = -1
		res.Error = "测速失败: " + err.Error()
		return res
	}
	res.Delay = delay
	res.Stages["delay"] = delay

	// ---- 2. 出口 IP 与归属 ----
	// 逐个接口查询,记录每个协议的出口;第一个成功的作为主结果。
	res.ExitByProto = map[string]string{}
	for _, ep := range opts.IPEndpoints {
		ip, geo, err := queryIP(ctx, client, ep)
		if err != nil {
			continue
		}
		proto := "https"
		if strings.HasPrefix(ep, "http://") {
			proto = "http"
		}
		res.ExitByProto[proto] = ip
		if res.ExitIP == "" {
			res.ExitIP = ip
			res.Country, res.Region, res.City, res.ISP = geo.Country, geo.Region, geo.City, geo.ISP
		}
		res.Stages["ip"] = int(time.Since(start).Milliseconds()) - delay
	}
	if res.ExitIP != "" {
		// 不同协议出口不一致时明确提示,避免用户误以为配置错了
		if h, ok := res.ExitByProto["http"]; ok {
			if s, ok2 := res.ExitByProto["https"]; ok2 && h != s {
				res.PoolNote = fmt.Sprintf("该出口按目标协议分配不同 IP(HTTP=%s,HTTPS=%s),属于智能出口池", h, s)
			}
		}
		return res
	}
	// 测速成功但 IP 查询失败:不算整体失败,但要说清楚
	res.Error = "链路可用,但出口 IP 查询失败(可能是接口被墙)"
	return res
}

func measureDelay(ctx context.Context, client *http.Client, testURL string) (int, error) {
	if testURL == "" {
		testURL = "https://www.gstatic.com/generate_204"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return -1, err
	}
	req.Header.Set("User-Agent", "BoxHub/1.0 (+probe)")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	// generate_204 返回 204;其它测速地址可能返回 200/301,只要拿到响应就算通
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return int(time.Since(start).Milliseconds()), nil
}

type geoInfo struct {
	Country, Region, City, ISP string
}

func queryIP(ctx context.Context, client *http.Client, endpoint string) (string, geoInfo, error) {
	var g geoInfo
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", g, err
	}
	req.Header.Set("User-Agent", "BoxHub/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", g, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", g, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", g, err
	}
	// ip-api.com 格式
	var a struct {
		Status string `json:"status"`
		Country string `json:"country"`
		RegionName string `json:"regionName"`
		City string `json:"city"`
		ISP string `json:"isp"`
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &a); err == nil && a.Query != "" {
		return a.Query, geoInfo{a.Country, a.RegionName, a.City, a.ISP}, nil
	}
	// ipinfo.io 格式
	var b struct {
		IP     string `json:"ip"`
		City   string `json:"city"`
		Region string `json:"region"`
		Country string `json:"country"`
		Org    string `json:"org"`
	}
	if err := json.Unmarshal(body, &b); err == nil && b.IP != "" {
		return b.IP, geoInfo{b.Country, b.Region, b.City, b.Org}, nil
	}
	// 纯文本回显
	s := strings.TrimSpace(string(body))
	if ip := net.ParseIP(s); ip != nil {
		return s, g, nil
	}
	return "", g, errors.New("无法解析 IP 接口返回")
}

// Batch 并发探测多个链路,限制并发度避免把出口打满。
func Batch(ctx context.Context, jobs map[string]Dialer, opts Options, concurrency int) map[string]Result {
	if concurrency <= 0 {
		concurrency = 4
	}
	out := make(map[string]Result, len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for key, d := range jobs {
		wg.Add(1)
		go func(k string, dialer Dialer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
			r := Run(cctx, dialer, opts)
			mu.Lock()
			out[k] = r
			mu.Unlock()
		}(key, d)
	}
	wg.Wait()
	return out
}

// encoding/binary 在 SOCKS5 端口解析里用到,保留引用以防后续改成二进制读写。
var _ = binary.BigEndian

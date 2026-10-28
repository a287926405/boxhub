package probe_test

// 这是整个项目里最有说服力的一个测试:**真的搭起一条链,验证探测能拿到出口 IP**。
//
// 拓扑:
//
//	测试客户端 --SOCKS5--> sing-box(前置=direct) --SOCKS5--> 本地出口代理 --> 互联网
//
// 出口是一个本地最小 SOCKS5 服务,它只做 TCP 转发。如果探测结果里的 IP
// 能正常返回,说明"经前置再到出口"的链路是通的、探测逻辑是对的。
//
// 没有 sing-box 或没有外网时自动跳过,不阻塞其它测试。

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"boxhub/internal/probe"
)

// fakeSocks5 是一个最小 SOCKS5 服务器,记录它被连接的次数,
// 用来证明"流量确实经过了出口"。
type fakeSocks5 struct {
	ln    net.Listener
	hits  int32
	mu    sync.Mutex
	addr  string
}

func newFakeSocks5(t *testing.T) *fakeSocks5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSocks5{ln: ln, addr: ln.Addr().String()}
	go s.serve()
	return s
}

func (s *fakeSocks5) Close() { s.ln.Close() }

func (s *fakeSocks5) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.hits)
}

func (s *fakeSocks5) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		go handleSocks(c)
	}
}

func handleSocks(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		return
	}
	methods := make([]byte, buf[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	c.Write([]byte{0x05, 0x00})
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	var host string
	switch head[3] {
	case 0x01:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, l[0])
		io.ReadFull(c, b)
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	port := int(binary.BigEndian.Uint16(pb))
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 8*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func TestProbeThroughRealChain(t *testing.T) {
	bin := singBox(t)
	if bin == "" {
		t.Skip("未找到 sing-box,跳过真实链路探测测试")
	}
	// 先确认有外网,否则这个测试没有意义
	if !hasInternet() {
		t.Skip("当前无外网,跳过真实链路探测")
	}

	exit := newFakeSocks5(t)
	defer exit.Close()

	// 起一个 sing-box:混合入站 → 链式出站(前置 direct → 本地 SOCKS5 出口)
	mixedPort := freePort(t)
	apiPort := freePort(t)
	dir := t.TempDir()
	host, portStr, _ := net.SplitHostPort(exit.addr)
	cfgJSON := fmt.Sprintf(`{
  "log":{"level":"warn"},
  "inbounds":[{"type":"mixed","tag":"mixed-in","listen":"127.0.0.1","listen_port":%d}],
  "outbounds":[
    {"type":"socks","tag":"chain-exit","server":%q,"server_port":%s,"version":"5","detour":"group-front"},
    {"type":"selector","tag":"group-front","outbounds":["direct"],"default":"direct"},
    {"type":"selector","tag":"group-main","outbounds":["chain-exit"],"default":"chain-exit"},
    {"type":"direct","tag":"direct"}
  ],
  "route":{"final":"group-main","auto_detect_interface":true},
  "experimental":{"clash_api":{"external_controller":"127.0.0.1:%d","default_mode":"rule"}}
}`, mixedPort, host, portStr, apiPort)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "-c", cfgPath, "-D", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var logBuf syncBuffer
	cmd.Stdout = &logBuf
	cmd.Stderr = &logBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	// 等混合端口就绪
	if !waitPort(net.JoinHostPort("127.0.0.1", strconv.Itoa(mixedPort)), 8*time.Second) {
		t.Fatalf("sing-box 未就绪:\n%s", logBuf.String())
	}

	before := exit.Hits()
	d := &probe.ChainDialer{ProxyAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(mixedPort))}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	res := probe.Run(ctx, d, probe.DefaultOptions())

	if res.Delay <= 0 {
		t.Fatalf("链路测速失败: %s\nsing-box 日志:\n%s", res.Error, logBuf.String())
	}
	if res.ExitIP == "" {
		t.Fatalf("未拿到出口 IP: %+v", res)
	}
	// 关键断言:流量确实经过了出口代理
	if exit.Hits() <= before {
		t.Fatalf("出口代理没有被访问,hits=%d", exit.Hits())
	}
	t.Logf("链路探测成功: 延时 %dms, 出口 IP %s (%s %s %s)", res.Delay, res.ExitIP, res.Country, res.Region, res.ISP)
}

func TestChainDialerUnreachableProxy(t *testing.T) {
	// 指向一个不存在的端口,必须快速失败而不是挂住
	d := &probe.ChainDialer{ProxyAddr: "127.0.0.1:1"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := probe.Run(ctx, d, probe.Options{Timeout: 2 * time.Second, TestURL: "https://www.gstatic.com/generate_204"})
	if res.Delay != -1 || res.Error == "" {
		t.Fatalf("不可达的代理应快速失败: %+v", res)
	}
}

func TestHTTPDialerConnect(t *testing.T) {
	// 起一个假的 HTTP CONNECT 代理,验证握手与透传
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
		c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		time.Sleep(50 * time.Millisecond)
	}()
	u, _ := neturl(ln.Addr().String())
	d := &probe.HTTPDialer{ProxyURL: u, Timeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("CONNECT 失败: %v", err)
	}
	conn.Close()
	req := <-got
	if !contains(req, "CONNECT example.com:443") {
		t.Fatalf("代理收到的请求不对: %q", req)
	}
}

// ---------- 小工具 ----------

func singBox(t *testing.T) string {
	if p := os.Getenv("BOXHUB_SING_BOX"); p != "" {
		return p
	}
	for _, p := range []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func hasInternet() bool {
	conn, err := net.DialTimeout("tcp", "www.gstatic.com:443", 4*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitPort(addr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

func neturl(hostport string) (*url.URL, error) {
	return url.Parse("http://" + hostport)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

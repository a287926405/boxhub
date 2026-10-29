package render_test

// 53 端口 DNS 入口的回归测试。
//
// 网关模式(iKuai 把全网设备的网关与 DNS 指向 BoxHub)需要本机有一个
// 53 端口的 DNS 入口,查询才能进内核 hijack-dns 按分流规则解析。
// 这里锁住四个行为:
//   1. dns_listen 开启时生成 direct 入站 dns-in;
//   2. 嗅探规则覆盖 dns-in,查询最终交给 hijack-dns;
//   3. dns_listen 关闭时不生成;
//   4. 端口被占用(比如手工装的 dnsmasq)时自动跳过,内核照常启动。

import (
	"encoding/json"
	"net"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
)

func buildInboundTags(t *testing.T, cfg *config.Config) (map[string]any, []map[string]any) {
	t.Helper()
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sb.Inbounds)
	var inbounds []map[string]any
	if err := json.Unmarshal(raw, &inbounds); err != nil {
		t.Fatal(err)
	}
	byTag := map[string]any{}
	for _, ib := range inbounds {
		byTag[ib["tag"].(string)] = ib
	}
	rawRules, _ := json.Marshal(sb.Route.Rules)
	var rules []map[string]any
	_ = json.Unmarshal(rawRules, &rules)
	return byTag, rules
}

// freeUDPPort 返回一个当前空闲的 UDP 端口,用于避免测试依赖 53 端口的占用情况。
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func TestDNSInboundRenderedWhenEnabled(t *testing.T) {
	cfg := config.NewDefault()
	port := freeUDPPort(t)
	cfg.Inbounds.DNSListen = true
	cfg.Inbounds.DNSPort = port

	byTag, rules := buildInboundTags(t, cfg)
	ib, ok := byTag["dns-in"]
	if !ok {
		t.Fatal("dns_listen 开启但没有生成 dns-in 入站")
	}
	if ib.(map[string]any)["type"] != "direct" {
		t.Fatalf("dns-in 应该是 direct 入站,得到 %v", ib)
	}

	// 嗅探规则要覆盖 dns-in,否则 DNS 协议识别不出来
	sniffFound, hijackFound := false, false
	for _, r := range rules {
		switch r["action"] {
		case "sniff":
			inbounds, _ := r["inbound"].([]any)
			for _, in := range inbounds {
				if in == "dns-in" {
					sniffFound = true
				}
			}
		case "hijack-dns":
			hijackFound = true
		}
	}
	if !sniffFound {
		t.Fatal("嗅探规则没有覆盖 dns-in,DNS 查询无法被识别")
	}
	if !hijackFound {
		t.Fatal("路由里没有 hijack-dns 规则")
	}
}

func TestDNSInboundOmittedWhenDisabled(t *testing.T) {
	cfg := config.NewDefault()
	cfg.Inbounds.DNSListen = false
	cfg.Inbounds.DNSPort = freeUDPPort(t)

	byTag, _ := buildInboundTags(t, cfg)
	if _, ok := byTag["dns-in"]; ok {
		t.Fatal("dns_listen 关闭时不应生成 dns-in 入站")
	}
}

func TestDNSInboundSkippedWhenPortBusy(t *testing.T) {
	// 模拟手工装的 dnsmasq:先占住端口,渲染必须自动跳过而不是让内核绑定失败
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	busy := pc.LocalAddr().(*net.UDPAddr).Port

	cfg := config.NewDefault()
	cfg.Inbounds.DNSListen = true
	cfg.Inbounds.DNSPort = busy

	byTag, _ := buildInboundTags(t, cfg)
	if _, ok := byTag["dns-in"]; ok {
		t.Fatalf("端口 %d 被占用时仍生成了 dns-in,内核会绑定失败起不来", busy)
	}
}

// 内核正在运行时,53 端口是被"即将被替换掉的旧内核"自己占着的。
//
// 这是个真实事故:面板上改一次设置,boxd 先渲染新配置、再停旧内核。
// 渲染那一刻旧内核还活着占着 53,端口探测拿到 EADDRINUSE,于是
// dns-in 被当成"端口被别的程序占用"静默删掉。新内核起来后没人监听 53,
// 局域网所有设备的 DNS 全部超时——电脑 ERR_NAME_NOT_RESOLVED 上不了网,
// 手机因为不走网关 DNS 所以看着一切正常,极易误判成"只有电脑坏了"。
func TestDNSInboundKeptWhenKernelRunningHoldsPort(t *testing.T) {
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	busy := pc.LocalAddr().(*net.UDPAddr).Port

	cfg := config.NewDefault()
	cfg.Inbounds.DNSListen = true
	cfg.Inbounds.DNSPort = busy

	sb, err := render.Build(cfg, render.Context{KernelRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ib := range sb.Inbounds {
		if ib.Tag == "dns-in" {
			found = true
		}
	}
	if !found {
		t.Fatal("内核正在运行时端口必然被自己占着,不能据此删掉 dns-in,否则重启后局域网 DNS 全挂")
	}
}

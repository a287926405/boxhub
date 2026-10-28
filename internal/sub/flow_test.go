package sub_test

// 覆盖 YAML「行内流式映射」格式的订阅解析。
//
// 这是真实踩过的坑:某些机场(如 cmed 系)返回的 Clash 配置里,节点写成
//
//	- {name: JP, server: 1.2.3.4, port: 443, type: vless, ...
//	   ws-opts: {path: "/?ed=2560", headers: {Host: x}}}
//
// 整条节点压缩在一行大括号里,还嵌套子映射。早期解析器只认块格式
// (`- name: x` 换行 `type: y`),结果把每一行都当成"纯字符串列表项"跳过,
// 表现为"订阅添加成功但没有节点"。这类格式现在必须优先识别。

import (
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
	"boxhub/internal/sub"
)

const flowStyleSubscription = `
dns:
  enable: true
  nameserver:
    - https://sm2.doh.pub/dns-query
port: 7890
socks-port: 7891
allow-lan: true
mode: rule
external-controller: 127.0.0.1:9090
proxies:
  - {name: JP 1, server: 172.64.53.120, port: 443, type: vless, uuid: 77e254e8-9f90-4bd5-8150-aacdd2ce7808, tls: true, skip-cert-verify: false, servername: cmed.example.io, client-fingerprint: chrome, network: ws, ws-opts: {path: "/?ed=2560", headers: {Host: cmed.example.io}}, ech-opts: {enable: true, query-server-name: cloudflare-ech.com}}
  - {name: JP 2, server: 172.64.52.30, port: 8443, type: vless, uuid: 77e254e8-9f90-4bd5-8150-aacdd2ce7808, tls: true, servername: cmed.example.io, network: ws, ws-opts: {path: "/?ed=2560", headers: {Host: cmed.example.io}}}
  - {name: "US, West", server: 1.2.3.4, port: 443, type: trojan, password: "p,a,s,s", sni: us.example.io, skip-cert-verify: true}
  - {name: HK SS, server: 5.6.7.8, port: 8388, type: ss, cipher: aes-256-gcm, password: hkpass}
  - {name: 韩国 Hy2, server: 9.9.9.9, port: 443, type: hysteria2, password: hy2pass, sni: kr.example.io, skip-cert-verify: true}
proxy-groups:
  - name: 🚀 节点选择
    type: select
    proxies:
      - JP 1
      - JP 2
rules:
  - DOMAIN-SUFFIX,google.com,🚀 节点选择
`

func TestFlowStyleYAML(t *testing.T) {
	res, err := sub.ParseSubscription([]byte(flowStyleSubscription), "s1", "测试机场")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(res.Nodes) != 5 {
		t.Fatalf("应解析出 5 个节点, 实际 %d; 跳过: %v", len(res.Nodes), res.Skipped)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("不应有跳过项: %v", res.Skipped)
	}

	byName := map[string]config.Node{}
	for _, n := range res.Nodes {
		byName[strings.TrimPrefix(n.Name, "测试机场 · ")] = n
	}

	// 1. vless + ws + 嵌套 ws-opts
	jp, ok := byName["JP 1"]
	if !ok {
		t.Fatal("缺少 JP 1")
	}
	if jp.Type != "vless" || jp.Server != "172.64.53.120" || jp.Port != 443 {
		t.Fatalf("JP 1 基础字段错误: %+v", jp)
	}
	if jp.TLS == nil || jp.TLS.ServerName != "cmed.example.io" {
		t.Fatalf("JP 1 SNI 错误: %+v", jp.TLS)
	}
	if jp.TLS.UTLSFingerprint != "chrome" {
		t.Fatalf("JP 1 uTLS 指纹错误: %q", jp.TLS.UTLSFingerprint)
	}
	if jp.Transport == nil || jp.Transport.Type != "ws" {
		t.Fatalf("JP 1 传输层错误: %+v", jp.Transport)
	}
	if jp.Transport.Path != "/?ed=2560" {
		t.Fatalf("JP 1 ws path 错误: %q", jp.Transport.Path)
	}
	if jp.Transport.Host != "cmed.example.io" {
		t.Fatalf("JP 1 ws Host 错误: %q", jp.Transport.Host)
	}

	// 2. 名称里含逗号(引号内的逗号不能被当分隔符)
	us, ok := byName["US, West"]
	if !ok {
		keys := make([]string, 0, len(byName))
		for k := range byName {
			keys = append(keys, k)
		}
		t.Fatalf("名称含逗号的节点丢失,实际有: %v", keys)
	}
	if us.Password != "p,a,s,s" {
		t.Fatalf("密码里的逗号被切坏了: %q", us.Password)
	}
	if us.Type != "trojan" {
		t.Fatalf("US 类型错误: %s", us.Type)
	}

	// 3. ss
	ss, ok := byName["HK SS"]
	if !ok || ss.Method != "aes-256-gcm" || ss.Password != "hkpass" {
		t.Fatalf("ss 节点错误: %+v", ss)
	}

	// 4. hysteria2
	hy, ok := byName["韩国 Hy2"]
	if !ok || hy.Type != "hysteria2" || hy.Password != "hy2pass" {
		t.Fatalf("hysteria2 节点错误: %+v", hy)
	}
}

// TestFlowStyleCompiles 流式格式解析出的节点必须能生成内核可加载的配置。
func TestFlowStyleCompiles(t *testing.T) {
	res, err := sub.ParseSubscription([]byte(flowStyleSubscription), "s1", "测试机场")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = 28890
	cfg.Inbounds.ClashAPIPort = 28990
	cfg.Nodes = res.Nodes
	cfg.Groups = []config.Group{
		{ID: "g1", Name: "代理", Type: config.GroupSelect, Members: tagAll(res.Nodes)},
	}
	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("校验失败: %v", e)
		}
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if bin := singBox(t); bin != "" {
		writeAndRun(t, bin, sb)
	}
}

package sub_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/render"
	"boxhub/internal/sub"
)

const (
	linkSS = "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQxMjM@203.0.113.5:8388#%E9%A6%99%E6%B8%AFSS"
	// vmess: base64({"v":"2","ps":"东京 VMess","add":"203.0.113.6","port":"443","id":"11111111-2222-3333-4444-555555555555","aid":"0","net":"ws","path":"/ray","host":"cdn.example.com","tls":"tls","sni":"cdn.example.com"})
	linkVMess = "vmess://eyJ2IjoiMiIsInBzIjoi5Lic5LqsIFZtZXNzIiwiYWRkIjoiMjAzLjAuMTEzLjYiLCJwb3J0IjoiNDQzIiwiaWQiOiIxMTExMTExMS0yMjIyLTMzMzMtNDQ0NC01NTU1NTU1NTU1NTUiLCJhaWQiOiIwIiwibmV0Ijoid3MiLCJwYXRoIjoiL3JheSIsImhvc3QiOiJjZG4uZXhhbXBsZS5jb20iLCJ0bHMiOiJ0bHMiLCJzbmkiOiJjZG4uZXhhbXBsZS5jb20ifQ=="
	linkVLESS = "vless://aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee@203.0.113.7:443?security=reality&sni=www.microsoft.com&pbk=iW1CjE8zmD_9bSQ2vIqhBS2x1nY_8QsFhT9n3lEZmSA&sid=6ba85179&fp=chrome&type=tcp&flow=xtls-rprx-vision#%E7%BE%8E%E5%9B%BD%20Reality"
	linkTrojan = "trojan://mypassword@203.0.113.8:443?sni=tr.example.com&allowInsecure=1#%E6%96%B0%E5%8A%A0%E5%9D%A1"
	linkHy2   = "hysteria2://secret@203.0.113.9:8443?sni=hy.example.com&insecure=1&obfs=salamander&obfs-password=abc#%E6%97%A5%E6%9C%AC%20Hy2"
	linkTUIC  = "tuic://11111111-2222-3333-4444-666666666666:pass@203.0.113.10:443?sni=tuic.example.com&congestion_control=bbr#%E9%9F%A9%E5%9B%BD"
)

func TestParseIndividualLinks(t *testing.T) {
	cases := []struct {
		raw        string
		wantType   string
		wantServer string
		wantPort   int
		wantName   string
	}{
		{linkSS, "shadowsocks", "203.0.113.5", 8388, "香港SS"},
		{linkVMess, "vmess", "203.0.113.6", 443, "东京 Vmess"},
		{linkVLESS, "vless", "203.0.113.7", 443, "美国 Reality"},
		{linkTrojan, "trojan", "203.0.113.8", 443, "新加坡"},
		{linkHy2, "hysteria2", "203.0.113.9", 8443, "日本 Hy2"},
		{linkTUIC, "tuic", "203.0.113.10", 443, "韩国"},
	}
	for _, c := range cases {
		n, err := sub.ParseLink(c.raw)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", c.wantType, err)
		}
		if n.Type != c.wantType || n.Server != c.wantServer || n.Port != c.wantPort || n.Name != c.wantName {
			t.Fatalf("%s 解析结果不对: %+v", c.wantType, n)
		}
	}
}

func TestSSDetails(t *testing.T) {
	n, err := sub.ParseLink(linkSS)
	if err != nil {
		t.Fatal(err)
	}
	if n.Method != "aes-256-gcm" || n.Password != "password123" {
		t.Fatalf("ss 加密方式或密码解析错误: method=%q password=%q", n.Method, n.Password)
	}
}

func TestVLESSReality(t *testing.T) {
	n, err := sub.ParseLink(linkVLESS)
	if err != nil {
		t.Fatal(err)
	}
	if n.TLS == nil || n.TLS.RealityPublicKey == "" {
		t.Fatalf("REALITY 参数应被解析: %+v", n.TLS)
	}
	if n.Flow != "xtls-rprx-vision" {
		t.Fatalf("flow 解析错误: %q", n.Flow)
	}
}

func TestVmessWS(t *testing.T) {
	n, err := sub.ParseLink(linkVMess)
	if err != nil {
		t.Fatal(err)
	}
	if n.Transport == nil || n.Transport.Type != "ws" || n.Transport.Path != "/ray" {
		t.Fatalf("ws 传输层解析错误: %+v", n.Transport)
	}
	if n.TLS == nil || n.TLS.ServerName != "cdn.example.com" {
		t.Fatalf("TLS/SNI 解析错误: %+v", n.TLS)
	}
}

func TestBase64Subscription(t *testing.T) {
	body := strings.Join([]string{linkSS, linkVMess, linkVLESS, linkTrojan, linkHy2, linkTUIC}, "\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	res, err := sub.ParseSubscription([]byte(encoded), "sub1", "我的机场")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 6 {
		t.Fatalf("应解析出 6 个节点,实际 %d,跳过: %v", len(res.Nodes), res.Skipped)
	}
	for _, n := range res.Nodes {
		if !strings.HasPrefix(n.Name, "我的机场 · ") {
			t.Fatalf("名称应带订阅前缀: %q", n.Name)
		}
		if n.ID == "" || n.Source != "sub:sub1" {
			t.Fatalf("节点缺少 ID 或来源: %+v", n)
		}
	}
}

// TestNodeIDsAreStable 订阅更新后同一节点必须保持同一 ID,否则节点组里的选择会错位。
func TestNodeIDsAreStable(t *testing.T) {
	body := []byte(linkSS + "\n" + linkTrojan)
	res1, err := sub.ParseSubscription(body, "s1", "机场")
	if err != nil {
		t.Fatal(err)
	}
	// 顺序调换后再解析
	res2, err := sub.ParseSubscription([]byte(linkTrojan+"\n"+linkSS), "s1", "机场")
	if err != nil {
		t.Fatal(err)
	}
	ids1 := map[string]string{}
	for _, n := range res1.Nodes {
		ids1[n.Server] = n.ID
	}
	for _, n := range res2.Nodes {
		if ids1[n.Server] != n.ID {
			t.Fatalf("节点 %s 的 ID 不稳定: %s != %s", n.Server, ids1[n.Server], n.ID)
		}
	}
}

func TestClashYAML(t *testing.T) {
	yaml := `
mixed-port: 7890
proxies:
  - name: "HK 01"
    type: ss
    server: 203.0.113.20
    port: 8388
    cipher: chacha20-ietf-poly1305
    password: "abc:def"
  - name: JP VMess
    type: vmess
    server: 203.0.113.21
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    alterId: 0
    cipher: auto
    tls: true
    servername: jp.example.com
    network: ws
    ws-opts:
      path: /vm
      headers:
        Host: jp.example.com
  - name: 不支持的 SSR
    type: ssr
    server: 203.0.113.22
    port: 1080
proxy-groups:
  - name: PROXY
    type: select
    proxies:
      - HK 01
`
	res, err := sub.ParseSubscription([]byte(yaml), "s2", "YAML机场")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("应解析出 2 个节点,实际 %d (跳过: %v)", len(res.Nodes), res.Skipped)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "不支持的协议类型") {
		t.Fatalf("ssr 应被跳过并记录原因,实际: %v", res.Skipped)
	}
	var ss, vm *config.Node
	for i := range res.Nodes {
		switch res.Nodes[i].Type {
		case "shadowsocks":
			ss = &res.Nodes[i]
		case "vmess":
			vm = &res.Nodes[i]
		}
	}
	if ss == nil || ss.Method != "chacha20-ietf-poly1305" || ss.Password != "abc:def" {
		t.Fatalf("ss 字段解析错误: %+v", ss)
	}
	if vm == nil || vm.Transport == nil || vm.Transport.Path != "/vm" {
		t.Fatalf("vmess ws 解析错误: %+v", vm)
	}
	if vm.TLS == nil || vm.TLS.ServerName != "jp.example.com" {
		t.Fatalf("vmess TLS 解析错误: %+v", vm.TLS)
	}
}

// TestParsedNodesCompileToValidKernelConfig 是本文件最重要的一条:
// 从真实格式解析出的节点,必须能编译成内核**能加载**的配置。
func TestParsedNodesCompileToValidKernelConfig(t *testing.T) {
	body := []byte(strings.Join([]string{linkSS, linkVMess, linkVLESS, linkTrojan, linkHy2, linkTUIC}, "\n"))
	res, err := sub.ParseSubscription(body, "s1", "机场")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewDefault()
	cfg.Inbounds.MixedListen = "127.0.0.1"
	cfg.Inbounds.MixedPort = 27890
	cfg.Inbounds.ClashAPIPort = 27990
	cfg.Nodes = res.Nodes

	// 建两个组:一个普通选择组,一个前置组
	cfg.Groups = []config.Group{
		{ID: "g-main", Name: "代理", Type: config.GroupSelect, Members: tagAll(res.Nodes)},
		{ID: "g-front", Name: "前置线路", Type: config.GroupSelect, Members: tagAll(res.Nodes)},
	}
	// 用订阅节点做前置,出口用 socks5
	cfg.Chains = []config.Chain{
		{ID: "c1", Name: "住宅出口", Type: config.ChainSocks5, Host: "198.51.100.20", Port: 1080,
			Username: "u", Password: "p", FrontGroupID: "g-front"},
	}
	cfg.Groups[0].Members = append(cfg.Groups[0].Members, "chain:c1")

	if errs := config.ValidateAll(cfg); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("校验失败: %v", e)
		}
	}
	sb, err := render.Build(cfg, render.Context{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if _, err := sb.Marshal(); err != nil {
		t.Fatal(err)
	}
	// 真实内核校验(需要 BOXHUB_SING_BOX)
	if bin := singBox(t); bin != "" {
		writeAndRun(t, bin, sb)
	}
}

func tagAll(nodes []config.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, "node:"+n.ID)
	}
	return out
}

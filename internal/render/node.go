package render

import (
	"fmt"
	"regexp"
	"strings"

	"boxhub/internal/config"
)

var reCache = map[string]*regexp.Regexp{}

func regexMatch(expr, s string) bool {
	re, ok := reCache[expr]
	if !ok {
		compiled, err := regexp.Compile(expr)
		if err != nil {
			return false
		}
		re = compiled
		reCache[expr] = re
	}
	return re.MatchString(s)
}

// nodeOutbound 把面板节点翻译成 sing-box 出站。只覆盖 sing-box 自己支持的协议,
// 其余协议在订阅解析阶段就会被丢弃(宁缺毋滥,避免内核起不来)。
func nodeOutbound(n config.Node, tag string) (SBOutbound, error) {
	ob := SBOutbound{Tag: tag, Server: n.Server, ServerPort: n.Port}
	switch n.Type {
	case "shadowsocks":
		ob.Type = "shadowsocks"
		ob.Method = n.Method
		ob.Password = n.Password
		if n.Plugin != "" {
			ob.Plugin = n.Plugin
			if n.Plugin == "obfs-local" || n.Plugin == "simple-obfs" {
				ob.Plugin = "obfs-local"
			}
			if len(n.PluginOpts) > 0 {
				ob.PluginOpts = n.PluginOpts
			}
		}
		if n.UpMbps > 0 || n.DownMbps > 0 {
			ob.Multiplex = nil // shadowsocks 的 2022 加密不需要多路复用
		}
	case "vmess":
		ob.Type = "vmess"
		ob.UUID = n.UUID
		if n.AlterID > 0 {
			ob.AlterID = n.AlterID
		}
		ob.Security = n.Security
		ob.TLS = tlsFromNode(n)
		ob.Transport = transportFromNode(n)
	case "vless":
		ob.Type = "vless"
		ob.UUID = n.UUID
		if n.Flow != "" {
			ob.Flow = n.Flow
		}
		ob.TLS = tlsFromNode(n)
		ob.Transport = transportFromNode(n)
	case "trojan":
		ob.Type = "trojan"
		ob.Password = n.Password
		tls := tlsFromNode(n)
		if tls == nil {
			tls = &SBTLS{Enabled: true, ServerName: n.Server}
		}
		if tls.ServerName == "" {
			tls.ServerName = n.Server
		}
		ob.TLS = tls
		ob.Transport = transportFromNode(n)
	case "hysteria2":
		ob.Type = "hysteria2"
		ob.Password = n.Password
		ob.UpMbps = n.UpMbps
		ob.DownMbps = n.DownMbps
		tls := tlsFromNode(n)
		if tls == nil {
			tls = &SBTLS{Enabled: true}
		}
		if tls.ServerName == "" {
			tls.ServerName = n.Server
		}
		ob.TLS = tls
		ob.Obfs = &SBObfs{Type: "salamander", Password: n.ObfsPassword}
		if n.Obfs == "" || n.ObfsPassword == "" {
			ob.Obfs = nil
		}
	case "tuic":
		ob.Type = "tuic"
		ob.UUID = n.UUID
		ob.Password = n.Password
		ob.CongestionControl = n.CongestionControl
		ob.UDPRelayMode = "native"
		tls := tlsFromNode(n)
		if tls == nil {
			tls = &SBTLS{Enabled: true}
		}
		if tls.ServerName == "" {
			tls.ServerName = n.Server
		}
		tls.ALPN = []string{"h3"}
		ob.TLS = tls
	case "anytls":
		ob.Type = "anytls"
		ob.Password = n.Password
		tls := tlsFromNode(n)
		if tls == nil {
			tls = &SBTLS{Enabled: true}
		}
		if tls.ServerName == "" {
			tls.ServerName = n.Server
		}
		ob.TLS = tls
	case "socks":
		ob.Type = "socks"
		ob.Version = "5"
		ob.Username = n.Username
		ob.Password = n.Password
	case "http":
		ob.Type = "http"
		ob.Username = n.Username
		ob.Password = n.Password
		if n.TLS != nil && n.TLS.Enabled {
			ob.TLS = tlsFromNode(n)
		}
	case "wireguard":
		ob.Type = "wireguard"
		// wireguard 的 server/server_port 在 sing-box 里仍可用(1.11+ 推荐 peers,
		// 但单 peer 直填兼容性最好)
		ob.UUID = ""
		ob.Password = ""
		ob.LocalAddress = splitList(n.PluginOpts["address"])
		ob.PrivateKey = n.Password
		ob.PublicKey = n.UUID
		if len(n.PluginOpts) > 0 {
			ob.Reserved = nil
		}
	default:
		return ob, fmt.Errorf("不支持的协议 %q", n.Type)
	}
	if n.Multiplex != nil && n.Multiplex.Enabled {
		ob.Multiplex = &SBMultiplex{
			Enabled:    true,
			Protocol:   firstNonEmpty(n.Multiplex.Protocol, "h2mux"),
			MaxStreams: n.Multiplex.MaxStreams,
		}
	}
	if n.UDPOverTCP && n.Type == "shadowsocks" {
		ob.UDPOverTCP = &SBUDPOverTCP{Enabled: true}
	}
	return ob, nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func tlsFromNode(n config.Node) *SBTLS {
	t := n.TLS
	if t == nil {
		// vmess/vless 的 security=tls 也可能只写在 Security 字段里
		if strings.EqualFold(n.Security, "tls") || strings.EqualFold(n.Security, "reality") {
			t = &config.TLSConfig{Enabled: true}
		} else {
			return nil
		}
	}
	if !t.Enabled && t.RealityPublicKey == "" {
		return nil
	}
	out := &SBTLS{
		Enabled:    true,
		ServerName: t.ServerName,
		Insecure:   t.Insecure,
		ALPN:       t.ALPN,
	}
	if t.UTLSFingerprint != "" {
		out.UTLS = &SBUTLS{Enabled: true, Fingerprint: t.UTLSFingerprint}
	}
	if t.RealityPublicKey != "" {
		out.Reality = &SBReality{
			Enabled:   true,
			PublicKey: t.RealityPublicKey,
			ShortID:   t.RealityShortID,
		}
	}
	if len(t.ECH) > 0 {
		out.ECH = &SBECH{Enabled: true, Config: t.ECH}
	}
	if out.ServerName == "" {
		out.ServerName = n.Server
	}
	return out
}

func transportFromNode(n config.Node) *SBTransport {
	tr := n.Transport
	// network 字段的简写:ws / grpc
	if tr == nil && (n.Network == "ws" || n.Network == "grpc") {
		tr = &config.TransportConfig{Type: n.Network}
	}
	if tr == nil || tr.Type == "" || tr.Type == "tcp" {
		return nil
	}
	out := &SBTransport{Type: tr.Type}
	switch tr.Type {
	case "ws":
		path := tr.Path
		if path == "" {
			path = "/"
		}
		out.Path = path
		host := tr.Host
		if host == "" && len(tr.Headers) > 0 {
			host = tr.Headers["Host"]
		}
		if host != "" {
			out.Headers = map[string]string{"Host": host}
		}
	case "grpc":
		out.ServiceName = tr.ServiceName
	case "http", "httpupgrade":
		out.Path = tr.Path
		out.Host = tr.Host
		if len(tr.Headers) > 0 {
			out.Headers = tr.Headers
		}
	default:
		return nil
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

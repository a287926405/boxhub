// Package sub 解析订阅内容,把各种分享链接格式转成统一的 config.Node。
//
// 支持的输入:
//   - base64 编码的多行分享链接(最常见的机场格式)
//   - 明文多行分享链接
//   - Clash / Mihomo YAML 配置的 proxies 段
//
// 支持的协议:ss / vmess / vless(含 REALITY)/ trojan / hysteria2 / tuic /
// socks5 / http(s)。其它协议(ssr、snell 等)会被跳过并记入 Skipped,
// 这样一份订阅里混着不支持的节点也不会导致整份导入失败。
package sub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"boxhub/internal/config"
)

// Result 解析结果。
type Result struct {
	Nodes   []config.Node
	Skipped []string // 无法解析的条目说明
}

// ParseSubscription 解析一份订阅正文。subID 用于给节点生成稳定 ID。
func ParseSubscription(body []byte, subID, subName string) (*Result, error) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, fmt.Errorf("订阅内容为空")
	}
	// Clash YAML 直接解析
	if looksLikeYAML(text) {
		return parseClashYAML(text, subID, subName)
	}
	// base64 整体编码的情况
	if decoded, ok := tryBase64(text); ok && looksLikeLinks(decoded) {
		text = decoded
	}
	return parseLinkList(text, subID, subName), nil
}

func looksLikeYAML(text string) bool {
	if strings.HasPrefix(text, "proxies:") || strings.HasPrefix(text, "proxy-groups:") {
		return true
	}
	// 有 YAML 特征的多行文档
	if strings.Contains(text, "\nproxies:") || strings.Contains(text, "\nproxy-groups:") {
		return true
	}
	return strings.HasPrefix(text, "mixed-port:") || strings.HasPrefix(text, "port:")
}

func looksLikeLinks(text string) bool {
	for _, scheme := range []string{"ss://", "vmess://", "vless://", "trojan://", "hysteria2://", "hy2://", "tuic://", "socks://", "http://"} {
		if strings.Contains(text, scheme) {
			return true
		}
	}
	return false
}

func tryBase64(s string) (string, bool) {
	// 去掉所有空白
	s = strings.Join(strings.Fields(s), "")
	if s == "" {
		return "", false
	}
	// 补 padding
	for len(s)%4 != 0 {
		s += "="
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := enc.DecodeString(s); err == nil {
			str := string(out)
			if isPrintable(str) {
				return str, true
			}
		}
	}
	return "", false
}

func isPrintable(s string) bool {
	if len(s) == 0 {
		return false
	}
	bad := 0
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			bad++
		}
	}
	return bad*20 < len(s)
}

func parseLinkList(text, subID, subName string) *Result {
	res := &Result{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// 一行里可能有多个链接(少见),按协议前缀切开
		for _, raw := range splitLinks(line) {
			n, err := ParseLink(raw)
			if err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", abbreviate(raw), err))
				continue
			}
			finalize(&n, subID, subName)
			res.Nodes = append(res.Nodes, n)
		}
	}
	return res
}

// splitLinks 用协议前缀切分一行内的多个链接。
// 这里的实现必须保守:分享链接里可能含 base64 与查询串,不能按任意字符切,
// 只认协议前缀作为新链接的起点。
func splitLinks(line string) []string {
	schemes := []string{"ssr://", "ss://", "vmess://", "vless://", "trojan://",
		"hysteria2://", "hy2://", "hysteria://", "tuic://", "socks5://", "socks://",
		"anytls://", "http://", "https://"}
	// 找出所有协议前缀出现的位置
	type hit struct{ pos int; scheme string }
	var hits []hit
	for _, s := range schemes {
		for off := 0; ; {
			i := strings.Index(line[off:], s)
			if i < 0 {
				break
			}
			pos := off + i
			// 必须位于行首或前面是空白,避免把 path 里的 "http://" 当成新链接
			if pos == 0 || line[pos-1] == ' ' || line[pos-1] == '\t' {
				hits = append(hits, hit{pos, s})
				break
			}
			off = pos + len(s)
		}
	}
	if len(hits) == 0 {
		if t := strings.TrimSpace(line); t != "" {
			return []string{t}
		}
		return nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	// 去重(同一位置可能被多个协议前缀匹配,如 socks5:// 与 socks:// 不会重叠,但保险)
	var out []string
	for i, h := range hits {
		end := len(line)
		if i+1 < len(hits) {
			end = hits[i+1].pos
		}
		if seg := strings.TrimSpace(line[h.pos:end]); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func abbreviate(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[:45] + "..."
}

// finalize 补全名称前缀与 ID。
func finalize(n *config.Node, subID, subName string) {
	if n.Name == "" {
		n.Name = n.Server
	}
	if subName != "" {
		n.Name = subName + " · " + n.Name
	}
	n.Source = "sub:" + subID
	// ID 由订阅 + 服务器 + 端口 + 名称决定:订阅更新后同一节点保持同一 ID,
	// 因此它在节点组里的选择不会错位。
	n.ID = stableID(subID, n.Server, n.Port, n.Name)
}

// stableID 生成 12 位稳定 ID(可读性不重要,稳定性重要)。
func stableID(parts ...interface{}) string {
	h := uint64(1469598103934665603) // FNV-1a 64
	for _, p := range parts {
		s := fmt.Sprint(p)
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
		h ^= 0xff // 分隔符,避免 ("a","b") 与 ("ab","") 冲突
		h *= 1099511628211
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 12)
	for i := range b {
		b[i] = alphabet[h%36]
		h /= 36
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 单个分享链接
// ---------------------------------------------------------------------------

// ParseLink 解析一条分享链接。
func ParseLink(raw string) (config.Node, error) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "vmess://"):
		return parseVMess(raw)
	case strings.HasPrefix(lower, "vless://"):
		return parseVLESS(raw)
	case strings.HasPrefix(lower, "trojan://"):
		return parseTrojan(raw)
	case strings.HasPrefix(lower, "ss://"):
		return parseSS(raw)
	case strings.HasPrefix(lower, "hysteria2://"), strings.HasPrefix(lower, "hy2://"):
		return parseHysteria2(raw)
	case strings.HasPrefix(lower, "tuic://"):
		return parseTUIC(raw)
	case strings.HasPrefix(lower, "anytls://"):
		return parseAnyTLS(raw)
	case strings.HasPrefix(lower, "socks5://"), strings.HasPrefix(lower, "socks://"):
		return parseSocks(raw)
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return parseHTTP(raw)
	case strings.HasPrefix(lower, "ssr://"):
		return config.Node{}, fmt.Errorf("不支持 ShadowsocksR 协议")
	case strings.HasPrefix(lower, "hysteria://"):
		return config.Node{}, fmt.Errorf("不支持 Hysteria v1(请使用 hysteria2)")
	}
	return config.Node{}, fmt.Errorf("无法识别的链接")
}

func parseVMess(raw string) (config.Node, error) {
	var n config.Node
	payload := strings.TrimPrefix(raw, "vmess://")
	// vmess 有两种:base64 JSON(主流)与标准 URI
	if decoded, ok := tryBase64(payload); ok && strings.HasPrefix(strings.TrimSpace(decoded), "{") {
		var v struct {
			PS   string `json:"ps"`
			Add  string `json:"add"`
			Port any    `json:"port"`
			ID   string `json:"id"`
			Aid  any    `json:"aid"`
			Net  string `json:"net"`
			Host string `json:"host"`
			Path string `json:"path"`
			TLS  string `json:"tls"`
			SNI  string `json:"sni"`
			ALPN string `json:"alpn"`
			Type string `json:"type"`
			FP   string `json:"fp"`
			Scy  string `json:"scy"`
		}
		if err := json.Unmarshal([]byte(decoded), &v); err != nil {
			return n, fmt.Errorf("vmess JSON 解析失败: %w", err)
		}
		n = config.Node{
			Name:   strings.TrimSpace(v.PS),
			Type:   "vmess",
			Server: v.Add,
			Port:   anyInt(v.Port),
			UUID:   v.ID,
			AlterID: anyInt(v.Aid),
			Security: v.Scy,
			Network: v.Net,
			Raw:    raw,
		}
		if strings.EqualFold(v.TLS, "tls") || strings.EqualFold(v.TLS, "reality") {
			n.TLS = &config.TLSConfig{Enabled: true, ServerName: v.SNI, ALPN: splitComma(v.ALPN)}
			if n.TLS.ServerName == "" {
				n.TLS.ServerName = v.Host
			}
			if v.FP != "" {
				n.TLS.UTLSFingerprint = v.FP
			}
		}
		n.Transport = transportFromFields(v.Net, v.Path, v.Host, v.Type)
		if n.Security == "" {
			n.Security = "auto"
		}
		return n, nil
	}
	// 标准 URI: vmess://uuid@host:port?....
	u, err := url.Parse(raw)
	if err != nil {
		return n, err
	}
	n = config.Node{Type: "vmess", Server: u.Hostname(), Port: portOf(u), Network: "tcp", Raw: raw}
	if u.User != nil {
		n.UUID = u.User.Username()
	}
	q := u.Query()
	if v := q.Get("alterId"); v != "" {
		n.AlterID, _ = strconv.Atoi(v)
	}
	n.Security = firstNonEmpty(q.Get("encryption"), "auto")
	if q.Get("security") == "tls" || q.Get("tls") == "tls" {
		n.TLS = &config.TLSConfig{Enabled: true, ServerName: q.Get("sni")}
	}
	n.Transport = transportFromFields(q.Get("type"), q.Get("path"), q.Get("host"), q.Get("mode"))
	n.Name = u.Fragment
	return n, nil
}

func parseVLESS(raw string) (config.Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return config.Node{}, err
	}
	q := u.Query()
	n := config.Node{
		Type: "vless", Server: u.Hostname(), Port: portOf(u),
		Network: firstNonEmpty(q.Get("type"), "tcp"), Raw: raw,
		Flow: q.Get("flow"),
	}
	if u.User != nil {
		n.UUID = u.User.Username()
	}
	security := q.Get("security")
	if security == "tls" || security == "reality" {
		n.TLS = &config.TLSConfig{
			Enabled:    true,
			ServerName: q.Get("sni"),
			ALPN:       splitComma(q.Get("alpn")),
			UTLSFingerprint: q.Get("fp"),
		}
		if n.TLS.ServerName == "" {
			n.TLS.ServerName = q.Get("host")
		}
		if security == "reality" {
			n.TLS.RealityPublicKey = q.Get("pbk")
			n.TLS.RealityShortID = q.Get("sid")
			if n.TLS.UTLSFingerprint == "" {
				n.TLS.UTLSFingerprint = "chrome"
			}
		}
	}
	n.Transport = transportFromFields(n.Network, q.Get("path"), q.Get("host"), q.Get("serviceName"))
	n.Name = u.Fragment
	if strings.EqualFold(q.Get("allowInsecure"), "1") || q.Get("insecure") == "1" {
		if n.TLS != nil {
			n.TLS.Insecure = true
		}
	}
	return n, nil
}

func parseTrojan(raw string) (config.Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return config.Node{}, err
	}
	q := u.Query()
	n := config.Node{
		Type: "trojan", Server: u.Hostname(), Port: portOf(u),
		Network: firstNonEmpty(q.Get("type"), "tcp"), Raw: raw,
	}
	if u.User != nil {
		n.Password = u.User.Username()
	}
	n.TLS = &config.TLSConfig{
		Enabled:    true,
		ServerName: firstNonEmpty(q.Get("sni"), q.Get("peer")),
		ALPN:       splitComma(q.Get("alpn")),
		Insecure:   q.Get("allowInsecure") == "1",
	}
	if n.TLS.ServerName == "" {
		n.TLS.ServerName = q.Get("host")
	}
	n.Transport = transportFromFields(n.Network, q.Get("path"), q.Get("host"), q.Get("serviceName"))
	n.Name = u.Fragment
	return n, nil
}

func parseSS(raw string) (config.Node, error) {
	body := strings.TrimPrefix(raw, "ss://")
	// 去掉 fragment 前先记下名称
	name := ""
	if i := strings.Index(body, "#"); i >= 0 {
		if decoded, err := url.PathUnescape(body[i+1:]); err == nil {
			name = decoded
		} else {
			name = body[i+1:]
		}
		body = body[:i]
	}
	// 两种格式:
	//   ss://base64(method:password)@host:port?plugin=...
	//   ss://base64(method:password@host:port)
	query := ""
	if i := strings.Index(body, "?"); i >= 0 {
		query = body[i+1:]
		body = body[:i]
	}
	var method, password, hostport string
	if i := strings.LastIndex(body, "@"); i >= 0 {
		userinfo := body[:i]
		hostport = body[i+1:]
		decoded, ok := tryBase64(userinfo)
		if !ok {
			decoded, _ = url.QueryUnescape(userinfo)
		}
		if j := strings.Index(decoded, ":"); j >= 0 {
			method, password = decoded[:j], decoded[j+1:]
		} else {
			return config.Node{}, fmt.Errorf("ss 用户信息缺少加密方式")
		}
	} else {
		decoded, ok := tryBase64(body)
		if !ok {
			return config.Node{}, fmt.Errorf("ss 链接无法解码")
		}
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return config.Node{}, fmt.Errorf("ss 链接格式错误")
		}
		userinfo := decoded[:at]
		hostport = decoded[at+1:]
		if j := strings.Index(userinfo, ":"); j >= 0 {
			method, password = userinfo[:j], userinfo[j+1:]
		} else {
			return config.Node{}, fmt.Errorf("ss 用户信息缺少加密方式")
		}
	}
	host, port := hostPort(hostport)
	if host == "" || port == 0 {
		return config.Node{}, fmt.Errorf("ss 服务器地址无效")
	}
	n := config.Node{
		Type: "shadowsocks", Server: host, Port: port,
		Method: method, Password: password, Name: name, Raw: raw,
	}
	if query != "" {
		q, err := url.ParseQuery(query)
		if err == nil {
			if plugin := q.Get("plugin"); plugin != "" {
				parts := strings.Split(plugin, ";")
				n.Plugin = parts[0]
				opts := map[string]string{}
				for _, p := range parts[1:] {
					if kv := strings.SplitN(p, "=", 2); len(kv) == 2 {
						opts[kv[0]] = kv[1]
					} else if p != "" {
						opts[p] = "true"
					}
				}
				n.PluginOpts = opts
			}
		}
	}
	return n, nil
}

func parseHysteria2(raw string) (config.Node, error) {
	// hysteria2://password@host:port?sni=..&obfs=salamander&obfs-password=..&insecure=1
	normalized := strings.Replace(raw, "hy2://", "hysteria2://", 1)
	u, err := url.Parse(normalized)
	if err != nil {
		return config.Node{}, err
	}
	q := u.Query()
	n := config.Node{
		Type: "hysteria2", Server: u.Hostname(), Port: portOf(u), Raw: raw,
	}
	if u.User != nil {
		n.Password = u.User.Username()
		if pw, ok := u.User.Password(); ok && pw != "" {
			n.Password = pw
		}
	}
	n.TLS = &config.TLSConfig{
		Enabled:    true,
		ServerName: q.Get("sni"),
		Insecure:   q.Get("insecure") == "1",
	}
	if n.TLS.ServerName == "" {
		n.TLS.ServerName = u.Hostname()
	}
	if obfs := q.Get("obfs"); obfs != "" {
		n.Obfs = obfs
		n.ObfsPassword = firstNonEmpty(q.Get("obfs-password"), q.Get("obfs_password"))
	}
	n.HopPorts = q.Get("mport")
	if n.HopPorts != "" {
		if iv := q.Get("mport-interval"); iv != "" {
			n.HopInterval, _ = strconv.Atoi(iv)
		}
	}
	n.Name = u.Fragment
	return n, nil
}

func parseTUIC(raw string) (config.Node, error) {
	// tuic://uuid:password@host:port?sni=..&congestion_control=bbr&alpn=h3
	u, err := url.Parse(raw)
	if err != nil {
		return config.Node{}, err
	}
	q := u.Query()
	n := config.Node{
		Type: "tuic", Server: u.Hostname(), Port: portOf(u), Raw: raw,
		CongestionControl: firstNonEmpty(q.Get("congestion_control"), "bbr"),
	}
	if u.User != nil {
		n.UUID = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			n.Password = pw
		}
	}
	n.TLS = &config.TLSConfig{
		Enabled:    true,
		ServerName: q.Get("sni"),
		Insecure:   q.Get("allow_insecure") == "1" || q.Get("insecure") == "1",
		ALPN:       splitComma(q.Get("alpn")),
	}
	if n.TLS.ServerName == "" {
		n.TLS.ServerName = u.Hostname()
	}
	n.Name = u.Fragment
	return n, nil
}

func parseAnyTLS(raw string) (config.Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return config.Node{}, err
	}
	q := u.Query()
	n := config.Node{Type: "anytls", Server: u.Hostname(), Port: portOf(u), Raw: raw}
	if u.User != nil {
		n.Password = u.User.Username()
	}
	n.TLS = &config.TLSConfig{
		Enabled:    true,
		ServerName: q.Get("sni"),
		Insecure:   q.Get("insecure") == "1",
	}
	if n.TLS.ServerName == "" {
		n.TLS.ServerName = u.Hostname()
	}
	n.Name = u.Fragment
	return n, nil
}

func parseSocks(raw string) (config.Node, error) {
	u, err := url.Parse(strings.Replace(raw, "socks://", "socks5://", 1))
	if err != nil {
		return config.Node{}, err
	}
	n := config.Node{Type: "socks", Server: u.Hostname(), Port: portOf(u), Raw: raw}
	if u.User != nil {
		n.Username = u.User.Username()
		n.Password, _ = u.User.Password()
	}
	n.Name = u.Fragment
	return n, nil
}

func parseHTTP(raw string) (config.Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return config.Node{}, err
	}
	n := config.Node{Type: "http", Server: u.Hostname(), Port: portOf(u), Raw: raw}
	if u.User != nil {
		n.Username = u.User.Username()
		n.Password, _ = u.User.Password()
	}
	if u.Scheme == "https" {
		n.TLS = &config.TLSConfig{Enabled: true, ServerName: u.Hostname()}
	}
	n.Name = u.Fragment
	return n, nil
}

// ---------------------------------------------------------------------------
// Clash YAML
// ---------------------------------------------------------------------------

// parseClashYAML 只实现够用的小 YAML 解析器:逐行扫描 proxies 段,
// 按缩进切分列表项,处理引号与常见标量。不引第三方 YAML 库,保持单二进制。
func parseClashYAML(text, subID, subName string) (*Result, error) {
	res := &Result{}
	lines := strings.Split(text, "\n")

	inProxies := false
	proxiesIndent := -1
	var cur map[string]string
	curIndent := -1
	// 嵌套字段栈:把 ws-opts: / path: /vm 展开成 "ws-opts.path"="/vm",
	// 这样 clashProxyToNode 只查扁平键即可。
	type nest struct{ indent int; key string }
	var nests []nest
	flush := func() {
		if cur != nil {
			name := cur["name"]
			if n, err := clashProxyToNode(cur); err == nil {
				finalize(&n, subID, subName)
				res.Nodes = append(res.Nodes, n)
			} else {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %v", name, err))
			}
		}
		cur = nil
	}
	for _, rawLine := range lines {
		line := stripYAMLComment(rawLine)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)

		if !inProxies {
			if trimmed == "proxies:" || strings.HasPrefix(trimmed, "proxies: ") {
				inProxies = true
				proxiesIndent = indent
			}
			continue
		}

		isListItem := strings.HasPrefix(trimmed, "- ") || trimmed == "-"
		// 离开 proxies 段的前提:同级或更浅、且不是列表项。
		// proxies 列表项的缩进一定比 proxiesIndent 深(标准 YAML),
		// 所以这一判断不会误伤 - name: ... 这样的合法列表项。
		if !isListItem && indent <= proxiesIndent {
			flush()
			inProxies = false
			continue
		}

		if isListItem {
			if indent <= proxiesIndent {
				// 与 proxies: 同级的列表项属于别的段(例如 proxy-groups 的列表),
				// 处理到这里说明 YAML 结构异常,直接结束 proxies 段。
				flush()
				inProxies = false
				continue
			}
			flush()
			curIndent = indent
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			// 流式映射写法(整条节点在一行大括号里)是机场最常见的格式,
			// 必须优先识别,否则会被当成"纯字符串列表项"跳过。
			if fm, ok := parseFlowMapping(rest); ok {
				cur = fm
				nests = nil
				continue
			}
			kv, vval, ok := "", "", false
			if rest != "" {
				kv, vval, ok = splitKV(rest)
			}
			if !ok {
				// 形如 "- HK 01" 这种纯字符串列表项,属于别的段(比如 proxy-groups
				// 的 proxies 引用列表),不是 proxy 定义,不能建空条目。
				continue
			}
			cur = map[string]string{kv: vval}
			nests = nil
			continue
		}
		if cur != nil && indent > curIndent {
			k, v, ok := splitKV(trimmed)
			if !ok {
				continue
			}
			// 弹掉比当前行更深的嵌套层级
			for len(nests) > 0 && nests[len(nests)-1].indent >= indent {
				nests = nests[:len(nests)-1]
			}
			if v == "" {
				// 这是一个父键(如 ws-opts:),入栈等待子键
				nests = append(nests, nest{indent: indent, key: k})
				continue
			}
			flat := k
			if len(nests) > 0 {
				parts := make([]string, 0, len(nests)+1)
				for _, n := range nests {
					parts = append(parts, n.key)
				}
				parts = append(parts, k)
				flat = strings.Join(parts, ".")
			}
			cur[flat] = v
			continue
		}
		// 其它结构(如 proxy-groups 的嵌套)一律忽略,不影响已收集的字段
	}
	flush()
	if len(res.Nodes) == 0 && len(res.Skipped) == 0 {
		return nil, fmt.Errorf("未从订阅中找到任何节点")
	}
	return res, nil
}

func stripYAMLComment(line string) string {
	inS, inD := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
			// 行内的 #(如名称 "A#1")保留原样

		}
	}
	return line
}

func splitKV(s string) (string, string, bool) {
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(s[:i])
	v := strings.TrimSpace(s[i+1:])
	v = strings.Trim(v, `"'`)
	return k, v, true
}

// parseFlowMapping 解析 YAML 行内流式映射:
//
//	{name: JP, server: 1.2.3.4, port: 443, network: ws,
//	 ws-opts: {path: "/x", headers: {Host: h}}}
//
// 关键点(踩过的坑):
//   - 值里可能含逗号(如 "a,b"),引号内的逗号不能当分隔符
//   - 可能嵌套花括号(ws-opts / headers / reality-opts),
//     必须按括号深度切分,不能简单 strings.Split(",")
//   - 嵌套子映射要展开成扁平键 "ws-opts.path",与块格式保持一致的键空间
func parseFlowMapping(s string) (map[string]string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, false
	}
	inner := s[1 : len(s)-1]
	out := map[string]string{}
	for _, kv := range splitFlow(inner) {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		i := indexTopLevelColon(kv)
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(kv[:i])
		val := strings.TrimSpace(kv[i+1:])
		key = strings.Trim(key, `"'`)
		if strings.HasPrefix(val, "{") {
			// 嵌套映射:递归展开成 parent.child
			if sub, ok := parseFlowMapping(val); ok {
				for k, v := range sub {
					out[key+"."+k] = v
				}
				continue
			}
		}
		out[key] = strings.Trim(val, `"'`)
	}
	return out, true
}

// splitFlow 按顶层逗号切分(忽略引号内与花括号/方括号内的逗号)。
func splitFlow(s string) []string {
	var parts []string
	depth := 0
	inS, inD := false, false
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && (inS || inD) && i+1 < len(s):
			buf.WriteByte(c)
			buf.WriteByte(s[i+1])
			i++
			continue
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case (c == '{' || c == '[') && !inS && !inD:
			depth++
		case (c == '}' || c == ']') && !inS && !inD:
			depth--
		case c == ',' && depth == 0 && !inS && !inD:
			parts = append(parts, buf.String())
			buf.Reset()
			continue
		}
		buf.WriteByte(c)
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

// indexTopLevelColon 找出不在引号/括号内的第一个冒号位置。
// "http://x" 里的冒号不能算分隔符,所以要求冒号后不是 //。
func indexTopLevelColon(s string) int {
	depth := 0
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case (c == '{' || c == '[') && !inS && !inD:
			depth++
		case (c == '}' || c == ']') && !inS && !inD:
			depth--
		case c == ':' && depth == 0 && !inS && !inD:
			return i
		}
	}
	return -1
}

// clashProxyToNode 把一条 Clash proxy 映射成节点。
// Clash 的字段名与 sing-box 差异较大,这里逐协议翻译。
func clashProxyToNode(fields map[string]string) (config.Node, error) {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := fields[k]; ok && v != "" {
				return v
			}
		}
		return ""
	}
	typ := strings.ToLower(get("type"))
	switch typ {
	case "ss", "shadowsocks", "vmess", "vless", "trojan", "hysteria2", "hy2",
		"tuic", "socks5", "socks", "http", "anytls":
		// 支持的协议,继续
	default:
		return config.Node{}, fmt.Errorf("不支持的协议类型 %q", typ)
	}
	_ = typ
	n := config.Node{
		Name:   get("name"),
		Server: get("server"),
		Port:   atoiSafe(get("port")),
	}
	if n.Server == "" || n.Port == 0 {
		return n, fmt.Errorf("缺少服务器或端口")
	}
	switch typ {
	case "ss", "shadowsocks":
		n.Type = "shadowsocks"
		n.Method = get("cipher", "method")
		n.Password = get("password")
		switch strings.ToLower(get("plugin")) {
		case "obfs", "simple-obfs":
			n.Plugin = "obfs-local"
			n.PluginOpts = map[string]string{}
			if m := get("plugin-opts.mode", "plugin-opts-mode"); m != "" {
				n.PluginOpts["obfs"] = m
			}
			if h := get("plugin-opts.host", "plugin-opts-host"); h != "" {
				n.PluginOpts["obfs-host"] = h
			}
		case "v2ray-plugin":
			n.Plugin = "v2ray-plugin"
			n.PluginOpts = map[string]string{}
			if m := get("plugin-opts.mode"); m != "" {
				n.PluginOpts["mode"] = m
			}
			if p := get("plugin-opts.path"); p != "" {
				n.PluginOpts["path"] = p
			}
		}
	case "vmess":
		n.Type = "vmess"
		n.UUID = get("uuid")
		n.AlterID = atoiSafe(get("alterId"))
		n.Security = firstNonEmpty(get("cipher"), "auto")
		n.Network = firstNonEmpty(get("network"), "tcp")
		n.Transport = clashTransport(fields, n.Network)
		if strings.ToLower(get("tls")) == "true" {
			n.TLS = &config.TLSConfig{
				Enabled:    true,
				ServerName: get("servername", "sni"),
				Insecure:   strings.ToLower(get("skip-cert-verify")) == "true",
				ALPN:       splitComma(get("alpn")),
			}
		}
		if fp := get("client-fingerprint"); fp != "" && n.TLS != nil {
			n.TLS.UTLSFingerprint = fp
		}
	case "vless":
		n.Type = "vless"
		n.UUID = get("uuid")
		n.Flow = get("flow")
		n.Network = firstNonEmpty(get("network"), "tcp")
		n.Transport = clashTransport(fields, n.Network)
		reality := strings.ToLower(get("reality-opts.public-key", "reality-opts-public-key"))
		if get("tls") == "true" || reality != "" {
			n.TLS = &config.TLSConfig{
				Enabled:         true,
				ServerName:      get("servername", "sni"),
				Insecure:        strings.ToLower(get("skip-cert-verify")) == "true",
				ALPN:            splitComma(get("alpn")),
				UTLSFingerprint: get("client-fingerprint"),
				RealityPublicKey: reality,
				RealityShortID:  get("reality-opts.short-id", "reality-opts-short-id"),
			}
		}
	case "trojan":
		n.Type = "trojan"
		n.Password = get("password")
		n.Network = firstNonEmpty(get("network"), "tcp")
		n.Transport = clashTransport(fields, n.Network)
		n.TLS = &config.TLSConfig{
			Enabled:    true,
			ServerName: get("sni", "servername"),
			Insecure:   strings.ToLower(get("skip-cert-verify")) == "true",
			ALPN:       splitComma(get("alpn")),
		}
	case "hysteria2", "hy2":
		n.Type = "hysteria2"
		n.Password = firstNonEmpty(get("password"), get("auth"))
		n.TLS = &config.TLSConfig{
			Enabled:    true,
			ServerName: firstNonEmpty(get("sni"), get("servername"), n.Server),
			Insecure:   strings.ToLower(get("skip-cert-verify")) == "true",
		}
		if o := get("obfs"); o != "" {
			n.Obfs = o
			n.ObfsPassword = get("obfs-password")
		}
		n.UpMbps = atoiSafe(get("up"))
		n.DownMbps = atoiSafe(get("down"))
		n.HopPorts = get("ports")
	case "tuic":
		n.Type = "tuic"
		n.UUID = get("uuid")
		n.Password = get("password")
		n.TLS = &config.TLSConfig{
			Enabled:    true,
			ServerName: firstNonEmpty(get("sni"), get("servername"), n.Server),
			Insecure:   strings.ToLower(get("skip-cert-verify")) == "true",
			ALPN:       splitComma(firstNonEmpty(get("alpn"), "h3")),
		}
		n.CongestionControl = get("congestion-controller")
	case "socks5", "socks":
		n.Type = "socks"
		n.Username = get("username")
		n.Password = get("password")
	case "http":
		n.Type = "http"
		n.Username = get("username")
		n.Password = get("password")
		if strings.ToLower(get("tls")) == "true" {
			n.TLS = &config.TLSConfig{Enabled: true, ServerName: n.Server}
		}
	case "anytls":
		n.Type = "anytls"
		n.Password = get("password")
		n.TLS = &config.TLSConfig{
			Enabled:    true,
			ServerName: firstNonEmpty(get("sni"), n.Server),
			Insecure:   strings.ToLower(get("skip-cert-verify")) == "true",
		}
	}
	return n, nil
}

func clashTransport(fields map[string]string, network string) *config.TransportConfig {
	switch strings.ToLower(network) {
	case "ws":
		t := &config.TransportConfig{Type: "ws", Path: firstNonEmpty(fields["ws-opts.path"], fields["ws-path"], "/")}
		t.Host = firstNonEmpty(fields["ws-opts.headers.Host"], fields["ws-opts.headers.host"], fields["ws-host"])
		return t
	case "grpc":
		return &config.TransportConfig{Type: "grpc", ServiceName: firstNonEmpty(fields["grpc-opts.grpc-service-name"], fields["grpc-service-name"])}
	case "h2", "http":
		return &config.TransportConfig{Type: "http", Path: fields["h2-opts.path"], Host: fields["h2-opts.host"]}
	case "httpupgrade":
		return &config.TransportConfig{Type: "httpupgrade", Path: fields["http-opts.path"], Host: fields["http-opts.host"]}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

func portOf(u *url.URL) int {
	if p := u.Port(); p != "" {
		return atoiSafe(p)
	}
	switch u.Scheme {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}

func hostPort(hp string) (string, int) {
	if strings.HasPrefix(hp, "[") {
		if i := strings.Index(hp, "]"); i > 0 {
			host := hp[1:i]
			rest := strings.TrimPrefix(hp[i+1:], ":")
			return host, atoiSafe(rest)
		}
	}
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		return hp, 0
	}
	return hp[:i], atoiSafe(hp[i+1:])
}

func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	n := 0
	if s == "" {
		return 0
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func anyInt(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		return atoiSafe(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

func splitComma(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func transportFromFields(network, path, host, serviceName string) *config.TransportConfig {
	switch strings.ToLower(network) {
	case "ws":
		return &config.TransportConfig{Type: "ws", Path: firstNonEmpty(path, "/"), Host: host}
	case "grpc":
		return &config.TransportConfig{Type: "grpc", ServiceName: serviceName}
	case "http", "h2":
		return &config.TransportConfig{Type: "http", Path: path, Host: host}
	case "httpupgrade":
		return &config.TransportConfig{Type: "httpupgrade", Path: path, Host: host}
	}
	return nil
}

// Dedup 按 服务器+端口+协议+凭证 去重,保留先出现的(通常是更靠前的订阅)。
func Dedup(nodes []config.Node) []config.Node {
	seen := map[string]bool{}
	out := make([]config.Node, 0, len(nodes))
	for _, n := range nodes {
		key := fmt.Sprintf("%s|%s|%d|%s|%s", n.Type, n.Server, n.Port, n.UUID, n.Password)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// MatchFilter 按关键词过滤订阅节点(机场常用"只要 xx 地区"的用法)。
func MatchFilter(nodes []config.Node, keyword string) []config.Node {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nodes
	}
	lower := strings.ToLower(keyword)
	out := make([]config.Node, 0, len(nodes))
	for _, n := range nodes {
		if strings.Contains(strings.ToLower(n.Name), lower) {
			out = append(out, n)
		}
	}
	return out
}

// SortByName 稳定排序,便于面板显示与测试断言。
func SortByName(nodes []config.Node) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
}

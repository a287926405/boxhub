package api

// 仪表盘两项统计(对齐 Open-Box 面板概览):
//
//  1. 常用站点测速:百度/Google/OpenAI/Telegram 经当前代理链路的延时。
//     不依赖 Open-Box —— 用 BoxHub 自己的内核测速接口。
//
//  2. 流量统计:按「出站节点 / 终端设备 / 访问目标」聚合上传下载。
//     sing-box 的 /connections 只反映**当前活跃**连接,连接一关闭就从列表消失,
//     直接展示会严重少算。这里按轮询采样累计:每次快照把新增字节数累加进
//     各维度的累计账本,这样页面刷新/隔了很久再看,数字仍然接近真实总量。
//     (采样间隔内完成的极短连接可能来不及计入,对应 Open-Box 表格里
//     "未采样到的短连接"一行,这里用 reset 按钮对账。)

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"boxhub/internal/config"
)

// ---------------------------------------------------------------------------
// 1) 测速
// ---------------------------------------------------------------------------

// speedSample 一次测速样本。
type speedSample struct {
	At      int64  `json:"at"` // Unix 秒
	DelayMs int    `json:"delay_ms"`
	Chain   string `json:"chain,omitempty"` // 当时走过的线路(内核 chains,已可读名)
	Fail    bool   `json:"fail,omitempty"`
}

// speedHistory 每个测速目标保留最近 N 条样本(服务内存活,面板重启不丢)。
const speedHistoryKeep = 60

var (
	speedHistMu sync.Mutex
	speedHistory = map[string][]speedSample{}
)

func recordSpeed(name string, at time.Time, delayMs int, chain string) {
	speedHistMu.Lock()
	defer speedHistMu.Unlock()
	h := append(speedHistory[name], speedSample{At: at.Unix(), DelayMs: delayMs, Chain: chain, Fail: delayMs <= 0})
	if len(h) > speedHistoryKeep {
		h = h[len(h)-speedHistoryKeep:]
	}
	speedHistory[name] = h
}

// SpeedHistoryFor 返回某目标的样本副本(导出给测试与快照用)。
func SpeedHistoryFor(name string) []speedSample {
	speedHistMu.Lock()
	defer speedHistMu.Unlock()
	return append([]speedSample(nil), speedHistory[name]...)
}


// SpeedTestTarget 一个测速项。
type SpeedTestTarget struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	DelayMs   int    `json:"delay_ms"` // -1 失败
	Chain     string `json:"chain,omitempty"`
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

var speedTestTargets = []struct{ name, url string }{
	{"百度", "https://www.baidu.com"},
	{"Google", "https://www.google.com/generate_204"},
	{"OpenAI", "https://api.openai.com/v1/models"},
	{"Telegram", "https://t.me/telegram"},
}

// handleSpeedtest 并发测四个站点。走哪个出口由内核路由决定
// (即用户当前的线路选择),所以测出来就是"现在这条路能不能到、多久到"。
func (s *Server) handleSpeedtest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if r.URL.Query().Get("action") == "history" {
		speedHistMu.Lock()
		snap := map[string][]speedSample{}
		for k, v := range speedHistory {
			snap[k] = append([]speedSample(nil), v...)
		}
		speedHistMu.Unlock()
		writeOK(w, map[string]any{"history": snap})
		return
	}
	if r.URL.Query().Get("action") == "list" {
		type tv struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		out := make([]tv, 0, len(speedTestTargets))
		for _, t := range speedTestTargets {
			out = append(out, tv{t.name, t.url})
		}
		writeOK(w, map[string]any{"targets": out})
		return
	}
	// ?url=xx 单测一个站点:仪表盘打开时四项并行发起,
	// 谁先返回谁先上屏,不用等最慢的(最长 8s)。
	testURL := r.URL.Query().Get("url")
	targets := speedTestTargets
	if testURL != "" {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "custom"
		}
		targets = []struct{ name, url string }{{name, testURL}}
	}
	type result struct {
		idx int
		t   SpeedTestTarget
	}
	now := time.Now().Format(time.RFC3339)
	ch := make(chan result, len(targets))
	// 混合端口同时支持 HTTP 代理协议:测 https 站点要用 Transport.Proxy
	// 走 CONNECT 隧道。之前误把代理地址当 TCP 直连目标 Dial,
	// TLS 握手发给 7890 必然失败(表现为测速四项全部超时)。
	proxyStr := s.localProxyAddr()
	proxyURL, perr := neturl.Parse("http://" + proxyStr)
	if perr != nil {
		writeErr(w, 500, "代理地址无效: "+proxyStr)
		return
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		go func(i int, name, u string) {
			defer wg.Done()
			res := SpeedTestTarget{Name: name, URL: u, DelayMs: -1, UpdatedAt: now}
			host := urlHost(u)
			// 线路捕获:/connections 只含**当前活跃**连接,测速连接几十~几百毫秒
			// 就关闭,请求期间必须轮询(120ms)才可能抓到它的 chains。
			// 抓不到不算错(极快连接可能整个窗口都没样本),前端显示"线路未捕获"。
			captured := make(chan string, 1)
			stopCap := make(chan struct{})
			go func() {
				last := ""
				tick := time.NewTicker(120 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stopCap:
						captured <- last
						return
					case <-tick.C:
						cs, cerr := s.clash().Connections(ctx)
						if cerr != nil {
							continue
						}
						for _, c := range cs {
							if c.Meta.Host == host && len(c.Chains) > 0 {
								last = strings.Join(c.Chains, " → ")
							}
						}
					}
				}
			}()
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err == nil {
				req.Header.Set("User-Agent", "BoxHub/1.0 (+speedtest)")
				var resp *http.Response
				resp, err = client.Do(req)
				if err == nil {
					// 拿到响应即认为可达(401/403 也算:说明链路通,只是没权限)
					resp.Body.Close()
					res.DelayMs = int(time.Since(start).Milliseconds())
				}
			}
			if err != nil {
				res.Error = shortErr(err)
			}
			close(stopCap)
			res.Chain = <-captured
			recordSpeed(name, time.Now(), res.DelayMs, res.Chain)
			ch <- result{i, res}
		}(i, tg.name, tg.url)
	}
	wg.Wait()
	close(ch)
	out := make([]SpeedTestTarget, len(targets))
	for res := range ch {
		out[res.idx] = res.t
	}
	writeOK(w, map[string]any{"results": out, "proxy": s.localProxyAddr()})
}

// urlHost 取测速地址的主机名(用于匹配连接的目标)。
func urlHost(u string) string {
	pu, err := neturl.Parse(u)
	if err != nil {
		return ""
	}
	return pu.Hostname()
}

func shortErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 && len(msg) > 60 {
		msg = msg[:min(60, len(msg))]
	}
	return msg
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// 2) 流量统计(采样累计)
// ---------------------------------------------------------------------------

type trafficKey = string

type trafficEntry struct {
	Upload, Download int64
	LastSeen        time.Time
	// By 细分账本:出站节点按"访问目标"细分、终端设备/访问目标按"落地节点"细分。
	By map[string]*trafficSub
}

type trafficSub struct{ Up, Down int64 }

type trafficLedger struct {
	mu       sync.Mutex
	outbound map[trafficKey]*trafficEntry
	client   map[trafficKey]*trafficEntry
	target   map[trafficKey]*trafficEntry
	known    map[string]*connSample // 连接 id → 上次采到的字节数
	lastAt   time.Time
	// 本次快照的活跃连接(输出里单独返回,避免与累计重复计算观感)
	activeNow []map[string]any
}

type connSample struct {
	up, down int64
	key      string // 目标(展示用)
}

var ledger = &trafficLedger{
	outbound: map[trafficKey]*trafficEntry{},
	client:   map[trafficKey]*trafficEntry{},
	target:   map[trafficKey]*trafficEntry{},
	known:    map[string]*connSample{},
}

// applyTrafficReset 清零(面板"重新开始统计")。
func (l *trafficLedger) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outbound = map[trafficKey]*trafficEntry{}
	l.client = map[trafficKey]*trafficEntry{}
	l.target = map[trafficKey]*trafficEntry{}
	l.known = map[string]*connSample{}
	l.lastAt = time.Time{}
}

// sample 拉取一次 /connections 快照并入账本。
func (s *Server) sampleTraffic(ctx context.Context) error {
	raw, err := s.clash().RawGet(ctx, "/connections")
	if err != nil {
		return err
	}
	var data struct {
		Connections []struct {
			ID       string   `json:"id"`
			Upload   int64    `json:"upload"`
			Download int64    `json:"download"`
			Chains   []string `json:"chains"`
			Meta     struct {
				Host     string `json:"host"`
				DestIP   string `json:"destinationIP"`
				DestPort string `json:"destinationPort"`
				SourceIP string `json:"sourceIP"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	now := time.Now()
	// 组名集合:chains 里出现的 Proxy/筛选组/节点组要下钻到真正的落地节点
	cfg := s.store.Current()
	virtual := map[string]bool{"Proxy": true, "GLOBAL": true}
	// 筛选组、节点组、规则组的 tag 都在组名里
	if cfg != nil {
		for _, g := range cfg.Groups {
			virtual[g.Name] = true
		}
		for _, f := range cfg.Filters {
			if f.Enabled && !f.CountOnly {
				virtual[f.Name] = true
			}
		}
		for _, p := range cfg.Policies {
			if p.Enabled && p.AsGroup {
				virtual[p.Name] = true
			}
		}
	}
	// 当前各组的选择(clash 列表拿不到时退化为:chains 里最后一个非 virtual 元素)
	var groupNow map[string]string
	if proxies, perr := s.clash().Proxies(ctx); perr == nil {
		groupNow = map[string]string{}
		for name, info := range proxies {
			if info.Type == "Selector" || info.Type == "URLTest" || info.Type == "Fallback" || info.Type == "LoadBalance" {
				groupNow[name] = info.Now
			}
		}
	}

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range data.Connections {
		seen[c.ID] = true
		outKey := resolveOutbound(c.Chains, virtual, groupNow)
		srcKey := c.Meta.SourceIP
		if srcKey == "" {
			srcKey = "未知"
		}
		dstKey := c.Meta.Host
		if dstKey == "" {
			dstKey = c.Meta.DestIP
			if dstKey != "" && c.Meta.DestPort != "" {
				dstKey += ":" + c.Meta.DestPort
			}
		}
		if dstKey == "" {
			dstKey = "未知目标"
		}
		old := ledger.known[c.ID]
		if old == nil {
			old = &connSample{}
			ledger.known[c.ID] = old
		}
		du, dd := c.Upload-old.up, c.Download-old.down
		if du < 0 {
			du = 0
		}
		if dd < 0 {
			dd = 0
		}
		old.up, old.down = c.Upload, c.Download
		// 出站节点 → 按访问目标细分;终端设备/访问目标 → 按落地节点细分
		addTraffic(ledger.outbound, outKey, du, dd, now, dstKey)
		addTraffic(ledger.client, srcKey, du, dd, now, outKey)
		addTraffic(ledger.target, dstKey, du, dd, now, outKey)
	}
	// 清掉早已消失的连接记录(留着没意义,还会撑大内存)
	for id := range ledger.known {
		if !seen[id] {
			delete(ledger.known, id)
		}
	}
	ledger.lastAt = now
	return nil
}

// resolveOutbound 把 chains 下钻到真正落地节点。
// chains 从近到远排列([最后=最外层组]),最后一个元素往往是 Proxy 组,
// 需要查它的当前选择;选择若又是组(如筛选组)继续下钻。
func resolveOutbound(chains []string, virtual map[string]bool, groupNow map[string]string) string {
	if len(chains) == 0 {
		return "直连"
	}
	// 从最外层(数组末尾)开始解析
	name := chains[len(chains)-1]
	for depth := 0; depth < 5; depth++ {
		if !virtual[name] {
			return name
		}
		next := groupNow[name]
		if next == "" || next == name {
			break
		}
		name = next
	}
	// 下钻失败:往回找 chains 里第一个非 virtual
	for i := len(chains) - 1; i >= 0; i-- {
		if !virtual[chains[i]] {
			return chains[i]
		}
	}
	return name
}

func addTraffic(m map[trafficKey]*trafficEntry, key string, up, down int64, now time.Time, subKey string) {
	if up == 0 && down == 0 {
		return // 首采样/零增量不建条目,避免表格里一堆 0 B 占位行
	}
	e := m[key]
	if e == nil {
		e = &trafficEntry{By: map[string]*trafficSub{}}
		m[key] = e
	}
	if e.By == nil {
		e.By = map[string]*trafficSub{}
	}
	e.Upload += up
	e.Download += down
	e.LastSeen = now
	if subKey != "" {
		sub := e.By[subKey]
		if sub == nil {
			// 每个条目的细分上限:超过 64 项时淘汰流量最小的,防止内存无限增长
			if len(e.By) >= 64 {
				if k, least := weakest(e.By); least >= 0 && up+down >= least {
					delete(e.By, k)
				} else if k != "" {
					delete(e.By, k)
				}
			}
			sub = &trafficSub{}
			e.By[subKey] = sub
		}
		sub.Up += up
		sub.Down += down
	}
}

// weakest 返回总流量最小的细分项。
func weakest(m map[string]*trafficSub) (string, int64) {
	k, least := "", int64(-1)
	for name, s := range m {
		t := s.Up + s.Down
		if least < 0 || t < least {
			k, least = name, t
		}
	}
	return k, least
}

// trafficView 输出一个维度的排行榜。
func trafficView(m map[trafficKey]*trafficEntry, limit int) []map[string]any {
	type kv struct {
		k string
		e *trafficEntry
	}
	var all []kv
	var total int64
	for k, e := range m {
		all = append(all, kv{k, e})
		total += e.Upload + e.Download
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].e.Upload+all[i].e.Download > all[j].e.Upload+all[j].e.Download
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out := make([]map[string]any, 0, len(all))
	for _, x := range all {
		sum := x.e.Upload + x.e.Download
		pct := 0.0
		if total > 0 {
			pct = float64(sum) / float64(total) * 100
		}
		var by []map[string]any
		for _, b := range sortedSubs(x.e.By, 10) {
			bt := b.Up + b.Down
			bp := 0.0
			if sum > 0 {
				bp = float64(bt) / float64(sum) * 100
			}
			by = append(by, map[string]any{"name": b.name, "upload": b.Up, "download": b.Down, "total": bt, "percent": bp})
		}
		out = append(out, map[string]any{
			"name": x.k, "upload": x.e.Upload, "download": x.e.Download,
			"total": sum, "percent": pct, "by": by,
		})
	}
	return out
}

type namedSub struct {
	name      string
	Up, Down  int64
}

func sortedSubs(m map[string]*trafficSub, limit int) []namedSub {
	all := make([]namedSub, 0, len(m))
	for k, v := range m {
		all = append(all, namedSub{name: k, Up: v.Up, Down: v.Down})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Up+all[i].Down > all[j].Up+all[j].Down })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (l *trafficLedger) snapshot() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{
		"outbound": trafficView(l.outbound, 30),
		"clients":  trafficView(l.client, 30),
		"targets":  trafficView(l.target, 50),
		"since":    l.lastAt,
	}
}

// handleTraffic 仪表盘流量统计接口。?action=reset 清零重新计。
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("action") == "reset" {
		ledger.reset()
		writeOK(w, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	// 采样失败不致命:返回上一次账本即可(刚开机则全零)
	_ = s.sampleTraffic(ctx)
	writeOK(w, ledger.snapshot())
}

var _ = config.CurrentSchema

package api

// DNS 事件跟踪器。
//
// 为什么不直接在 handleDNSStats 里读日志尾部:面板每 5 秒刷新,
// 测速/流量采样也会持续产生 outbound 日志(实测两三兆),
// 而 DNS 行本身稀疏(FakeIP 生效时走代理的域名不产生 DNS 日志)。
// "读最后 N 行"的滑动窗口会被 outbound 行挤出 DNS 行,统计隔一会儿就"又没了"
// ——同一个 bug 已经复现两次(先是被启动时间窗口掐空,再是被日志滚动滑出)。
//
// 这里改成增量扫描:记住上次读到的文件偏移,每次只处理新增字节,
// 把 DNS 事件解析进内存环形缓冲(上限 5000 条,超出丢最旧)。
// 统计窗口 = "自 boxd 启动以来",语义稳定、开销 O(新增日志量)。
// 日志被 ClearLog 删除/重建(size 回退)时自动从头重扫。

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"
)

type dnsEvent struct {
	At time.Time
	OK bool // false = 查询失败
	NX bool // NXDOMAIN
	Ms float64
}

type dnsTracker struct {
	mu     sync.Mutex
	path   string
	off    int64
	events []dnsEvent
}

func newDNSTracker(path string) *dnsTracker {
	return &dnsTracker{path: path}
}

// poll 扫描自上次以来的新增日志。单次增量封顶 8MB(5 秒刷新足够),
// 超出则丢弃中间段,保证异常刷屏也不会拖死面板。
func (t *dnsTracker) poll() error {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			t.mu.Lock()
			t.off, t.events = 0, nil
			t.mu.Unlock()
			return nil // 还没产生日志不算错
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 文件被清空/重建:size 回退即从头扫
	if st.Size() < t.off {
		t.off, t.events = 0, nil
	}
	if st.Size() == t.off {
		return nil
	}
	const maxScan = 8 << 20
	if st.Size()-t.off > maxScan {
		target := st.Size() - maxScan
		if _, err := f.Seek(target, 0); err == nil {
			buf := make([]byte, 1024)
			if n2, err2 := f.Read(buf); err2 == nil {
				if idx := strings.IndexByte(string(buf[:n2]), '\n'); idx >= 0 {
					t.off = target + int64(idx) + 1
				} else {
					return nil
				}
			}
		}
	}
	if _, err := f.Seek(t.off, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		n += len(line) + 1
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "dns:") {
			continue
		}
		if ev, ok := parseDNSEvent(line, lower); ok {
			t.events = append(t.events, ev)
		}
	}
	t.off += int64(n)
	const capEvents = 5000
	if len(t.events) > capEvents {
		t.events = append([]dnsEvent(nil), t.events[len(t.events)-capEvents:]...)
	}
	return sc.Err()
}

// parseDNSEvent 识别两种形态:
//
//	INFO  dns: exchanged A www.example.com. 600 IN A 1.2.3.4
//	ERROR dns: lookup failed for xxx: (...)
//
// exchanged 与 lookup succeed 会同时出现,只数 exchanged 避免双计;
// 耗时从 "[id 123ms]" 片段提取,拿不到记 0(不计入均值)。
func parseDNSEvent(line, lower string) (dnsEvent, bool) {
	switch {
	case strings.Contains(lower, "lookup failed"):
		return dnsEvent{At: parseLogTimeOrNow(line), OK: false,
			NX: strings.Contains(lower, "nxdomain")}, true
	case strings.Contains(lower, "dns: exchanged"):
		ms, _ := parseMs(line)
		return dnsEvent{At: parseLogTimeOrNow(line), OK: true, Ms: ms}, true
	}
	return dnsEvent{}, false
}

func parseLogTimeOrNow(line string) time.Time {
	if ts, ok := parseLogTime(line); ok {
		return ts
	}
	return time.Now()
}

// snapshot 汇总统计。since 为最早一条事件时间。
func (t *dnsTracker) snapshot() (total, success, failed, nxd int, avgMs float64, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var durSum float64
	var durN int
	first := true
	for _, e := range t.events {
		total++
		if first || e.At.Before(since) {
			since = e.At
			first = false
		}
		if e.OK {
			success++
			if e.Ms > 0 {
				durSum += e.Ms
				durN++
			}
		} else {
			failed++
			if e.NX {
				nxd++
			}
		}
	}
	if durN > 0 {
		avgMs = durSum / float64(durN)
	}
	return
}

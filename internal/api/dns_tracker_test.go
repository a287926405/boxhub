package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDNSTrackerBasics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.log")
	now := time.Now().Format("2006-01-02 15:04:05")
	writeLog(t, path, strings.Join([]string{
		"+0800 " + now + " INFO dns: exchanged A www.google.com. 600 IN A 142.250.0.1",
		"+0800 " + now + " INFO dns: lookup succeed for www.google.com: 142.250.0.1", // 与上行重复,不应双计
		"+0800 " + now + " ERROR dns: lookup failed for xxx.io: (i/o timeout)",
		"+0800 " + now + " INFO outbound/vless[node:1]: outbound connection to a.com:443",
	}, "\n")+"\n")
	tr := newDNSTracker(path)
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	total, success, failed, _, _, since := tr.snapshot()
	if total != 2 || success != 1 || failed != 1 {
		t.Fatalf("统计不对: total=%d success=%d failed=%d", total, success, failed)
	}
	if since.IsZero() {
		t.Fatal("since 应有值")
	}
}

// 回归测试(踩过的坑):日志尾部被大量 outbound 行滚动时,
// 早期实现读"最后 N 行"会把稀疏的 DNS 行挤出窗口,统计隔会儿就归零。
// 增量扫描必须不受后续日志量影响。
func TestDNSTrackerSurvivesLogFlood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.log")
	now := time.Now().Format("2006-01-02 15:04:05")
	writeLog(t, path, "+0800 "+now+" INFO dns: exchanged A www.baidu.com. 60 IN A 1.1.1.1\n")
	tr := newDNSTracker(path)
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	// 灌入 5 万行无关日志
	var junk strings.Builder
	for i := 0; i < 50000; i++ {
		junk.WriteString("+0000 " + now + " INFO [123 45ms] outbound/vless[n]: outbound connection to flood.example:443\n")
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		f.WriteString(junk.String())
		f.Close()
	}
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	total, _, _, _, _, _ := tr.snapshot()
	if total != 1 {
		t.Fatalf("洪泛日志后统计应保留,实际 total=%d", total)
	}
}

// ClearLog/重启把日志清空(size 回退)时应从头重扫。
func TestDNSTrackerResetOnShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.log")
	now := time.Now().Format("2006-01-02 15:04:05")
	writeLog(t, path, "+0800 "+now+" INFO dns: exchanged A a.com. 60 IN A 1.1.1.1\n")
	tr := newDNSTracker(path)
	_ = tr.poll()
	writeLog(t, path, "+0800 "+now+" INFO dns: exchanged A b.com. 60 IN A 2.2.2.2\n")
	if err := tr.poll(); err != nil {
		t.Fatal(err)
	}
	total, _, _, _, _, _ := tr.snapshot()
	if total != 1 {
		t.Fatalf("清空后应只剩新的一条,实际 %d", total)
	}
}

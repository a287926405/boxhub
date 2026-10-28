package api

// 系统信息采集:对应 SMBox 仪表盘上的"设备信息 / 硬件信息 / Singbox"三张卡片。
//
// 全部从 /proc 与 /sys 读取,不依赖外部命令(容器里往往没装 sysstat/lshw),
// 也不引入 cgo。读不到的项目返回空值,前端显示 "—",不让页面报错。

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SysInfo 一次系统信息快照。
type SysInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel"`
	IP       string `json:"ip"`
	Uptime   int64  `json:"uptime_seconds"`

	CPUModel   string  `json:"cpu_model"`
	CPUCores   int     `json:"cpu_cores"`
	CPUPercent float64 `json:"cpu_percent"`

	MemTotal     uint64  `json:"mem_total"`
	MemAvailable uint64  `json:"mem_available"`
	MemUsed      uint64  `json:"mem_used"`
	MemPercent   float64 `json:"mem_percent"`

	DiskTotal   uint64  `json:"disk_total"`
	DiskUsed    uint64  `json:"disk_used"`
	DiskPercent float64 `json:"disk_percent"`

	LoadAvg [3]float64 `json:"load_avg"`
	// Container 是否跑在容器里(影响 TUN 可用性判断)。
	Container string `json:"container,omitempty"`
	// TunAvailable /dev/net/tun 是否可用。
	TunAvailable bool `json:"tun_available"`
}

// collectSysInfo 采集系统信息。
func collectSysInfo() SysInfo {
	si := SysInfo{
		OS:     runtime.GOOS,
		Arch:   runtime.GOARCH,
		Kernel: readKernel(),
	}
	si.Hostname, _ = os.Hostname()
	si.IP = localIP()
	si.Uptime = readUptime()
	si.CPUModel, si.CPUCores = readCPUInfo()
	si.MemTotal, si.MemAvailable = readMemInfo()
	if si.MemTotal > 0 {
		si.MemUsed = si.MemTotal - si.MemAvailable
		si.MemPercent = round1(float64(si.MemUsed) / float64(si.MemTotal) * 100)
	}
	si.DiskTotal, si.DiskUsed = readDisk("/")
	if si.DiskTotal > 0 {
		si.DiskPercent = round1(float64(si.DiskUsed) / float64(si.DiskTotal) * 100)
	}
	si.LoadAvg = readLoadAvg()
	si.Container = detectContainer()
	si.TunAvailable = tunAvailable()
	return si
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readKernel() string {
	if s := readFileTrim("/proc/sys/kernel/osrelease"); s != "" {
		return s
	}
	return runtime.GOOS
}

func readUptime() int64 {
	s := readFileTrim("/proc/uptime")
	if s == "" {
		return 0
	}
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

// readCPUInfo 读 /proc/cpuinfo:模型名与核心数。
func readCPUInfo() (string, int) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", runtime.NumCPU()
	}
	defer f.Close()
	var model string
	cores := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "processor") {
			cores++
		}
		if model == "" {
			// x86: "model name";ARM: "Hardware" 或 "model name"
			for _, key := range []string{"model name", "Hardware", "Processor"} {
				if strings.HasPrefix(line, key) {
					if i := strings.Index(line, ":"); i > 0 {
						if v := strings.TrimSpace(line[i+1:]); v != "" {
							model = v
						}
					}
				}
			}
		}
	}
	if cores == 0 {
		cores = runtime.NumCPU()
	}
	return model, cores
}

// readMemInfo 返回总内存与可用内存(字节)。
func readMemInfo() (total, available uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		val *= 1024 // kB -> B
		switch fields[0] {
		case "MemTotal:":
			total = val
		case "MemAvailable:":
			available = val
		}
	}
	return total, available
}

func readLoadAvg() [3]float64 {
	var out [3]float64
	s := readFileTrim("/proc/loadavg")
	if s == "" {
		return out
	}
	f := strings.Fields(s)
	for i := 0; i < 3 && i < len(f); i++ {
		out[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return out
}

func detectContainer() string {
	if s := readFileTrim("/run/systemd/container"); s != "" {
		return s
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if b, err := os.ReadFile("/proc/1/environ"); err == nil {
		if strings.Contains(string(b), "container=lxc") {
			return "lxc"
		}
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		if strings.Contains(s, "lxd") || strings.Contains(s, "incus") {
			return "lxc"
		}
		if strings.Contains(s, "docker") {
			return "docker"
		}
	}
	return ""
}

func tunAvailable() bool {
	fi, err := os.Stat("/dev/net/tun")
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeDevice != 0
}

// localIP 返回用于对外通信的网卡 IP。
func localIP() string {
	// 用一个 UDP "连接"探测出口网卡(不会真的发包)
	conn, err := netDialUDP("8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().String()[:strings.LastIndex(conn.LocalAddr().String(), ":")]
}

// cpuUsageSample 计算两次读数之间的 CPU 使用率。
// /proc/stat 第一行是总体 cpu 时间片,差值比即使用率。
type cpuSample struct {
	idle, total uint64
}

func readCPUSample() (cpuSample, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 {
		return cpuSample{}, false
	}
	var total, idle uint64
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		total += n
		if i == 3 { // idle
			idle = n
		}
	}
	return cpuSample{idle: idle, total: total}, true
}

// cpuPercent 采样两次 CPU 使用率,间隔 200ms。
func cpuPercent() float64 {
	a, ok1 := readCPUSample()
	if !ok1 {
		return 0
	}
	time.Sleep(200 * time.Millisecond)
	b, ok2 := readCPUSample()
	if !ok2 {
		return 0
	}
	if b.total <= a.total {
		return 0
	}
	dTotal := float64(b.total - a.total)
	dIdle := float64(b.idle - a.idle)
	return round1((dTotal - dIdle) / dTotal * 100)
}

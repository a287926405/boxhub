// Package kernel 管理 sing-box 内核进程。
//
// BoxHub 自己拉起内核(systemd 只管 boxd 一个进程),好处是:
//   - 面板能精确知道内核状态,并在配置变更时「先校验、再热切换」;
//   - 校验失败时内核继续用旧配置运行,不会把用户的网络弄断;
//   - 内核崩溃后能自动拉起并记录原因,面板上能看到日志。
package kernel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager 管理内核单例。
type Manager struct {
	binPath   string
	configPath string
	workDir   string
	logPath   string

	mu       sync.Mutex
	cmd      *exec.Cmd
	startedAt time.Time
	lastErr  string
	restarts int
	stopCh   chan struct{}
	stopping bool

	logMu    sync.Mutex
	logLines []string
	logFile  *os.File
}

// Options 构造参数。
type Options struct {
	BinPath    string
	ConfigPath string
	WorkDir    string
	LogPath    string
}

// New 创建管理器(不启动进程)。
func New(o Options) *Manager {
	return &Manager{
		binPath:    o.BinPath,
		configPath: o.ConfigPath,
		workDir:    o.WorkDir,
		logPath:    o.LogPath,
	}
}

// Status 内核状态快照。
type Status struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version"`
	LastError string    `json:"last_error"`
	Restarts  int       `json:"restarts"`
	Binary    string    `json:"binary"`
}

// Status 返回当前状态。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Binary:    m.binPath,
		LastError: m.lastErr,
		Restarts:  m.restarts,
		Version:   m.Version(),
	}
	if m.cmd != nil && m.cmd.Process != nil {
		st.Running = true
		st.PID = m.cmd.Process.Pid
		st.StartedAt = m.startedAt
	}
	return st
}

// Version 读取内核版本(缓存到包级变量,避免频繁 fork)。
// BinPath 返回当前内核二进制路径(升级替换用)。
func (m *Manager) BinPath() string { return m.binPath }

// SetVersion 升级后刷新缓存的版本显示。
func (m *Manager) SetVersion(v string) {
	versionMu.Lock()
	versionStr = v
	versionMu.Unlock()
}

func (m *Manager) Version() string {
	versionOnce.Do(func() {
		out, err := exec.Command(m.binPath, "version").CombinedOutput()
		if err != nil {
			versionMu.Lock()
			versionStr = ""
			versionMu.Unlock()
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "sing-box version") {
				versionMu.Lock()
				versionStr = strings.TrimSpace(strings.TrimPrefix(line, "sing-box version"))
				versionMu.Unlock()
				return
			}
		}
	})
	versionMu.Lock()
	defer versionMu.Unlock()
	return versionStr
}

var (
	versionOnce sync.Once
	versionMu   sync.Mutex
	versionStr  string
)

// Check 用内核自身校验配置文件。这是"写对了没有"的最终判据:
// 字段拼写、出站依赖环、规则集引用等问题都会在这一步暴露。
func (m *Manager) Check(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.binPath, "check", "-c", path, "-D", filepath.Dir(path))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 只保留有意义的错误行,避免把整段日志塞给面板
		return errors.New(condenseOutput(string(out), err))
	}
	return nil
}

// condenseOutput 从内核输出里挑出 FATAL/ERROR 行,最多 5 行。
func condenseOutput(out string, err error) string {
	var keep []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// 去掉 ANSI 颜色
		line = stripANSI(line)
		if strings.Contains(line, "FATAL") || strings.Contains(line, "ERROR") {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		keep = append(keep, stripANSI(strings.TrimSpace(out)))
	}
	if len(keep) > 5 {
		keep = keep[:5]
	}
	msg := strings.Join(keep, "\n")
	if strings.TrimSpace(msg) == "" {
		msg = err.Error()
	}
	return msg
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Start 启动内核。若已在运行则先停止。
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

func (m *Manager) startLocked() error {
	if m.cmd != nil && m.cmd.Process != nil {
		return errors.New("内核已在运行")
	}
	if err := m.Check(m.configPath); err != nil {
		m.lastErr = err.Error()
		return fmt.Errorf("配置校验失败: %w", err)
	}
	if err := os.MkdirAll(m.workDir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command(m.binPath, "run", "-c", m.configPath, "-D", m.workDir)
	// 内核日志直接写文件,避免管道读端阻塞导致内核挂起
	logf, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("打开日志失败: %w", err)
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	// 独立进程组:停止时能连带清理内核拉起的子进程
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		logf.Close()
		m.lastErr = err.Error()
		return fmt.Errorf("启动内核失败: %w", err)
	}
	m.cmd = cmd
	m.startedAt = time.Now()
	m.lastErr = ""
	if m.logFile != nil {
		m.logFile.Close()
	}
	m.logFile = logf
	// 监视退出
	go m.watch(cmd)

	// 活性探针:`sing-box check` 只做结构校验,不校验出站引用是否存在。
	// 例如 route.final 指向一个空组时,check 通过、run 立刻 FATAL。
	// 这里在内核启动后观察一小段时间,把这类问题变成明确的中文错误告诉用户,
	// 而不是让面板显示"运行中"但实际已经退出。
	if err := m.waitReady(cmd); err != nil {
		m.lastErr = err.Error()
		return err
	}
	return nil
}

// waitReady 等待内核真正就绪(或确认它已经失败退出)。
//
// 注意:调用方(startLocked)持有 m.mu,所以这里**不能再加锁**。
// 早期版本在这里 Lock() 造成了自己等自己的死锁,面板会在启动时卡住
// (进程活着但不监听端口),很难判断。判定存活用进程信号,不需要锁。
func (m *Manager) waitReady(cmd *exec.Cmd) error {
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		// watch() 发现退出后会把 m.cmd 置空;但为免与锁竞争,直接用进程状态判断
		if !processAlive(cmd) {
			// 给 watch 一点时间把 lastErr 写好
			time.Sleep(150 * time.Millisecond)
			// 内核退出了:把它最后几行致命错误取出来
			reason := m.lastFatalReason()
			if reason == "" {
				reason = "内核启动后立即退出"
			}
			return errors.New(reason)
		}
	}
	return nil
}

// lastFatalReason 从内核日志尾部找 FATAL 行。
func (m *Manager) lastFatalReason() string {
	raw, err := os.ReadFile(m.logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	var reason string
	// 只看最后 40 行:重启会让日志不断追加,更早的 FATAL 不是这次的
	start := len(lines) - 40
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		clean := stripANSI(line)
		if strings.Contains(clean, "FATAL") {
			// 去掉时间戳前缀,只留错误本身
			if i := strings.Index(clean, "FATAL"); i >= 0 {
				clean = clean[i+len("FATAL"):]
				clean = strings.TrimLeft(clean, "] [0-9:.")
			}
			reason = strings.TrimSpace(clean)
		}
	}
	return reason
}

// watch 等待内核退出并记录原因。只有非主动停止才计入崩溃重启。
func (m *Manager) watch(cmd *exec.Cmd) {
	err := cmd.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != cmd {
		return // 已被替换
	}
	m.cmd = nil
	if m.stopping {
		return
	}
	m.restarts++
	if err != nil {
		m.lastErr = "内核异常退出: " + err.Error()
	} else {
		m.lastErr = "内核已退出"
	}
	// 自动拉起:最多连续 5 次,超过后交由运维处理,避免疯狂重启
	if m.restarts <= 5 {
		time.AfterFunc(3*time.Second, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.cmd == nil && !m.stopping {
				_ = m.startLocked()
			}
		})
	}
}

// Stop 停止内核。
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked()
}

func (m *Manager) stopLocked() error {
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}
	m.stopping = true
	defer func() { m.stopping = false }()
	done := make(chan struct{})
	go func() {
		_ = m.cmd.Process.Signal(os.Interrupt)
		close(done)
	}()
	<-done
	// 给内核 8 秒优雅退出(TUN 与 UDP 会话需要时间释放)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Unlock()
		running := processAlive(m.cmd)
		m.mu.Lock()
		if !running {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	killProcess(m.cmd)
	return nil
}

// Restart 重启内核。
func (m *Manager) Restart() error {
	if err := m.Stop(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return m.Start()
}

// Reload 用新配置热替换:先校验新文件,通过则重启内核;不通过则保持旧内核运行。
// 这是"切换不掐断现有连接"的基础:校验失败时用户的网络完全不受影响。
func (m *Manager) Reload(newConfigPath string) error {
	if err := m.Check(newConfigPath); err != nil {
		return err
	}
	m.mu.Lock()
	wasRunning := m.cmd != nil
	m.mu.Unlock()
	if !wasRunning {
		return m.Start()
	}
	return m.Restart()
}

// TailLog 返回最后 n 行内核日志。
func (m *Manager) TailLog(n int) ([]string, error) {
	raw, err := os.ReadFile(m.logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// 日志文件可能很大:只取末尾 256KB 已经足够,这里做简单截断
	if len(lines) > 4000 {
		lines = lines[len(lines)-4000:]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = stripANSI(lines[i])
	}
	return lines, nil
}

// ClearLog 清空内核日志。
func (m *Manager) ClearLog() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logFile != nil {
		m.logFile.Close()
		m.logFile = nil
	}
	if err := os.Remove(m.logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.cmd != nil {
		f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return err
		}
		m.cmd.Stdout = f
		m.cmd.Stderr = f
		m.logFile = f
	}
	return nil
}

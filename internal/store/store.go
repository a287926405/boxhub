// Package store 负责配置的持久化。
//
// 关键约束(改坏了会让用户丢配置):
//   - 写入必须原子:先写临时文件、fsync、再 rename。直接截断写会在断电/崩溃时留下半份配置。
//   - 每次保存前把旧文件复制到 backups/,保留最近 N 份。
//   - 进程内加锁,避免面板连续点击与订阅定时更新同时写。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"boxhub/internal/config"
)

// Store 是配置仓库。
type Store struct {
	dir       string
	path      string
	backupDir string
	mu        sync.RWMutex
	cache     *config.Config
	maxBackup int
}

// Open 打开(不存在则创建)配置仓库,并加载现有配置。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	s := &Store{
		dir:       dir,
		path:      filepath.Join(dir, "config.json"),
		backupDir: filepath.Join(dir, "backups"),
		maxBackup: 20,
	}
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return nil, err
	}
	cfg, err := s.load()
	if err != nil {
		return nil, err
	}
	s.cache = cfg
	return s, nil
}

// Dir 数据目录。
func (s *Store) Dir() string { return s.dir }

// Path 配置文件路径。
func (s *Store) Path() string { return s.path }

func (s *Store) load() (*config.Config, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// 配置损坏不能让面板起不来:试着用最近的可用备份顶上,
		// 把坏文件改名留档,并在日志里说清楚发生了什么。
		badPath := s.path + ".corrupt"
		_ = os.Rename(s.path, badPath)
		if restored, name, rerr := s.restoreLatestUsable(); rerr == nil && restored != nil {
			fmt.Fprintf(os.Stderr, "[警告] 配置损坏,已从备份 %s 恢复(坏文件保留在 %s)\n", name, badPath)
			return restored, nil
		}
		fmt.Fprintf(os.Stderr, "[警告] 配置损坏且无可用备份,将从默认配置开始(坏文件保留在 %s)\n", badPath)
		return nil, nil
	}
	return &cfg, nil
}

// restoreLatestUsable 依次尝试最近的备份,返回第一个能解析的。
func (s *Store) restoreLatestUsable() (*config.Config, string, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return nil, "", err
	}
	// 按文件名倒序 = 时间倒序(文件名带时间戳)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(s.backupDir, name))
		if err != nil {
			continue
		}
		var cfg config.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		if err := s.persist(&cfg); err != nil {
			continue
		}
		return &cfg, name, nil
	}
	return nil, "", errors.New("没有可用备份")
}

// Current 返回当前配置的深拷贝,调用方可以随意修改。
func (s *Store) Current() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cache == nil {
		return nil
	}
	return clone(s.cache)
}

// Update 在锁内读-改-写配置。fn 返回错误则不改动任何东西。
// checkShape 保证配置能被自己原样读回来。任何字段类型不匹配(例如前端把
// inbounds 传成数组)都会在这里被拦下,而不是等到下次启动才发现配置不可用。
func checkShape(cfg *config.Config) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("配置无法序列化: %w", err)
	}
	var back config.Config
	if err := json.Unmarshal(raw, &back); err != nil {
		return fmt.Errorf("配置结构不合法(字段类型不匹配): %w", err)
	}
	return nil
}

func (s *Store) Update(fn func(cfg *config.Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cache
	if cfg == nil {
		cfg = config.NewDefault()
	}
	next := clone(cfg)
	if err := fn(next); err != nil {
		return err
	}
	next.UpdatedAt = time.Now()
	if err := checkShape(next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.cache = next
	return nil
}

// Replace 整体替换配置(导入场景)。会先做结构校验。
func (s *Store) Replace(next *config.Config) []config.RefError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errs := config.ValidateAll(next); len(errs) > 0 {
		return errs
	}
	if next.Schema == 0 {
		next.Schema = config.CurrentSchema
	}
	if err := checkShape(next); err != nil {
		return []config.RefError{{Where: "保存", Message: err.Error()}}
	}
	next.UpdatedAt = time.Now()
	if err := s.persist(next); err != nil {
		return []config.RefError{{Where: "保存", Message: err.Error()}}
	}
	s.cache = clone(next)
	return nil
}

// persist 原子写入并轮转备份。调用方必须持有写锁。
func (s *Store) persist(cfg *config.Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 备份当前文件
	if _, err := os.Stat(s.path); err == nil {
		_ = s.backup()
	}
	tmp, err := os.CreateTemp(s.dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *Store) backup() error {
	stamp := time.Now().Format("20060102-150405")
	// 同一秒内多次保存时加序号,避免覆盖
	dest := filepath.Join(s.backupDir, "config-"+stamp+".json")
	for i := 2; ; i++ {
		if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
			break
		}
		dest = filepath.Join(s.backupDir, fmt.Sprintf("config-%s-%d.json", stamp, i))
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		return err
	}
	return s.pruneBackups()
}

func (s *Store) pruneBackups() error {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	if len(names) <= s.maxBackup {
		return nil
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-s.maxBackup] {
		_ = os.Remove(filepath.Join(s.backupDir, name))
	}
	return nil
}

// Backups 列出可用备份(新的在前)。
func (s *Store) Backups() []BackupInfo {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return nil
	}
	var out []BackupInfo
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Name: e.Name(), Size: info.Size(), At: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

type BackupInfo struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
}

// RestoreBackup 从备份恢复配置。
func (s *Store) RestoreBackup(name string) error {
	if filepath.Base(name) != name {
		return errors.New("非法的备份文件名")
	}
	raw, err := os.ReadFile(filepath.Join(s.backupDir, name))
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("备份文件损坏: %w", err)
	}
	if errs := config.ValidateAll(&cfg); len(errs) > 0 {
		return fmt.Errorf("备份配置校验失败: %v", errs[0])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(&cfg); err != nil {
		return err
	}
	s.cache = &cfg
	return nil
}

// clone 深拷贝配置(经 JSON 往返,保证切片/指针不共享)。
func clone(in *config.Config) *config.Config {
	raw, _ := json.Marshal(in)
	var out config.Config
	_ = json.Unmarshal(raw, &out)
	return &out
}

// RandomID 生成 8 位十六进制随机串,用于新实体的 ID。
func RandomID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b)
}

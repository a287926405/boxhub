package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"boxhub/internal/config"
	"boxhub/internal/store"
)

func TestOpenCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Current() != nil {
		t.Fatal("新目录不应该有配置")
	}
	if err := s.Update(func(cfg *config.Config) error {
		cfg.Nodes = append(cfg.Nodes, config.Node{
			ID: "n1", Name: "测试节点", Type: "shadowsocks", Server: "1.2.3.4", Port: 8388,
			Method: "aes-256-gcm", Password: "p", Source: "manual",
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 重新打开应读到
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Current(); got == nil || len(got.Nodes) != 1 {
		t.Fatalf("配置未持久化: %+v", got)
	}
	// 文件权限应是 0600(含密码)
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置文件权限应为 0600,实际 %o", perm)
	}
}

func TestUpdateRollbackOnError(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(cfg *config.Config) error {
		cfg.Nodes = []config.Node{{ID: "a", Name: "A", Type: "shadowsocks", Server: "1.1.1.1", Port: 1}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(cfg *config.Config) error {
		cfg.Nodes[0].Name = "被改坏的名字"
		return os.ErrPermission
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if s.Current().Nodes[0].Name != "A" {
		t.Fatal("出错时不应改动内存中的配置")
	}
}

func TestBackupCreatedAndListed(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Update(func(cfg *config.Config) error {
			cfg.Panel.Theme = []string{"auto", "dark", "light"}[i]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	backups := s.Backups()
	if len(backups) < 2 {
		t.Fatalf("应有备份,实际 %d", len(backups))
	}
}

func TestReplaceRejectsInvalid(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bad := config.NewDefault()
	bad.Chains = []config.Chain{{ID: "c1", Name: "坏链", Type: "socks5", Host: "1.2.3.4", Port: 1}}
	if errs := s.Replace(bad); len(errs) == 0 {
		t.Fatal("缺少前置组的链式代理应被拒绝")
	}
}

func TestRandomIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := store.RandomID()
		if seen[id] {
			t.Fatalf("随机 ID 重复: %s", id)
		}
		seen[id] = true
	}
}

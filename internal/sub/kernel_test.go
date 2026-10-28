package sub_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"boxhub/internal/render"
)

func singBox(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BOXHUB_SING_BOX"); p != "" {
		return p
	}
	for _, p := range []string{"/usr/local/bin/sing-box", "/usr/bin/sing-box"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func writeAndRun(t *testing.T, bin string, sb *render.SBConfig) {
	t.Helper()
	raw, err := sb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 只用 check 而不是 run:这里验证的是"节点字段拼对了",不需要联网建连。
	// check 会覆盖字段拼写与出站依赖环;依赖环由 render 包的 run 测试覆盖。
	cmd := exec.Command(bin, "check", "-c", path, "-D", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("内核拒绝了生成的配置:\n%s", strings.TrimSpace(string(out)))
	}
}

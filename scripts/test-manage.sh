#!/bin/bash
# =============================================================================
# manage.sh 逻辑测试(在不安装服务的前提下验证脚本行为)
#
# 用途:在开发机(macOS)或 CI 上快速验证脚本的分发、检测与安全逻辑,
# 不需要真的装 systemd 服务。
#
# 用法: bash scripts/test-manage.sh
# =============================================================================

set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

SCRIPT="./scripts/manage.sh"
PASS=0
FAIL=0
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

ok()   { PASS=$((PASS+1)); printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31m✗\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

check() {
    local desc=$1; shift
    if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

check_fail() {
    local desc=$1; shift
    if "$@" >/dev/null 2>&1; then bad "$desc" "预期失败但成功了"; else ok "$desc"; fi
}

echo
echo "== 1. 基础可用性 =="
check "脚本语法正确" bash -n "$SCRIPT"
check "help 输出正常" bash -c "bash $SCRIPT help | grep -q 'BoxHub'"
check "help 含 install 命令" bash -c "bash $SCRIPT help | grep -q 'install'"
check "help 含 diagnose 命令" bash -c "bash $SCRIPT help | grep -q 'diagnose'"
check_fail "未知命令返回非零" bash "$SCRIPT" nosuchcommand

echo
echo "== 2. 非 root 保护 =="
if [ "$(id -u)" -ne 0 ]; then
    check_fail "install 在非 root 下被拒绝" bash "$SCRIPT" install
    check_fail "uninstall 在非 root 下被拒绝" bash "$SCRIPT" uninstall
    check_fail "restart 在非 root 下被拒绝" bash "$SCRIPT" restart
else
    ok "(以 root 运行,跳过非 root 检查)"
fi

echo
echo "== 3. 只读命令在异常环境下的容错 =="
export BOXHUB_DIR="$TMP/notinstalled"
check "status 在未安装时正常退出" bash "$SCRIPT" status
check "diagnose 在未安装时正常退出" bash "$SCRIPT" diagnose
check "status 输出含安装目录" bash -c "BOXHUB_DIR=$TMP/notinstalled bash $SCRIPT status | grep -q '安装目录'"
check "diagnose 输出含系统信息" bash -c "BOXHUB_DIR=$TMP/notinstalled bash $SCRIPT diagnose | grep -q '系统信息'"
check "diagnose 检查 TUN 支持" bash -c "BOXHUB_DIR=$TMP/notinstalled bash $SCRIPT diagnose | grep -q 'TUN'"

echo
echo "== 4. 环境变量生效 =="
check "BOXHUB_DIR 生效" bash -c "BOXHUB_DIR=$TMP/custom bash $SCRIPT status | grep -q '$TMP/custom'"
check "BOXHUB_PORT 出现在配置文件里" bash -c "grep -q 'PANEL_PORT=\"\${BOXHUB_PORT:-3036}\"' $SCRIPT"

echo
echo "== 5. 关键安全约束(静态检查) =="
check "卸载只删自己的子目录(存在 rmdir 而非 rm -rf 安装目录)" \
    bash -c "grep -q 'rmdir \"\$INSTALL_DIR\"' $SCRIPT"
check "备份与恢复逻辑存在" bash -c "grep -q 'restore_existing' $SCRIPT && grep -q 'backup_existing' $SCRIPT"
check "安装失败会回滚" bash -c "grep -q 'restore_existing \"\$backup\"' $SCRIPT"
check "服务启动失败时会打印日志" bash -c "grep -q 'journalctl -u' $SCRIPT"
check "下载后有可执行性校验" bash -c "grep -q 'version >/dev/null 2>&1' $SCRIPT"
check "支持离线安装(BOXHUB_LOCAL_BOXD)" bash -c "grep -q 'BOXHUB_LOCAL_BOXD' $SCRIPT"
check "安装后自动放行防火墙" bash -c "grep -q 'open_firewall' $SCRIPT"

echo
echo "== 6. 产物完整性 =="
for f in dist/boxd-linux-amd64 dist/boxd-linux-arm64; do
    check "$f 存在且为 Linux 可执行文件" bash -c "[ -f $f ] && file $f | grep -q ELF"
done
check "amd64 是静态链接(无 glibc 依赖)" bash -c "file dist/boxd-linux-amd64 | grep -q 'statically linked'"
check "二进制体积合理(<20MB)" bash -c "[ \$(stat -c%s dist/boxd-linux-amd64 2>/dev/null || stat -f%z dist/boxd-linux-amd64) -lt 20971520 ]"

echo
echo "──────────────────────────────────────"
printf '通过: %d  失败: %d\n' "$PASS" "$FAIL"
echo "──────────────────────────────────────"
[ "$FAIL" -eq 0 ] || exit 1

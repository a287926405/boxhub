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
check "面板默认端口是 80(BOXHUB_PORT 可覆盖)" bash -c "grep -q 'PANEL_PORT_DEFAULT=\"80\"' $SCRIPT && grep -q 'PANEL_PORT=\"\\\${BOXHUB_PORT:-\\\$PANEL_PORT_DEFAULT}\"' $SCRIPT"
check "内核面板改成 zashboard" bash -c "grep -q 'ZASHBOARD_REPO=\"Zephyruso/zashboard\"' $SCRIPT"
check "不再下载 metacubexd" bash -c "! grep -q 'METACUBEXD_REPO' $SCRIPT"

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
echo "== 6. 内核面板 zashboard(功能测试) =="
# 造一个假发行包:真实 dist.zip 里有 dist/ 一层,装完必须拍平到 ui 根
FAKE="$TMP/zbfake"
mkdir -p "$FAKE/src/dist/assets"
cat > "$FAKE/src/dist/index.html" <<'HTML'
<!doctype html><html><head><script src="./assets/app.js"></script>
<link rel="manifest" href="./manifest.webmanifest"><script id="vite-plugin-pwa:register-sw" src="./registerSW.js"></script></head>
<body><div id="app"></div><script src="./workbox-deadbeef.js"></script></body></html>
HTML
echo "console.log(1)" > "$FAKE/src/dist/assets/app.js"
echo "old-sw" > "$FAKE/src/dist/sw.js"
echo "old-register" > "$FAKE/src/dist/registerSW.js"
echo "workbox" > "$FAKE/src/dist/workbox-deadbeef.js"
python3 - "$FAKE/src" "$FAKE/dist.zip" <<'PY' 2>/dev/null
import os, sys, zipfile
src, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, 'w') as z:
    for root, _, files in os.walk(src):
        for f in files:
            p = os.path.join(root, f)
            z.write(p, os.path.relpath(p, src))
PY
if [ -s "$FAKE/dist.zip" ]; then
    export BOXHUB_DIR="$TMP/zbinst"
    mkdir -p "$TMP/zbwork"
    sed '/^main "\$@"$/d' "$SCRIPT" > "$TMP/mh.sh"
    # macOS 开发机没有 coreutils 的 timeout,给个透传桩(Linux 上装不用管)
    mkdir -p "$TMP/bin"; printf '#!/bin/bash\nshift\nexec "$@"\n' > "$TMP/bin/timeout"; chmod +x "$TMP/bin/timeout"
    PATH="$TMP/bin:$PATH" BOXHUB_ZASHBOARD_URL="file://$FAKE/dist.zip" \
        bash -c "source '$TMP/mh.sh'; detect_downloader; install_clash_ui '$TMP/zbwork'" >/dev/null 2>&1
    check "zashboard 装到 data/ui/index.html(dist/ 前缀已拍平)" \
        bash -c "[ -f $TMP/zbinst/data/ui/index.html ]"
    check "JS 资源到位(data/ui/assets/app.js)" \
        bash -c "[ -f $TMP/zbinst/data/ui/assets/app.js ]"
    check "不再装 metacubexd 的 config.js" bash -c "[ ! -f $TMP/zbinst/data/ui/config.js ]"
    check "Service Worker 换成自杀脚本" \
        bash -c "grep -q skipWaiting $TMP/zbinst/data/ui/sw.js"
    check "index.html 里的 registerSW 标签被摘掉" \
        bash -c "! grep -q 'registerSW' $TMP/zbinst/data/ui/index.html"
    check "workbox 脚本文件已删除" bash -c "[ ! -f $TMP/zbinst/data/ui/workbox-deadbeef.js ]"
    # 下载失败时不能碰已有面板
    BOXHUB_DIR="$TMP/zbinst" mkdir -p "$TMP/zbwork2"
    PATH="$TMP/bin:$PATH" BOXHUB_ZASHBOARD_URL="file:///nonexistent.zip" \
        bash -c "source '$TMP/mh.sh'; detect_downloader; install_clash_ui '$TMP/zbwork2'" >/dev/null 2>&1
    check "下载失败时保留现有面板" \
        bash -c "[ -f $TMP/zbinst/data/ui/index.html ]"
    unset BOXHUB_DIR
else
    bad "构造假 zashboard 包失败(需要 python3)"
fi

echo
echo "== 7. 面板端口:默认 80 + 占用回退(功能测试) =="
# port_busy 读 /proc/net/tcp;这里用 BOXHUB_PROC_NET_TCP 注入快照来模拟
mk_tcp() { printf '  sl  local_address rem_address st\n%b' "$1" > "$2"; }
mk_tcp "   0: 0100007F:1F90 00000000:0000 0A 0\n" "$TMP/tcp_free"          # 只有 8080
mk_tcp "   0: 00000000:0050 00000000:0000 0A 0\n" "$TMP/tcp_busy80"        # 80 被占
mk_tcp "   0: 00000000:0050 00000000:0000 0A 0\n   1: 00000000:0BDC 00000000:0000 0A 0\n" "$TMP/tcp_busy_both"
PORT_RUNNER="$TMP/port.sh"
cat > "$PORT_RUNNER" <<'SH'
source "$SRC"
[ -n "$2" ] && { PANEL_PORT="$2"; PORT_EXPLICIT=1; }
EXISTING_PORT="$3"
[ -n "$3" ] && [ "$PORT_EXPLICIT" != "1" ] && PANEL_PORT="$3"
BOXHUB_PROC_NET_TCP="$1"; BOXHUB_PROC_NET_TCP6=/dev/null
resolve_panel_port >/dev/null 2>&1
printf '%s' "$PANEL_PORT"
SH
port_case() { # 快照 显式端口 现存端口 期望 描述
    got=$(SRC="$TMP/mh.sh" bash "$PORT_RUNNER" "$1" "$2" "$3" 2>/dev/null)
    if [ "$got" = "$4" ]; then ok "$5"; else bad "$5" "得到 $got,期望 $4"; fi
}
port_case "$TMP/tcp_free"     ""     ""     80    "全新安装 + 80 空闲 → 默认 80"
port_case "$TMP/tcp_busy80"   ""     ""     3036  "80 被 nginx 占用 → 退回 3036"
port_case "$TMP/tcp_free"     ""     "3036" 3036  "已装机器(3036)升级 → 保留原端口"
port_case "$TMP/tcp_free"     ""     "8080" 8080  "已装机器(8080)升级 → 保留原端口"
port_case "$TMP/tcp_free"     "80"   "3036" 80    "显式 BOXHUB_PORT=80 → 改掉旧端口"
check_fail "80 与 3036 都被占用时报错退出" \
    bash -c "SRC='$TMP/mh.sh' bash '$PORT_RUNNER' '$TMP/tcp_busy_both' '' '' >/dev/null 2>&1"
check "help 里写明默认端口是 80" bash -c "bash $SCRIPT help | grep -q '面板端口(默认 80'"
check "install.sh 说明默认端口 80" bash -c "grep -q '默认 80' install.sh"

echo
echo "== 8. 规则删除(前端) =="
# 曾经卡片/表格视图的删除按钮调用了从未定义的 deleteRule,点了没反应
check "deleteRule 已定义" \
    bash -c "grep -qE '^(async )?function deleteRule' cmd/boxd/web/static/pages2.js"
check "删除按钮与函数同名" \
    bash -c "grep -c 'deleteRule(p)' cmd/boxd/web/static/pages2.js | grep -q '^[2-9]'"

echo
echo "== 9. 产物完整性 =="
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

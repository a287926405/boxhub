#!/bin/bash
# =============================================================================
# BoxHub 安装管理脚本
#
# 一键部署「面板 + sing-box 内核」,并提供升级、卸载、状态查看、诊断等管理能力。
#
# 设计原则(改脚本前请先读):
#   1. 下载与校验全部在临时目录完成,任何一步失败都不碰安装目录;
#   2. 覆盖安装前先备份配置,失败可回滚;
#   3. 所有操作可重复执行(幂等),重复跑不会弄坏已有部署;
#   4. 权限:安装/升级/卸载需要 root;只读操作(状态/日志)普通用户也能看。
#
# 用法:
#   bash manage.sh                  # 交互式菜单
#   bash manage.sh install          # 安装或升级
#   bash manage.sh status           # 查看状态
#   bash manage.sh uninstall        # 卸载
# =============================================================================

set -o pipefail

VERSION="1.13.0"   # build.sh 打包时会被覆盖成实际发布版本

# ==================== 配置 ====================
INSTALL_DIR="${BOXHUB_DIR:-/opt/boxhub}"
DATA_DIR="${INSTALL_DIR}/data"
BIN_DIR="${INSTALL_DIR}/bin"
SERVICE_NAME="boxhub"
# ==================== 面板端口 ====================
# 默认端口 80:装完直接 http://<IP> 就能开,不用记端口。
#   - 80 是特权端口,但服务以 root 跑(systemd 单元还额外给了
#     CAP_NET_BIND_SERVICE),绑定没有问题;
#   - 80 也最容易被 nginx / apache 占着,所以占用时不硬碰:
#     告警并退回历史默认 3036,保证安装能走完。
PANEL_PORT_DEFAULT="80"
PANEL_PORT_FALLBACK="3036"
PANEL_PORT="${BOXHUB_PORT:-$PANEL_PORT_DEFAULT}"
PORT_EXPLICIT=0
[ -n "${BOXHUB_PORT:-}" ] && PORT_EXPLICIT=1
# 已装机器现存端口(空 = 全新安装)。
EXISTING_PORT=""
# 已装机器的端口原样保留(哪怕它等于历史默认 3036):
# 升级不该把用户已有的书签 / 防火墙规则 / 设备配置打掉。
# 只有显式 BOXHUB_PORT=xx 才能改变它。
detect_existing_port() {
    local svc=""
    if [ "$INIT" = "systemd" ]; then
        svc="/etc/systemd/system/${SERVICE_NAME}.service"
    else
        svc="/etc/init.d/${SERVICE_NAME}"
    fi
    [ -f "$svc" ] || return 0
    # 从 -listen 0.0.0.0:PORT / [::]:PORT 提取端口
    EXISTING_PORT=$(grep -oE '\-listen [^ ]*[:%][0-9]+' "$svc" 2>/dev/null | grep -oE '[0-9]+$' | head -n1)
    [ -n "$EXISTING_PORT" ] || return 0
    if [ "$PORT_EXPLICIT" = "1" ]; then
        info "按 BOXHUB_PORT 指定的端口安装: $PANEL_PORT(现存 $EXISTING_PORT)" >&2
    else
        PANEL_PORT="$EXISTING_PORT"
    fi
    return 0
}

# port_busy <端口>:TCP LISTEN 探测。直接读 /proc/net/tcp(6),
# 不依赖 ss / netstat / lsof 这些不一定装的家伙(容器里常常一个都没有)。
# BOXHUB_PROC_NET_TCP / _TCP6 只为测试留的注入点(见 scripts/test-manage.sh),
# 正常安装不要设:默认就是内核的真实监听表。
port_busy() {
    local port=$1 f hex
    hex=$(printf '%04X' "$port") || return 1
    for f in "${BOXHUB_PROC_NET_TCP:-/proc/net/tcp}" "${BOXHUB_PROC_NET_TCP6:-/proc/net/tcp6}"; do
        [ -r "$f" ] || continue
        if awk -v p="$hex" '$4 == "0A" { split($2, a, ":"); if (a[2] == p) { found = 1; exit } } END { exit !found }' "$f"; then
            return 0
        fi
    done
    return 1
}

# 端口冲突处理:必须在写服务文件之前调用,否则会装出一个起不来的服务。
resolve_panel_port() {
    # 沿用现存端口时,"被占用"是正常的 —— 占用者就是还没重启的旧 boxhub
    [ "$PANEL_PORT" = "$EXISTING_PORT" ] && return 0
    port_busy "$PANEL_PORT" || return 0
    if [ "$PORT_EXPLICIT" = "1" ]; then
        die "端口 $PANEL_PORT 已被其他程序占用。换个端口(BOXHUB_PORT=8080)或先停掉占用它的服务(ss -lntp | grep :$PANEL_PORT)"
    fi
    if [ "$PANEL_PORT" = "$PANEL_PORT_DEFAULT" ] && ! port_busy "$PANEL_PORT_FALLBACK"; then
        warn "端口 $PANEL_PORT_DEFAULT 已被占用(常见于 nginx / apache),面板退回 $PANEL_PORT_FALLBACK"
        warn "想让面板用别的端口:BOXHUB_PORT=8080 bash manage.sh install"
        PANEL_PORT="$PANEL_PORT_FALLBACK"
        return 0
    fi
    die "端口 $PANEL_PORT 与 $PANEL_PORT_FALLBACK 都被占用,请用 BOXHUB_PORT=<端口> 指定一个空闲端口"
}

# 面板访问地址(80 端口不写 ":80",直接 http://IP)
panel_url() {
    local host=$1
    if [ "$PANEL_PORT" = "80" ]; then
        printf 'http://%s' "$host"
    else
        printf 'http://%s:%s' "$host" "$PANEL_PORT"
    fi
}

# 下载源
GITHUB_REPO="${BOXHUB_REPO:-a287926405/boxhub}"
RELEASE_BASE="${BOXHUB_RELEASE_BASE:-}"
# GitHub 镜像前缀(gh-proxy 类服务,国内直连不通时必备):
#   BOXHUB_GH_MIRROR=https://gh-proxy.com bash -c "$(curl -fsSL <install.sh>)"
GH_MIRROR="${BOXHUB_GH_MIRROR:-}"
SINGBOX_REPO="SagerNet/sing-box"
SINGBOX_VERSION="${BOXHUB_SINGBOX_VERSION:-}"   # 留空 = 用 Latest
# 内核面板:zashboard(Zephyruso/zashboard)。
# 它源自 metacubexd,但**没有 config.js**:后端参数由入口 URL 的 query string
# 提供(protocol/hostname/port/secondaryPath/secret),契约见 internal/api/server.go
# 的 handleKernelUIURL。发行包是 Release 资产 dist.zip(不是 metacubexd 的
# compressed-dist.tgz,装错面板会导致 #kernel-ui 打不开)。
ZASHBOARD_REPO="Zephyruso/zashboard"
ZASHBOARD_ASSET="dist.zip"

# ==================== 输出 ====================
if [ -t 1 ]; then
    RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[0;33m'
    BLUE=$'\033[0;34m'; CYAN=$'\033[0;36m'; BOLD=$'\033[1m'; NC=$'\033[0m'
else
    RED=; GREEN=; YELLOW=; BLUE=; CYAN=; BOLD=; NC=
fi

info()  { printf '%s[信息]%s %s\n' "$GREEN" "$NC" "$*"; }
warn()  { printf '%s[警告]%s %s\n' "$YELLOW" "$NC" "$*"; }
error() { printf '%s[错误]%s %s\n' "$RED" "$NC" "$*" >&2; }
# 注意:step/info/warn 在"命令替换"场景里会被调用(例如 $(install_singbox ...)),
# 所以进度类输出一律走 stderr,避免混进函数返回值把路径弄脏。
step()  { printf '%s==>%s %s\n' "$CYAN" "$NC" "$*" >&2; }
step_stdout() { printf '%s==>%s %s\n' "$CYAN" "$NC" "$*"; }
die()   { error "$*"; exit 1; }

# ==================== 前置检查 ====================
require_root() {
    [ "$(id -u)" -eq 0 ] || die "此操作需要 root 权限,请用 sudo 运行"
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "不支持的架构: $(uname -m)(BoxHub 支持 amd64 / arm64)" ;;
    esac
}

detect_os() {
    # 只读操作(状态/诊断)在非 Linux 环境下要能优雅降级,不能直接退出
    if [ ! -f /etc/os-release ]; then
        OS_ID="unknown"
        PRETTY_NAME="$(uname -s)"
        INIT="unknown"
        return 0
    fi
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-unknown}"
    case "$OS_ID" in
        debian|ubuntu|raspbian|linuxmint|pop) INIT="systemd" ;;
        alpine) INIT="openrc" ;;
        *)
            if command -v systemctl >/dev/null 2>&1; then
                INIT="systemd"
            else
                INIT="unknown"
            fi
            ;;
    esac
}

# 安装前必须确认平台可服务化
require_supported_platform() {
    detect_os
    detect_arch
    case "$INIT" in
        systemd|openrc) ;;
        *) die "不支持的系统: ${PRETTY_NAME:-$OS_ID}。BoxHub 支持 Debian / Ubuntu / Alpine" ;;
    esac
    if [ "$INIT" = "systemd" ] && ! command -v systemctl >/dev/null 2>&1; then
        die "检测到 systemd 系统但找不到 systemctl"
    fi
}

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"
}

detect_downloader() {
    if command -v curl >/dev/null 2>&1; then
        DL="curl"
    elif command -v wget >/dev/null 2>&1; then
        DL="wget"
    else
        die "需要 curl 或 wget,请先安装"
    fi
}

fetch() {
    # fetch URL [输出文件];不给文件则输出到 stdout
    local url=$1 out=${2:-}
    if [ "$DL" = "curl" ]; then
        if [ -n "$out" ]; then
            curl -fsSL --connect-timeout 20 --max-time 600 --retry 2 -o "$out" "$url"
        else
            curl -fsSL --connect-timeout 20 --max-time 60 "$url"
        fi
    else
        if [ -n "$out" ]; then
            wget -q --timeout=60 --tries=2 -O "$out" "$url"
        else
            wget -q --timeout=20 -O - "$url"
        fi
    fi
}

# ==================== 安装 ====================
install_deps() {
    step_stdout "检查系统依赖"
    local missing=()
    for c in tar gzip; do
        command -v "$c" >/dev/null 2>&1 || missing+=("$c")
    done
    if [ ${#missing[@]} -eq 0 ]; then
        info "依赖完整"
        return 0
    fi
    info "安装缺失依赖: ${missing[*]}"
    case "$OS_ID" in
        alpine) apk add --no-cache "${missing[@]}" >/dev/null ;;
        *)      apt-get update -qq && apt-get install -y -qq "${missing[@]}" >/dev/null ;;
    esac
}

# 获取 sing-box 二进制,输出其路径。
# 支持 BOXHUB_LOCAL_SINGBOX 指定已有二进制(内网/离线环境必备:
# 不少服务器直连 GitHub Releases 是 0 字节)。
# gh_url 给 GitHub **下载** URL 加镜像前缀。
# 注意:API 请求不走镜像 —— gh-proxy 类服务通常只代理 releases/raw。
gh_url() {
    local u=$1
    if [ -n "${GH_MIRROR:-}" ]; then
        printf '%s/%s' "${GH_MIRROR%/}" "$u"
    else
        printf '%s' "$u"
    fi
}

# ------------------------------------------------------------------
# 多源下载:纯国内网络(直连 GitHub 不通)也要能装。
# 候选前缀 = 用户镜像(BOXHUB_GH_MIRROR,支持逗号分隔)→ 直连 → 内置镜像。
# 内置镜像实测(2026-10)支持 raw / releases 大文件 / api;失效会随版本更新淘汰。
MIRRORS_DEFAULT="https://gh-proxy.com https://gh-proxy.net https://gh.ddlc.top"

github_direct() {
    if [ "$DL" = "curl" ]; then
        curl -fsI --connect-timeout 5 --max-time 8 https://github.com >/dev/null 2>&1
    else
        wget -q --timeout=5 --tries=1 -O /dev/null https://github.com 2>/dev/null
    fi
}

# prefix_list 输出候选镜像前缀,空行表示"直连"。
prefix_list() {
    if [ -n "${GH_MIRROR:-}" ]; then
        printf '%s\n' ${GH_MIRROR} | sed 's:/*$::'
    elif github_direct; then
        printf '\n%s\n' "$MIRRORS_DEFAULT"
    else
        printf '%s\n' $MIRRORS_DEFAULT
        printf '\n'
    fi
}

# try_sources 依次尝试"每个前缀 × 每个 URL 候选",成功即返回 0。
# 用法: try_sources 输出文件 URL1 [URL2 ...](URL 为不带前缀的原始地址)
try_sources() {
    local out=$1; shift
    local prefix u
    while IFS= read -r prefix; do
        for u in "$@"; do
            [ -n "$prefix" ] && u="$prefix/$u"
            if fetch "$u" "$out" 2>/dev/null && [ -s "$out" ]; then
                return 0
            fi
        done
    done <<EOF
$(prefix_list)
EOF
    return 1
}

# resolve_latest_tag 取仓库最新 release 的 tag。
# 先走 API(一次请求拿到准确版本号,不依赖页面结构);
# 被限流时退回抓 releases/latest 跳转 HTML 里的 tag 链接。
resolve_latest_tag() {
    local repo=$1 tag="" prefix u
    while IFS= read -r prefix; do
        for u in "https://api.github.com/repos/${repo}/releases/latest" \
                 "https://github.com/${repo}/releases/latest"; do
            [ -n "$prefix" ] && u="$prefix/$u"
            tag=$(fetch "$u" 2>/dev/null \
                | grep -oE '("tag_name":[[:space:]]*"[^"]+"|/releases/tag/v[0-9.]+)' | head -n1 \
                | sed -e 's/.*"\([^"]*\)"$/\1/' -e 's|.*/||') || true
            [ -n "$tag" ] && { printf '%s' "$tag"; return 0; }
        done
    done <<EOF
$(prefix_list)
EOF
    printf '%s' "$tag"
}

install_singbox() {
    local tmp=$1
    # 本函数的 stdout 只用于返回二进制路径,所有日志必须写 stderr
    if [ -n "${BOXHUB_LOCAL_SINGBOX:-}" ]; then
        step "使用本地 sing-box 内核"
        [ -f "$BOXHUB_LOCAL_SINGBOX" ] || die "指定的内核不存在: $BOXHUB_LOCAL_SINGBOX"
        [ -x "$BOXHUB_LOCAL_SINGBOX" ] || chmod +x "$BOXHUB_LOCAL_SINGBOX" 2>/dev/null || true
        local localbin="$BOXHUB_LOCAL_SINGBOX"
        if ! "$localbin" version >/dev/null 2>&1; then
            die "指定的文件无法运行或架构不匹配: $localbin"
        fi
        info "使用: $localbin ($("$localbin" version 2>/dev/null | head -n1))" >&2
        printf '%s\n' "$localbin"
        return 0
    fi
    step "下载 sing-box 内核"
    local tag="${SINGBOX_VERSION}"
    if [ -z "$tag" ]; then
        # 注:官方并没有 sing-box-latest-linux-X 这种"稳定直链"资产
        # (实测 302 后 404),必须先拿到真实版本号再拼 URL。
        tag=$(resolve_latest_tag "$SINGBOX_REPO")
    fi
    [ -n "$tag" ] || die "无法确定 sing-box 版本号,可用 BOXHUB_SINGBOX_VERSION=v1.14.4 指定"
    local raw_url="https://github.com/${SINGBOX_REPO}/releases/download/${tag}/sing-box-${tag#v}-linux-${ARCH}.tar.gz"
    info "版本: $tag" >&2
    if ! try_sources "$tmp/singbox.tar.gz" "$raw_url"; then
        warn "sing-box 下载失败(镜像与直连都试过)。可选办法:"
        warn "  1) 指定镜像: BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install"
        warn "  2) 离线: BOXHUB_LOCAL_SINGBOX=/path/to/sing-box bash manage.sh install"
        warn "  3) 指定版本: BOXHUB_SINGBOX_VERSION=v1.14.4 bash manage.sh install"
        die "sing-box 下载失败"
    fi
    tar -xzf "$tmp/singbox.tar.gz" -C "$tmp" || die "sing-box 解压失败"
    local bin
    bin=$(find "$tmp" -type f -name sing-box -perm -u+x | head -n1)
    [ -n "$bin" ] || bin=$(find "$tmp" -type f -name sing-box | head -n1)
    [ -n "$bin" ] || die "安装包里没有找到 sing-box 可执行文件"
    chmod +x "$bin"
    "$bin" version >/dev/null 2>&1 || die "下载的 sing-box 无法运行,可能是架构不匹配"
    echo "$bin"
}

# 下载内核面板 zashboard(可选,失败不影响主功能)
# 去掉面板的 PWA/Service Worker:SW 在浏览器里缓存旧文件,
# boxd/内核升级换文件后 SW 会用失效缓存把 /ui 打成 404("内核面板打不开"根因);
# 且 SW 注册脚本在跨源 iframe 中抛 SecurityError 导致整页白屏。
# 局域网工具不需要离线缓存,直接移除,并用 kill-switch 让浏览器里
# 已注册的旧 SW 自行清缓存卸载。
strip_pwa() {
    local dir=$1
    [ -d "$dir" ] || return 0
    cat > "$dir/sw.js" 2>/dev/null <<'SW'
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.map((k) => caches.delete(k)));
    await self.registration.unregister();
  })());
});
SW
    cat > "$dir/registerSW.js" 2>/dev/null <<'RJ'
try {
  if (navigator.serviceWorker) {
    navigator.serviceWorker.getRegistrations().then((rs) => {
      rs.forEach((r) => r.unregister());
      if (caches && caches.keys) caches.keys().then((ks) => ks.forEach((k) => caches.delete(k)));
    });
  }
} catch (e) {}
RJ
    if [ -f "$dir/index.html" ]; then
        # 注意:不能写 `sed -i`(BSD sed 的 -i 需要参数,GNU 的不需要),
        # 在 mac/部分精简系统上会静默失败,留下引用已删除 workbox-*.js 的
        # <script> 标签(404)与 registerSW.js(重新注册 SW)。
        # 用"输出到临时文件再 mv"的写法,两种 sed 都成立。
        local idx="$dir/index.html" tmpf="$dir/.index.html.tmp"
        if sed -e 's|<script[^>]*workbox[^>]*></script>||g' \
               -e 's|<script[^>]*registerSW[^>]*></script>||g' \
               -e 's|<script[^>]*id="vite-plugin-pwa:register-sw"[^>]*></script>||g' \
               "$idx" > "$tmpf" 2>/dev/null; then
            mv "$tmpf" "$idx"
        else
            rm -f "$tmpf"
        fi
    fi
    rm -f "$dir"/workbox-*.js 2>/dev/null || true
}

# unzip_archive <zip> <目标目录>:把 zip 解到目录里。
# zashboard 发行包是 zip,而精简系统(debian-slim / alpine 基础镜像)经常没有 unzip,
# 所以按可用性依次退到 bsdtar / busybox / python3,都不行才尝试装一次 unzip。
unzip_archive() {
    local zip=$1 dest=$2
    mkdir -p "$dest" || return 1
    if command -v unzip >/dev/null 2>&1; then
        unzip -oq "$zip" -d "$dest" && return 0
    elif command -v bsdtar >/dev/null 2>&1; then
        bsdtar -xf "$zip" -C "$dest" >/dev/null 2>&1 && return 0
    elif command -v busybox >/dev/null 2>&1 && busybox unzip --help 2>&1 | grep -q unzip; then
        busybox unzip -q -o "$zip" -d "$dest" && return 0
    elif command -v python3 >/dev/null 2>&1; then
        python3 - "$zip" "$dest" <<'PY' >/dev/null 2>&1 && return 0
import sys, zipfile
with zipfile.ZipFile(sys.argv[1]) as z:
    z.extractall(sys.argv[2])
PY
    fi
    # 实在没有解压工具:装一个再试(内网 apt 不通时会失败,那就跳过面板)
    case "$OS_ID" in
        alpine) apk add --no-cache unzip >/dev/null 2>&1 ;;
        *)      apt-get install -y -qq unzip >/dev/null 2>&1 ;;
    esac
    command -v unzip >/dev/null 2>&1 || return 1
    unzip -oq "$zip" -d "$dest"
}

install_clash_ui() {
    # 可选项:内网/受限网络下载不通时不能卡住整个安装
    if [ "${BOXHUB_NO_CLASH_UI:-0}" = "1" ]; then
        info "已跳过内核面板 zashboard(BOXHUB_NO_CLASH_UI=1)"
        return 0
    fi
    step_stdout "下载内核面板 zashboard(可选)"
    local tmp=$1 dest url zip
    dest="${DATA_DIR}/ui"
    zip="$tmp/zashboard.zip"
    # 和 sing-box 一样支持 GitHub 镜像:国内直连 releases 不通时这是唯一出路。
    # BOXHUB_ZASHBOARD_URL 可整条替换(锁定某个版本、指向自建源,或本地 file:// 路径)。
    url="${BOXHUB_ZASHBOARD_URL:-}"
    if [ -n "$url" ]; then
        # 整条替换(锁定版本/自建源/本地路径):行为保持单一来源
        if ! timeout 240 sh -c "if [ \"$DL\" = curl ]; then curl -fsSL --connect-timeout 15 --max-time 230 -o '$zip' '$url'; else wget -q --timeout=15 --tries=1 -O '$zip' '$url'; fi" 2>/dev/null; then
            warn "zashboard 下载失败,已跳过(BoxHub 控制台不受影响): $url"
            return 0
        fi
    elif ! try_sources "$zip" "https://github.com/${ZASHBOARD_REPO}/releases/latest/download/${ZASHBOARD_ASSET}"; then
        warn "zashboard 下载失败,已跳过(BoxHub 控制台不受影响)"
        warn "  走镜像:BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install"
        warn "  手动补:下载 dist.zip 解压到 $dest(保留 index.html 在根)后刷新内核面板"
        return 0
    fi
    local work="$tmp/zashboard"
    rm -rf "$work"
    if ! unzip_archive "$zip" "$work"; then
        warn "zashboard 解压失败(缺少 unzip/bsdtar/python3),已跳过"
        warn "  手动补:apt-get install -y unzip 后重跑安装,或自行解压 $url 到 $dest"
        return 0
    fi
    # dist.zip 里带一层 dist/ 前缀,直接铺到 /ui 根会让 /ui/index.html 变 404
    if [ -d "$work/dist" ] && [ -f "$work/dist/index.html" ]; then
        work="$work/dist"
    fi
    # 校验产物完整再动安装目录:任何一步失败都不碰现有面板
    if [ ! -f "$work/index.html" ]; then
        warn "zashboard 包里没有 index.html,已跳过(不覆盖现有面板)"
        return 0
    fi
    if [ ! -d "$work/assets" ] && [ -z "$(find "$work" -name '*.js' -print -quit 2>/dev/null)" ]; then
        warn "zashboard 包里没有 JS 资源,已跳过(不覆盖现有面板)"
        return 0
    fi
    rm -rf "$dest"
    mkdir -p "$dest" || { warn "创建 $dest 失败,已跳过"; return 0; }
    if ! cp -a "$work/." "$dest/"; then
        warn "复制 zashboard 失败,已跳过"
        return 0
    fi
    # zashboard 是纯 SPA:后端地址来自入口 URL 的 query string(由 boxd 生成),
    # 不需要 config.js。这里只清掉 PWA/Service Worker,避免旧缓存把 /ui 打成 404。
    strip_pwa "$dest"
    info "zashboard 已安装 → $dest"
}


backup_existing() {
    local dest=$1
    if [ -d "$DATA_DIR" ]; then
        mkdir -p "$dest"
        cp -a "$DATA_DIR" "$dest/" 2>/dev/null || true
        info "已备份现有配置到 $dest"
    fi
}

restore_existing() {
    local backup=$1
    if [ -d "$backup/data" ]; then
        rm -rf "${DATA_DIR:?}"
        cp -a "$backup/data" "$DATA_DIR" || return 1
        info "已恢复配置"
    fi
}

write_service() {
    step_stdout "创建系统服务"
    mkdir -p /etc/systemd/system /etc/init.d 2>/dev/null || true

    if [ "$INIT" = "systemd" ]; then
        cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=BoxHub - 代理控制台(sing-box)
Documentation=https://github.com/BoxHub-dev/boxhub
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DIR}/boxd -data ${DATA_DIR} -listen 0.0.0.0:${PANEL_PORT} -sing-box ${BIN_DIR}/sing-box
Restart=on-failure
RestartSec=3
# 内核需要管理 TUN 接口与 nftables 规则
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE CAP_NET_RAW
LimitNOFILE=1048576
# 注意:这里刻意不加 ProtectSystem / PrivateTmp / ReadWritePaths。
#   - LXC 等容器里 ProtectSystem=full 会让内核无法写 /run 与规则集缓存;
#   - PrivateTmp=true 会让 manage.sh 与 boxd 看到不同的 /tmp,
#     导致"诊断说配置坏了但面板一切正常"这类误导。
# 服务本身以 root 运行在受控主机上,这些加固带来的收益有限、兼容成本高。

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload || die "systemctl daemon-reload 失败"
        systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || warn "设置开机自启失败(可稍后手动执行 systemctl enable $SERVICE_NAME)"
        info "systemd 服务已创建"
    else
        cat > "/etc/init.d/${SERVICE_NAME}" <<EOF
#!/sbin/openrc-run
name="boxhub"
description="BoxHub - 代理控制台(sing-box)"
command="${BIN_DIR}/boxd"
command_args="-data ${DATA_DIR} -listen 0.0.0.0:${PORT_PLACEHOLDER} -sing-box ${BIN_DIR}/sing-box"
command_background="yes"
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="/var/log/boxhub.log"
error_log="/var/log/boxhub.log"
supervisor="supervise-daemon"
respawn_delay=3
depend() { need net; after firewall; }
EOF
        sed -i "s/\${PORT_PLACEHOLDER}/${PANEL_PORT}/" "/etc/init.d/${SERVICE_NAME}"
        chmod +x "/etc/init.d/${SERVICE_NAME}"
        rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 || warn "设置开机自启失败"
        info "OpenRC 服务已创建"
    fi
}

service_start() {
    if [ "$INIT" = "unknown" ]; then
        error "当前系统不支持服务管理(需要 systemd 或 OpenRC)"
        return 1
    fi
    if [ "$INIT" = "systemd" ]; then
        if ! systemctl restart "$SERVICE_NAME"; then
            systemctl status "$SERVICE_NAME" --no-pager -l | tail -n 20
            return 1
        fi
    else
        rc-service "$SERVICE_NAME" restart || return 1
    fi
}

service_stop() {
    case "$INIT" in
        systemd|openrc) ;;
        *) return 0 ;;
    esac
    if [ "$INIT" = "systemd" ]; then
        systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    else
        rc-service "$SERVICE_NAME" stop 2>/dev/null || true
    fi
}

service_running() {
    case "$INIT" in
        systemd|openrc) ;;
        *) return 1 ;;
    esac
    if [ "$INIT" = "systemd" ]; then
        systemctl is-active --quiet "$SERVICE_NAME"
    else
        rc-service "$SERVICE_NAME" status >/dev/null 2>&1
    fi
}

# 判断是否运行在容器里(LXC / Docker)。容器对 TUN 与内核模块的支持与物理机不同,
# 装之前就说清楚,避免装完发现透明代理用不了。
detect_container() {
    IS_CONTAINER=0
    CONTAINER_TYPE=""
    if [ -f /run/systemd/container ]; then
        IS_CONTAINER=1
        CONTAINER_TYPE=$(cat /run/systemd/container 2>/dev/null)
    elif [ -f /.dockerenv ]; then
        IS_CONTAINER=1
        CONTAINER_TYPE="docker"
    elif [ -f /proc/1/environ ] && tr '\0' '\n' </proc/1/environ 2>/dev/null | grep -q '^container=lxc'; then
        IS_CONTAINER=1
        CONTAINER_TYPE="lxc"
    elif grep -qaE 'container=lxc|container=lxd' /proc/1/environ 2>/dev/null; then
        IS_CONTAINER=1
        CONTAINER_TYPE="lxc"
    fi
    # LXD / Incus 的容器还会在 cgroup 里留下痕迹
    if [ "$IS_CONTAINER" = "0" ]; then
        if grep -qaE '(lxd|incus)' /proc/1/cgroup 2>/dev/null; then
            IS_CONTAINER=1
            CONTAINER_TYPE="lxc"
        fi
    fi
    [ -n "$CONTAINER_TYPE" ] || CONTAINER_TYPE="host"
    return 0
}

# 检查 TUN 是否可用(在容器里通常是不可用的)
tun_available() {
    [ -c /dev/net/tun ] || return 1
    # 设备节点存在不代表能用:容器需要额外放行 capabilities 与设备
    if [ -r /dev/net/tun ] && [ -w /dev/net/tun ]; then
        return 0
    fi
    return 1
}

get_lan_ip() {
    local ip=""
    if command -v ip >/dev/null 2>&1; then
        ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' | head -n1)
    fi
    if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
        ip=$(hostname -I 2>/dev/null | awk '{print $1}')
    fi
    echo "${ip:-127.0.0.1}"
}

open_firewall() {
    # 尽量打开面板与代理端口;失败不影响安装
    [ "$INIT" = "systemd" ] || return 0
    local port
    for port in "$PANEL_PORT" 7890 9090; do
        if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
            ufw allow "$port/tcp" >/dev/null 2>&1 && info "已放行防火墙端口 $port"
        elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
            firewall-cmd --permanent --add-port="$port/tcp" >/dev/null 2>&1 && info "已放行防火墙端口 $port"
        fi
    done
    command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --reload >/dev/null 2>&1 || true
}

do_install() {
    require_root
    require_supported_platform
    detect_container
    detect_downloader
    detect_existing_port
    # 端口冲突要在写服务文件之前处理掉:默认 80 经常撞上 nginx
    resolve_panel_port
    if [ -n "$EXISTING_PORT" ] && [ "$PANEL_PORT" = "$EXISTING_PORT" ] && [ "$PORT_EXPLICIT" != "1" ]; then
        info "面板端口: $PANEL_PORT(沿用已安装的端口,改端口用 BOXHUB_PORT=xx)"
    else
        info "面板端口: $PANEL_PORT"
    fi
    # 内网环境:apt 与 GitHub 都可能不通,提前确认关键工具
    if ! command -v tar >/dev/null 2>&1 || ! command -v gzip >/dev/null 2>&1; then
        die "缺少 tar/gzip,请先安装"
    fi

    if [ "$IS_CONTAINER" = "1" ]; then
        info "检测到容器环境($CONTAINER_TYPE)"
        if ! tun_available; then
            warn "容器内 /dev/net/tun 不可用:TUN 透明代理将无法使用"
            warn "不影响使用 —— 通过混合端口 7890 给局域网设备提供 HTTP/SOCKS5 代理即可"
            echo
        fi
    fi
    install_deps

    step "准备安装"
    local tmp backup
    tmp=$(mktemp -d) || die "无法创建临时目录"
    # shellcheck disable=SC2064
    trap "rm -rf '$tmp'" EXIT

    # ---- 下载阶段:全部在临时目录完成 ----
    local singbox_bin boxd_asset boxd_url
    singbox_bin=$(install_singbox "$tmp") || die "内核下载失败"

    step "下载 BoxHub 面板"
    if [ -n "${BOXHUB_LOCAL_BOXD:-}" ]; then
        # 本地安装(开发/离线部署)
        [ -f "$BOXHUB_LOCAL_BOXD" ] || die "指定的 boxd 不存在: $BOXHUB_LOCAL_BOXD"
        cp "$BOXHUB_LOCAL_BOXD" "$tmp/boxd"
        info "使用本地文件: $BOXHUB_LOCAL_BOXD"
    else
        if [ -n "${BOXHUB_RELEASE_BASE:-}" ]; then
            # 显式指定了发布源:行为保持单一来源,不做多源回退
            [ -n "$RELEASE_BASE" ] || RELEASE_BASE="$BOXHUB_RELEASE_BASE"
            boxd_url=$(gh_url "${RELEASE_BASE}/boxd-linux-${ARCH}")
            info "地址: $boxd_url"
            if ! fetch "$boxd_url" "$tmp/boxd"; then
                warn "BoxHub 下载失败: $boxd_url"
                die "BoxHub 面板下载失败"
            fi
        elif ! try_sources "$tmp/boxd" "https://github.com/${GITHUB_REPO}/releases/latest/download/boxd-linux-${ARCH}"; then
            warn "BoxHub 下载失败(镜像与直连都试过)。可选办法:"
            warn "  1) 指定镜像: BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install"
            warn "  2) 离线: BOXHUB_LOCAL_BOXD=/path/to/boxd bash manage.sh install"
            die "BoxHub 面板下载失败"
        fi
    fi
    chmod +x "$tmp/boxd"
    "$tmp/boxd" -version >/dev/null 2>&1 || die "下载的 boxd 无法运行,可能是架构不匹配"

    # ---- 备份 ----
    backup=$(mktemp -d) || die "无法创建备份目录"
    backup_existing "$backup"

    # ---- 部署 ----
    step "部署到 $INSTALL_DIR"
    mkdir -p "$BIN_DIR" "$DATA_DIR" || die "创建目录失败"
    install -m 0755 "$singbox_bin" "$BIN_DIR/sing-box" || die "安装内核失败"
    install -m 0755 "$tmp/boxd" "$BIN_DIR/boxd" || die "安装面板失败"
    install -m 0755 "$tmp/boxd" "$INSTALL_DIR/boxd" 2>/dev/null || true
    # 管理脚本自身也留一份,方便以后直接调用
    if [ -f "$0" ] && [ -r "$0" ]; then
        cp "$0" "$INSTALL_DIR/manage.sh" 2>/dev/null && chmod +x "$INSTALL_DIR/manage.sh" || true
    fi
    install_clash_ui "$tmp"

    # ---- 服务 ----
    write_service || { restore_existing "$backup"; die "服务创建失败,已回滚配置"; }
    step "启动服务"
    if ! service_start; then
        error "服务启动失败"
        restore_existing "$backup"
        die "已回滚。可查看日志: journalctl -u ${SERVICE_NAME} -n 50"
    fi

    # 等面板就绪
    local i
    for i in $(seq 1 20); do
        if fetch "http://127.0.0.1:${PANEL_PORT}/api/session" >/dev/null 2>&1; then
            break
        fi
        sleep 0.5
    done
    open_firewall
    rm -rf "$backup"
    show_access_info
}

# ==================== 管理 ====================
show_access_info() {
    local ip
    ip=$(get_lan_ip)
    echo
    printf '%s╔══════════════════════════════════════════════╗%s\n' "$GREEN$BOLD" "$NC"
    printf '%s║          BoxHub 安装完成                     ║%s\n' "$GREEN$BOLD" "$NC"
    printf '%s╚══════════════════════════════════════════════╝%s\n' "$GREEN$BOLD" "$NC"
    echo
    printf '  面板地址: %s%s%s\n' "$BLUE" "$(panel_url "$ip")" "$NC"
    printf '  本机访问: %s%s%s\n' "$BLUE" "$(panel_url 127.0.0.1)" "$NC"
    printf '  内核面板: %s%s/ui/%s\n' "$BLUE" "$(panel_url "$ip")" "$NC"
    echo
    printf '  %s首次使用请打开面板设置访问密码%s\n' "$YELLOW" "$NC"
    echo
    printf '  安装目录: %s\n' "$INSTALL_DIR"
    printf '  数据目录: %s\n' "$DATA_DIR"
    printf '  面板端口: %s\n' "$PANEL_PORT"
    printf '  代理端口: 7890 (混合 HTTP/SOCKS5)\n'
    echo
    printf '  常用命令:\n'
    if [ "$INIT" = "systemd" ]; then
        printf '    启动   systemctl start %s\n' "$SERVICE_NAME"
        printf '    停止   systemctl stop %s\n' "$SERVICE_NAME"
        printf '    重启   systemctl restart %s\n' "$SERVICE_NAME"
        printf '    日志   journalctl -u %s -f\n' "$SERVICE_NAME"
    else
        printf '    启动   rc-service %s start\n' "$SERVICE_NAME"
        printf '    停止   rc-service %s stop\n' "$SERVICE_NAME"
        printf '    重启   rc-service %s restart\n' "$SERVICE_NAME"
    fi
    printf '    管理   bash %s/manage.sh\n' "$INSTALL_DIR"
    echo
}

do_status() {
    detect_os 2>/dev/null || true
    echo
    printf '%sBoxHub 状态%s\n' "$BOLD" "$NC"
    echo "────────────────────────────────────────"
    if [ -x "$BIN_DIR/boxd" ]; then
        printf '面板版本: %s\n' "$("$BIN_DIR/boxd" -version 2>/dev/null | awk '{print $2}')"
    else
        printf '面板版本: %s未安装%s\n' "$RED" "$NC"
    fi
    if [ -x "$BIN_DIR/sing-box" ]; then
        printf '内核版本: %s\n' "$("$BIN_DIR/sing-box" version 2>/dev/null | head -n1 | awk '{print $3}')"
    else
        printf '内核版本: %s无%s\n' "$RED" "$NC"
    fi
    if service_running; then
        printf '服务状态: %s运行中%s\n' "$GREEN" "$NC"
    else
        printf '服务状态: %s未运行%s\n' "$RED" "$NC"
    fi
    # 面板端口以服务文件里实际写的为准(老机器可能停在 3036)
    detect_existing_port
    printf '面板端口: %s(%s)\n' "$PANEL_PORT" "$(panel_url "$(get_lan_ip)")"
    if [ -f "$DATA_DIR/ui/index.html" ]; then
        printf '内核面板: %szashboard 已安装%s\n' "$GREEN" "$NC"
    else
        printf '内核面板: %s未安装(bash manage.sh install 可补装)%s\n' "$YELLOW" "$NC"
    fi
    printf '安装目录: %s\n' "$INSTALL_DIR"
    if [ -f "$DATA_DIR/config.json" ]; then
        local counts
        counts=$(python3 - "$DATA_DIR/config.json" <<'PY' 2>/dev/null || true
import json,sys
d=json.load(open(sys.argv[1]))
print(f"节点 {len(d.get('nodes',[]))} 个 · 链式代理 {len(d.get('chains',[]))} 条 · 节点组 {len(d.get('groups',[]))} 个 · 分流规则 {len(d.get('policies',[]))} 条")
PY
)
        [ -n "$counts" ] && printf '配置统计: %s\n' "$counts"
    fi
    echo "────────────────────────────────────────"
    # 端口监听
    if command -v ss >/dev/null 2>&1; then
        echo "监听端口:"
        ss -lntp 2>/dev/null | grep -E ":(${PANEL_PORT}|7890|9090)\b" | awk '{printf "  %s\n", $4}' || true
    fi
    echo
}

do_logs() {
    local lines=${1:-100}
    if [ "$INIT" = "systemd" ] && command -v journalctl >/dev/null 2>&1; then
        journalctl -u "$SERVICE_NAME" -n "$lines" --no-pager
    else
        tail -n "$lines" /var/log/boxhub.log 2>/dev/null || warn "找不到日志"
    fi
    if [ -f "$DATA_DIR/kernel.log" ]; then
        echo
        echo "──── 内核日志(最近 $lines 行)────"
        tail -n "$lines" "$DATA_DIR/kernel.log"
    fi
}

do_diagnose() {
    # 必须先探测系统类型,否则 INIT 为空会让 service_running 一律判定"未运行"
    detect_os
    detect_arch
    # 端口以服务文件里的为准:默认已是 80,老机器可能还是 3036,
    # 不探测就会误报"面板无响应"
    detect_existing_port
    echo
    printf '%sBoxHub 诊断%s\n' "$BOLD" "$NC"
    echo "════════════════════════════════════════"
    echo "1) 系统信息"
    printf '   系统: %s\n' "$(grep PRETTY_NAME /etc/os-release 2>/dev/null | cut -d= -f2- | tr -d '\"')"
    printf '   内核: %s\n' "$(uname -r)"
    printf '   架构: %s\n' "$(uname -m)"
    printf '   内存: %s\n' "$(free -h 2>/dev/null | awk '/^Mem:/{print $2" (可用 "$7")"}')"
    echo
    echo "2) 安装文件"
    for f in "$BIN_DIR/boxd" "$BIN_DIR/sing-box" "$DATA_DIR/config.json" "$DATA_DIR/core.json"; do
        if [ -e "$f" ]; then
            printf '   %s %s\n' "$GREEN✓$NC" "$f"
        else
            printf '   %s %s (缺失)\n' "$RED✗$NC" "$f"
        fi
    done
    # 内核面板是可选组件:没装不影响控制台,但"内核面板打不开"多半就是它
    if [ -f "$DATA_DIR/ui/index.html" ]; then
        printf '   %s %s (内核面板 zashboard)\n' "$GREEN✓$NC" "$DATA_DIR/ui"
    else
        printf '   %s %s (未装:GitHub 下载不通时会被跳过;manage.sh help 里有补装方法)\n' "$YELLOW!$NC" "$DATA_DIR/ui"
    fi
    echo
    echo "3) 服务状态"
    if service_running; then
        printf '   %s 服务运行中\n' "$GREEN✓$NC"
    else
        printf '   %s 服务未运行\n' "$RED✗$NC"
        if [ "$INIT" = "systemd" ]; then
            echo "   最近错误:"
            journalctl -u "$SERVICE_NAME" -n 8 --no-pager 2>/dev/null | sed 's/^/     /'
        fi
    fi
    echo
    echo "4) 端口监听"
    if command -v ss >/dev/null 2>&1; then
        ss -lntp 2>/dev/null | grep -E ":(${PANEL_PORT}|7890|9090)\b" | sed 's/^/   /' || echo "   端口未监听"
    fi
    echo
    echo "5) 配置校验"
    if [ -f "$DATA_DIR/core.json" ] && [ -x "$BIN_DIR/sing-box" ]; then
        local out
        if out=$("$BIN_DIR/sing-box" check -c "$DATA_DIR/core.json" -D "$DATA_DIR" 2>&1); then
            printf '   %s 内核配置合法\n' "$GREEN✓$NC"
        else
            printf '   %s 内核配置有问题:\n' "$RED✗$NC"
            echo "$out" | sed 's/^/     /'
        fi
    fi
    echo
    echo "6) 面板接口"
    if fetch "http://127.0.0.1:${PANEL_PORT}/api/session" >/dev/null 2>&1; then
        printf '   %s 面板响应正常\n' "$GREEN✓$NC"
    else
        printf '   %s 面板无响应\n' "$RED✗$NC"
    fi
    echo
    echo "7) 运行环境与 TUN"
    detect_container
    if [ "$IS_CONTAINER" = "1" ]; then
        printf '   容器: %s\n' "$CONTAINER_TYPE"
    else
        printf '   容器: 否(物理机或虚拟机)\n'
    fi
    if tun_available; then
        printf '   %s /dev/net/tun 可用(可开启 TUN 透明代理)\n' "$GREEN✓$NC"
    else
        printf '   %s TUN 不可用(容器需额外放行;不影响端口代理)\n' "$YELLOW!$NC"
    fi
    echo "════════════════════════════════════════"
    echo
}

do_uninstall() {
    require_root
    detect_os
    echo
    warn "即将卸载 BoxHub"
    echo "  安装目录: $INSTALL_DIR"
    echo "  配置数据: $DATA_DIR"
    echo
    if [ "${BOXHUB_FORCE:-0}" != "1" ]; then
        read -r -p "是否保留配置文件备份? [Y/n]: " keep
        case "$keep" in
            [nN]*) KEEP=0 ;;
            *) KEEP=1 ;;
        esac
        read -r -p "确认卸载?输入 YES 继续: " confirm
        [ "$confirm" = "YES" ] || { info "已取消"; return 0; }
    else
        KEEP=${BOXHUB_KEEP_DATA:-0}
    fi

    step_stdout "停止服务"
    service_stop

    step_stdout "移除服务定义"
    if [ "$INIT" = "systemd" ]; then
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
        rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
        systemctl daemon-reload 2>/dev/null || true
    else
        rc-update del "$SERVICE_NAME" >/dev/null 2>&1 || true
        rm -f "/etc/init.d/${SERVICE_NAME}"
    fi

    if [ "$KEEP" = "1" ] && [ -d "$DATA_DIR" ]; then
        local dest="/root/boxhub-backup-$(date +%Y%m%d-%H%M%S)"
        step_stdout "备份配置到 $dest"
        mkdir -p "$dest"
        cp -a "$DATA_DIR" "$dest/" 2>/dev/null || warn "备份失败"
        info "配置已备份: $dest"
    fi

    step_stdout "清理文件"
    # 只删自己创建的目录,避免误删用户放在同一路径下的其他文件
    rm -rf "$BIN_DIR" 2>/dev/null || true
    if [ "$KEEP" = "0" ]; then
        rm -rf "$DATA_DIR" 2>/dev/null || true
    fi
    # 安装目录为空才删
    if [ -d "$INSTALL_DIR" ] && [ -z "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
        rmdir "$INSTALL_DIR" 2>/dev/null || true
    fi
    info "BoxHub 已卸载"
    [ "$KEEP" = "1" ] && info "配置保留在 $dest,重新安装后可用面板「导入配置」恢复"
    return 0
}

do_restart()  { require_root; step_stdout "重启服务"; service_start && info "已重启" || error "重启失败"; }
do_stop()     { require_root; step_stdout "停止服务"; service_stop && info "已停止"; }
do_start()    { require_root; step_stdout "启动服务"; service_start && info "已启动" || error "启动失败"; }

do_upgrade() {
    # 升级 = 重新安装(配置会被保留:install 只覆盖 bin/ 与可重新下载的资源)
    step_stdout "升级 BoxHub"
    do_install
}

# ==================== 菜单 ====================
show_menu() {
    clear 2>/dev/null || true
    echo
    printf '%s══════════════════════════════════════════════%s\n' "$CYAN" "$NC"
    printf '%s            BoxHub 安装管理脚本 v%s%s\n' "$BOLD" "$VERSION" "$NC"
    printf '%s══════════════════════════════════════════════%s\n' "$CYAN" "$NC"
    detect_os 2>/dev/null || true
    printf '系统: %s / 架构: %s\n' "${PRETTY_NAME:-未知}" "$(uname -m)"
    if [ -x "$BIN_DIR/boxd" ]; then
        printf '状态: %s已安装%s' "$GREEN" "$NC"
        service_running && printf ' %s(运行中)%s' "$GREEN" "$NC"
        echo
    else
        printf '状态: %s未安装%s\n' "$YELLOW" "$NC"
    fi
    echo "──────────────────────────────────────────────"
    printf '  %s安装与升级%s\n' "$BOLD" "$NC"
    echo "   1. 安装 / 升级 BoxHub"
    echo
    printf '  %s服务管理%s\n' "$BOLD" "$NC"
    echo "   2. 查看状态"
    echo "   3. 启动服务"
    echo "   4. 停止服务"
    echo "   5. 重启服务"
    echo "   6. 查看日志"
    echo
    printf '  %s维护%s\n' "$BOLD" "$NC"
    echo "   7. 系统诊断"
    echo "   8. 备份配置"
    printf '   %s9. 卸载 BoxHub%s\n' "$RED" "$NC"
    echo
    echo "   0. 退出"
    echo "──────────────────────────────────────────────"
    read -r -p "请输入选项 [0-9]: " choice
    case "$choice" in
        1) do_install ;;
        2) do_status ;;
        3) do_start ;;
        4) do_stop ;;
        5) do_restart ;;
        6) do_logs ;;
        7) do_diagnose ;;
        8) backup_config ;;
        9) do_uninstall ;;
        0) exit 0 ;;
        *) error "无效选项" ;;
    esac
    echo
    read -r -p "按回车返回菜单..." _
    show_menu
}

backup_config() {
    require_root
    [ -d "$DATA_DIR" ] || die "没有可备份的数据目录"
    local dest="/root/boxhub-backup-$(date +%Y%m%d-%H%M%S).tar.gz"
    tar -czf "$dest" -C "$(dirname "$DATA_DIR")" "$(basename "$DATA_DIR")" 2>/dev/null || die "打包失败"
    info "已备份到 $dest"
}

# ==================== 帮助 ====================
show_help() {
    cat <<EOF
BoxHub 安装管理脚本 v${VERSION}

用法: bash manage.sh [命令]

命令:
  install            安装或升级(自动检测)
  upgrade            升级
  uninstall          卸载(会询问是否保留配置备份)
  start              启动服务
  stop               停止服务
  restart            重启服务
  status             查看状态
  logs [行数]        查看日志(默认 100 行)
  diagnose           系统诊断(排查故障用)
  backup             备份配置
  help               显示本帮助

环境变量:
  BOXHUB_DIR                 安装目录(默认 /opt/boxhub)
  BOXHUB_PORT                面板端口(默认 80;80 被占用时自动退回 3036。
                             已经装过的机器默认沿用原端口,要用这个变量才会改)
  BOXHUB_NO_CLASH_UI=1       跳过内核面板 zashboard 的下载(离线/内网部署)
  BOXHUB_ZASHBOARD_URL       内核面板发行包地址(默认 GitHub 最新 dist.zip;
                             可锁版本、指向自建源或本地 file:// 路径)
  BOXHUB_SINGBOX_VERSION     指定 sing-box 版本(如 v1.14.4)
  BOXHUB_REPO                面板下载仓库(默认内置)
  BOXHUB_GH_MIRROR           GitHub 下载镜像前缀(如 https://gh-proxy.com)
  BOXHUB_LOCAL_BOXD          使用本地 boxd 文件安装(离线部署)
  BOXHUB_LOCAL_SINGBOX       使用本地 sing-box 内核(离线部署)
  BOXHUB_FORCE=1             卸载时不交互
  BOXHUB_KEEP_DATA=1         卸载时保留配置(配合 FORCE)

内核面板(zashboard)手动补装:
  # 发行包是 Release 资产 dist.zip,解压后把里面的 dist/ 内容铺到 data/ui/,
  # 保证 data/ui/index.html 存在(带一层 dist/ 会 404)。
  curl -fsSL -o zashboard.zip https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip
  unzip -o zashboard.zip -d /tmp/zab && mkdir -p /opt/boxhub/data/ui
  cp -a /tmp/zab/dist/. /opt/boxhub/data/ui/
  # 国内直连 GitHub 不通时,整体走镜像:BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install

示例:
  sudo bash manage.sh install
  sudo BOXHUB_PORT=8080 bash manage.sh install
  sudo BOXHUB_LOCAL_BOXD=./boxd-linux-amd64 bash manage.sh install
  bash manage.sh diagnose

不带参数运行会显示交互式菜单。
EOF
}

# ==================== 入口 ====================
main() {
    local action=${1:-}
    local arg2=${2:-}
    case "$action" in
        install|upgrade)   do_install ;;
        uninstall|remove)  do_uninstall ;;
        start)             do_start ;;
        stop)              do_stop ;;
        restart)           do_restart ;;
        status)            do_status ;;
        logs|log)          do_logs "${arg2:-100}" ;;
        diagnose|diag)     do_diagnose ;;
        backup)            backup_config ;;
        help|--help|-h)    show_help ;;
        '')                show_menu ;;
        *)                 error "未知命令: $action"; echo; show_help; exit 1 ;;
    esac
}

main "$@"

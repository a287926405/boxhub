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

VERSION="1.12.4"   # build.sh 打包时会被覆盖成实际发布版本

# ==================== 配置 ====================
INSTALL_DIR="${BOXHUB_DIR:-/opt/boxhub}"
DATA_DIR="${INSTALL_DIR}/data"
BIN_DIR="${INSTALL_DIR}/bin"
SERVICE_NAME="boxhub"
PANEL_PORT="${BOXHUB_PORT:-3036}"
# 已装过的机器:未显式指定端口时沿用服务文件里现存的端口。
# 否则用户把面板改到 80 后,一次 install 升级就被默认 3036 打回原形。
detect_existing_port() {
    local svc=""
    if [ "$INIT" = "systemd" ]; then
        svc="/etc/systemd/system/${SERVICE_NAME}.service"
    else
        svc="/etc/init.d/${SERVICE_NAME}"
    fi
    [ -f "$svc" ] || return 0
    local cur
    # 从 -listen 0.0.0.0:PORT / [::]:PORT 提取端口
    cur=$(grep -oE '\-listen [^ ]*[:%][0-9]+' "$svc" 2>/dev/null | grep -oE '[0-9]+$' | head -n1)
    if [ -n "$cur" ] && [ "$cur" != "3036" ]; then
        # 只有现存端口不是默认值(即用户改过)才沿用;
        # 从未改过的机器维持 BOXHUB_PORT/3036 语义
        PANEL_PORT="$cur"
        info "沿用已安装的端口: $PANEL_PORT(用 BOXHUB_PORT=xx 可覆盖)" >&2
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
METACUBEXD_REPO="MetaCubeX/metacubexd"

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

# resolve_latest_tag 取仓库最新 release 的 tag。
# 先走 API(一次请求拿到准确版本号,不依赖页面结构);
# 被限流时退回抓 releases/latest 跳转 HTML 里的 tag 链接。
resolve_latest_tag() {
    local repo=$1 tag=""
    tag=$(fetch "https://api.github.com/repos/${repo}/releases/latest" 2>/dev/null \
        | grep -oE '"tag_name":[[:space:]]*"[^"]+"' | head -n1 \
        | sed 's/.*"\([^"]*\)"$/\1/') || true
    if [ -z "$tag" ]; then
        tag=$(fetch "https://github.com/${repo}/releases/latest" 2>/dev/null \
            | grep -oE '/releases/tag/v[0-9.]+' | head -n1 | sed 's|.*/||') || true
    fi
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
    local url
    url=$(gh_url "https://github.com/${SINGBOX_REPO}/releases/download/${tag}/sing-box-${tag#v}-linux-${ARCH}.tar.gz")
    info "版本: $tag" >&2
    info "地址: $url" >&2
    if ! fetch "$url" "$tmp/singbox.tar.gz"; then
        warn "sing-box 下载失败(国内直连 GitHub 常见)。可选办法:"
        warn "  1) 走镜像: BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install"
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

# 下载 metacubexd(可选,失败不影响主功能)
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
        sed -i 's|<script[^>]*workbox[^>]*></script>||g; s|<script[^>]*registerSW[^>]*></script>||g' "$dir/index.html" 2>/dev/null || true
    fi
    rm -f "$dir"/workbox-*.js 2>/dev/null || true
}

install_clash_ui() {
    # 可选项:内网/受限网络下载不通时不能卡住整个安装
    if [ "${BOXHUB_NO_CLASH_UI:-0}" = "1" ]; then
        info "已跳过 metacubexd(BOXHUB_NO_CLASH_UI=1)"
        return 0
    fi
    step_stdout "下载内核面板 metacubexd(可选)"
    local tmp=$1 dest
    dest="${DATA_DIR}/ui"
    local url="https://github.com/${METACUBEXD_REPO}/releases/latest/download/compressed-dist.tgz"
    # 60 秒拿不到就跳过
    if ! timeout 60 sh -c "if [ \"$DL\" = curl ]; then curl -fsSL --connect-timeout 15 --max-time 55 -o '$tmp/ui.tgz' '$url'; else wget -q --timeout=15 --tries=1 -O '$tmp/ui.tgz' '$url'; fi" 2>/dev/null; then
        warn "metacubexd 下载失败,跳过(BoxHub 面板不受影响;补装方法见 manage.sh help)"
        return 0
    fi
    if fetch "$url" "$tmp/ui.tgz" 2>/dev/null; then
        mkdir -p "$tmp/ui" && tar -xzf "$tmp/ui.tgz" -C "$tmp/ui" 2>/dev/null || {
            warn "metacubexd 解压失败,跳过"
            return 0
        }
        rm -rf "$dest"
        mkdir -p "$dest"
        cp -a "$tmp/ui/." "$dest/" || { warn "复制失败,跳过"; return 0; }
        # 让 metacubexd 默认连到本机 Clash API
        # 后端走 BoxHub 面板的同源反代(/clash),避免跨域与额外放行 9090。
        # 注意:这里必须用 <<'EOF' 原样写入,不要让 shell 展开 $ 变量。
        cat > "$dest/config.js" <<'EOF'
window.__METACUBEXD_CONFIG__ = {
  defaultBackendURL: window.location.origin + "/clash",
  githubToken: "",    strip_pwa "$dest"
}
EOF
        info "metacubexd 已安装"
    else
        warn "metacubexd 下载失败,跳过(BoxHub 面板不受影响)"
    fi
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
        [ -n "$RELEASE_BASE" ] || RELEASE_BASE="https://github.com/${GITHUB_REPO}/releases/latest/download"
        boxd_url=$(gh_url "${RELEASE_BASE}/boxd-linux-${ARCH}")
        info "地址: $boxd_url"
        if ! fetch "$boxd_url" "$tmp/boxd"; then
            warn "BoxHub 下载失败。可选办法:"
            warn "  1) 走镜像: BOXHUB_GH_MIRROR=https://gh-proxy.com bash manage.sh install"
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
    printf '  面板地址: %shttp://%s:%s%s\n' "$BLUE" "$ip" "$PANEL_PORT" "$NC"
    printf '  本机访问: %shttp://127.0.0.1:%s%s\n' "$BLUE" "$PANEL_PORT" "$NC"
    printf '  内核面板: %shttp://%s:%s/ui/%s\n' "$BLUE" "$ip" "$PANEL_PORT" "$NC"
    echo
    printf '  %s首次使用请打开面板设置访问密码%s\n' "$YELLOW" "$NC"
    echo
    printf '  安装目录: %s\n' "$INSTALL_DIR"
    printf '  数据目录: %s\n' "$DATA_DIR"
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
  BOXHUB_PORT                面板端口(默认 3036)
  BOXHUB_SINGBOX_VERSION     指定 sing-box 版本(如 v1.14.4)
  BOXHUB_REPO                面板下载仓库(默认内置)
  BOXHUB_LOCAL_BOXD          使用本地 boxd 文件安装(离线部署)
  BOXHUB_FORCE=1             卸载时不交互
  BOXHUB_KEEP_DATA=1         卸载时保留配置(配合 FORCE)

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

#!/bin/bash
# =============================================================================
# BoxHub 一键安装
#
#   curl -fsSL https://raw.githubusercontent.com/a287926405/boxhub/main/install.sh | bash
#
# 本脚本只负责把 manage.sh 取下来并转交全部参数,真正的安装/升级/卸载
# 逻辑都在 scripts/manage.sh 里(可单独下载运行,便于离线与二次运维)。
#
# 常用变体:
#   # 换端口(默认 80;80 被 nginx 之类占用时安装脚本会退回 3036)
#   BOXHUB_PORT=8080 curl -fsSL <本地址> | bash
#   # 国内直连 GitHub 不通时走镜像
#   BOXHUB_GH_MIRROR=https://gh-proxy.com curl -fsSL <本地址> | bash
#   # 装完直接进管理菜单
#   curl -fsSL <本地址> | bash -s -- menu
# =============================================================================

set -e

REPO="${BOXHUB_REPO:-a287926405/boxhub}"
BRANCH="${BOXHUB_BRANCH:-main}"
BASE="https://github.com/${REPO}/releases/latest/download"
RAW="https://raw.githubusercontent.com/${REPO}/${BRANCH}"

# ------------------------------------------------------------------
# 下载源策略:纯国内网络也要能装。
#
# 候选前缀顺序:
#   1. BOXHUB_GH_MIRROR(用户显式指定,最优先,支持逗号分隔多个)
#   2. 直连 GitHub(境外/有代理的机器一次命中)
#   3. 内置公共镜像(gh-proxy 类,国内直连不通时的兜底;实测可用性
#      变动频繁,失效的会被淘汰,也欢迎用 BOXHUB_GH_MIRROR 指定自建源)
# ------------------------------------------------------------------
MIRRORS_DEFAULT="https://gh-proxy.com https://gh-proxy.net https://gh.ddlc.top"

fetch() {
    # fetch URL 输出文件
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 12 --max-time 120 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --timeout=15 --tries=1 -O "$2" "$1"
    else
        echo "需要 curl 或 wget:apt-get install -y curl 或 yum install -y curl" >&2
        exit 1
    fi
}

# github_direct 探测直连 GitHub 是否可达(5 秒内出结果,避免国内机器干等超时)
github_direct() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsI --connect-timeout 5 --max-time 8 https://github.com >/dev/null 2>&1
    else
        wget -q --timeout=5 --tries=1 -O /dev/null https://github.com 2>/dev/null
    fi
}

# prefix_list 输出候选镜像前缀,空行表示"直连"。
prefix_list() {
    if [ -n "${BOXHUB_GH_MIRROR:-}" ]; then
        printf '%s\n' ${BOXHUB_GH_MIRROR} | sed 's:/*$::'
    elif github_direct; then
        printf '\n%s\n' "$MIRRORS_DEFAULT"
    else
        printf '%s\n' $MIRRORS_DEFAULT
        printf '\n'
    fi
}

# try_sources 尝试"每个前缀 × 每个 URL 候选",成功即把内容写进 $1 并返回 0。
# 用法: try_sources 输出文件 URL1 [URL2 ...]
try_sources() {
    local out=$1; shift
    local prefix u
    while IFS= read -r prefix; do
        for u in "$@"; do
            [ -n "$prefix" ] && u="$prefix/$u"
            if fetch "$u" "$out" 2>/dev/null && [ -s "$out" ] && head -n1 "$out" | grep -q '^#!'; then
                return 0
            fi
        done
    done <<EOF
$(prefix_list)
EOF
    return 1
}

# 先试 release 资产(随版本发布,最稳),失败再退回仓库原始文件。
TMP=$(mktemp -d) || exit 1
trap 'rm -rf "$TMP"' EXIT
MANAGE="$TMP/manage.sh"

if ! try_sources "$MANAGE" "$BASE/manage.sh" "$RAW/scripts/manage.sh"; then
    echo "拿不到 manage.sh(镜像与直连都失败)。" >&2
    echo "若本机只有国内网络,整条命令都走镜像重试(注意 curl 的地址也换了):" >&2
    echo "  BOXHUB_GH_MIRROR=https://gh-proxy.com curl -fsSL https://gh-proxy.com/https://raw.githubusercontent.com/${REPO}/main/install.sh | bash" >&2
    echo "或手动下载 manage.sh 后运行: bash manage.sh install" >&2
    exit 1
fi

chmod +x "$MANAGE"
# 无参数时默认 install(最常见的用法)
if [ "$#" -eq 0 ]; then
    set -- install
fi
exec bash "$MANAGE" "$@"

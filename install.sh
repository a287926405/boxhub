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

# 给下载 URL 加镜像前缀(gh-proxy 类服务:前缀 + 完整 URL)
gh_url() {
    if [ -n "${BOXHUB_GH_MIRROR:-}" ]; then
        printf '%s/%s' "${BOXHUB_GH_MIRROR%/}" "$1"
    else
        printf '%s' "$1"
    fi
}

fetch() {
    # fetch URL 输出文件
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 15 --max-time 120 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --timeout=20 -O "$2" "$1"
    else
        echo "需要 curl 或 wget:apt-get install -y curl 或 yum install -y curl" >&2
        exit 1
    fi
}

# 先试 release 资产(随版本发布,最稳),失败再退回仓库原始文件。
TMP=$(mktemp -d) || exit 1
trap 'rm -rf "$TMP"' EXIT
MANAGE="$TMP/manage.sh"

ok=0
for u in "$(gh_url "$BASE/manage.sh")" "$(gh_url "$RAW/scripts/manage.sh")"; do
    if fetch "$u" "$MANAGE" 2>/dev/null && [ -s "$MANAGE" ] && head -n1 "$MANAGE" | grep -q '^#!/bin/bash'; then
        ok=1
        break
    fi
done

if [ "$ok" != "1" ]; then
    echo "拿不到 manage.sh。" >&2
    echo "若本机直连 GitHub 受限,加镜像前缀重试:" >&2
    echo "  BOXHUB_GH_MIRROR=https://gh-proxy.com bash install.sh" >&2
    echo "或手动下载后运行: bash manage.sh install" >&2
    exit 1
fi

chmod +x "$MANAGE"
# 无参数时默认 install(最常见的用法)
if [ "$#" -eq 0 ]; then
    set -- install
fi
exec bash "$MANAGE" "$@"

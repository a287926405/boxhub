#!/bin/bash
# =============================================================================
# BoxHub 构建脚本:交叉编译 Linux 产物 + 生成发布包
#
# 用法:
#   ./scripts/build.sh               # 构建 release 版
#   ./scripts/build.sh --dev         # 构建开发版(保留调试符号)
#   VERSION=1.0.1 ./scripts/build.sh # 指定版本号
#
# 产物:
#   dist/boxd-linux-amd64      面板二进制 (x86_64)
#   dist/boxd-linux-arm64      面板二进制 (aarch64)
#   dist/boxhub-<ver>.tar.gz   发布包(含 manage.sh 与两个二进制)
# =============================================================================

set -euo pipefail
cd "$(dirname "$0")/.." || exit 1

VERSION="${VERSION:-1.0.0}"
DEV=0
for arg in "$@"; do
    case "$arg" in
        --dev) DEV=1 ;;
        *) echo "未知参数: $arg"; exit 1 ;;
    esac
done

echo "==> BoxHub 构建 v${VERSION}"

# ---- 环境检查 ----
GO=""
for cand in "${GO_BIN:-}" "$(command -v go || true)" /tmp/go/bin/go /usr/local/go/bin/go; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then GO="$cand"; break; fi
done
[ -n "$GO" ] || { echo "错误: 找不到 go 工具链(可用 GO_BIN 指定)"; exit 1; }
echo "    Go: $("$GO" version)"

# ---- 质量门禁 ----
echo "==> 代码检查"
"$GO" vet ./... || { echo "错误: go vet 未通过"; exit 1; }

if [ "$DEV" = "0" ]; then
    echo "==> 运行测试"
    # 有内核就带上真实内核测试;没有就跳过(不阻塞构建)
    SING_BOX_BIN=""
    for cand in "${BOXHUB_SING_BOX:-}" ./bin/sing-box /usr/local/bin/sing-box; do
        if [ -n "$cand" ] && [ -x "$cand" ]; then SING_BOX_BIN="$cand"; break; fi
    done
    if [ -n "$SING_BOX_BIN" ]; then
        echo "    使用内核: $SING_BOX_BIN"
        BOXHUB_SING_BOX="$SING_BOX_BIN" "$GO" test ./... -count=1 -timeout 300s
    else
        echo "    未找到 sing-box,跳过真实内核测试"
        "$GO" test ./... -count=1 -timeout 300s -short
    fi
fi

# ---- 交叉编译 ----
mkdir -p dist
LDFLAGS="-X main.version=${VERSION}"
[ "$DEV" = "1" ] || LDFLAGS="$LDFLAGS -s -w"

for arch in amd64 arm64; do
    echo "==> 编译 linux/${arch}"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        "$GO" build -trimpath -ldflags "$LDFLAGS" -o "dist/boxd-linux-${arch}" ./cmd/boxd
done

# 本机平台也构建一份,便于本地调试
CGO_ENABLED=0 "$GO" build -trimpath -ldflags "$LDFLAGS" -o "dist/boxd-local" ./cmd/boxd

# ---- 前端资源完整性检查 ----
echo "==> 检查前端资源"
for f in cmd/boxd/web/static/index.html cmd/boxd/web/static/app.css \
         cmd/boxd/web/static/core.js cmd/boxd/web/static/pages.js cmd/boxd/web/static/pages2.js; do
    [ -s "$f" ] || { echo "错误: 缺少 ${f}"; exit 1; }
done
if command -v node >/dev/null 2>&1; then
    for f in cmd/boxd/web/static/core.js cmd/boxd/web/static/pages.js cmd/boxd/web/static/pages2.js; do
        node --check "$f" || { echo "错误: ${f} 语法错误"; exit 1; }
    done
    echo "    前端语法检查通过"
fi

# ---- 打包 ----
echo "==> 生成发布包"
PKG="dist/boxhub-${VERSION}"
rm -rf "$PKG" && mkdir -p "$PKG/bin"
cp dist/boxd-linux-amd64 dist/boxd-linux-arm64 "$PKG/bin/"
cp scripts/manage.sh "$PKG/manage.sh"
# 把发布版本号写进安装脚本自述(build.sh 是唯一权威版本源,避免手动漏改)
sed -i.bak "s/^VERSION=\".*\"/VERSION=\"$VERSION\"/" "$PKG/manage.sh" && rm -f "$PKG/manage.sh.bak"
chmod +x "$PKG/manage.sh" "$PKG/bin/"*
cp README.md "$PKG/README.md" 2>/dev/null || true
(
    cd dist
    tar -czf "boxhub-${VERSION}.tar.gz" "boxhub-${VERSION}"
    shasum -a 256 "boxhub-${VERSION}.tar.gz" 2>/dev/null | awk '{print $1"  boxhub-'"${VERSION}"'.tar.gz"}' > "boxhub-${VERSION}.tar.gz.sha256" \
        || sha256sum "boxhub-${VERSION}.tar.gz" | awk '{print $1"  boxhub-'"${VERSION}"'.tar.gz"}' > "boxhub-${VERSION}.tar.gz.sha256"
)
rm -rf "$PKG"

echo
echo "==> 构建完成"
ls -lh dist/ | grep -vE '^total'
echo
echo "安装方式:"
echo "  tar -xzf dist/boxhub-${VERSION}.tar.gz && cd boxhub-${VERSION}"
echo "  sudo BOXHUB_LOCAL_BOXD=bin/boxd-linux-\$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/') bash manage.sh install"

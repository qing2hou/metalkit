#!/usr/bin/env bash
# scripts/package.sh — 打包可部署的 metalkit 发布包
#
# 用途：把构建好的二进制 + boot 文件 + 安装脚本打包成 tar.gz
# 输出：dist/metalkit-<version>.tar.gz
#
# 使用：
#   ./scripts/package.sh              # 自动检测版本或用时间戳
#   ./scripts/package.sh v1.2.3       # 指定版本号

set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
REPO_DIR="$( cd -- "${SCRIPT_DIR}/.." &> /dev/null && pwd )"

log()  { printf '\033[1;34m[package]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# 版本号：参数 > git tag > 时间戳
VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
    if git describe --tags --exact-match 2>/dev/null; then
        VERSION="$(git describe --tags --exact-match)"
    else
        VERSION="$(date +%Y%m%d-%H%M%S)"
    fi
fi

log "packaging metalkit version: $VERSION"

# 检查必需文件
for f in "${REPO_DIR}/bin/controller" "${REPO_DIR}/bin/agent"; do
    [[ -x "$f" ]] || die "missing binary: $f — run 'go build' first"
done
for f in vmlinuz initrd.img filesystem.squashfs; do
    [[ -f "${REPO_DIR}/boot/$f" ]] || die "missing boot artifact: ${REPO_DIR}/boot/$f — run scripts/build-live.sh first"
done
[[ -f "${REPO_DIR}/scripts/install.sh" ]] || die "missing scripts/install.sh"

# 创建 dist/ 目录
DIST_DIR="${REPO_DIR}/dist"
mkdir -p "$DIST_DIR"

# 临时打包目录
STAGE_DIR="$(mktemp -d)"
trap 'rm -rf "$STAGE_DIR"' EXIT

STAGE_ROOT="${STAGE_DIR}/metalkit"
mkdir -p "${STAGE_ROOT}/bin" "${STAGE_ROOT}/boot" "${STAGE_ROOT}/scripts" "${STAGE_ROOT}/docs"

log "staging files to ${STAGE_DIR}"

# 复制文件
install -m 0755 "${REPO_DIR}/bin/controller" "${STAGE_ROOT}/bin/controller"
install -m 0755 "${REPO_DIR}/bin/agent"      "${STAGE_ROOT}/bin/agent"

install -m 0644 "${REPO_DIR}/boot/vmlinuz"            "${STAGE_ROOT}/boot/"
install -m 0644 "${REPO_DIR}/boot/initrd.img"         "${STAGE_ROOT}/boot/"
install -m 0644 "${REPO_DIR}/boot/filesystem.squashfs" "${STAGE_ROOT}/boot/"

install -m 0755 "${REPO_DIR}/scripts/install.sh" "${STAGE_ROOT}/scripts/"

# 可选：复制配置示例和文档
if [[ -f "${REPO_DIR}/config.example.yaml" ]]; then
    install -m 0644 "${REPO_DIR}/config.example.yaml" "${STAGE_ROOT}/"
fi
if [[ -f "${REPO_DIR}/README.md" ]]; then
    install -m 0644 "${REPO_DIR}/README.md" "${STAGE_ROOT}/"
fi
if [[ -d "${REPO_DIR}/docs" ]]; then
    cp -r "${REPO_DIR}/docs"/* "${STAGE_ROOT}/docs/" 2>/dev/null || true
fi

# 打包
TARBALL="${DIST_DIR}/metalkit-${VERSION}.tar.gz"
log "creating tarball: ${TARBALL}"
tar -czf "$TARBALL" -C "$STAGE_DIR" metalkit

# 输出信息
SIZE="$(du -h "$TARBALL" | cut -f1)"
log "package created: ${TARBALL} (${SIZE})"
echo ""
echo "Deploy to a new server:"
echo "  1. scp ${TARBALL} root@<target-ip>:/root/"
echo "  2. ssh root@<target-ip>"
echo "  3. tar -xzf /root/$(basename "$TARBALL")"
echo "  4. cd metalkit && sudo ./scripts/install.sh"
echo ""

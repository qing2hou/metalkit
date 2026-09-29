#!/usr/bin/env bash
# Builds the Debian live netboot image (vmlinuz / initrd.img / filesystem.squashfs).
#
# Two backends — picked automatically, override with --native or --docker:
#   native: runs `lb build` directly on the host (Debian/Ubuntu with `live-build`
#           installed; faster, no privileged container). Requires root or sudo
#           because live-build itself wants root for debootstrap / chroot mounts.
#   docker: runs the same `lb build` inside a debian:bookworm container (works
#           anywhere docker is available; the only option on non-Debian hosts).
#
# Default: prefer native if `lb` is on PATH; otherwise docker; otherwise error.
# Expects a matching-arch agent binary: bin/agent-<arch> (make agent GOARCH=<arch>).
#
# --arch amd64|arm64 (default amd64): the target CPU of the live image.
#   amd64 builds anywhere live-build runs. arm64 needs an arm64-capable
#   environment: either binfmt+qemu-user-static on the host (native backend),
#   or `docker run --platform linux/arm64` (docker backend, which this script
#   requests automatically). Artifacts land in boot/<arch>/ so both trees can
#   coexist for mixed fleets.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIVE_DIR="$REPO_ROOT/live-image"

BACKEND=auto
ARCH=amd64
while [[ $# -gt 0 ]]; do
    case "$1" in
        --native) BACKEND=native; shift ;;
        --docker) BACKEND=docker; shift ;;
        --arch)   ARCH="$2"; shift 2 ;;
        -h|--help)
            sed -n '2,16p' "$0"
            exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

case "$ARCH" in
    amd64|arm64) ;;
    *) echo "--arch must be amd64 or arm64 (got: $ARCH)" >&2; exit 2 ;;
esac

AGENT_SRC="$REPO_ROOT/bin/agent-$ARCH"
AGENT_DST="$LIVE_DIR/config/includes.chroot/usr/local/bin/metalkit-agent"
MONITOR_SRC="$REPO_ROOT/bin/monitor-$ARCH"
MONITOR_DST="$LIVE_DIR/config/includes.chroot/usr/local/bin/metalkit-monitor"
BOOT_OUT="$REPO_ROOT/boot/$ARCH"

if [[ ! -x "$AGENT_SRC" ]]; then
    echo "agent binary not found at $AGENT_SRC — run 'make agent GOARCH=$ARCH' first" >&2
    exit 1
fi
if [[ ! -x "$MONITOR_SRC" ]]; then
    echo "monitor binary not found at $MONITOR_SRC — run 'make monitor GOARCH=$ARCH' first" >&2
    exit 1
fi

# Pick backend.
if [[ "$BACKEND" == auto ]]; then
    if command -v lb >/dev/null 2>&1; then
        BACKEND=native
    elif command -v docker >/dev/null 2>&1; then
        BACKEND=docker
    else
        echo "no backend available: install 'live-build' (Debian/Ubuntu, native) or 'docker' (any host)" >&2
        exit 1
    fi
fi

mkdir -p "$BOOT_OUT" "$(dirname "$AGENT_DST")"

# Stage the agent + monitor binaries into includes.chroot. Cleaned up on exit
# so we never commit them. The agent's systemd unit at
# config/includes.chroot/etc/systemd/system/ enables it; the 0600 hook chmods
# it and `systemctl enable`s the unit. The monitor has NO unit in the live
# image — it is cargo only: the installer's implant stage copies
# /usr/local/bin/metalkit-monitor into the installed OS together with a
# generated per-machine unit (see internal/installer/implant.go).
echo "=== staging $AGENT_SRC -> $AGENT_DST ==="
cp "$AGENT_SRC" "$AGENT_DST"
chmod 0755 "$AGENT_DST"
echo "=== staging $MONITOR_SRC -> $MONITOR_DST ==="
cp "$MONITOR_SRC" "$MONITOR_DST"
chmod 0755 "$MONITOR_DST"
trap 'rm -f "$AGENT_DST" "$MONITOR_DST"' EXIT

case "$BACKEND" in
    native)
        echo "=== building live image natively (lb build) — may take 10-25 min ==="
        if [[ $EUID -ne 0 ]]; then
            echo "native build requires root for debootstrap/chroot — re-run with sudo" >&2
            exit 1
        fi
        (
            cd "$LIVE_DIR"
            lb clean --purge || true
            lb config --architectures "$ARCH"
            lb build
        )
        ;;
    docker)
        if ! command -v docker >/dev/null 2>&1; then
            echo "docker not found — install docker or use --native" >&2
            exit 1
        fi
        echo "=== building live image via docker (may take 10-25 min) ==="
        # Forward host HTTP(S) proxy into the container so apt-get update,
        # live-build's lb config, and debootstrap can reach Debian mirrors.
        # On hosts without a proxy these vars are empty and have no effect.
        # linux/arm64 containers on an x86 host need binfmt/qemu registered
        # (docker run --privileged + --platform handles the rest). amd64 is a
        # no-op on amd64 hosts.
        PLATFORM_FLAG=()
        [[ "$ARCH" == arm64 ]] && PLATFORM_FLAG=(--platform linux/arm64)
        docker run --rm --privileged "${PLATFORM_FLAG[@]}" \
            -e "ARCH=$ARCH" \
            -e "http_proxy=${HTTP_PROXY:-${http_proxy:-}}" \
            -e "https_proxy=${HTTPS_PROXY:-${https_proxy:-}}" \
            -e "HTTP_PROXY=${HTTP_PROXY:-${http_proxy:-}}" \
            -e "HTTPS_PROXY=${HTTPS_PROXY:-${https_proxy:-}}" \
            -e "no_proxy=${NO_PROXY:-${no_proxy:-}}" \
            -v "$LIVE_DIR:/build" \
            -w /build \
            debian:bookworm \
            bash -c '
                set -euo pipefail
                # Pin the proxy for apt explicitly — env vars work for most tools
                # but a config file is the canonical hook live-build/debootstrap
                # both honor.
                if [[ -n "${http_proxy:-}" ]]; then
                    echo "Acquire::http::Proxy \"$http_proxy\";"   >  /etc/apt/apt.conf.d/00proxy
                    echo "Acquire::https::Proxy \"${https_proxy:-$http_proxy}\";" >> /etc/apt/apt.conf.d/00proxy
                fi
                apt-get update
                apt-get install -y --no-install-recommends live-build
                lb clean --purge || true
                lb config --architectures "$ARCH"
                lb build
            '
        ;;
esac

# Per-arch package list substitutions inside the live tree. live-build picks
# up config/package-lists; we swap arch-specific package names via markers in
# installer.list.chroot (see the file's comments).
echo "=== adjusting package lists for $ARCH ==="
PKG_LIST="$LIVE_DIR/config/package-lists/installer.list.chroot"
if [[ "$ARCH" == arm64 ]]; then
    # arm64: no amd64-microcode; kernel metapackage and grubEFI binaries differ.
    sed -i 's/^amd64-microcode/#amd64-microcode (not on arm64)/' "$PKG_LIST"
    sed -i 's/^linux-image-amd64/linux-image-arm64/' "$PKG_LIST"
    sed -i 's/grub-efi-amd64-bin/grub-efi-arm64-bin/' "$PKG_LIST"
    sed -i 's/grub-efi-amd64-signed/grub-efi-arm64-signed/' "$PKG_LIST"
else
    # amd64: restore the upstream names in case an arm64 build ran before.
    sed -i 's/^#amd64-microcode (not on arm64)/amd64-microcode/' "$PKG_LIST"
    sed -i 's/^linux-image-arm64/linux-image-amd64/' "$PKG_LIST"
    sed -i 's/grub-efi-arm64-bin/grub-efi-amd64-bin/' "$PKG_LIST"
    sed -i 's/grub-efi-arm64-signed/grub-efi-amd64-signed/' "$PKG_LIST"
fi

echo "=== copying artifacts to $BOOT_OUT ==="
# live-build netboot outputs: kernel+initrd in tftpboot/live/, squashfs in binary/live/
cp "$LIVE_DIR/tftpboot/live/vmlinuz" "$BOOT_OUT/vmlinuz"
cp "$LIVE_DIR/tftpboot/live/initrd.img" "$BOOT_OUT/initrd.img"
cp "$LIVE_DIR/binary/live/filesystem.squashfs" "$BOOT_OUT/filesystem.squashfs"

# Post-build guard: the agent AND the monitor MUST be inside the squashfs.
# The agent is live functionality (collect + install); the monitor is
# installer cargo — when a profile asks for agent_installed the implant
# stage copies /usr/local/bin/metalkit-monitor out of the live root, and a
# missing binary would turn every such install into a failure at the very
# end of the pipeline. This catches the incremental-build trap: a build
# interrupted mid-run (SIGKILL/TaskStop) leaves chroot/cache timestamps
# behind, and the NEXT lb build skips the includes.chroot copy stage as
# "already done" — producing an agentless image that still builds green.
# (Incident 2026-09-28: two live boots booted silent before this check.)
#
# Implementation note: do NOT use `unsquashfs | grep -q` — under this
# script's `set -o pipefail`, grep -q exits on first match and unsquashfs
# dies of SIGPIPE(141), which pipefail turns into a false FATAL. Capture the
# listing to a variable/file first, then grep the complete output.
SQ_LIST=$(unsquashfs -ll "$BOOT_OUT/filesystem.squashfs" 2>/dev/null || true)
MISSING=0
if ! grep -q 'usr/local/bin/metalkit-agent' <<< "$SQ_LIST"; then
    echo "FATAL: metalkit-agent missing from filesystem.squashfs — build produced a bootable brick." >&2
    MISSING=1
fi
if ! grep -q 'usr/local/bin/metalkit-monitor' <<< "$SQ_LIST"; then
    echo "FATAL: metalkit-monitor missing from filesystem.squashfs — installs with agent_installed would fail at implant." >&2
    MISSING=1
fi
if [[ "$MISSING" -ne 0 ]]; then
    echo "  This happens when a previous build was interrupted and lb skipped the includes copy." >&2
    echo "  Fix: rm -rf live-image/{chroot,cache,.build,binary} binary && re-run this script." >&2
    exit 1
fi

echo "=== done (backend=$BACKEND) ==="
ls -la "$BOOT_OUT"

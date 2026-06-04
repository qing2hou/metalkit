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
# Expects bin/agent to exist (run `make agent` or `go build -o bin/agent ./cmd/agent`).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIVE_DIR="$REPO_ROOT/live-image"
BOOT_OUT="$REPO_ROOT/boot"
AGENT_SRC="$REPO_ROOT/bin/agent"
AGENT_DST="$LIVE_DIR/config/includes.chroot/usr/local/bin/metalkit-agent"

BACKEND=auto
while [[ $# -gt 0 ]]; do
    case "$1" in
        --native) BACKEND=native; shift ;;
        --docker) BACKEND=docker; shift ;;
        -h|--help)
            sed -n '2,16p' "$0"
            exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

if [[ ! -x "$AGENT_SRC" ]]; then
    echo "agent binary not found at $AGENT_SRC — run 'go build -o bin/agent ./cmd/agent' first" >&2
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

# Stage the agent binary into includes.chroot. Cleaned up on exit so we never
# commit it. The systemd unit at config/includes.chroot/etc/systemd/system/
# enables it; the 0600 hook chmods it and `systemctl enable`s the unit.
echo "=== staging $AGENT_SRC -> $AGENT_DST ==="
cp "$AGENT_SRC" "$AGENT_DST"
chmod 0755 "$AGENT_DST"
trap 'rm -f "$AGENT_DST"' EXIT

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
            lb config
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
        docker run --rm --privileged \
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
                lb config
                lb build
            '
        ;;
esac

echo "=== copying artifacts to $BOOT_OUT ==="
# live-build netboot outputs: kernel+initrd in tftpboot/live/, squashfs in binary/live/
cp "$LIVE_DIR/tftpboot/live/vmlinuz" "$BOOT_OUT/vmlinuz"
cp "$LIVE_DIR/tftpboot/live/initrd.img" "$BOOT_OUT/initrd.img"
cp "$LIVE_DIR/binary/live/filesystem.squashfs" "$BOOT_OUT/filesystem.squashfs"

echo "=== done (backend=$BACKEND) ==="
ls -la "$BOOT_OUT"

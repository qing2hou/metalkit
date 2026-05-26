#!/bin/bash
# Builds the Debian live netboot image via docker.
# Requires: docker, ~3GB free disk, network access for apt mirror.
# Expects bin/agent to exist (produced by `make agent`).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIVE_DIR="$REPO_ROOT/live-image"
BOOT_OUT="$REPO_ROOT/boot"
AGENT_SRC="$REPO_ROOT/bin/agent"
AGENT_DST="$LIVE_DIR/config/includes.chroot/usr/local/bin/metalkit-agent"

if ! command -v docker >/dev/null 2>&1; then
    echo "docker not found - install docker first" >&2
    exit 1
fi

if [ ! -x "$AGENT_SRC" ]; then
    echo "agent binary not found at $AGENT_SRC — run 'make agent' first" >&2
    exit 1
fi

mkdir -p "$BOOT_OUT" "$(dirname "$AGENT_DST")"

# Stage the agent binary into includes.chroot. Cleaned up on exit so we never
# commit it. The systemd unit at config/includes.chroot/etc/systemd/system/
# enables it; the 0600 hook chmods it and `systemctl enable`s the unit.
echo "=== staging $AGENT_SRC -> $AGENT_DST ==="
cp "$AGENT_SRC" "$AGENT_DST"
chmod 0755 "$AGENT_DST"
trap 'rm -f "$AGENT_DST"' EXIT

echo "=== building live image via docker (may take 10-25 min) ==="
docker run --rm --privileged \
    -v "$LIVE_DIR:/build" \
    -w /build \
    debian:bookworm \
    bash -c '
        set -euo pipefail
        apt-get update
        apt-get install -y --no-install-recommends live-build
        lb clean --purge || true
        lb config
        lb build
    '

echo "=== copying artifacts to $BOOT_OUT ==="
# live-build netboot outputs: kernel+initrd in tftpboot/live/, squashfs in binary/live/
cp "$LIVE_DIR/tftpboot/live/vmlinuz" "$BOOT_OUT/vmlinuz"
cp "$LIVE_DIR/tftpboot/live/initrd.img" "$BOOT_OUT/initrd.img"
cp "$LIVE_DIR/binary/live/filesystem.squashfs" "$BOOT_OUT/filesystem.squashfs"

echo "=== done ==="
ls -la "$BOOT_OUT"

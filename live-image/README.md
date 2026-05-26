# live-image

Debian live-build configuration for the metalkit installer environment.

## What this is

A minimal Debian bookworm live filesystem booted via PXE/iPXE. The build
produces three artifacts consumed by the controller's HTTP server:

- `vmlinuz`           — kernel
- `initrd.img`        — initramfs with live-boot
- `filesystem.squashfs` — root filesystem (squashfs, fetched via HTTP)

## How it's built

```
make live
```

That invokes `scripts/build-live.sh`, which runs `live-build` inside a
privileged `debian:bookworm` docker container (live-build needs loopback
mounts, so it cannot run in an unprivileged container).

Build time: ~10-25 minutes on first run; ~2-5 minutes once apt cache is warm.

## Output

After a successful build, artifacts are copied to:

```
/opt/claude/devops/boot/
  vmlinuz
  initrd.img
  filesystem.squashfs
```

These are then served by the controller's HTTP handler under `/boot/`.

## M1 scope

The M1 image boots to a shell (no agent yet). M2 will drop
an installer-agent binary + systemd unit into `config/includes.chroot/`.

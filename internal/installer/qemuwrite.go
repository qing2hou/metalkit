// qemuwrite.go spools a qcow2 image to a tmpfs scratch file then runs
// `qemu-img convert -f qcow2 -O raw <spool> <devPath>` to materialise it
// on the target block device.
//
// We tried piping the verifying downloader straight into qemu-img stdin
// (avoiding scratch storage) but qemu-img's "file" driver refuses
// non-regular files: "qemu-img: Could not open '/dev/stdin': 'file'
// driver requires '/dev/stdin' to be a regular file". qcow2 reading
// needs random-access seeking on the source, so a pipe or FIFO cannot
// work; we must materialise the bytes first.
//
// The spool file lives in deps.WorkDir (default /tmp/metalkit-install,
// on the live boot's tmpfs). Cloud images are typically < 2 GB and the
// R630 / equivalents have plenty of RAM. If we ever need to support
// huge custom images we'll have to teach this stage to spool onto an
// unused partition of the *target* disk instead — but that's M2.4+.
//
// SHA256 verification still happens via the upstream sha256VerifyReader
// that fires on EOF; io.Copy below propagates that error.
package installer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteImage spools src to a tmpfs scratch file, then runs qemu-img
// convert from that file to devPath. After the convert finishes it
// calls sync(1) (best-effort) and partprobe(8) (best-effort: a warning
// log on failure is enough — the kernel re-reads the partition table on
// next mount anyway).
func WriteImage(ctx context.Context, deps Deps, src io.Reader, devPath string) error {
	if src == nil {
		return fmt.Errorf("install: WriteImage: src is nil")
	}
	if devPath == "" {
		return fmt.Errorf("install: WriteImage: devPath is empty")
	}

	workDir := deps.WorkDir
	if workDir == "" {
		workDir = "/tmp/metalkit-install"
	}
	if err := deps.FS.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("install: prepare workdir %s: %w", workDir, err)
	}
	spool := filepath.Join(workDir, "image.qcow2")
	f, err := os.Create(spool)
	if err != nil {
		return fmt.Errorf("install: create spool %s: %w", spool, err)
	}
	n, copyErr := io.Copy(f, src)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(spool)
		return fmt.Errorf("install: spool image to %s after %d bytes: %w", spool, n, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(spool)
		return fmt.Errorf("install: close spool %s: %w", spool, closeErr)
	}
	defer os.Remove(spool)

	if _, err := deps.Exec.Run(ctx, "qemu-img", "convert",
		"-f", "qcow2", "-O", "raw", spool, devPath,
	); err != nil {
		return fmt.Errorf("install: qemu-img convert to %s: %w", devPath, err)
	}

	// sync after the convert so the kernel flushes before we touch the
	// partition table. Fatal if it actually errors — we shouldn't run
	// partprobe against dirty buffers.
	if _, err := deps.Exec.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("install: sync after image write: %w", err)
	}

	// partprobe is best-effort: on some kernels it returns non-zero even
	// after re-reading correctly, and growpart will trigger a re-read
	// anyway. Log and continue.
	if _, err := deps.Exec.Run(ctx, "partprobe", devPath); err != nil {
		if deps.Reporter != nil {
			_ = deps.Reporter.Log(ctx, "warn",
				fmt.Sprintf("partprobe %s failed (continuing): %v", devPath, err))
		}
		if deps.Logger != nil {
			deps.Logger.Warn("partprobe failed", "dev", devPath, "err", err)
		}
	}

	// Force the kernel to re-read the partition table a second time via
	// blockdev. partprobe on its own sometimes leaves the kernel serving a
	// stale partition list — particularly right after qemu-img convert
	// wrote a fresh GPT image onto a disk that previously held a different
	// table. The symptom is lsblk / sfdisk returning "no partitions" even
	// though the on-disk table is valid, which then makes GrowLastPartition
	// fail with "no partitions on /dev/sdX". blockdev --rereadpt issues the
	// BLKRRPART ioctl directly. Best-effort, like partprobe.
	if _, err := deps.Exec.Run(ctx, "blockdev", "--rereadpt", devPath); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("blockdev --rereadpt failed (continuing)", "dev", devPath, "err", err)
		}
	}
	return nil
}

// installer.go is the stage orchestrator. Run is the single entry point
// the agent calls once per install job. The stage names match the
// Reporter.Stage strings; we emit them in order so the controller side
// can render a progress UI without parsing free-form log lines.
package installer

import (
	"context"
	"fmt"
	"path/filepath"

	"metalkit/internal/jobs"
)

// Stage names — exported so the agent main and tests can reference them
// without stringly-typed duplication.
const (
	StageBootDetect  = "boot-detect"
	StageDiskPick    = "disk-pick"
	StageDownload    = "download"
	StageWrite       = "write"
	StageGrow        = "grow"
	StageMount       = "mount"
	StageSeed        = "seed"
	StageGrubInstall = "grub-install"
	StageUnmount     = "umount"
)

// Run executes the full install pipeline for spec. On any error Reporter.Fail
// is invoked with the wrapped error message and Run returns the same error.
// On success Reporter.Succeed is invoked exactly once.
func Run(ctx context.Context, deps Deps, spec jobs.InstallSpec) (retErr error) {
	if deps.Reporter == nil {
		return fmt.Errorf("install: Reporter is required")
	}
	if deps.Exec == nil || deps.FS == nil || deps.Downloader == nil || deps.Disks == nil {
		return fmt.Errorf("install: Deps incomplete (Exec/FS/Downloader/Disks)")
	}

	// On any premature exit, make sure Fail is reported. We defer here so
	// even a panic inside a stage produces a structured failure.
	defer func() {
		if retErr != nil {
			_ = deps.Reporter.Fail(ctx, retErr.Error())
		}
	}()

	workDir := deps.WorkDir
	if workDir == "" {
		workDir = "/tmp/metalkit-install"
	}
	if err := deps.FS.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("install: prepare workdir %s: %w", workDir, err)
	}

	// --- boot-detect -----------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageBootDetect); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageBootDetect, err)
	}
	bootMode, err := DetectBootMode(deps.FS)
	if err != nil {
		return err
	}
	_ = deps.Reporter.Log(ctx, "info", fmt.Sprintf("boot mode: %s", bootMode))

	// --- disk-pick -------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageDiskPick); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageDiskPick, err)
	}
	disks, err := deps.Disks.List(ctx)
	if err != nil {
		return fmt.Errorf("install: list disks: %w", err)
	}
	target, err := PickDisk(disks, spec.Profile.TargetDisk)
	if err != nil {
		return err
	}
	_ = deps.Reporter.Log(ctx, "info", fmt.Sprintf("selected disk %s (%d bytes, %s)",
		target.DevPath, target.SizeBytes, target.Transport))

	// Zap stale partition table signatures (GPT primary+backup, MBR boot
	// sector) so the fresh image writes onto a clean slate. Without this,
	// a disk that previously held a larger GPT install keeps a backup
	// GPT header at the end of the disk that confuses sgdisk -p and can
	// mislead the firmware's boot-order scan. Best-effort: a missing
	// sgdisk on the live image is logged but doesn't abort.
	wipeDisk(ctx, deps, target.DevPath)

	// --- download --------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageDownload); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageDownload, err)
	}
	resolved, err := ResolveBlobURL(deps.BaseURL, spec.ImageBlobURL)
	if err != nil {
		return err
	}
	_ = deps.Reporter.Log(ctx, "info", fmt.Sprintf("streaming %s", resolved))
	rc, err := deps.Downloader.Stream(ctx, resolved, spec.ImageSHA256)
	if err != nil {
		return fmt.Errorf("install: open image stream: %w", err)
	}
	defer rc.Close()

	// --- write -----------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageWrite); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageWrite, err)
	}
	if err := WriteImage(ctx, deps, rc, target.DevPath); err != nil {
		return err
	}

	// If the live system booted UEFI but the image has no ESP, create one
	// before growing so growpart stops at the ESP boundary.
	if _, err := createESPIfMissing(ctx, deps, target.DevPath, "", bootMode); err != nil {
		return err
	}

	// --- grow ------------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageGrow); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageGrow, err)
	}
	grown, err := GrowLastPartition(ctx, deps, target.DevPath)
	if err != nil {
		return err
	}

	// --- mount -----------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageMount); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageMount, err)
	}
	mntRoot := filepath.Join(workDir, "rootfs")
	espMount, cleanup, err := Mount(ctx, deps, grown.PartDev, mntRoot)
	if err != nil {
		return err
	}
	// We don't `defer cleanup()` because the umount stage is reported
	// explicitly below and we want failures from cleanup folded into the
	// retErr so the reporter sees them. The deferred safety-net below
	// guarantees cleanup runs even on a panic/early-return mid-pipeline.
	cleanupDone := false
	defer func() {
		if !cleanupDone {
			if err := cleanup(); err != nil && retErr == nil {
				retErr = err
			}
		}
	}()

	// XFS deferred grow happens here while the FS is mounted.
	if grown.XFSPendingGrow {
		if _, err := deps.Exec.Run(ctx, "xfs_growfs", mntRoot); err != nil {
			return fmt.Errorf("install: xfs_growfs %s: %w", mntRoot, err)
		}
	}

	// --- seed ------------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageSeed); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageSeed, err)
	}
	if err := BuildSeed(ctx, deps, spec, mntRoot); err != nil {
		return err
	}

	// --- grub-install ----------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageGrubInstall); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageGrubInstall, err)
	}
	if err := InstallGRUB(ctx, deps, spec, mntRoot, target.DevPath, espMount); err != nil {
		return err
	}

	// Prune stale NVRAM boot entries that referenced pre-write partitions
	// on this disk. Without this, the firmware wastes BootOrder slots
	// retrying entries whose PARTUUID no longer exists, and on confused
	// firmware (Dell R630 BIOS 2.3.4 observed) can produce "Boot Failed"
	// noise that masks the real boot attempt. UEFI-only — efibootmgr
	// doesn't exist on BIOS boots and bootMode gates that.
	//
	// Runs AFTER grub-install so the fresh BootXXXX entry for the
	// just-installed OS (whose GUID is in the current partition table) is
	// preserved; everything else pointing at this disk's old partitions
	// (MBR sigs or vanished GPT GUIDs) gets deleted.
	if bootMode == "uefi" {
		pruneStaleNVRAM(ctx, deps, target.DevPath)
	}

	// --- umount ----------------------------------------------------------
	if err := deps.Reporter.Stage(ctx, StageUnmount); err != nil {
		return fmt.Errorf("install: stage %s: %w", StageUnmount, err)
	}
	if err := cleanup(); err != nil {
		return err
	}
	cleanupDone = true

	return deps.Reporter.Succeed(ctx)
}

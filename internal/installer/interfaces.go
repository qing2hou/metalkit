// Package installer is the agent-side install pipeline. It runs on the live
// boot image after the controller has scheduled an install job for the
// machine and the BMC has been told to PXE.
//
// The flow, from inside Run, is:
//
//	boot-detect → disk-pick → download → write → grow → mount → seed
//	→ grub-install → umount → succeed.
//
// All external IO is behind small interfaces (Exec, FS, Downloader,
// DiskLister, Reporter) so each stage is unit-testable with hand-rolled
// mocks. The prod implementations live in realdeps.go. The orchestrator in
// installer.go composes the stages and emits progress through Reporter.
//
// Design choices worth flagging:
//
//   - We do NOT repartition the image. The cloud image already ships an
//     ESP + root layout; we grow the last partition (root) to fill the
//     disk. This keeps the agent code small and avoids tripping over
//     vendor-specific partition orderings.
//   - The cloud-init seed ISO lives on the ESP (FAT32) so grub can hand it
//     to the kernel via ds=nocloud-net;s=file:///boot/efi/. The ESP is
//     mounted at /boot/efi at runtime so a single absolute URL works both
//     pre- and post-boot.
//   - UEFI only. M2.3 explicitly drops legacy BIOS; we fail boot-detect
//     rather than silently picking an install path that won't boot on
//     Dell R6x0 servers we test on.
package installer

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Exec runs external commands. Two flavours: Run is request/response,
// RunPipe streams stdin in (used to feed qemu-img convert from the
// downloader). Implementations MUST honour ctx cancellation.
type Exec interface {
	// Run executes name with args and returns combined stdout+stderr.
	// On non-zero exit the error message MUST include the captured output
	// so callers don't lose diagnostics.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)

	// RunPipe streams stdin into the command. Stdout+stderr are captured
	// and folded into the returned error on non-zero exit. The reader is
	// consumed up to EOF or first error from the spawned process.
	RunPipe(ctx context.Context, stdin io.Reader, name string, args ...string) error
}

// FS is the small slice of the filesystem the installer touches outside of
// what Exec already covers (mounts, cp, etc.). Kept narrow so tests can
// supply an in-memory implementation.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
	Stat(path string) (os.FileInfo, error)
	// Exists is a convenience for the boot-mode probe; equivalent to
	// Stat-and-check-IsNotExist but keeps callers tidy.
	Exists(path string) bool
	// Symlink creates oldname → newname (os.Symlink semantics).
	Symlink(oldname, newname string) error
	// Remove deletes the named file or empty directory.
	Remove(path string) error
}

// Downloader streams the image bytes. The returned ReadCloser MUST verify
// sha256 while streaming and surface a mismatch as a Read or Close error
// (we don't want to silently flash a corrupt image and only catch it
// post-install).
type Downloader interface {
	Stream(ctx context.Context, url, expectedSHA256 string) (io.ReadCloser, error)
}

// Disk is the structured form of a block device candidate as seen by
// lsblk. DiskLister produces these; PickDisk consumes them. Fields map
// 1:1 to lsblk -J -d -bo NAME,PATH,SIZE,RM,RO,TRAN,MODEL,WWN.
type Disk struct {
	Name      string // sda
	DevPath   string // /dev/sda
	SizeBytes int64
	Removable bool
	ReadOnly  bool
	Transport string // sata / nvme / usb / virtio …
	Model     string
	WWN       string
	ByPath    string // /dev/disk/by-path/…
}

// DiskLister enumerates candidate target disks. The list excludes nothing
// — filtering (removable, RO, transport=usb) is PickDisk's job so the
// "smallest" and "by-*" modes can disagree about what counts.
type DiskLister interface {
	List(ctx context.Context) ([]Disk, error)
}

// Reporter is how the installer talks back to the controller. The agent
// main wires a concrete implementation that POSTs to /api/v1/agent/jobs/…;
// for unit tests we use a recording mock.
//
// Stage transitions are coarse ("download", "write", "grub-install", …).
// Log lines are free-form per-stage progress.
type Reporter interface {
	Stage(ctx context.Context, stage string) error
	Log(ctx context.Context, level, message string) error
	Succeed(ctx context.Context) error
	Fail(ctx context.Context, errMsg string) error
}

// NICInfo is a minimal NIC descriptor used at seed time to resolve interface
// names to MAC addresses for netplan match.macaddress blocks. Populated by the
// agent from the live system's NIC list during inventory collection.
type NICInfo struct {
	Name string
	MAC  string
}

// Deps is the bag of injected dependencies threaded through every stage.
// Constructed once in cmd/agent/main and passed by value into Run.
type Deps struct {
	Exec       Exec
	FS         FS
	Downloader Downloader
	Disks      DiskLister
	Reporter   Reporter
	// BaseURL is the controller base, e.g. http://10.0.0.1:8080. Used by
	// ResolveBlobURL to join the relative ImageBlobURL onto an absolute
	// URL the downloader can GET.
	BaseURL string
	// WorkDir is the scratch directory for the seed ISO and any other
	// temporary artifacts. Defaults to /tmp/metalkit-install if empty.
	WorkDir string
	Logger  *slog.Logger
	// NICs is the live system's NIC list from inventory collection.
	// Used at seed time to resolve bond slave interface names to MACs.
	// May be nil (e.g. unit tests); in that case bond slaves fall back
	// to match.name in the emitted netplan config.
	NICs []NICInfo
}

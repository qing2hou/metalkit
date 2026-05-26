// Package ipmi wraps the system `ipmitool` binary for the few operations
// metalkit needs against target machines' BMCs:
//
//   - SetBootDevice(pxe|disk)  one-shot next-boot target
//   - PowerCycle               warm reboot if on, power-on if off
//   - PowerStatus              on / off / unknown
//   - BootForPXE               composite: bootdev=pxe + power cycle (start reinstall)
//   - FinalizeBootDisk         composite: bootdev=disk + power cycle (boot into installed OS)
//
// Password handling: `-E` makes ipmitool read the password from the
// IPMI_PASSWORD env var instead of `-P <pass>` on the command line. That keeps
// the secret out of `ps aux` and the systemd journal. We never log the
// password or the env var; the only place it lives is the child process's
// memory for the duration of the call.
package ipmi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"metalkit/internal/bmc"
)

// DefaultTimeout is generous enough for slow / overloaded BMCs but short
// enough that a stuck call doesn't wedge the orchestrator goroutine.
const DefaultTimeout = 10 * time.Second

// BootDevice is the one-shot next-boot target.
type BootDevice string

const (
	BootDevicePXE  BootDevice = "pxe"
	BootDeviceDisk BootDevice = "disk"
)

// CommandRunner abstracts exec.CommandContext so tests can stub the binary.
// Implementations must:
//   - honour ctx (cancel → kill child)
//   - return (stdout, stderr, err)
//   - err non-nil iff the child exited non-zero OR couldn't start
type CommandRunner interface {
	Run(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error)
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	// BinPath overrides the lookup. Empty means LookPath("ipmitool").
	BinPath string
	// Runner overrides exec. Tests inject a fake; production leaves it nil.
	Runner CommandRunner
	// Timeout overrides DefaultTimeout per call.
	Timeout time.Duration
}

// Client wraps ipmitool. Construct once at controller startup; safe for
// concurrent use (each call gets its own subprocess).
type Client struct {
	bin     string
	runner  CommandRunner
	timeout time.Duration
	logger  *slog.Logger
}

// NewClient resolves the ipmitool binary (or honours BinPath) and returns a
// ready client. Returns an error if the binary cannot be found and no fake
// Runner was injected — in production we want a loud failure rather than
// "everything looks fine until you trigger an install."
func NewClient(logger *slog.Logger, opts ClientOptions) (*Client, error) {
	if logger == nil {
		return nil, errors.New("ipmi: logger is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runner := opts.Runner
	if runner == nil {
		runner = execRunner{}
	}
	bin := opts.BinPath
	if bin == "" {
		bin = "ipmitool"
	}
	// Skip the existence check when a fake Runner is injected — tests don't
	// install ipmitool and we don't want them to need a stub binary on $PATH.
	if opts.Runner == nil {
		resolved, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("ipmi: locate ipmitool: %w", err)
		}
		bin = resolved
	}
	return &Client{bin: bin, runner: runner, timeout: timeout, logger: logger}, nil
}

// SetBootDevice issues `chassis bootdev <dev>`. One-shot (resets after next boot).
func (c *Client) SetBootDevice(ctx context.Context, cred bmc.PasswordedCredential, dev BootDevice) error {
	switch dev {
	case BootDevicePXE, BootDeviceDisk:
	default:
		return fmt.Errorf("ipmi: unsupported boot device %q", dev)
	}
	_, err := c.run(ctx, cred, "chassis", "bootdev", string(dev))
	return err
}

// PowerCycle issues `chassis power cycle`. The BMC translates this to "warm
// reboot" if already on, or "power on" if off (Dell iDRAC behaviour; other
// vendors may differ — we treat the difference as opaque).
func (c *Client) PowerCycle(ctx context.Context, cred bmc.PasswordedCredential) error {
	_, err := c.run(ctx, cred, "chassis", "power", "cycle")
	return err
}

// PowerOn issues `chassis power on` (no-op if already on per IPMI spec).
func (c *Client) PowerOn(ctx context.Context, cred bmc.PasswordedCredential) error {
	_, err := c.run(ctx, cred, "chassis", "power", "on")
	return err
}

// PowerOff issues `chassis power off` (hard cut, no graceful shutdown). Use
// PowerSoft when you want the OS to be asked nicely first.
func (c *Client) PowerOff(ctx context.Context, cred bmc.PasswordedCredential) error {
	_, err := c.run(ctx, cred, "chassis", "power", "off")
	return err
}

// PowerSoft sends ACPI soft-off (graceful shutdown). Requires acpid/systemd
// to be running in the host OS; behaves like PowerOff if the host ignores it.
func (c *Client) PowerSoft(ctx context.Context, cred bmc.PasswordedCredential) error {
	_, err := c.run(ctx, cred, "chassis", "power", "soft")
	return err
}

// PowerReset issues `chassis power reset` — hardware reset without going
// through power-off. Equivalent to pressing the reset button.
func (c *Client) PowerReset(ctx context.Context, cred bmc.PasswordedCredential) error {
	_, err := c.run(ctx, cred, "chassis", "power", "reset")
	return err
}

// PowerStatus returns "on" / "off" / "unknown". An error means we couldn't
// reach the BMC at all (network down, bad creds) — distinct from "BMC
// answered but power state is unknown".
func (c *Client) PowerStatus(ctx context.Context, cred bmc.PasswordedCredential) (string, error) {
	out, err := c.run(ctx, cred, "chassis", "power", "status")
	if err != nil {
		return "", err
	}
	// Typical: "Chassis Power is on\n"
	line := strings.ToLower(strings.TrimSpace(string(out)))
	switch {
	case strings.HasSuffix(line, " on"):
		return "on", nil
	case strings.HasSuffix(line, " off"):
		return "off", nil
	default:
		return "unknown", nil
	}
}

// BootForPXE is the composite operation used to start a (re)install:
// set next-boot=pxe, then power cycle. Returns the underlying error if either
// step fails.
func (c *Client) BootForPXE(ctx context.Context, cred bmc.PasswordedCredential) error {
	if err := c.SetBootDevice(ctx, cred, BootDevicePXE); err != nil {
		return fmt.Errorf("set bootdev=pxe: %w", err)
	}
	if err := c.PowerCycle(ctx, cred); err != nil {
		return fmt.Errorf("power cycle: %w", err)
	}
	return nil
}

// FinalizeBootDisk is the composite operation used after a successful
// install: set next-boot=disk, then power cycle so the machine reboots
// into the freshly-installed OS instead of looping back to PXE. Mirrors
// BootForPXE's pattern.
func (c *Client) FinalizeBootDisk(ctx context.Context, cred bmc.PasswordedCredential) error {
	if err := c.SetBootDevice(ctx, cred, BootDeviceDisk); err != nil {
		return fmt.Errorf("set bootdev=disk: %w", err)
	}
	if err := c.PowerCycle(ctx, cred); err != nil {
		return fmt.Errorf("power cycle: %w", err)
	}
	return nil
}

// run builds the ipmitool argv (no password), passes the password via
// IPMI_PASSWORD in env, applies the timeout, and logs at debug.
func (c *Client) run(ctx context.Context, cred bmc.PasswordedCredential, args ...string) ([]byte, error) {
	port := cred.Port
	if port == 0 {
		port = 623
	}
	iface := cred.IPMIInterface
	if iface == "" {
		iface = "lanplus"
	}
	base := []string{
		"-H", cred.IP,
		"-p", fmt.Sprintf("%d", port),
		"-I", iface,
		"-U", cred.Username,
		"-E", // password from IPMI_PASSWORD
	}
	full := append(base, args...)

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	env := []string{"IPMI_PASSWORD=" + cred.Password}
	c.logger.Debug("ipmitool",
		"bmc_ip", cred.IP,
		"port", port,
		"iface", iface,
		"user", cred.Username,
		"args", args,
	)
	stdout, stderr, err := c.runner.Run(callCtx, env, c.bin, full...)
	if err != nil {
		// Sanitize stderr in case ipmitool ever echoed the password back
		// (it does not in practice, but defense-in-depth).
		safe := redact(stderr, cred.Password)
		return stdout, fmt.Errorf("ipmitool %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(safe)))
	}
	return stdout, nil
}

func redact(b []byte, secret string) []byte {
	if secret == "" {
		return b
	}
	return bytes.ReplaceAll(b, []byte(secret), []byte("***"))
}

// execRunner is the production CommandRunner: spawns the real child.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Inherit nothing else; IPMI_PASSWORD-only env keeps the binary from
	// picking up stray vars and keeps the secret out of any forks.
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

package ipmi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"metalkit/internal/bmc"
)

// fakeRunner captures the most recent call and returns the stub response.
type fakeRunner struct {
	calls    []recordedCall
	stdout   []byte
	stderr   []byte
	err      error
	delay    time.Duration // simulate slow BMC
	envFound string        // captures IPMI_PASSWORD value
}

type recordedCall struct {
	name string
	args []string
	env  []string
}

func (f *fakeRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	rec := recordedCall{name: name, args: append([]string(nil), args...), env: append([]string(nil), env...)}
	f.calls = append(f.calls, rec)
	for _, e := range env {
		if strings.HasPrefix(e, "IPMI_PASSWORD=") {
			f.envFound = strings.TrimPrefix(e, "IPMI_PASSWORD=")
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return f.stdout, f.stderr, f.err
}

func newClient(t *testing.T, fr *fakeRunner) *Client {
	t.Helper()
	c, err := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), ClientOptions{
		BinPath: "ipmitool", // unused because Runner is injected
		Runner:  fr,
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func sampleCred() bmc.PasswordedCredential {
	return bmc.PasswordedCredential{
		Credential: bmc.Credential{
			MachineUUID:   "4c4c4544-0058-3210-8053-c5c04f463830",
			IP:            "10.0.0.7",
			Port:          623,
			Username:      "ADMIN",
			IPMIInterface: "lanplus",
		},
		Password: "hunter2",
	}
}

func TestNewClientRequiresLogger(t *testing.T) {
	if _, err := NewClient(nil, ClientOptions{Runner: &fakeRunner{}}); err == nil {
		t.Errorf("nil logger should fail")
	}
}

func TestNewClientNoFakeNoBinary(t *testing.T) {
	// With no Runner and a binary that won't exist anywhere, LookPath fails.
	_, err := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), ClientOptions{
		BinPath: "/nonexistent/no-such-ipmitool-zzz",
	})
	if err == nil {
		t.Errorf("missing binary should fail")
	}
}

func TestSetBootDevicePXE(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("Set Boot Device to pxe\n")}
	c := newClient(t, fr)
	if err := c.SetBootDevice(context.Background(), sampleCred(), BootDevicePXE); err != nil {
		t.Fatalf("SetBootDevice: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(fr.calls))
	}
	args := fr.calls[0].args
	wantTail := []string{"chassis", "bootdev", "pxe", "options=efiboot"}
	if !endsWith(args, wantTail) {
		t.Errorf("args tail=%v want %v (full=%v)", args[len(args)-4:], wantTail, args)
	}
	if !containsAll(args, []string{"-H", "10.0.0.7", "-p", "623", "-I", "lanplus", "-U", "ADMIN", "-E"}) {
		t.Errorf("args missing standard fields: %v", args)
	}
	if containsString(args, "hunter2") {
		t.Errorf("password leaked into argv: %v", args)
	}
	if fr.envFound != "hunter2" {
		t.Errorf("IPMI_PASSWORD env not passed (got %q)", fr.envFound)
	}
}

func TestSetBootDeviceDisk(t *testing.T) {
	fr := &fakeRunner{}
	c := newClient(t, fr)
	if err := c.SetBootDevice(context.Background(), sampleCred(), BootDeviceDisk); err != nil {
		t.Fatalf("SetBootDevice disk: %v", err)
	}
	if !endsWith(fr.calls[0].args, []string{"chassis", "bootdev", "disk", "options=efiboot"}) {
		t.Errorf("disk args: %v", fr.calls[0].args)
	}
}

func TestSetBootDeviceUnknown(t *testing.T) {
	c := newClient(t, &fakeRunner{})
	if err := c.SetBootDevice(context.Background(), sampleCred(), BootDevice("floppy")); err == nil {
		t.Errorf("unsupported device should fail")
	}
}

func TestPowerCycle(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("Chassis Power Control: Cycle\n")}
	c := newClient(t, fr)
	if err := c.PowerCycle(context.Background(), sampleCred()); err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	if !endsWith(fr.calls[0].args, []string{"chassis", "power", "cycle"}) {
		t.Errorf("args: %v", fr.calls[0].args)
	}
}

func TestPowerStatusOn(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("Chassis Power is on\n")}
	c := newClient(t, fr)
	s, err := c.PowerStatus(context.Background(), sampleCred())
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if s != "on" {
		t.Errorf("got %q want on", s)
	}
}

func TestPowerStatusOff(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("Chassis Power is off\n")}
	c := newClient(t, fr)
	s, _ := c.PowerStatus(context.Background(), sampleCred())
	if s != "off" {
		t.Errorf("got %q want off", s)
	}
}

func TestPowerStatusUnknown(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("garbage\n")}
	c := newClient(t, fr)
	s, _ := c.PowerStatus(context.Background(), sampleCred())
	if s != "unknown" {
		t.Errorf("got %q want unknown", s)
	}
}

func TestPowerStatusErrorPropagates(t *testing.T) {
	fr := &fakeRunner{err: errors.New("conn refused"), stderr: []byte("Unable to establish IPMI v2 / RMCP+ session\n")}
	c := newClient(t, fr)
	if _, err := c.PowerStatus(context.Background(), sampleCred()); err == nil {
		t.Errorf("BMC unreachable should propagate as error")
	}
}

func TestBootForPXEComposite(t *testing.T) {
	fr := &fakeRunner{}
	c := newClient(t, fr)
	if err := c.BootForPXE(context.Background(), sampleCred()); err != nil {
		t.Fatalf("BootForPXE: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("calls=%d want 2", len(fr.calls))
	}
	if !endsWith(fr.calls[0].args, []string{"chassis", "bootdev", "pxe", "options=efiboot"}) {
		t.Errorf("first call: %v", fr.calls[0].args)
	}
	if !endsWith(fr.calls[1].args, []string{"chassis", "power", "cycle"}) {
		t.Errorf("second call: %v", fr.calls[1].args)
	}
}

func TestBootForPXEStopsOnFirstError(t *testing.T) {
	fr := &fakeRunner{err: errors.New("auth failure"), stderr: []byte("invalid user")}
	c := newClient(t, fr)
	if err := c.BootForPXE(context.Background(), sampleCred()); err == nil {
		t.Fatalf("error should propagate")
	}
	if len(fr.calls) != 1 {
		t.Errorf("should stop after first call, got %d", len(fr.calls))
	}
}

func TestFinalizeBootDisk(t *testing.T) {
	fr := &fakeRunner{}
	c := newClient(t, fr)
	if err := c.FinalizeBootDisk(context.Background(), sampleCred()); err != nil {
		t.Fatalf("FinalizeBootDisk: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("calls=%d want 2 (bootdev=disk + power cycle)", len(fr.calls))
	}
	if !endsWith(fr.calls[0].args, []string{"chassis", "bootdev", "disk", "options=efiboot"}) {
		t.Errorf("first call: %v", fr.calls[0].args)
	}
	if !endsWith(fr.calls[1].args, []string{"chassis", "power", "cycle"}) {
		t.Errorf("second call: %v", fr.calls[1].args)
	}
}

// TestFinalizeBootDiskStopsOnFirstError mirrors the BootForPXE error-path
// test: if SetBootDevice fails we MUST NOT issue the power cycle (would
// reboot into PXE again).
func TestFinalizeBootDiskStopsOnFirstError(t *testing.T) {
	fr := &fakeRunner{err: errors.New("auth failure"), stderr: []byte("invalid user")}
	c := newClient(t, fr)
	if err := c.FinalizeBootDisk(context.Background(), sampleCred()); err == nil {
		t.Fatalf("error should propagate")
	}
	if len(fr.calls) != 1 {
		t.Errorf("should stop after first call, got %d", len(fr.calls))
	}
}

func TestTimeoutKillsSlowChild(t *testing.T) {
	fr := &fakeRunner{delay: 200 * time.Millisecond}
	c, _ := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), ClientOptions{
		Runner:  fr,
		Timeout: 50 * time.Millisecond,
	})
	err := c.PowerCycle(context.Background(), sampleCred())
	if err == nil {
		t.Errorf("slow call should hit timeout")
	}
}

func TestRedactStripsPasswordFromStderr(t *testing.T) {
	fr := &fakeRunner{
		err:    errors.New("exit 1"),
		stderr: []byte("Password=hunter2 rejected"),
	}
	c := newClient(t, fr)
	err := c.PowerCycle(context.Background(), sampleCred())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("error should contain *** redaction marker: %v", err)
	}
}

func TestDefaultPortAndInterface(t *testing.T) {
	fr := &fakeRunner{}
	c := newClient(t, fr)
	cred := sampleCred()
	cred.Port = 0
	cred.IPMIInterface = ""
	if err := c.PowerCycle(context.Background(), cred); err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	if !containsAll(fr.calls[0].args, []string{"-p", "623", "-I", "lanplus"}) {
		t.Errorf("defaults missing: %v", fr.calls[0].args)
	}
}

func endsWith(haystack, tail []string) bool {
	if len(haystack) < len(tail) {
		return false
	}
	off := len(haystack) - len(tail)
	for i, v := range tail {
		if haystack[off+i] != v {
			return false
		}
	}
	return true
}

func containsAll(haystack, needles []string) bool {
	// needles is a flat sequence; verify it appears in order as a contiguous slice.
	for i := 0; i+len(needles) <= len(haystack); i++ {
		match := true
		for j, n := range needles {
			if haystack[i+j] != n {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func containsString(haystack []string, s string) bool {
	for _, v := range haystack {
		if v == s {
			return true
		}
	}
	return false
}

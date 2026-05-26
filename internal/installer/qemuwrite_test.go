package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteImage_SpoolsAndConverts(t *testing.T) {
	work := t.TempDir()
	exec := newMockExec()
	deps := Deps{Exec: exec, FS: newMockFS(), WorkDir: work}

	payload := "THIS_IS_A_FAKE_QCOW2_PAYLOAD"
	if err := WriteImage(context.Background(), deps, strings.NewReader(payload), "/dev/sda"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	calls := exec.Calls()
	if len(calls) < 3 {
		t.Fatalf("want at least 3 Run calls (qemu-img, sync, partprobe), got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "qemu-img" {
		t.Fatalf("first call should be qemu-img, got %s", calls[0].Name)
	}
	wantArgs := []string{"convert", "-f", "qcow2", "-O", "raw", filepath.Join(work, "image.qcow2"), "/dev/sda"}
	if len(calls[0].Args) != len(wantArgs) {
		t.Fatalf("argv length mismatch: got %v want %v", calls[0].Args, wantArgs)
	}
	for i, a := range wantArgs {
		if calls[0].Args[i] != a {
			t.Fatalf("argv[%d]=%q want %q (full %v)", i, calls[0].Args[i], a, calls[0].Args)
		}
	}
	if calls[1].Name != "sync" {
		t.Fatalf("second call should be sync, got %s", calls[1].Name)
	}
	if calls[2].Name != "partprobe" || calls[2].Args[0] != "/dev/sda" {
		t.Fatalf("third call should be partprobe /dev/sda, got %v", calls[2])
	}

	// Spool file should have been cleaned up.
	if _, err := os.Stat(filepath.Join(work, "image.qcow2")); !os.IsNotExist(err) {
		t.Fatalf("spool file should have been removed, stat err=%v", err)
	}
}

func TestWriteImage_SpoolReceivesPayload(t *testing.T) {
	work := t.TempDir()
	exec := newMockExec()
	// Capture the spool file's contents before WriteImage deletes it,
	// by piggy-backing on the qemu-img Run hook.
	var captured []byte
	exec.OnFunc = func(name string, args []string) {
		if name == "qemu-img" && len(args) >= 6 {
			b, err := os.ReadFile(args[5])
			if err == nil {
				captured = b
			}
		}
	}
	deps := Deps{Exec: exec, FS: newMockFS(), WorkDir: work}

	payload := "THIS_IS_A_FAKE_QCOW2_PAYLOAD"
	if err := WriteImage(context.Background(), deps, strings.NewReader(payload), "/dev/sda"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(captured) != payload {
		t.Fatalf("spool didn't receive payload; got %q", captured)
	}
}

func TestWriteImage_PartprobeFailureIsNonFatal(t *testing.T) {
	exec := newMockExec()
	exec.On["partprobe"] = mockExecResult{Err: errString("not ready")}
	rep := &mockReporter{}
	deps := Deps{Exec: exec, FS: newMockFS(), Reporter: rep, WorkDir: t.TempDir()}

	if err := WriteImage(context.Background(), deps,
		strings.NewReader("x"), "/dev/sda"); err != nil {
		t.Fatalf("partprobe failure should not be fatal, got %v", err)
	}
	if len(rep.logs) == 0 {
		t.Fatalf("expected a warn log line about partprobe")
	}
}

func TestWriteImage_SyncFailureIsFatal(t *testing.T) {
	exec := newMockExec()
	exec.On["sync"] = mockExecResult{Err: errString("io error")}
	deps := Deps{Exec: exec, FS: newMockFS(), WorkDir: t.TempDir()}

	err := WriteImage(context.Background(), deps,
		strings.NewReader("x"), "/dev/sda")
	if err == nil {
		t.Fatal("sync failure must be reported as error")
	}
	if !strings.Contains(err.Error(), "sync") {
		t.Fatalf("err should mention sync: %v", err)
	}
}

func TestWriteImage_QemuImgError(t *testing.T) {
	exec := newMockExec()
	exec.On["qemu-img"] = mockExecResult{Err: errString("convert blew up")}
	deps := Deps{Exec: exec, FS: newMockFS(), WorkDir: t.TempDir()}
	err := WriteImage(context.Background(), deps,
		strings.NewReader("x"), "/dev/sda")
	if err == nil {
		t.Fatal("qemu-img error must surface")
	}
}

// errString is a quick way to fabricate an error in tests.
type errString string

func (e errString) Error() string { return string(e) }

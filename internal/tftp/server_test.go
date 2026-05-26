package tftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pin/tftp/v3"
)

func newTestFS() fs.FS {
	return fstest.MapFS{
		"undionly.kpxe": {Data: bytes.Repeat([]byte("A"), 1024)},
		"snponly.efi":   {Data: []byte("efi-binary-bytes")},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeReaderFrom captures bytes written via ReadFrom and optionally implements
// tftp.OutgoingTransfer.
type fakeReaderFrom struct {
	buf     bytes.Buffer
	size    int64
	hasSize bool
	addr    net.UDPAddr
}

func (f *fakeReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(&f.buf, r)
}

func (f *fakeReaderFrom) SetSize(n int64) {
	f.size = n
	f.hasSize = true
}

func (f *fakeReaderFrom) RemoteAddr() net.UDPAddr {
	return f.addr
}

func TestReadHandler_Success(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Logger: discardLogger()}, newTestFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rf := &fakeReaderFrom{addr: net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}}
	if err := s.readHandler("undionly.kpxe", rf); err != nil {
		t.Fatalf("readHandler: %v", err)
	}
	if rf.buf.Len() != 1024 {
		t.Errorf("expected 1024 bytes, got %d", rf.buf.Len())
	}
	if !rf.hasSize {
		t.Error("expected SetSize to be called")
	}
	if rf.size != 1024 {
		t.Errorf("expected size=1024, got %d", rf.size)
	}
}

func TestReadHandler_NotFound(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Logger: discardLogger()}, newTestFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rf := &fakeReaderFrom{}
	err = s.readHandler("missing.bin", rf)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestReadHandler_PathTraversal(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Logger: discardLogger()}, newTestFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []string{"../etc/passwd", "foo/../bar", "/etc/passwd"}
	for _, name := range cases {
		rf := &fakeReaderFrom{}
		err := s.readHandler(name, rf)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q: expected ErrNotExist, got %v", name, err)
		}
		if rf.buf.Len() != 0 {
			t.Errorf("%q: expected no bytes written, got %d", name, rf.buf.Len())
		}
	}
}

func TestNew_NilFS(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Logger: discardLogger()}, nil); err == nil {
		t.Error("expected error for nil fsys")
	}
}

// pickFreeUDPPort returns an unused UDP port on 127.0.0.1.
func pickFreeUDPPort(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := c.LocalAddr().String()
	c.Close()
	return addr
}

func TestServerEndToEnd(t *testing.T) {
	addr := pickFreeUDPPort(t)
	srv, err := New(Config{ListenAddr: addr, Logger: discardLogger()}, newTestFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	// Give the server a moment to bind.
	time.Sleep(150 * time.Millisecond)

	cli, err := tftp.NewClient(addr)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cli.SetTimeout(2 * time.Second)
	cli.RequestTSize(true)

	t.Run("download", func(t *testing.T) {
		wt, err := cli.Receive("undionly.kpxe", "octet")
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		var buf bytes.Buffer
		n, err := wt.WriteTo(&buf)
		if err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		if n != 1024 {
			t.Errorf("expected 1024 bytes, got %d", n)
		}
		if !bytes.Equal(buf.Bytes(), bytes.Repeat([]byte("A"), 1024)) {
			t.Error("content mismatch")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, err := cli.Receive("does-not-exist", "octet")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "not found") &&
			!strings.Contains(err.Error(), "File not found") {
			t.Logf("got error (acceptable): %v", err)
		}
	})

	t.Run("traversal_rejected", func(t *testing.T) {
		_, err := cli.Receive("../etc/passwd", "octet")
		if err == nil {
			t.Fatal("expected error for traversal path")
		}
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestServer_BindFailure(t *testing.T) {
	// Bind something on a port, then have the server try the same.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer pc.Close()
	addr := pc.LocalAddr().String()

	srv, err := New(Config{ListenAddr: addr, Logger: discardLogger()}, newTestFS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected bind error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("Start did not return on bind failure")
		cancel()
	}
}

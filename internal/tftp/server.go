package tftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/pin/tftp/v3"
)

type Config struct {
	ListenAddr string
	Logger     *slog.Logger
}

type Server struct {
	cfg  Config
	fsys fs.FS
	srv  *tftp.Server
}

func New(cfg Config, fsys fs.FS) (*Server, error) {
	if fsys == nil {
		return nil, errors.New("tftp: fsys is nil")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":69"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, fsys: fsys}
	s.srv = tftp.NewServer(s.readHandler, nil)
	return s, nil
}

// readHandler serves a file from fsys read-only.
func (s *Server) readHandler(filename string, rf io.ReaderFrom) error {
	remote := ""
	if ot, ok := rf.(tftp.OutgoingTransfer); ok {
		ra := ot.RemoteAddr()
		remote = ra.String()
	}

	// Reject traversal and absolute paths.
	if strings.Contains(filename, "..") || strings.HasPrefix(filename, "/") {
		s.cfg.Logger.Debug("tftp: rejected unsafe path",
			"filename", filename, "remote_addr", remote)
		return os.ErrNotExist
	}

	f, err := s.fsys.Open(filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.cfg.Logger.Warn("tftp: file not found",
				"filename", filename, "remote_addr", remote)
			return os.ErrNotExist
		}
		s.cfg.Logger.Error("tftp: open failed",
			"filename", filename, "remote_addr", remote, "err", err)
		return err
	}
	defer f.Close()

	var size int64 = -1
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}

	if ot, ok := rf.(tftp.OutgoingTransfer); ok && size >= 0 {
		ot.SetSize(size)
	}

	s.cfg.Logger.Info("tftp: serving file",
		"filename", filename, "remote_addr", remote, "size", size)

	n, err := rf.ReadFrom(f)
	if err != nil {
		s.cfg.Logger.Error("tftp: send failed",
			"filename", filename, "remote_addr", remote, "sent", n, "err", err)
		return err
	}
	return nil
}

// Start runs the server until ctx is cancelled; returns nil on graceful
// shutdown, non-nil on fatal listen error.
func (s *Server) Start(ctx context.Context) error {
	s.cfg.Logger.Info("tftp: starting", "addr", s.cfg.ListenAddr)

	errCh := make(chan error, 1)
	var once sync.Once
	go func() {
		err := s.srv.ListenAndServe(s.cfg.ListenAddr)
		once.Do(func() { errCh <- err })
	}()

	select {
	case <-ctx.Done():
		s.cfg.Logger.Info("tftp: shutting down")
		s.srv.Shutdown()
		// Drain listen goroutine.
		<-errCh
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("tftp: listen failed: %w", err)
		}
		return nil
	}
}

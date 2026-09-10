package httpd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"metalkit/internal/audit"
	"metalkit/internal/authapi"
	"metalkit/internal/bindings"
	"metalkit/internal/bmc"
	"metalkit/internal/images"
	"metalkit/internal/inventory"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
	"metalkit/internal/sessions"
	"metalkit/internal/settings"
	"metalkit/internal/subnets"
	"metalkit/internal/util"
)

// Config configures the HTTP server.
type Config struct {
	ListenAddr string // e.g. ":8080" or "0.0.0.0:8080"
	ServerIP   string // IPv4 literal advertised in iPXE URLs
	BootDir    string // directory containing vmlinuz, initrd.img, filesystem.squashfs
	Logger     *slog.Logger

	// Optional inventory store. When set, mounts the /api/v1/* endpoints.
	Store *inventory.Store

	// Optional dependencies that enable DELETE /api/v1/machines/{uuid}. Both
	// must be set together. When either is nil, the delete route is not
	// registered (read-only inventory).
	MachineActiveJob inventory.ActiveJobChecker
	MachineBindings  inventory.BindingDeleter

	// Optional images API. When set, mounts the /api/v1/images* endpoints.
	Images *images.API

	// Optional profiles API. When set, mounts the /api/v1/profiles* endpoints.
	Profiles *profiles.API

	// Optional subnets API. When set, mounts the /api/v1/subnets* endpoints.
	Subnets *subnets.API

	// Optional bindings API. When set, mounts the /api/v1/bindings* endpoints.
	Bindings *bindings.API

	// Optional BMC credentials API. When set, mounts the /api/v1/bmc* endpoints.
	BMC *bmc.API

	// Optional jobs API (operator side). When set, mounts the /api/v1/jobs*
	// endpoints. Agent-side endpoints (claim, log, succeed/fail) are mounted
	// separately by the agent jobs handler.
	Jobs *jobs.API

	// Optional agent-facing jobs API. When set, mounts the
	// /api/v1/agent/jobs/* endpoints. These bypass Basic Auth — live-boot
	// agents have no credential store — and use a body-level machine_uuid
	// consistency check as a foot-gun guard (not real authentication).
	AgentJobs *jobs.AgentAPI

	// Optional util API. When set, mounts /api/v1/util/* (currently just
	// the SHA-512 crypt helper used by the operator UI).
	Util *util.API

	// Optional settings API. When set, mounts /api/v1/settings/* — used by
	// the UI to read and persist runtime DHCP config without rewriting
	// config.yaml on disk.
	Settings *settings.API

	// Optional audit API. When set, mounts GET /api/v1/audit for the
	// operator UI's audit page (who did what when).
	Audit *audit.API

	// Optional sessions store. When set, the auth middleware will accept
	// a metalkit_session cookie in addition to Basic Auth. Required for the
	// browser login flow; nil keeps Basic-Auth-only behavior (back-compat).
	Sessions *sessions.Store

	// Optional auth API. When set, mounts /api/v1/auth/{login,logout,me}.
	Auth *authapi.API

	// Optional Web UI handler. When set, mounts under /ui/.
	UI http.Handler

	// Basic Auth credentials. Empty AdminPass disables auth (open mode).
	AdminUser string
	AdminPass string

	// HTTPS, when non-nil, serves the listener over TLS. See HTTPSConfig.
	HTTPS *HTTPSConfig

	// Users are operator accounts (username + sha512crypt hash + role).
	// When non-empty the auth middleware accepts them in addition to the
	// legacy AdminUser/AdminPass basic-auth pair. Each has its own audit
	// identity.
	Users []OperatorUser
}

// OperatorUser is one operator account for the UI/API.
type OperatorUser struct {
	Username string
	PassHash string // $6$ sha512crypt, verified by util.VerifyCrypt
	Role     string // "admin" | "operator"
}

// Server serves the iPXE chain script and boot artifacts.
type Server struct {
	cfg      Config
	httpAddr string // ":PORT" form used inside iPXE URLs
	srv      *http.Server
	audit    *audit.Store
}

// New validates cfg and constructs a Server. ServerIP must be an IPv4 literal
// because the live-boot initramfs uses busybox-wget, which does not resolve DNS.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		return nil, errors.New("httpd: Logger is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("httpd: ListenAddr is required")
	}
	if cfg.BootDir == "" {
		return nil, errors.New("httpd: BootDir is required")
	}
	if ip := net.ParseIP(cfg.ServerIP); ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("httpd: ServerIP %q must be an IPv4 literal (live-boot busybox-wget has no DNS)", cfg.ServerIP)
	}

	httpAddr, err := derivePortSuffix(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("httpd: %w", err)
	}

	var auditStore *audit.Store
	if cfg.Audit != nil {
		auditStore = cfg.Audit.Store()
	}
	return &Server{cfg: cfg, httpAddr: httpAddr, audit: auditStore}, nil
}

// derivePortSuffix returns ":PORT" from either ":PORT" or "HOST:PORT".
func derivePortSuffix(addr string) (string, error) {
	if strings.HasPrefix(addr, ":") {
		return addr, nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid ListenAddr %q: %w", addr, err)
	}
	return ":" + port, nil
}

// Start runs the HTTP server until ctx is cancelled. It returns nil on a
// clean shutdown; non-context errors are returned as-is.
//
// When cfg.HTTPS is set the listener speaks TLS (static operator-provided
// keypair, or a self-signed certificate auto-generated on first boot).
// PXE/iPXE clients keep working: the iPXE binaries shipped with metalkit
// are built with TLS support and trust is not enforced on the boot path
// (see handleIPXE) — operators requiring strict TLS on /boot/* should put
// a real certificate in place.
func (s *Server) Start(ctx context.Context) error {
	mux := s.routes()
	// Order matters for the audit trail: auth outermost, then logging.
	// The auth middleware attaches the operator identity to the REQUEST
	// context it passes inward (r.WithContext), so the logging layer must
	// sit inside auth to see that identity — otherwise every audited call
	// would be attributed to "anonymous".
	handler := s.logMiddleware(sessionOrBasicAuth(s.cfg.AdminUser, s.cfg.AdminPass, s.cfg.Users, s.cfg.Sessions, s.cfg.Logger, s.auditMiddleware(mux)))
	s.srv = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout intentionally 0: boot file downloads can be slow.
	}

	useTLS := s.cfg.HTTPS != nil
	if useTLS {
		h := *s.cfg.HTTPS
		cert, err := ensureTLSMaterial(h, s.cfg.ServerIP, h.AutoDNSName, s.cfg.Logger)
		if err != nil {
			return err
		}
		s.srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Info("httpd listening", "addr", s.cfg.ListenAddr, "server_ip", s.cfg.ServerIP, "boot_dir", s.cfg.BootDir, "tls", useTLS)
		if s.cfg.AdminPass == "" && len(s.cfg.Users) == 0 {
			s.cfg.Logger.Warn("httpd auth disabled: no adminPass and no users — UI and /api/v1/* are open to the network")
		}
		var err error
		if useTLS {
			// Certificates come from TLSConfig; the file args stay empty.
			err = s.srv.ListenAndServeTLS("", "")
		} else {
			err = s.srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.cfg.Logger.Warn("httpd shutdown error", "err", err)
		}
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// routes builds the mux. Exposed so tests can mount it on httptest.NewServer.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/boot/ipxe", s.handleIPXE)
	mux.HandleFunc("/boot/vmlinuz", s.bootFile("vmlinuz"))
	mux.HandleFunc("/boot/initrd.img", s.bootFile("initrd.img"))
	mux.HandleFunc("/boot/filesystem.squashfs", s.bootFile("filesystem.squashfs"))

	if s.cfg.Store != nil {
		inventory.RegisterRoutes(mux, s.cfg.Store, s.cfg.Logger.With("component", "api"))
		if s.cfg.MachineActiveJob != nil && s.cfg.MachineBindings != nil {
			inventory.RegisterDelete(mux, s.cfg.Store, s.cfg.MachineActiveJob, s.cfg.MachineBindings, s.cfg.Logger.With("component", "api"))
		}
	}
	if s.cfg.Images != nil {
		s.cfg.Images.RegisterRoutes(mux)
	}
	if s.cfg.Profiles != nil {
		s.cfg.Profiles.RegisterRoutes(mux)
	}
	if s.cfg.Subnets != nil {
		s.cfg.Subnets.RegisterRoutes(mux)
	}
	if s.cfg.Bindings != nil {
		s.cfg.Bindings.RegisterRoutes(mux)
	}
	if s.cfg.BMC != nil {
		s.cfg.BMC.RegisterRoutes(mux)
	}
	if s.cfg.Jobs != nil {
		s.cfg.Jobs.RegisterRoutes(mux)
	}
	if s.cfg.AgentJobs != nil {
		s.cfg.AgentJobs.RegisterRoutes(mux)
	}
	if s.cfg.Util != nil {
		s.cfg.Util.RegisterRoutes(mux)
	}
	if s.cfg.Settings != nil {
		s.cfg.Settings.RegisterRoutes(mux)
	}
	if s.cfg.Audit != nil {
		s.cfg.Audit.RegisterRoutes(mux)
	}
	if s.cfg.Auth != nil {
		s.cfg.Auth.RegisterRoutes(mux)
	}
	if s.cfg.UI != nil {
		// UI mounts under /ui/. The handler itself owns the subtree (server.go
		// in internal/webui handles redirect from /ui → /ui/ and serves the
		// embedded assets).
		mux.Handle("/ui", s.cfg.UI)
		mux.Handle("/ui/", s.cfg.UI)
	}

	// Anything else: 404 — but bare "/" redirects to the Web UI when one is
	// mounted so operators can just point their browser at the controller's
	// port and land on the dashboard ("访问端口就能访问到平台"). Unknown
	// non-root paths still 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && s.cfg.UI != nil {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleIPXE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := renderIPXE(s.cfg.ServerIP, s.httpAddr)
	if err != nil {
		s.cfg.Logger.Error("ipxe render failed", "err", err)
		http.Error(w, "ipxe render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// bootFile returns a handler that serves a single named file from BootDir.
// Uses http.ServeFile so Range/If-Modified-Since/etc. just work.
func (s *Server) bootFile(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := filepath.Join(s.cfg.BootDir, name)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code and
// byte count so the logger can report them.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (s *Server) logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		s.cfg.Logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"remote_addr", r.RemoteAddr,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)

		// Mutating operator API calls are audited by the inner
		// auditMiddleware (mounted inside the auth middleware so it sees
		// the authenticated actor); this outer layer only writes the
		// access log above.
	})
}

// auditMiddleware wraps the mux and records every mutating operator API
// call into the audit store. It sits INSIDE the auth middleware so
// r.Context() carries the authenticated username (sessions.WithUser),
// which is exactly what the audit trail needs to attribute actions.
func (s *Server) auditMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.audit == nil || r.Method == http.MethodGet || r.Method == http.MethodHead ||
			!strings.HasPrefix(r.URL.Path, "/api/v1/") ||
			strings.HasPrefix(r.URL.Path, "/api/v1/agent/") ||
			strings.HasPrefix(r.URL.Path, "/api/v1/report") {
			h.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		outcome := "ok"
		if rec.status >= 400 {
			outcome = "failed"
		}
		actor := sessions.UserFromContext(r.Context())
		if actor == "" {
			actor = "anonymous" // e.g. failed login attempt itself
		}
		s.audit.Record(r.Context(), actor, r.Method+" "+r.URL.Path, "", outcome, map[string]any{
			"status": rec.status,
			"remote": remoteIP(r),
		})
	})
}

// remoteIP strips the port from RemoteAddr for audit records.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

package httpd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metalkit/internal/audit"
	"metalkit/internal/authapi"
	"metalkit/internal/bindings"
	"metalkit/internal/bmc"
	"metalkit/internal/config"
	"metalkit/internal/crypto"
	"metalkit/internal/images"
	"metalkit/internal/inventory"
	"metalkit/internal/jobs"
	"metalkit/internal/monitor"
	"metalkit/internal/profiles"
	"metalkit/internal/sessions"
	"metalkit/internal/settings"
	"metalkit/internal/sqlitedb"
	"metalkit/internal/subnets"
	"metalkit/internal/util"
)

// intendedOpen lists every route pattern that is DELIBERATELY reachable
// without credentials, with the reason. needsAuth is fail-closed: anything
// under /api/v1/ and /ui not listed here demands auth. Registering a new
// route without deciding its posture fails TestNeedsAuth_AllRegisteredRoutes.
var intendedOpen = map[string]string{
	"/healthz":                         "LB / install checks",
	"/boot/ipxe":                       "PXE iPXE script, plain HTTP",
	"/boot/vmlinuz":                    "PXE kernel",
	"/boot/initrd.img":                 "PXE initramfs",
	"/boot/filesystem.squashfs":        "PXE live root",
	"/boot/amd64/":                     "PXE arch-path kernel/initrd",
	"/boot/arm64/":                     "PXE arch-path kernel/initrd",
	"/":                                "redirect to /ui/ only; 404 otherwise",
	"POST /api/v1/report":              "live-boot agents, no credential store",
	"POST /api/v1/heartbeat/{uuid}":    "live agents heartbeat",
	"POST /api/v1/auth/login":          "login page itself",
	"POST /api/v1/auth/logout":         "logout must work with expired cookie",
	"POST /api/v1/agent/jobs/current":  "live agent job poll",
	"GET /api/v1/agent/jobs/current":   "live agent job poll",
	"GET /api/v1/agent/jobs/{id}/spec": "live agent job spec pull",
}

// agentWritePatterns are the agent *write* endpoints: open at the
// sessionOrBasicAuth layer (agents carry no credentials) and documented as
// guarded by unguessable 32-hex job/image IDs.
var agentWritePatterns = []string{
	"POST /api/v1/agent/jobs/{id}/claim",
	"POST /api/v1/agent/jobs/{id}/stage",
	"POST /api/v1/agent/jobs/{id}/logs",
	"POST /api/v1/agent/jobs/{id}/succeed",
	"POST /api/v1/agent/jobs/{id}/fail",
	"POST /api/v1/agent/metrics",
	"GET /api/v1/agent/images/{id}/blob",
}

func openPatternSet() map[string]bool {
	m := make(map[string]bool, len(intendedOpen)+len(agentWritePatterns))
	for p := range intendedOpen {
		m[p] = true
	}
	for _, p := range agentWritePatterns {
		m[p] = true
	}
	return m
}

// testCipher builds a throwaway master key for stores that encrypt at rest.
func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipher(bytes.Repeat([]byte{0x42}, crypto.KeySize))
	if err != nil {
		t.Fatalf("crypto.NewCipher: %v", err)
	}
	return c
}

// stubActiveJob / stubBindingDeleter satisfy inventory's machine-delete
// dependencies so the full-route fixture registers DELETE /machines/{uuid}.
type stubActiveJob struct{}

func (stubActiveJob) HasActiveJob(context.Context, string) (bool, error) { return false, nil }

type stubBindingDeleter struct{}

func (stubBindingDeleter) DeleteBindingForMachine(context.Context, string) error { return nil }

// newFullServer builds a Server with EVERY optional API attached, mirroring
// cmd/controller/main.go's wiring, so its mux carries the complete
// production route set.
func newFullServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: ":memory:", Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	inv, err := inventory.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	imgStore, err := images.NewStore(ctx, db, logger, t.TempDir())
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	profStore, err := profiles.NewStore(ctx, db, logger, "")
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	subStore, err := subnets.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("subnets: %v", err)
	}
	bindStore, err := bindings.NewStore(ctx, db, logger, nil)
	if err != nil {
		t.Fatalf("bindings: %v", err)
	}
	bmcStore, err := bmc.NewStore(ctx, db, logger, testCipher(t))
	if err != nil {
		t.Fatalf("bmc: %v", err)
	}
	jobStore, err := jobs.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	monStore, err := monitor.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("monitor: %v", err)
	}
	setStore, err := settings.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	auditStore, err := audit.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	sessStore, err := sessions.NewStore(ctx, db, logger)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}

	cfg := Config{
		ListenAddr: ":8080",
		ServerIP:   "10.99.0.1",
		BootDir:    t.TempDir(),
		Logger:     logger,
		AdminUser:  "admin",
		AdminPass:  "secret",
		Store:      inv,
		// MachineDelete deps (DELETE /api/v1/machines/{uuid} is registered
		// only when both are set — the controller always wires them).
		MachineActiveJob: stubActiveJob{},
		MachineBindings:  stubBindingDeleter{},
		Images:           images.NewAPI(imgStore, nil, logger),
		Profiles:         profiles.NewAPI(profStore, logger),
		Subnets:          subnets.NewAPI(subStore, logger),
		Bindings:         bindings.NewAPI(bindStore, logger),
		BMC:              bmc.NewAPI(bmcStore, logger),
		Jobs:             jobs.NewAPI(jobStore, logger),
		AgentJobs:        jobs.NewAgentAPI(jobStore, logger),
		Monitor:          monitor.NewAPI(monStore, logger),
		Util:             util.NewAPI(logger),
		Settings:         settings.NewAPI(setStore, &config.Config{}, logger),
		Audit:            audit.NewAPI(auditStore, logger),
		Auth: &authapi.API{
			Sessions:  sessStore,
			AdminUser: "admin",
			AdminPass: "secret",
			CookieTTL: 7 * 24 * time.Hour,
			Logger:    logger,
		},
		UI: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestNeedsAuth_AllRegisteredRoutes: every pattern in allRoutePatterns is
// (a) verified to actually exist on the production mux (mux.Handler must
// resolve this exact method+path to this exact pattern) and (b) checked
// against needsAuth (via a concrete example path) against the declared
// intent. Removing, renaming, or re-shaping a route fails (a); a route on
// the open list that needsAuth closes (or vice versa) fails (b).
//
// Known limitation, stated honestly: the stdlib offers no way to enumerate
// a ServeMux's registrations, so a BRAND-NEW route added without touching
// this table is NOT automatically caught here — needsAuth's fail-closed
// default means such a route ships authenticated (the safe direction), and
// the gap is policed by review + periodic recount (a full hand recount in
// Oct 2026 found 78 registrations; the table must match that count).
func TestNeedsAuth_AllRegisteredRoutes(t *testing.T) {
	s := newFullServer(t)
	mux := s.routes()

	// Control: a path with no specific registration falls through to the
	// catch-all "/" — otherwise the resolvePattern check below proves nothing.
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/v1/definitely-not-registered", nil)); pat != "/" {
		t.Fatalf("control probe resolved to %q, want catch-all \"/\" — resolvePattern cannot detect unregistered paths", pat)
	}
	if n := len(allRoutePatterns); n != 78 {
		t.Errorf("allRoutePatterns has %d entries; the mux registers 78 (recounted Oct 2026) — update both the table and this number when routes change", n)
	}

	open := openPatternSet()

	for _, pat := range allRoutePatterns {
		if patternPath(pat) == "/" {
			continue // catch-all; matches everything by design
		}
		example := wildcardExample(patternPath(pat))

		// (a) the table entry is genuinely registered: the mux must resolve
		// this exact method+path to this exact pattern.
		if got := resolvePattern(t, mux, patternMethod(pat), example); got != pat {
			t.Errorf("%s: mux resolves %s to pattern %q — stale or wrong table entry", pat, example, got)
			continue
		}

		// (b) auth posture matches the declared intent.
		wantOpen := open[pat]
		isOpen := !needsAuth(example)
		switch {
		case wantOpen && !isOpen:
			t.Errorf("%s: declared open but needsAuth requires auth (example %s)", pat, example)
		case !wantOpen && isOpen:
			t.Errorf("%s: needsAuth leaves it OPEN but it is not on the open list (example %s)", pat, example)
		}
	}
}

// patternMethod returns the METHOD part of "<METHOD> <path>", defaulting to
// GET for path-only patterns.
func patternMethod(pat string) string {
	if i := strings.Index(pat, " "); i > 0 {
		return pat[:i]
	}
	return http.MethodGet
}

// allRoutePatterns mirrors every mux registration in httpd/server.go's
// routes() plus the per-package RegisterRoutes implementations. The count
// assertion in the test above turns "forgot to add a new route" into a
// failure. Patterns appear as Go 1.22 ServeMux writes them.
var allRoutePatterns = []string{
	"/healthz",
	"/boot/ipxe",
	"/boot/vmlinuz",
	"/boot/initrd.img",
	"/boot/filesystem.squashfs",
	"/boot/amd64/",
	"/boot/arm64/",
	"/ui",
	"/ui/",
	"/",
	// inventory (report/heartbeat register in inventory's agent handlers)
	"POST /api/v1/report",
	"POST /api/v1/heartbeat/{uuid}",
	"GET /api/v1/machines",
	"GET /api/v1/machines/{uuid}",
	"GET /api/v1/machines/{uuid}/reports",
	"GET /api/v1/machines/{uuid}/reports/{id}",
	"DELETE /api/v1/machines/{uuid}",
	"GET /api/v1/lookup",
	// images
	"GET /api/v1/images",
	"POST /api/v1/images/uploads",
	"GET /api/v1/images/uploads/{uid}",
	"PUT /api/v1/images/uploads/{uid}/chunks/{n}",
	"POST /api/v1/images/uploads/{uid}/finalize",
	"DELETE /api/v1/images/uploads/{uid}",
	"GET /api/v1/images/{id}",
	"DELETE /api/v1/images/{id}",
	// profiles
	"GET /api/v1/profiles",
	"GET /api/v1/profiles/components",
	"POST /api/v1/profiles",
	"GET /api/v1/profiles/{id}",
	"PUT /api/v1/profiles/{id}",
	"DELETE /api/v1/profiles/{id}",
	// subnets
	"GET /api/v1/subnets",
	"POST /api/v1/subnets",
	"GET /api/v1/subnets/{id}",
	"PUT /api/v1/subnets/{id}",
	"DELETE /api/v1/subnets/{id}",
	// bindings
	"GET /api/v1/bindings",
	"GET /api/v1/bindings/{uuid}",
	"PUT /api/v1/bindings/{uuid}",
	"DELETE /api/v1/bindings/{uuid}",
	"GET /api/v1/bindings/{uuid}/password",
	// bmc
	"GET /api/v1/bmc",
	"POST /api/v1/bmc",
	"POST /api/v1/bmc/{uuid}/onboard",
	"GET /api/v1/bmc/{uuid}",
	"PUT /api/v1/bmc/{uuid}",
	"POST /api/v1/bmc/{uuid}/power/{action}",
	"POST /api/v1/bmc/{uuid}/test",
	"POST /api/v1/bmc/{uuid}/reconcile",
	"DELETE /api/v1/bmc/{uuid}",
	// jobs (operator)
	"GET /api/v1/jobs",
	"POST /api/v1/jobs/purge",
	"GET /api/v1/jobs/{id}",
	"GET /api/v1/jobs/{id}/logs",
	"POST /api/v1/jobs/{id}/cancel",
	"DELETE /api/v1/jobs/{id}",
	// jobs (agent)
	"GET /api/v1/agent/jobs/current",
	"GET /api/v1/agent/jobs/{id}/spec",
	"POST /api/v1/agent/jobs/{id}/claim",
	"POST /api/v1/agent/jobs/{id}/fail",
	"POST /api/v1/agent/jobs/{id}/logs",
	"POST /api/v1/agent/jobs/{id}/stage",
	"POST /api/v1/agent/jobs/{id}/succeed",
	"GET /api/v1/agent/images/{id}/blob",
	// monitor
	"GET /api/v1/metrics",
	"GET /api/v1/metrics/{uuid}",
	"POST /api/v1/agent/metrics",
	// util
	"POST /api/v1/util/crypt-sha512",
	// settings
	"GET /api/v1/settings/dhcp",
	"PUT /api/v1/settings/dhcp",
	"GET /api/v1/settings/interfaces",
	"GET /api/v1/settings/storage",
	"PUT /api/v1/settings/storage",
	// audit
	"GET /api/v1/audit",
	// auth
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
	"GET /api/v1/auth/me",
}

// patternPath strips the leading "<METHOD> " from a pattern if present.
func patternPath(pat string) string {
	if i := strings.Index(pat, " "); i > 0 {
		return pat[i+1:]
	}
	return pat
}

// resolvePattern asks the mux itself which pattern a concrete method+path
// resolves to (net/http's public (*ServeMux).Handler); "" means it fell
// through to no pattern. Method-specific routes ("GET /api/v1/...") only
// match when the method matches, which is exactly what we want.
func resolvePattern(t *testing.T, mux *http.ServeMux, method, path string) string {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	_, pattern := mux.Handler(req)
	return pattern
}

// wildcardExample replaces {name} (and trailing {path...}) placeholders with
// representative values so needsAuth sees a concrete path.
func wildcardExample(pattern string) string {
	out := pattern
	out = strings.ReplaceAll(out, "{uuid}", "4c4c4544-0058-3210-8053-c5c04f463830")
	out = strings.ReplaceAll(out, "{id}", "abcd1234abcd1234abcd1234abcd1234")
	out = strings.ReplaceAll(out, "{uid}", "abcd1234abcd1234abcd1234abcd1234")
	out = strings.ReplaceAll(out, "{n}", "1")
	out = strings.ReplaceAll(out, "{action}", "on")
	out = strings.ReplaceAll(out, "{path...}", "index.html")
	for strings.Contains(out, "{") {
		i := strings.Index(out, "{")
		j := strings.Index(out[i:], "}")
		if j < 0 {
			break
		}
		out = out[:i] + "placeholder" + out[i+j+1:]
	}
	return out
}

package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"metalkit/internal/audit"
	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
)

// TestAuditMiddlewareAttributesActor reproduces the production chain
// (log(auth(audit(mux)))) and asserts the audit record carries the
// authenticated actor, not "anonymous".
func TestAuditMiddlewareAttributesActor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := sqlitedb.Open(t.Context(), sqlitedb.Options{
		Path:   filepath.Join(t.TempDir(), "t.db"),
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sessStore, err := sessions.NewStore(t.Context(), db, logger)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}

	auditStore, err := audit.NewStore(t.Context(), db, logger)
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}

	handlerUser := ""
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/subnets", func(w http.ResponseWriter, r *http.Request) {
		handlerUser = sessions.UserFromContext(r.Context())
		w.WriteHeader(http.StatusCreated)
	})

	s := &Server{cfg: Config{Logger: logger}, audit: auditStore}
	chain := s.logMiddleware(sessionOrBasicAuth("admin", "secret", nil,
		sessStore, logger, s.auditMiddleware(mux)))

	ts := httptest.NewServer(chain)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/subnets", nil)
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (auth must pass through)", resp.StatusCode)
	}
	if handlerUser != "admin" {
		t.Fatalf("handler saw user %q, want admin (chain broken before mux)", handlerUser)
	}

	events, err := auditStore.List(t.Context(), audit.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no audit event recorded")
	}
	if events[0].Actor != "admin" {
		t.Errorf("audit actor = %q, want admin", events[0].Actor)
	}
}

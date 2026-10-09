package httpd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"metalkit/internal/apiutil"
	"metalkit/internal/images"
	"metalkit/internal/inventory"
	"metalkit/internal/sessions"
	"metalkit/internal/sqlitedb"
)

// TestSessionUserRecordedAsUploadActor is the regression test for the audit
// gap: a browser admin authenticated via session cookie used to be recorded
// as "anonymous" in uploaded_by / audit rows, because handlers resolved the
// actor from r.BasicAuth() only. The full chain — sessionOrBasicAuth puts
// the username in the context, apiutil.RequestActor reads it — is exercised
// end-to-end by uploading an image init request with only a session cookie.
func TestSessionUserRecordedAsUploadActor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: ":memory:", Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	inv, err := inventory.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	imgStore, err := images.NewStore(context.Background(), db, logger, t.TempDir())
	if err != nil {
		t.Fatalf("images: %v", err)
	}
	var seenActor string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenActor = apiutil.RequestActor(r)
		apiutil.WriteJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	_ = imgStore

	cfg := Config{
		ListenAddr: ":8080",
		ServerIP:   "10.99.0.1",
		BootDir:    t.TempDir(),
		Logger:     logger,
		AdminUser:  "admin",
		AdminPass:  "secret",
		Store:      inv,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s

	ss, err := sessions.NewStore(context.Background(), db, logger)
	if err != nil {
		t.Fatalf("sessions.NewStore: %v", err)
	}
	sess, err := ss.Create(context.Background(), "webadmin", 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Sessions.Create: %v", err)
	}

	handler := sessionOrBasicAuth("admin", "secret", nil, ss, logger, h)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Request with ONLY a session cookie — no Authorization header.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/machines", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session-authed request: status=%d want 200", resp.StatusCode)
	}
	if seenActor != "webadmin" {
		t.Errorf("RequestActor with session cookie: got %q want %q (the session username)", seenActor, "webadmin")
	}

	// Basic Auth still wins the actor when no session is present. Note
	// sessionOrBasicAuth only accepts the configured (adminUser, adminPass)
	// pair, so the actor's username comes from that pair.
	seenActor = ""
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/machines", nil)
	req2.Header.Set("Authorization", basicAuthValue("admin", "secret"))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do basic: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("basic-authed request: status=%d want 200", resp2.StatusCode)
	}
	if seenActor != "admin" {
		t.Errorf("RequestActor with basic auth: got %q want %q", seenActor, "admin")
	}
}

var _ = json.Marshal // keep encoding/json if future assertions need it

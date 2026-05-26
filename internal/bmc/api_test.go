package bmc

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"metalkit/internal/crypto"
	"metalkit/internal/sqlitedb"
)

type apiFixture struct {
	db    *sql.DB
	store *Store
	ts    *httptest.Server
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.Open(context.Background(), sqlitedb.Options{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("sqlitedb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(context.Background(), `
        CREATE TABLE IF NOT EXISTS machines (
            uuid TEXT PRIMARY KEY,
            first_seen INTEGER,
            last_seen INTEGER,
            status TEXT,
            latest_report INTEGER
        )`); err != nil {
		t.Fatalf("create machines stub: %v", err)
	}

	cip, err := crypto.NewCipher(bytes.Repeat([]byte{0x42}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	s, err := NewStore(context.Background(), db, logger, cip)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a := NewAPI(s, logger)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &apiFixture{db: db, store: s, ts: ts}
}

func (f *apiFixture) seedMachine(t *testing.T, suffix byte) string {
	t.Helper()
	uuid := "4c4c4544-0058-3210-8053-c5c04f46383" + string(suffix)
	now := time.Now().Unix()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO machines (uuid, first_seen, last_seen, status, latest_report)
         VALUES (?, ?, ?, 'online', NULL)`, uuid, now, now); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	return uuid
}

func doReq(t *testing.T, method, url string, payload any) (*http.Response, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, body)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAPIUpsertHappy(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '0')
	resp, body := doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip":             "10.0.0.7",
		"username":       "ADMIN",
		"password":       "hunter2",
		"ipmi_interface": "lanplus",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var c Credential
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.IP != "10.0.0.7" || c.Username != "ADMIN" || c.Port != 623 {
		t.Errorf("response: %+v", c)
	}
}

func TestAPIGetOmitsPassword(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '1')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.8", "username": "u", "password": "topsecret",
	})
	resp, body := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/"+mu, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte("topsecret")) {
		t.Errorf("GET response leaks plaintext password: %s", body)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("password")) {
		t.Errorf("GET response contains \"password\" key: %s", body)
	}
}

func TestAPIListOmitsPassword(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '2')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.9", "username": "u", "password": "verytopsecret",
	})
	resp, body := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte("verytopsecret")) {
		t.Errorf("LIST response leaks plaintext: %s", body)
	}
	if bytes.Contains(bytes.ToLower(body), []byte("password")) {
		t.Errorf("LIST response contains \"password\" key: %s", body)
	}
}

func TestAPIUpsertUnknownMachine(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/ffffffff-ffff-ffff-ffff-ffffffffffff",
		map[string]any{"ip": "10.0.0.7", "username": "u", "password": "p"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422, body=%s", resp.StatusCode, body)
	}
}

func TestAPIUpsertMissingPassword(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '3')
	resp, body := doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.7", "username": "u",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", resp.StatusCode, body)
	}
}

func TestAPIUpsertUnknownField(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '4')
	resp, _ := doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.7", "username": "u", "password": "p", "extra": "field",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 for unknown field", resp.StatusCode)
	}
}

func TestAPIGetNotFound(t *testing.T) {
	f := newAPIFixture(t)
	resp, _ := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/4c4c4544-0058-3210-8053-c5c04f463830", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

func TestAPIDelete(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '5')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.7", "username": "u", "password": "p",
	})
	resp, _ := doReq(t, http.MethodDelete, f.ts.URL+"/api/v1/bmc/"+mu, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d want 204", resp.StatusCode)
	}
	resp2, _ := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/"+mu, nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete status=%d want 404", resp2.StatusCode)
	}
}

func TestAPIInvalidUUID(t *testing.T) {
	f := newAPIFixture(t)
	resp, _ := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/not-a-uuid", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestAPIUpsertReplacePassword(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '6')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.7", "username": "u", "password": "first",
	})
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.7", "username": "u", "password": "second",
	})
	got, err := f.store.GetWithPassword(context.Background(), mu)
	if err != nil {
		t.Fatalf("GetWithPassword: %v", err)
	}
	if got.Password != "second" {
		t.Errorf("password after replace: got %q want \"second\"", got.Password)
	}
}

// stubTester is a deterministic Tester for /test and /power endpoint tests.
type stubTester struct {
	power      string
	err        error
	calls      int
	last       PasswordedCredential
	actionErr  error
	lastAction string
}

func (s *stubTester) PowerStatus(_ context.Context, c PasswordedCredential) (string, error) {
	s.calls++
	s.last = c
	return s.power, s.err
}

func (s *stubTester) PowerOn(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "on"
	return s.actionErr
}

func (s *stubTester) PowerOff(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "off"
	return s.actionErr
}

func (s *stubTester) PowerCycle(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "cycle"
	return s.actionErr
}

func (s *stubTester) PowerSoft(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "soft"
	return s.actionErr
}

func (s *stubTester) PowerReset(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "reset"
	return s.actionErr
}

func (s *stubTester) BootForPXE(_ context.Context, c PasswordedCredential) error {
	s.calls++
	s.last = c
	s.lastAction = "onboard"
	return s.actionErr
}

func newAPIFixtureWithTester(t *testing.T, te Tester) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	// Re-wire the mux with a tester-equipped API hitting the same store.
	a := NewAPI(f.store, slog.New(slog.NewTextHandler(io.Discard, nil))).WithTester(te)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	f.ts.Close()
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

func TestAPITestPowerOn(t *testing.T) {
	te := &stubTester{power: "on"}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, '7')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.10", "username": "ADMIN", "password": "secret",
	})
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["ok"] != true || m["power"] != "on" {
		t.Errorf("response: %v", m)
	}
	if te.calls != 1 {
		t.Errorf("tester calls = %d, want 1", te.calls)
	}
	if te.last.Password != "secret" {
		t.Errorf("tester saw password %q, want secret", te.last.Password)
	}
}

func TestAPITestPowerError(t *testing.T) {
	te := &stubTester{err: errors.New("auth failed")}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, '8')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.11", "username": "ADMIN", "password": "secret",
	})
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/test", nil)
	// Errors return 200 with {ok:false, error:...} so the UI can surface them.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["ok"] != false || !strings.Contains(m["error"].(string), "auth failed") {
		t.Errorf("response: %v", m)
	}
}

func TestAPITestNotFound(t *testing.T) {
	te := &stubTester{}
	f := newAPIFixtureWithTester(t, te)
	resp, _ := doReq(t, http.MethodPost,
		f.ts.URL+"/api/v1/bmc/4c4c4544-0058-3210-8053-c5c04f463830/test", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	if te.calls != 0 {
		t.Errorf("tester should not have been called")
	}
}

func TestAPITestNoTester(t *testing.T) {
	// Default fixture has no tester wired.
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '9')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.12", "username": "u", "password": "p",
	})
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/test", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

func TestAPIPowerActions(t *testing.T) {
	for _, action := range []string{"on", "off", "cycle", "soft", "reset"} {
		t.Run(action, func(t *testing.T) {
			te := &stubTester{}
			f := newAPIFixtureWithTester(t, te)
			mu := f.seedMachine(t, 'b')
			_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
				"ip": "10.0.0.20", "username": "ADMIN", "password": "secret",
			})
			resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/power/"+action, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if m["ok"] != true || m["action"] != action {
				t.Errorf("response: %v", m)
			}
			if te.lastAction != action {
				t.Errorf("tester saw action %q want %q", te.lastAction, action)
			}
			if te.last.Password != "secret" {
				t.Errorf("tester saw password %q want secret", te.last.Password)
			}
		})
	}
}

func TestAPIPowerError(t *testing.T) {
	te := &stubTester{actionErr: errors.New("BMC unreachable")}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, 'c')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.21", "username": "u", "password": "p",
	})
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/power/cycle", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["ok"] != false || !strings.Contains(m["error"].(string), "BMC unreachable") {
		t.Errorf("response: %v", m)
	}
}

func TestAPIPowerBadAction(t *testing.T) {
	te := &stubTester{}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, 'd')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.22", "username": "u", "password": "p",
	})
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/power/destroy", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	if te.calls != 0 {
		t.Errorf("tester should not have been called for invalid action")
	}
}

func TestAPIPowerNotFound(t *testing.T) {
	te := &stubTester{}
	f := newAPIFixtureWithTester(t, te)
	resp, _ := doReq(t, http.MethodPost,
		f.ts.URL+"/api/v1/bmc/4c4c4544-0058-3210-8053-c5c04f463830/power/on", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	if te.calls != 0 {
		t.Errorf("tester should not have been called")
	}
}

func TestAPIPowerNoTester(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, 'e')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.23", "username": "u", "password": "p",
	})
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/power/cycle", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

func TestAPIOnboard(t *testing.T) {
	te := &stubTester{}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, 'f')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.30", "username": "ADMIN", "password": "secret",
	})
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/onboard", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["ok"] != true || m["action"] != "onboard" {
		t.Errorf("response: %v", m)
	}
	if te.lastAction != "onboard" {
		t.Errorf("tester saw action %q want onboard", te.lastAction)
	}
	if te.last.Password != "secret" {
		t.Errorf("tester saw password %q want secret", te.last.Password)
	}
}

func TestAPIOnboardError(t *testing.T) {
	te := &stubTester{actionErr: errors.New("BMC unreachable")}
	f := newAPIFixtureWithTester(t, te)
	mu := f.seedMachine(t, '0')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.31", "username": "u", "password": "p",
	})
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/onboard", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["ok"] != false || !strings.Contains(m["error"].(string), "BMC unreachable") {
		t.Errorf("response: %v", m)
	}
}

func TestAPIOnboardNotFound(t *testing.T) {
	te := &stubTester{}
	f := newAPIFixtureWithTester(t, te)
	resp, _ := doReq(t, http.MethodPost,
		f.ts.URL+"/api/v1/bmc/4c4c4544-0058-3210-8053-c5c04f463830/onboard", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
	if te.calls != 0 {
		t.Errorf("tester should not have been called")
	}
}

func TestAPIOnboardNoTester(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, '1')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.32", "username": "u", "password": "p",
	})
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc/"+mu+"/onboard", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", resp.StatusCode)
	}
}

// TestAPIUpdateOmitPasswordKeeps validates the password-omitempty semantics
// already documented in store.go: a PUT that omits password preserves the
// existing ciphertext on disk.
func TestAPIUpdateOmitPasswordKeeps(t *testing.T) {
	f := newAPIFixture(t)
	mu := f.seedMachine(t, 'a')
	_, _ = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.13", "username": "u", "password": "keep-me",
	})
	// PUT without password — must keep "keep-me" on disk.
	resp, body := doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+mu, map[string]any{
		"ip": "10.0.0.14", "username": "u2",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got, err := f.store.GetWithPassword(context.Background(), mu)
	if err != nil {
		t.Fatalf("GetWithPassword: %v", err)
	}
	if got.Password != "keep-me" {
		t.Errorf("password after omit-PUT: got %q want \"keep-me\"", got.Password)
	}
	if got.IP != "10.0.0.14" || got.Username != "u2" {
		t.Errorf("other fields not updated: %+v", got.Credential)
	}
}

// Sanity check that all error paths properly return JSON with "error" key.
func TestAPIErrorJSONShape(t *testing.T) {
	f := newAPIFixture(t)
	_, body := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/not-a-uuid", nil)
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(m["error"], "machine_uuid") {
		t.Errorf("error msg: %q", m["error"])
	}
}

func TestAPICreatePlaceholder(t *testing.T) {
	f := newAPIFixture(t)
	// No machine seeded — operator registers BMC by IP, server creates placeholder.
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip":             "192.168.10.254",
		"username":       "root",
		"password":       "calvin",
		"ipmi_interface": "lanplus",
		"name":           "rack01-r630-01",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var c Credential
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.MachineUUID != "placeholder-192-168-10-254" {
		t.Errorf("derived uuid=%q", c.MachineUUID)
	}
	if c.Name != "rack01-r630-01" {
		t.Errorf("name not round-tripped: %q", c.Name)
	}
	if c.IPMIInterface != "lanplus" || c.Port != 623 {
		t.Errorf("defaults missing: %+v", c)
	}
}

func TestAPICreateDuplicateIP(t *testing.T) {
	f := newAPIFixture(t)
	// First create succeeds.
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip": "10.0.0.5", "username": "u", "password": "p",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d %s", resp.StatusCode, body)
	}
	// Second create at same IP must 409.
	resp, body = doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip": "10.0.0.5", "username": "u2", "password": "p2",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("dup create: status=%d body=%s", resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := m["existing_machine_uuid"].(string); got != "placeholder-10-0-0-5" {
		t.Errorf("existing_machine_uuid=%v", m["existing_machine_uuid"])
	}
}

func TestAPICreateBadIP(t *testing.T) {
	f := newAPIFixture(t)
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip": "not-an-ip", "username": "u", "password": "p",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

func TestAPICreateMissingPassword(t *testing.T) {
	f := newAPIFixture(t)
	resp, body := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip": "10.0.0.6", "username": "u",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

// After POST creates a placeholder, GET/PUT/DELETE for that uuid should work.
func TestAPIPlaceholderRoundTrip(t *testing.T) {
	f := newAPIFixture(t)
	resp, _ := doReq(t, http.MethodPost, f.ts.URL+"/api/v1/bmc", map[string]any{
		"ip": "10.0.0.7", "username": "u", "password": "p", "name": "lab-a",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	uuid := "placeholder-10-0-0-7"

	resp, body := doReq(t, http.MethodGet, f.ts.URL+"/api/v1/bmc/"+uuid, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d body=%s", resp.StatusCode, body)
	}

	// PUT updates the name (password omitted to test keep-existing).
	resp, body = doReq(t, http.MethodPut, f.ts.URL+"/api/v1/bmc/"+uuid, map[string]any{
		"ip": "10.0.0.7", "username": "u2", "name": "lab-renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d body=%s", resp.StatusCode, body)
	}

	resp, _ = doReq(t, http.MethodDelete, f.ts.URL+"/api/v1/bmc/"+uuid, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
}

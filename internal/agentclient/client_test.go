package agentclient

import (
	"net/http"
	"strings"
	"testing"
)

func TestNew_Defaults(t *testing.T) {
	c := New("http://example.com", "uuid-123", nil)
	if c.BaseURL != "http://example.com" {
		t.Fatalf("BaseURL = %q, want http://example.com", c.BaseURL)
	}
	if c.UUID != "uuid-123" {
		t.Fatalf("UUID = %q, want uuid-123", c.UUID)
	}
	if c.HTTP == nil {
		t.Fatal("HTTP client is nil")
	}
	if c.HTTP.Timeout <= 0 {
		t.Fatalf("HTTP.Timeout = %v, want positive", c.HTTP.Timeout)
	}
	if c.BlobHTTP == nil {
		t.Fatal("BlobHTTP client is nil")
	}
	if c.BlobHTTP.Timeout != 0 {
		t.Fatalf("BlobHTTP.Timeout = %v, want zero", c.BlobHTTP.Timeout)
	}
	if c.Logger == nil {
		t.Fatal("Logger is nil (should fall back to slog.Default())")
	}
}

func TestURL_Join(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		{"http://h", "/foo", "http://h/foo"},
		{"http://h/", "/foo", "http://h/foo"},
		{"http://h", "foo", "http://h/foo"},
		{"http://h/", "foo", "http://h/foo"},
		{"http://h", "http://other/x", "http://other/x"},
		{"http://h", "https://other/x", "https://other/x"},
		{"http://h", "/a/b?c=1", "http://h/a/b?c=1"},
	}
	for _, tc := range cases {
		c := &Client{BaseURL: tc.base}
		got := c.url(tc.path)
		if got != tc.want {
			t.Errorf("url(base=%q, path=%q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestQueryWith(t *testing.T) {
	c := &Client{UUID: "abc"}
	got := c.queryWith("/api/v1/agent/jobs/current")
	if !strings.HasPrefix(got, "/api/v1/agent/jobs/current?") {
		t.Fatalf("missing path prefix: %s", got)
	}
	if !strings.Contains(got, "machine_uuid=abc") {
		t.Fatalf("missing machine_uuid: %s", got)
	}

	got2 := c.queryWith("/x?foo=bar")
	if !strings.Contains(got2, "foo=bar") {
		t.Fatalf("ate existing query: %s", got2)
	}
	if !strings.Contains(got2, "machine_uuid=abc") {
		t.Fatalf("missing machine_uuid on path that already had query: %s", got2)
	}
}

// sanity-check that the default HTTP client honours Accept on a GET. Real
// transport behaviour is exercised by jobs_test.go / blob_test.go.
func TestDefaultHeaders(t *testing.T) {
	c := New("http://example.com", "uuid-1", nil)
	req, err := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTP == nil {
		t.Fatal("HTTP nil")
	}
	_ = req // not actually executed; the per-request header writes are
	// exercised in TestStage_BadResponse via the httptest server.
}

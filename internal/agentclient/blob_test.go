package agentclient

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageBlob_Streams(t *testing.T) {
	// 2 MiB of pseudo-random bytes — large enough to ensure we actually stream
	// (default Go transport reads in chunks).
	const size = 2 * 1024 * 1024
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	body, err := newTestClient(srv.URL).ImageBlob(context.Background(), "/api/v1/agent/images/abc/blob")
	if err != nil {
		t.Fatalf("ImageBlob: %v", err)
	}
	defer body.Close()
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		t.Fatalf("copy body: %v", err)
	}
	if n != int64(size) {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
}

func TestImageBlob_RelativeURL(t *testing.T) {
	const want = "/api/v1/agent/images/abc/blob"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	body, err := newTestClient(srv.URL).ImageBlob(context.Background(), want)
	if err != nil {
		t.Fatalf("ImageBlob: %v", err)
	}
	body.Close()
}

func TestImageBlob_AbsoluteURL(t *testing.T) {
	// Two servers: the client points at "primary", but we pass an absolute URL
	// to "other" and confirm the request lands on other.
	primaryHit := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHit = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()

	otherHit := false
	otherPath := ""
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHit = true
		otherPath = r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("hello"))
	}))
	defer other.Close()

	body, err := newTestClient(primary.URL).ImageBlob(context.Background(), other.URL+"/some/blob")
	if err != nil {
		t.Fatalf("ImageBlob: %v", err)
	}
	defer body.Close()

	if primaryHit {
		t.Error("primary server received the absolute-URL request")
	}
	if !otherHit {
		t.Error("other server did not receive the request")
	}
	if otherPath != "/some/blob" {
		t.Errorf("other path = %q, want /some/blob", otherPath)
	}
}

func TestImageBlob_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"image gone"}`))
	}))
	defer srv.Close()

	body, err := newTestClient(srv.URL).ImageBlob(context.Background(), "/x")
	if err == nil {
		body.Close()
		t.Fatal("want error")
	}
	if body != nil {
		t.Error("body should be nil on error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err is %T, want *APIError", err)
	}
	if ae.Code != http.StatusNotFound {
		t.Errorf("code = %d", ae.Code)
	}
	if ae.Message != "image gone" {
		t.Errorf("message = %q", ae.Message)
	}
}

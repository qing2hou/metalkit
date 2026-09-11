package authapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginThrottleBlocksAfterLimit(t *testing.T) {
	th := newLoginThrottle(3, time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "10.9.9.9:1234"

	for i := 0; i < 3; i++ {
		if th.blocked(req) {
			t.Fatalf("blocked at attempt %d before limit", i)
		}
		th.noteFailure(req)
	}
	if !th.blocked(req) {
		t.Fatal("should be blocked after 3 failures")
	}

	// Different IP unaffected.
	other := httptest.NewRequest(http.MethodPost, "/login", nil)
	other.RemoteAddr = "10.9.9.8:1234"
	if th.blocked(other) {
		t.Fatal("per-IP: other IP must not be blocked")
	}

	// Success clears the window.
	th.noteSuccess(req)
	if th.blocked(req) {
		t.Fatal("success must clear the window")
	}
}

func TestLoginThrottleWindowExpiry(t *testing.T) {
	th := newLoginThrottle(1, time.Millisecond)
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	th.noteFailure(req)
	if !th.blocked(req) {
		t.Fatal("should be blocked")
	}
	time.Sleep(5 * time.Millisecond)
	if th.blocked(req) {
		t.Fatal("window must free after expiry")
	}
}

package authapi

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginThrottle is a fixed-window-per-IP login attempt limiter. It exists to
// blunt credential brute-force (no lockout accounts exist on this system).
// Design constraints:
//   - per-IP, not per-account: NXDOMAIN floods share the proxy IP anyway and
//     per-account counters would let an attacker lock out legitimate users;
//   - fixed window, not sliding/leaky bucket: precision doesn't matter at
//     these scales, and the state is a single map entry per IP;
//   - applies to FAILED attempts only — a healthy operator typing one wrong
//     password never notices it.
type ipWindow struct {
	fails    int
	windowAt time.Time
}

type loginThrottle struct {
	mu     sync.Mutex
	byIP   map[string]*ipWindow
	limit  int           // failed attempts per window before 429
	window time.Duration // window size; entry freed on next check past it
}

func newLoginThrottle(limit int, window time.Duration) *loginThrottle {
	return &loginThrottle{byIP: map[string]*ipWindow{}, limit: limit, window: window}
}

// ipOf extracts the client IP (RemoteAddr sans port).
func ipOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports whether this IP is currently over the failed-login limit.
// Call before verifying credentials.
func (t *loginThrottle) blocked(r *http.Request) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.byIP[ipOf(r)]
	if !ok {
		return false
	}
	if time.Since(w.windowAt) >= t.window {
		delete(t.byIP, ipOf(r)) // window expired; free the entry
		return false
	}
	return w.fails >= t.limit
}

// noteFailure records a failed attempt, opening a window if none exists.
func (t *loginThrottle) noteFailure(r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ip := ipOf(r)
	w, ok := t.byIP[ip]
	if !ok || time.Since(w.windowAt) >= t.window {
		t.byIP[ip] = &ipWindow{fails: 1, windowAt: time.Now()}
		return
	}
	w.fails++
	// Opportunistic GC: entries for IPs that went quiet are already freed by
	// blocked(); nothing more to do here.
}

// noteSuccess clears the IP's window — a login proves a human got through.
func (t *loginThrottle) noteSuccess(r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byIP, ipOf(r))
}

package httpd

import (
	"net/http"
	"strings"
)

// machineChannelPaths are the endpoints PXE/iPXE clients and live-boot
// agents rely on. They have no trust store and cannot do TLS, so when the
// controller runs dual (HTTPS + HTTP) listeners these paths stay reachable
// over plain HTTP; everything human-facing redirects to the TLS listener.
func isMachineChannel(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/boot/"):
		return true
	case path == "/api/v1/report", strings.HasPrefix(path, "/api/v1/report/"):
		return true
	case strings.HasPrefix(path, "/api/v1/heartbeat/"):
		return true
	case strings.HasPrefix(path, "/api/v1/agent/"):
		return true
	}
	return false
}

// machineChannelOrRedirect wraps the full handler chain for the plain-HTTP
// listener in HTTPS mode: machine-channel paths are served verbatim, every
// other path gets a permanent (308) redirect to the same URL on the HTTPS
// listener. 308 (not 301/302) preserves the method and body, so API clients
// using http:// by accident transparently retry over TLS.
func machineChannelOrRedirect(httpsAddr string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMachineChannel(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// Build the absolute HTTPS URL: scheme + request host's name +
		// httpsAddr's port. When httpsAddr binds all interfaces (":8443")
		// take the port only; the hostname the client used (may be an IP
		// or DNS name that resolves differently) stays authoritative.
		host := r.Host
		if idx := strings.LastIndex(host, ":"); idx >= 0 && !strings.Contains(host[idx:], "]") {
			host = host[:idx]
		}
		port := httpsAddr
		if idx := strings.LastIndex(port, ":"); idx >= 0 {
			port = port[idx:]
		} else {
			port = ":8443"
		}
		http.Redirect(w, r, "https://"+host+port+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

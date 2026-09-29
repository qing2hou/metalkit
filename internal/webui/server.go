// Package webui serves the operator dashboard for metalkit. The UI is a
// Vue 3 single-page application (source in ../../frontend) built by Vite
// into this package's assets/ directory and embedded into the controller
// binary. All dynamic data is fetched by the browser from the REST API
// under /api/v1; this package never imports the API packages and never
// speaks to the database.
package webui

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets
var embeddedAssets embed.FS

// Config configures the Web UI handler.
type Config struct {
	// Mount is the URL prefix the UI is served under. Defaults to "/ui".
	// A non-empty value must start with "/" and must not end with "/".
	Mount string
}

const defaultMount = "/ui"

// Handler returns an http.Handler that serves the SPA under the configured
// mount point:
//
//	GET {mount}/              -> index.html (SPA shell; router handles the rest)
//	GET {mount}/assets/*      -> hashed Vite build output (immutable, no-cache headers kept for safety)
//	GET {mount}/{anything}    -> real file if it exists in the dist root (favicon etc.),
//	                             otherwise the SPA shell (history-mode fallback)
//	GET {mount}               -> 301 redirect to {mount}/
//
// Authentication is enforced by the httpd middleware: everything under the
// mount requires a session except /ui/login (an in-app route) and
// /ui/assets/* (see internal/httpd/auth.go needsAuth).
func Handler(cfg Config) http.Handler {
	mount := normaliseMount(cfg.Mount)

	distFS, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		// The embed directive is a compile-time guarantee; if Sub fails the
		// binary is broken. Panic so it's obvious during startup rather than
		// returning a half-wired handler.
		panic("webui: failed to scope embedded assets: " + err.Error())
	}

	// Vite emits hashed build output under assets/assets/*; expose it under
	// {mount}/assets/ so the httpd auth whitelist continues to match.
	hashedFS, err := fs.Sub(distFS, "assets")
	if err != nil {
		panic("webui: failed to scope hashed assets: " + err.Error())
	}
	assetFileServer := http.StripPrefix(mount+"/assets/", http.FileServer(http.FS(hashedFS)))

	mux := http.NewServeMux()

	// "{mount}" without a trailing slash redirects to "{mount}/" so relative
	// asset URLs resolve correctly in the browser.
	mux.HandleFunc("GET "+mount, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mount+"/", http.StatusMovedPermanently)
	})

	// Hashed Vite assets.
	mux.Handle("GET "+mount+"/assets/", noCache(assetFileServer))

	// History-mode SPA fallback for everything else under the mount. If the
	// requested path matches a real file in the dist root (favicon.ico,
	// index.html itself, ...) serve that file; otherwise serve index.html so
	// the Vue router can pick up deep links like /ui/m/{uuid}.
	mux.HandleFunc("GET "+mount+"/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, mount+"/")
		if rel != "" {
			if f, err := distFS.Open(rel); err == nil {
				f.Close()
				http.StripPrefix(mount+"/", http.FileServer(http.FS(distFS))).ServeHTTP(w, r)
				return
			}
		}
		serveFile(w, distFS, "index.html", "text/html; charset=utf-8")
	})

	return mux
}

// serveFile writes a named file from the embedded FS with the given content
// type. Files referenced here are bundled into the binary at build time, so
// a missing file means the binary is broken.
func serveFile(w http.ResponseWriter, fsys fs.FS, name, contentType string) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.Error(w, "webui: missing embedded asset: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// normaliseMount validates and normalises the mount prefix. Empty becomes
// the default. A trailing slash is stripped. A missing leading slash is
// rejected by panicking — misconfiguration here would silently produce an
// unreachable UI.
func normaliseMount(m string) string {
	if m == "" {
		return defaultMount
	}
	if !strings.HasPrefix(m, "/") {
		panic(errors.New("webui: Mount must start with '/'"))
	}
	if len(m) > 1 && strings.HasSuffix(m, "/") {
		m = strings.TrimRight(m, "/")
	}
	return m
}

// noCache wraps an http.Handler to set Cache-Control: no-cache on every
// response, forcing browsers to revalidate before serving from cache.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

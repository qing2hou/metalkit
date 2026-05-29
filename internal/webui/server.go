// Package webui serves a small, dependency-free operator dashboard for
// metalkit. It renders two static HTML pages (machine list and machine
// detail) and exposes a small set of static assets (JS/CSS) from an
// embedded filesystem. All dynamic data is fetched by the browser from
// the REST API exposed by the inventory package; this package never
// imports inventory and never speaks to the database.
package webui

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var embeddedAssets embed.FS

// Config configures the Web UI handler.
type Config struct {
	// Mount is the URL prefix the UI is served under. Defaults to "/ui".
	// A non-empty value must start with "/" and must not end with "/".
	Mount string
}

const defaultMount = "/ui"

// Handler returns an http.Handler that serves the operator UI under the
// configured mount point. The returned handler exposes:
//
//	GET {mount}/             -> machine list (index.html)
//	GET {mount}/login        -> login page (login.html)
//	GET {mount}/m/{uuid}     -> machine detail (detail.html)
//	GET {mount}/images       -> image catalog + upload (images.html)
//	GET {mount}/profiles     -> install profiles (profiles.html)
//	GET {mount}/subnets      -> subnet catalog (subnets.html)
//	GET {mount}/bmc          -> BMC credentials (bmc.html)
//	GET {mount}/jobs         -> jobs list (jobs.html)
//	GET {mount}/jobs/{id}    -> job detail (job.html)
//	GET {mount}/settings     -> settings (DHCP) page (settings.html)
//	GET {mount}/assets/*     -> embedded static assets
//
// All other paths under the mount return 404.
func Handler(cfg Config) http.Handler {
	mount := normaliseMount(cfg.Mount)

	assetsFS, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		// The embed directive is a compile-time guarantee; if Sub fails the
		// binary is broken. Panic so it's obvious during startup rather than
		// returning a half-wired handler.
		panic("webui: failed to scope embedded assets: " + err.Error())
	}
	assetFileServer := http.StripPrefix(mount+"/assets/", http.FileServer(http.FS(assetsFS)))

	mux := http.NewServeMux()

	// Machine list at "{mount}/" — the {$} pattern matches the exact path,
	// avoiding the default subtree match that would swallow "{mount}/foo".
	mux.HandleFunc("GET "+mount+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "index.html")
	})

	// "{mount}" without a trailing slash redirects to "{mount}/" so relative
	// asset URLs resolve correctly in the browser.
	mux.HandleFunc("GET "+mount, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mount+"/", http.StatusMovedPermanently)
	})

	// Login page (open — auth middleware allows /ui/login through).
	mux.HandleFunc("GET "+mount+"/login", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "login.html")
	})

	// Machine detail. The UUID is parsed client-side from the URL; we just
	// need to render the same HTML shell for any UUID.
	mux.HandleFunc("GET "+mount+"/m/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		// r.PathValue("uuid") is intentionally unused server-side: the page
		// is a static shell and app.js reads the UUID from window.location.
		_ = r.PathValue("uuid")
		serveEmbeddedHTML(w, assetsFS, "detail.html")
	})

	// Image catalog + upload. Static shell; images.js drives upload + list.
	mux.HandleFunc("GET "+mount+"/images", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "images.html")
	})

	// Install profiles. Static shell; profiles.js drives CRUD.
	mux.HandleFunc("GET "+mount+"/profiles", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "profiles.html")
	})

	// Subnet catalog. Static shell; subnets.js drives CRUD.
	mux.HandleFunc("GET "+mount+"/subnets", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "subnets.html")
	})

	// BMC credentials. Static shell; bmc.js drives CRUD + test endpoint.
	mux.HandleFunc("GET "+mount+"/bmc", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "bmc.html")
	})

	// Jobs list. Static shell; jobs.js drives filters/list/cancel.
	mux.HandleFunc("GET "+mount+"/jobs", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "jobs.html")
	})

	// Job detail. The ID is parsed client-side from the URL; we just
	// need to render the same HTML shell for any job ID.
	mux.HandleFunc("GET "+mount+"/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		// r.PathValue("id") is intentionally unused server-side: the page
		// is a static shell and job.js reads the ID from window.location.
		_ = r.PathValue("id")
		serveEmbeddedHTML(w, assetsFS, "job.html")
	})

	// Settings (currently just DHCP config). Static shell; settings.js
	// drives the form. Persists via /api/v1/settings/dhcp.
	mux.HandleFunc("GET "+mount+"/settings", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, assetsFS, "settings.html")
	})

	// Static assets with no-cache so browsers always revalidate.
	mux.Handle("GET "+mount+"/assets/", noCache(assetFileServer))

	return mux
}

// serveEmbeddedHTML writes the named file from the embedded asset FS as
// UTF-8 HTML. Files referenced here are bundled into the binary at build
// time, so a missing file means the binary is broken.
func serveEmbeddedHTML(w http.ResponseWriter, assetsFS fs.FS, name string) {
	body, err := fs.ReadFile(assetsFS, name)
	if err != nil {
		http.Error(w, "webui: missing embedded asset: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// normaliseMount validates and normalises the mount prefix. Empty becomes
// the default. A trailing slash is stripped. A missing leading slash is
// rejected by panicking — misconfiguration here would silently produce
// an unreachable UI.
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

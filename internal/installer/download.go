// download.go owns URL plumbing for the install. The actual byte streaming
// + sha256 verification is in realdeps.go (HTTPDownloader); this file
// just resolves the relative ImageBlobURL onto the agent's BaseURL.
//
// We keep this separate because:
//
//   - It's pure string handling, trivially unit-tested.
//   - The orchestrator (installer.go) calls ResolveBlobURL before invoking
//     Downloader.Stream, so a typo in BaseURL fails before any IO.
package installer

import (
	"fmt"
	"net/url"
	"strings"
)

// ResolveBlobURL joins a relative path (typically /api/v1/images/<id>/blob)
// onto the controller base URL. If rel is already an absolute URL with a
// scheme it's returned untouched — useful for testing or for images served
// off a separate mirror.
//
// Trailing/leading slashes on base+rel are normalised; an empty base with
// a non-absolute rel is a configuration error and returns an error rather
// than a silently relative URL.
func ResolveBlobURL(base, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("install: image_blob_url is empty")
	}
	// If rel is already absolute (http://, https://) return as-is.
	if u, err := url.Parse(rel); err == nil && u.IsAbs() {
		return u.String(), nil
	}

	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("install: BaseURL is empty but image_blob_url %q is relative", rel)
	}
	bu, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("install: parse base url %q: %w", base, err)
	}
	if !bu.IsAbs() {
		return "", fmt.Errorf("install: base url %q has no scheme", base)
	}

	// url.URL.ResolveReference handles the trailing-slash dance for us as
	// long as we feed it a parsed reference. We need to make sure the rel
	// starts with "/" so it resolves against the base host root rather
	// than the base's path.
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	ru, err := url.Parse(rel)
	if err != nil {
		return "", fmt.Errorf("install: parse rel url %q: %w", rel, err)
	}
	return bu.ResolveReference(ru).String(), nil
}

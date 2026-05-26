package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultTimeout bounds control-plane requests. The blob download path uses a
// separate http.Client with no timeout — see blob.go.
const defaultTimeout = 60 * time.Second

// Client speaks the controller's /api/v1/agent/* protocol on behalf of one
// machine. All fields are exported so tests / wiring code can override them;
// New() fills in working defaults.
type Client struct {
	// BaseURL is the controller's root (e.g. "http://10.0.0.1:8080"). May or
	// may not end with a slash; url() handles both.
	BaseURL string
	// HTTP is used for every call EXCEPT ImageBlob. Defaults to a 60s-timeout
	// client.
	HTTP *http.Client
	// BlobHTTP is used by ImageBlob only. Defaults to a no-timeout client
	// because image downloads can run for many minutes.
	BlobHTTP *http.Client
	// UUID is the machine's SMBIOS UUID; sent as machine_uuid on every call.
	UUID string
	// Logger receives debug lines on each request. Nil falls back to
	// slog.Default().
	Logger *slog.Logger
}

// New constructs a Client with sensible defaults. baseURL must include scheme.
func New(baseURL, machineUUID string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		BaseURL:  baseURL,
		HTTP:     &http.Client{Timeout: defaultTimeout},
		BlobHTTP: &http.Client{Timeout: 0},
		UUID:     machineUUID,
		Logger:   logger,
	}
}

// url joins p onto c.BaseURL. p may be:
//   - an absolute URL ("http://..."/"https://...") — returned as-is
//   - rooted ("/api/v1/...") — joined to BaseURL
//   - relative ("api/v1/...") — joined with a single slash
//
// BaseURL is tolerated with or without a trailing slash.
func (c *Client) url(p string) string {
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

// log emits a debug line for one request. Never panics on nil logger because
// New() always plants a default.
func (c *Client) log(method, fullURL string, status int) {
	if c.Logger == nil {
		return
	}
	c.Logger.Debug("agentclient request",
		slog.String("method", method),
		slog.String("url", fullURL),
		slog.Int("status", status))
}

// do performs an HTTP round-trip, decodes a 2xx JSON body into `into` (pass
// nil for 204 endpoints) and maps any non-2xx response to *APIError.
//
// `body` is encoded as JSON if non-nil; pass nil for bodyless requests.
func (c *Client) do(ctx context.Context, method, path string, body any, into any) error {
	fullURL := c.url(path)

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("agentclient: marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return fmt.Errorf("agentclient: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("agentclient: %s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	c.log(method, fullURL, resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	if into == nil || resp.StatusCode == http.StatusNoContent {
		// drain to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("agentclient: decode response from %s: %w", fullURL, err)
	}
	return nil
}

// get is a thin convenience over do() for GET requests.
func (c *Client) get(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodGet, path, nil, into)
}

// post is a thin convenience over do() for POST requests. Pass into=nil for
// 204 endpoints.
func (c *Client) post(ctx context.Context, path string, body any, into any) error {
	return c.do(ctx, http.MethodPost, path, body, into)
}

// parseAPIError consumes a non-2xx response and returns an *APIError. We
// always read the whole body (responses are tiny) so the caller sees the
// controller's "error" string.
func parseAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(resp.Body)
	msg := ""
	if len(raw) > 0 {
		var env struct {
			Error string `json:"error"`
		}
		if jerr := json.Unmarshal(raw, &env); jerr == nil && env.Error != "" {
			msg = env.Error
		} else {
			msg = strings.TrimSpace(string(raw))
		}
	}
	return &APIError{Code: resp.StatusCode, Message: msg}
}

// queryWith appends machine_uuid=<uuid> (and any extra pairs) to path.
func (c *Client) queryWith(path string, extra ...[2]string) string {
	v := url.Values{}
	v.Set("machine_uuid", c.UUID)
	for _, p := range extra {
		v.Set(p[0], p[1])
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + v.Encode()
}

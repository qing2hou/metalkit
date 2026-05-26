package agentclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// ImageBlob hits GET <BaseURL><blobURL>. blobURL is the relative path returned
// in InstallSpec.ImageBlobURL (typically "/api/v1/agent/images/{id}/blob") OR
// an absolute URL — both are accepted.
//
// The returned ReadCloser streams the image body; the caller MUST Close it.
// Uses Client.BlobHTTP (no timeout) because real images run multi-GB and the
// 60s control-plane timeout would abort the download.
func (c *Client) ImageBlob(ctx context.Context, blobURL string) (io.ReadCloser, error) {
	fullURL := c.url(blobURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("agentclient: build blob request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")

	httpc := c.BlobHTTP
	if httpc == nil {
		// Defensive: a Client built without New() may have left BlobHTTP nil.
		httpc = &http.Client{Timeout: 0}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentclient: GET %s: %w", fullURL, err)
	}
	c.log(http.MethodGet, fullURL, resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Parse the error envelope then close.
		err := parseAPIError(resp)
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

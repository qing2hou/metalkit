package images

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func newTestAPI(t *testing.T) (*API, *httptest.Server) {
	t.Helper()
	s := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewAPI(s, nil, logger)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return a, ts
}

func TestAPIUploadFlow(t *testing.T) {
	_, ts := newTestAPI(t)
	body := bytes.Repeat([]byte{0x33}, 30*1024) // 30 KiB

	// init: POST /uploads
	init := map[string]any{
		"name":            "ubuntu.qcow2",
		"family":          "ubuntu",
		"expected_sha256": sha256Hex(body),
		"total_size":      len(body),
		"chunk_size":      10 * 1024, // 10 KiB → 3 chunks
	}
	initBytes, _ := json.Marshal(init)
	resp, err := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(initBytes))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("init status=%d body=%s", resp.StatusCode, dump)
	}
	var sess UploadSession
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.NumChunks != 3 {
		t.Fatalf("NumChunks: %d", sess.NumChunks)
	}

	// PUT each chunk
	for i := 0; i < 3; i++ {
		off := i * 10 * 1024
		end := off + 10*1024
		if end > len(body) {
			end = len(body)
		}
		chunk := body[off:end]
		req, _ := http.NewRequest(http.MethodPut,
			ts.URL+"/api/v1/images/uploads/"+sess.ID+"/chunks/"+strconv.Itoa(i+1),
			bytes.NewReader(chunk))
		req.Header.Set("X-Chunk-Sha256", sha256Hex(chunk))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %d: %v", i+1, err)
		}
		if resp.StatusCode != http.StatusOK {
			dump, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT %d: status=%d body=%s", i+1, resp.StatusCode, dump)
		}
		resp.Body.Close()
	}

	// finalize
	resp, err = http.Post(ts.URL+"/api/v1/images/uploads/"+sess.ID+"/finalize", "application/json", nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("finalize status=%d body=%s", resp.StatusCode, dump)
	}
	var img Image
	_ = json.NewDecoder(resp.Body).Decode(&img)
	resp.Body.Close()
	if img.SHA256 != sha256Hex(body) {
		t.Errorf("img SHA256: got %q want %q", img.SHA256, sha256Hex(body))
	}

	// list shows the image
	resp, _ = http.Get(ts.URL + "/api/v1/images")
	var list []Image
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].ID != img.ID {
		t.Fatalf("list: %+v", list)
	}

	// detail
	resp, _ = http.Get(ts.URL + "/api/v1/images/" + img.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// delete
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/images/"+img.ID, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// list is now empty
	resp, _ = http.Get(ts.URL + "/api/v1/images")
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("list after delete: %+v", list)
	}
}

func TestAPIInitBadSHA(t *testing.T) {
	_, ts := newTestAPI(t)
	init := map[string]any{
		"name":            "x.qcow2",
		"expected_sha256": "not-hex",
		"total_size":      100,
	}
	b, _ := json.Marshal(init)
	resp, _ := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}

func TestAPIChunkSHAMismatch(t *testing.T) {
	_, ts := newTestAPI(t)
	body := bytes.Repeat([]byte{0x44}, 50)
	init := map[string]any{
		"name":            "x.qcow2",
		"expected_sha256": sha256Hex(body),
		"total_size":      len(body),
	}
	b, _ := json.Marshal(init)
	resp, _ := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	var sess UploadSession
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()

	// wrong per-chunk sha
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/v1/images/uploads/"+sess.ID+"/chunks/1",
		bytes.NewReader(body))
	req.Header.Set("X-Chunk-Sha256", sha256Hex([]byte("wrong")))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, dump)
	}
}

func TestAPIDuplicateFinalize(t *testing.T) {
	_, ts := newTestAPI(t)

	body := bytes.Repeat([]byte{0x55}, 20)
	// First upload.
	uploadAndFinalize(t, ts, "a.qcow2", body)

	// Second upload of same bytes → init should 409.
	init := map[string]any{
		"name":            "b.qcow2",
		"expected_sha256": sha256Hex(body),
		"total_size":      len(body),
	}
	b, _ := json.Marshal(init)
	resp, _ := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	if resp.StatusCode != http.StatusConflict {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d want 409 body=%s", resp.StatusCode, dump)
	}
}

func TestAPIAbortUpload(t *testing.T) {
	_, ts := newTestAPI(t)
	body := bytes.Repeat([]byte{0x66}, 50)
	init := map[string]any{
		"name":            "x.qcow2",
		"expected_sha256": sha256Hex(body),
		"total_size":      len(body),
	}
	b, _ := json.Marshal(init)
	resp, _ := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	var sess UploadSession
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/images/uploads/"+sess.ID, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: %d", resp.StatusCode)
	}

	// Second abort 404s.
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second abort: %d", resp.StatusCode)
	}
}

func TestAPIBadIDs(t *testing.T) {
	_, ts := newTestAPI(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/images/nope"},
		{http.MethodDelete, "/api/v1/images/nope"},
		{http.MethodGet, "/api/v1/images/uploads/nope"},
		{http.MethodDelete, "/api/v1/images/uploads/nope"},
		{http.MethodPost, "/api/v1/images/uploads/nope/finalize"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s: status %d want 400", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAPIExtractorPopulates(t *testing.T) {
	s := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ext := extractor{format: "qcow2", virtualSize: 12345, metadataJSON: `{"k":"v"}`}
	a := NewAPI(s, ext, logger)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body := bytes.Repeat([]byte{0x77}, 100)
	img := uploadAndFinalize(t, ts, "x.qcow2", body)
	if img.Format != "qcow2" || img.VirtualSize != 12345 || img.MetadataJSON != `{"k":"v"}` {
		t.Fatalf("extractor metadata not applied: %+v", img)
	}
}

// uploadAndFinalize is a test helper: full upload of body using one chunk.
func uploadAndFinalize(t *testing.T, ts *httptest.Server, name string, body []byte) Image {
	t.Helper()
	init := map[string]any{
		"name":            name,
		"expected_sha256": sha256Hex(body),
		"total_size":      len(body),
		"chunk_size":      len(body),
	}
	b, _ := json.Marshal(init)
	resp, err := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("init: %d body=%s", resp.StatusCode, dump)
	}
	var sess UploadSession
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/images/uploads/"+sess.ID+"/chunks/1", bytes.NewReader(body))
	req.Header.Set("X-Chunk-Sha256", sha256Hex(body))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: %d body=%s", resp.StatusCode, dump)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/api/v1/images/uploads/"+sess.ID+"/finalize", "application/json", nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("finalize: %d body=%s", resp.StatusCode, dump)
	}
	var img Image
	_ = json.NewDecoder(resp.Body).Decode(&img)
	resp.Body.Close()
	return img
}

// Ensure init rejects body that's too large (request entity).
func TestAPIInitOversizedBody(t *testing.T) {
	_, ts := newTestAPI(t)
	bigName := bytes.Repeat([]byte{'x'}, 100*1024) // > 64 KiB limit
	init := map[string]any{
		"name":            string(bigName),
		"expected_sha256": sha256Hex([]byte("x")),
		"total_size":      10,
	}
	b, _ := json.Marshal(init)
	resp, _ := http.Post(ts.URL+"/api/v1/images/uploads", "application/json", bytes.NewReader(b))
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize init: status %d", resp.StatusCode)
	}
}

// Spot-check Content-Type header on JSON responses.
func TestAPIContentType(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := http.Get(ts.URL + "/api/v1/images")
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type: %q", ct)
	}
	resp.Body.Close()
}

// TestGetBlobHappy: upload + finalize a small image, then fetch it through
// the unauthenticated /api/v1/agent/images/{id}/blob path. The bytes returned
// must match what we uploaded.
func TestGetBlobHappy(t *testing.T) {
	_, ts := newTestAPI(t)
	body := bytes.Repeat([]byte{0xab}, 1234)
	img := uploadAndFinalize(t, ts, "blob.qcow2", body)

	resp, err := http.Get(ts.URL + "/api/v1/agent/images/" + img.ID + "/blob")
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("blob status=%d body=%s", resp.StatusCode, dump)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("blob bytes mismatch: got %d bytes want %d", len(got), len(body))
	}
}

func TestGetBlobNotFound(t *testing.T) {
	_, ts := newTestAPI(t)
	// 32-hex but never uploaded.
	missing := "0123456789abcdef0123456789abcdef"
	resp, err := http.Get(ts.URL + "/api/v1/agent/images/" + missing + "/blob")
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("blob status=%d want 404", resp.StatusCode)
	}
}

func TestGetBlobBadID(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, err := http.Get(ts.URL + "/api/v1/agent/images/not-hex/blob")
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blob status=%d want 400", resp.StatusCode)
	}
}

// background context is unused in this file but keeps go vet from warning if a
// test imports it conditionally.
var _ = context.Background

package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metalkit/internal/bindings"
	"metalkit/internal/jobs"
	"metalkit/internal/profiles"
)

// newTestClient wires a Client at the given baseURL. UUID is fixed across
// tests so request bodies / query strings are easy to assert.
func newTestClient(baseURL string) *Client {
	return New(baseURL, "11111111-1111-1111-1111-111111111111", nil)
}

func sampleJob() jobs.Job {
	now := time.Unix(1700000000, 0).UTC()
	return jobs.Job{
		ID:          "deadbeefdeadbeefdeadbeefdeadbeef",
		MachineUUID: "11111111-1111-1111-1111-111111111111",
		Type:        "install",
		ImageID:     "img-1",
		ProfileID:   "prof-1",
		Status:      "running",
		Stage:       "download",
		CreatedAt:   now,
		CreatedBy:   "tester",
	}
}

// readBody is a tiny test helper that drains and JSON-decodes a request body
// into m. Fails the test on any error.
func readBody(t *testing.T, r *http.Request, into any) {
	t.Helper()
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("empty body")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode body %q: %v", string(raw), err)
	}
}

func TestCurrentJob_Happy(t *testing.T) {
	j := sampleJob()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/jobs/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(j)
	}))
	defer srv.Close()

	got, err := newTestClient(srv.URL).CurrentJob(context.Background())
	if err != nil {
		t.Fatalf("CurrentJob: %v", err)
	}
	if got.ID != j.ID {
		t.Fatalf("ID = %q, want %q", got.ID, j.ID)
	}
	if got.MachineUUID != j.MachineUUID {
		t.Fatalf("MachineUUID = %q, want %q", got.MachineUUID, j.MachineUUID)
	}
	if got.Stage != j.Stage {
		t.Fatalf("Stage = %q, want %q", got.Stage, j.Stage)
	}
}

func TestCurrentJob_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no in-flight job"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).CurrentJob(context.Background())
	if !errors.Is(err, ErrNoCurrentJob) {
		t.Fatalf("err = %v, want ErrNoCurrentJob", err)
	}
}

func TestCurrentJob_VerifiesMachineUUIDQuery(t *testing.T) {
	const want = "11111111-1111-1111-1111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("machine_uuid")
		if got != want {
			t.Errorf("machine_uuid = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _ = newTestClient(srv.URL).CurrentJob(context.Background())
}

func TestClaim_Happy(t *testing.T) {
	j := sampleJob()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/claim") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		var body map[string]string
		readBody(t, r, &body)
		if body["machine_uuid"] != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("body.machine_uuid = %q", body["machine_uuid"])
		}
		if len(body) != 1 {
			t.Errorf("body keys = %v, want exactly machine_uuid", body)
		}
		_ = json.NewEncoder(w).Encode(j)
	}))
	defer srv.Close()

	got, err := newTestClient(srv.URL).Claim(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != j.ID {
		t.Fatalf("ID = %q, want %q", got.ID, j.ID)
	}
}

func TestStage_204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		readBody(t, r, &body)
		if body["stage"] != "download" {
			t.Errorf("stage = %q", body["stage"])
		}
		if body["machine_uuid"] == "" {
			t.Errorf("missing machine_uuid")
		}
		if !strings.HasSuffix(r.URL.Path, "/stage") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Stage(context.Background(), "jobid", "download"); err != nil {
		t.Fatalf("Stage: %v", err)
	}
}

func TestStage_BadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad stage"}`))
	}))
	defer srv.Close()

	err := newTestClient(srv.URL).Stage(context.Background(), "jobid", "nope")
	if err == nil {
		t.Fatal("want error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err is %T, want *APIError", err)
	}
	if ae.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", ae.Code)
	}
	if ae.Message != "bad stage" {
		t.Errorf("message = %q, want 'bad stage'", ae.Message)
	}
}

func TestLog_BodyShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		readBody(t, r, &body)
		for _, k := range []string{"machine_uuid", "level", "message"} {
			if _, ok := body[k]; !ok {
				t.Errorf("missing key %q in body %v", k, body)
			}
		}
		if body["level"] != "info" {
			t.Errorf("level = %q", body["level"])
		}
		if body["message"] != "hello" {
			t.Errorf("message = %q", body["message"])
		}
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Log(context.Background(), "jobid", "info", "hello"); err != nil {
		t.Fatalf("Log: %v", err)
	}
}

func TestSucceed_204(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		readBody(t, r, &body)
		if body["machine_uuid"] == "" {
			t.Errorf("missing machine_uuid")
		}
		if !strings.HasSuffix(r.URL.Path, "/succeed") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Succeed(context.Background(), "jobid"); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
}

func TestFail_204_AndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		readBody(t, r, &body)
		if body["error"] != "kaboom" {
			t.Errorf("error = %q, want 'kaboom'", body["error"])
		}
		if body["machine_uuid"] == "" {
			t.Errorf("missing machine_uuid")
		}
		if !strings.HasSuffix(r.URL.Path, "/fail") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Fail(context.Background(), "jobid", "kaboom"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
}

// sampleSpec builds a minimal InstallSpec. The Profile/Binding structs have
// many optional fields; zero values are valid for the JSON round-trip test
// because we only assert top-level scalars survive the wire.
func sampleSpec() jobs.InstallSpec {
	return jobs.InstallSpec{
		JobID:        "deadbeefdeadbeefdeadbeefdeadbeef",
		MachineUUID:  "11111111-1111-1111-1111-111111111111",
		ImageID:      "img-1",
		ImageBlobURL: "/api/v1/agent/images/img-1/blob",
		ImageSHA256:  "abc123",
		ImageFormat:  "qcow2",
		Profile:      profiles.Profile{},
		Binding:      bindings.Binding{},
	}
}

func TestSpec_Happy(t *testing.T) {
	spec := sampleSpec()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/spec") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("machine_uuid"); got != spec.MachineUUID {
			t.Errorf("machine_uuid query = %q", got)
		}
		_ = json.NewEncoder(w).Encode(spec)
	}))
	defer srv.Close()

	got, err := newTestClient(srv.URL).Spec(context.Background(), spec.JobID)
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	if got.JobID != spec.JobID {
		t.Errorf("JobID = %q, want %q", got.JobID, spec.JobID)
	}
	if got.ImageBlobURL != spec.ImageBlobURL {
		t.Errorf("ImageBlobURL = %q, want %q", got.ImageBlobURL, spec.ImageBlobURL)
	}
	if got.ImageSHA256 != spec.ImageSHA256 {
		t.Errorf("ImageSHA256 = %q, want %q", got.ImageSHA256, spec.ImageSHA256)
	}
	if got.ImageFormat != spec.ImageFormat {
		t.Errorf("ImageFormat = %q, want %q", got.ImageFormat, spec.ImageFormat)
	}
}

func TestSpec_403_MachineMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"machine_uuid mismatch"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).Spec(context.Background(), "jobid")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err is %T, want *APIError", err)
	}
	if ae.Code != http.StatusForbidden {
		t.Errorf("code = %d", ae.Code)
	}
	if !strings.Contains(ae.Message, "mismatch") {
		t.Errorf("message = %q", ae.Message)
	}
}

func TestSpec_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown job"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).Spec(context.Background(), "jobid")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err is %T, want *APIError", err)
	}
	if ae.Code != http.StatusNotFound {
		t.Errorf("code = %d", ae.Code)
	}
}

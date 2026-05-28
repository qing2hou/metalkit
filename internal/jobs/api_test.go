package jobs

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestAPI(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	a := NewAPI(f.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts
}

func apiDo(t *testing.T, ts *httptest.Server, method, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAPIListAndGet(t *testing.T) {
	f, ts := newTestAPI(t)
	in := f.baseInput(t, '@')
	j, err := f.store.Create(t.Context(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// list
	code, body := apiDo(t, ts, "GET", "/api/v1/jobs")
	if code != http.StatusOK {
		t.Fatalf("list code=%d body=%s", code, body)
	}
	var list []Job
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list) != 1 || list[0].ID != j.ID {
		t.Errorf("list=%+v", list)
	}

	// get one
	code, body = apiDo(t, ts, "GET", "/api/v1/jobs/"+j.ID)
	if code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", code, body)
	}
	var got Job
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("get decode: %v", err)
	}
	if got.MachineUUID != in.MachineUUID {
		t.Errorf("got=%+v", got)
	}

	// 404
	code, _ = apiDo(t, ts, "GET", "/api/v1/jobs/00000000000000000000000000000000")
	if code != http.StatusNotFound {
		t.Errorf("not-found code=%d", code)
	}

	// invalid id → 400
	code, _ = apiDo(t, ts, "GET", "/api/v1/jobs/notahex")
	if code != http.StatusBadRequest {
		t.Errorf("invalid id code=%d", code)
	}
}

func TestAPIListFilters(t *testing.T) {
	f, ts := newTestAPI(t)
	in1 := f.baseInput(t, '#')
	in2 := f.baseInput(t, '$')
	j1, _ := f.store.Create(t.Context(), in1)
	_, _ = f.store.Create(t.Context(), in2)
	_, _ = f.store.Claim(t.Context(), j1.ID)
	_ = f.store.Fail(t.Context(), j1.ID, "boom")

	code, body := apiDo(t, ts, "GET", "/api/v1/jobs?status=pending")
	if code != http.StatusOK {
		t.Fatalf("filter code=%d body=%s", code, body)
	}
	var list []Job
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].Status != "pending" {
		t.Errorf("pending list=%+v", list)
	}

	code, body = apiDo(t, ts, "GET", "/api/v1/jobs?status=bogus")
	if code != http.StatusBadRequest {
		t.Errorf("bogus status code=%d body=%s", code, body)
	}
}

func TestAPILogs(t *testing.T) {
	f, ts := newTestAPI(t)
	in := f.baseInput(t, '%')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)
	_ = f.store.AppendLog(t.Context(), j.ID, "info", "one")
	_ = f.store.AppendLog(t.Context(), j.ID, "info", "two")

	code, body := apiDo(t, ts, "GET", "/api/v1/jobs/"+j.ID+"/logs")
	if code != http.StatusOK {
		t.Fatalf("logs code=%d body=%s", code, body)
	}
	var logs []JobLog
	_ = json.Unmarshal(body, &logs)
	if len(logs) != 2 || logs[0].Message != "one" || logs[1].Message != "two" {
		t.Errorf("logs=%+v", logs)
	}

	code, body = apiDo(t, ts, "GET", "/api/v1/jobs/"+j.ID+"/logs?since_id="+itoa(logs[0].ID))
	_ = body
	if code != http.StatusOK {
		t.Errorf("since code=%d", code)
	}
	var after []JobLog
	_ = json.Unmarshal(body, &after)
}

func TestAPICancel(t *testing.T) {
	f, ts := newTestAPI(t)
	in := f.baseInput(t, '^')
	j, _ := f.store.Create(t.Context(), in)

	code, _ := apiDo(t, ts, "POST", "/api/v1/jobs/"+j.ID+"/cancel")
	if code != http.StatusNoContent {
		t.Fatalf("cancel code=%d", code)
	}
	got, _ := f.store.Get(t.Context(), j.ID)
	if got.Status != "cancelled" {
		t.Errorf("status=%q", got.Status)
	}

	// cancel terminal → 409
	code, _ = apiDo(t, ts, "POST", "/api/v1/jobs/"+j.ID+"/cancel")
	if code != http.StatusConflict {
		t.Errorf("cancel-terminal code=%d", code)
	}

	// unknown id → 404
	code, _ = apiDo(t, ts, "POST", "/api/v1/jobs/00000000000000000000000000000000/cancel")
	if code != http.StatusNotFound {
		t.Errorf("cancel-unknown code=%d", code)
	}
}

func TestAPIDelete(t *testing.T) {
	f, ts := newTestAPI(t)

	// Cancelled job: deletable.
	in := f.baseInput(t, '!')
	j, _ := f.store.Create(t.Context(), in)
	_ = f.store.Cancel(t.Context(), j.ID)
	code, _ := apiDo(t, ts, "DELETE", "/api/v1/jobs/"+j.ID)
	if code != http.StatusNoContent {
		t.Fatalf("delete terminal code=%d", code)
	}
	if _, err := f.store.Get(t.Context(), j.ID); err == nil {
		t.Errorf("job still present after delete")
	}

	// Pending job: 409.
	in2 := f.baseInput(t, '$')
	j2, _ := f.store.Create(t.Context(), in2)
	code, _ = apiDo(t, ts, "DELETE", "/api/v1/jobs/"+j2.ID)
	if code != http.StatusConflict {
		t.Errorf("delete pending code=%d, want 409", code)
	}

	// Unknown id: 404.
	code, _ = apiDo(t, ts, "DELETE", "/api/v1/jobs/00000000000000000000000000000000")
	if code != http.StatusNotFound {
		t.Errorf("delete unknown code=%d, want 404", code)
	}
}

func TestAPIPurge(t *testing.T) {
	f, ts := newTestAPI(t)

	// One cancelled, one failed, one pending (should survive).
	in1 := f.baseInput(t, 'p')
	j1, _ := f.store.Create(t.Context(), in1)
	_ = f.store.Cancel(t.Context(), j1.ID)

	in2 := f.baseInput(t, 'q')
	j2, _ := f.store.Create(t.Context(), in2)
	_, _ = f.store.Claim(t.Context(), j2.ID)
	_ = f.store.Fail(t.Context(), j2.ID, "boom")

	in3 := f.baseInput(t, 'r')
	j3, _ := f.store.Create(t.Context(), in3)

	code, body := apiDo(t, ts, "POST", "/api/v1/jobs/purge")
	if code != http.StatusOK {
		t.Fatalf("purge code=%d body=%s", code, body)
	}
	var resp map[string]int
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["deleted"] != 2 {
		t.Errorf("deleted=%d want 2", resp["deleted"])
	}
	if _, err := f.store.Get(t.Context(), j1.ID); err == nil {
		t.Errorf("cancelled still present")
	}
	if _, err := f.store.Get(t.Context(), j2.ID); err == nil {
		t.Errorf("failed still present")
	}
	if _, err := f.store.Get(t.Context(), j3.ID); err != nil {
		t.Errorf("pending lost: %v", err)
	}

	// Idempotent: second purge returns 0.
	code, body = apiDo(t, ts, "POST", "/api/v1/jobs/purge")
	if code != http.StatusOK {
		t.Fatalf("purge#2 code=%d body=%s", code, body)
	}
	_ = json.Unmarshal(body, &resp)
	if resp["deleted"] != 0 {
		t.Errorf("second purge deleted=%d want 0", resp["deleted"])
	}
}

func itoa(n int64) string {
	b := bytes.Buffer{}
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	return b.String()
}

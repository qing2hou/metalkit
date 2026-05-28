package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"metalkit/internal/bindings"
	"metalkit/internal/images"
	"metalkit/internal/profiles"
	"metalkit/internal/subnets"
)

// fakeBindingFetcher / fakeProfileFetcher / fakeImageFetcher are tiny test
// doubles: the real bindings/profiles/images stores need their own SQLite
// schemas, which the jobs fixture doesn't set up (it stubs the FK targets
// only). Using fakes keeps the spec test focused on handler logic.
type fakeBindingFetcher struct {
	byMUUID map[string]*bindings.Binding
	passwd  map[string]string // optional per-muuid plaintext password
}

func (f *fakeBindingFetcher) Get(_ context.Context, muuid string) (*bindings.Binding, error) {
	if b, ok := f.byMUUID[muuid]; ok {
		return b, nil
	}
	return nil, ErrNotFound
}

func (f *fakeBindingFetcher) GetPassword(_ context.Context, muuid string) (string, error) {
	if p, ok := f.passwd[muuid]; ok {
		return p, nil
	}
	return "", bindings.ErrNotFound
}

type fakeProfileFetcher struct {
	byID map[string]*profiles.Profile
}

func (f *fakeProfileFetcher) Get(_ context.Context, id string) (*profiles.Profile, error) {
	if p, ok := f.byID[id]; ok {
		return p, nil
	}
	return nil, ErrNotFound
}

type fakeImageFetcher struct {
	byID map[string]*images.Image
}

func (f *fakeImageFetcher) GetImage(_ context.Context, id string) (*images.Image, error) {
	if i, ok := f.byID[id]; ok {
		return i, nil
	}
	return nil, ErrNotFound
}

// fakeSubnetFetcher is the spec-handler test double for subnets.Store.
type fakeSubnetFetcher struct {
	byID map[string]*subnets.Subnet
}

func (f *fakeSubnetFetcher) Get(_ context.Context, id string) (*subnets.Subnet, error) {
	if s, ok := f.byID[id]; ok {
		return s, nil
	}
	return nil, subnets.ErrNotFound
}

func newTestAgentAPI(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	f := newFixture(t)
	a := NewAgentAPI(f.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts
}

// newTestAgentAPIWithFetchers wires the agent API up with in-memory fake
// fetchers so the spec endpoint can be exercised end-to-end. Returns the
// fixture, the test server, and the fake maps so tests can seed them.
func newTestAgentAPIWithFetchers(t *testing.T) (*fixture, *httptest.Server, *fakeBindingFetcher, *fakeProfileFetcher, *fakeImageFetcher) {
	t.Helper()
	f := newFixture(t)
	fb := &fakeBindingFetcher{byMUUID: map[string]*bindings.Binding{}, passwd: map[string]string{}}
	fp := &fakeProfileFetcher{byID: map[string]*profiles.Profile{}}
	fi := &fakeImageFetcher{byID: map[string]*images.Image{}}
	a := NewAgentAPIWithFetchers(f.store, fb, fp, fi, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts, fb, fp, fi
}

func agentDo(t *testing.T, ts *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// jsonBody marshals m to a JSON string. Helper for tests.
func jsonBody(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestAgentCurrentHappy: GET /current returns the pending job for the machine.
func TestAgentCurrentHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '0')
	j, err := f.store.Create(t.Context(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	code, body := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/current?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusOK {
		t.Fatalf("current code=%d body=%s", code, body)
	}
	var got Job
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != j.ID || got.Status != "pending" {
		t.Errorf("got=%+v", got)
	}
}

func TestAgentCurrentMissingMachineUUID(t *testing.T) {
	_, ts := newTestAgentAPI(t)
	code, _ := agentDo(t, ts, "GET", "/api/v1/agent/jobs/current", "")
	if code != http.StatusBadRequest {
		t.Errorf("missing muuid code=%d want 400", code)
	}
}

func TestAgentCurrentNotFound(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	// seed a machine but no jobs for it
	muuid := f.seedMachine(t, '1')
	code, _ := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/current?machine_uuid="+muuid, "")
	if code != http.StatusNotFound {
		t.Errorf("no-job code=%d want 404", code)
	}
}

func TestAgentClaimHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '2')
	j, _ := f.store.Create(t.Context(), in)

	body := jsonBody(t, map[string]any{"machine_uuid": in.MachineUUID})
	code, b := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim", body)
	if code != http.StatusOK {
		t.Fatalf("claim code=%d body=%s", code, b)
	}
	var got Job
	_ = json.Unmarshal(b, &got)
	if got.Status != "running" || got.StartedAt == nil {
		t.Errorf("claim result %+v", got)
	}
}

// TestAgentClaimMachineMismatch: body.machine_uuid ≠ job.machine_uuid → 403.
// This is the foot-gun guard — not authentication, just a consistency check.
func TestAgentClaimMachineMismatch(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '3')
	j, _ := f.store.Create(t.Context(), in)
	otherMUUID := f.seedMachine(t, '4')

	body := jsonBody(t, map[string]any{"machine_uuid": otherMUUID})
	code, b := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim", body)
	if code != http.StatusForbidden {
		t.Fatalf("mismatch code=%d body=%s want 403", code, b)
	}
	// Job should still be pending.
	cur, _ := f.store.Get(t.Context(), j.ID)
	if cur.Status != "pending" {
		t.Errorf("job moved despite mismatch: %s", cur.Status)
	}
}

func TestAgentClaimInvalidTransition(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '5')
	j, _ := f.store.Create(t.Context(), in)
	// already claim via store, so the second claim via API should 409
	_, _ = f.store.Claim(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{"machine_uuid": in.MachineUUID})
	code, b := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim", body)
	if code != http.StatusConflict {
		t.Fatalf("double-claim code=%d body=%s want 409", code, b)
	}
}

func TestAgentClaimUnknownJob(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	muuid := f.seedMachine(t, '6')
	body := jsonBody(t, map[string]any{"machine_uuid": muuid})
	code, _ := agentDo(t, ts, "POST",
		"/api/v1/agent/jobs/"+strings.Repeat("0", 32)+"/claim", body)
	if code != http.StatusNotFound {
		t.Errorf("unknown job code=%d want 404", code)
	}
}

func TestAgentClaimBadJSON(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '7')
	j, _ := f.store.Create(t.Context(), in)

	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim", `{not valid`)
	if code != http.StatusBadRequest {
		t.Errorf("bad json code=%d want 400", code)
	}

	// missing machine_uuid → 400
	code, _ = agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("missing muuid code=%d want 400", code)
	}

	// unknown field → 400
	code, _ = agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/claim",
		`{"machine_uuid":"`+in.MachineUUID+`","bogus":"x"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown field code=%d want 400", code)
	}
}

func TestAgentStageHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '8')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"stage":        "download",
	})
	code, b := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/stage", body)
	if code != http.StatusNoContent {
		t.Fatalf("stage code=%d body=%s", code, b)
	}
	got, _ := f.store.Get(t.Context(), j.ID)
	if got.Stage != "download" {
		t.Errorf("stage=%q", got.Stage)
	}
}

func TestAgentStageOnPendingRejected(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '9')
	j, _ := f.store.Create(t.Context(), in) // pending, not running

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"stage":        "anything",
	})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/stage", body)
	if code != http.StatusConflict {
		t.Errorf("stage-on-pending code=%d want 409", code)
	}
}

func TestAgentLogsHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'a')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"level":        "info",
		"message":      "downloading image",
	})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/logs", body)
	if code != http.StatusNoContent {
		t.Fatalf("logs code=%d", code)
	}
	got, _ := f.store.Logs(t.Context(), j.ID, 0, 10)
	if len(got) != 1 || got[0].Message != "downloading image" || got[0].Level != "info" {
		t.Errorf("logs=%+v", got)
	}
}

func TestAgentLogsBadLevel(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'b')
	j, _ := f.store.Create(t.Context(), in)

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"level":        "fatal",
		"message":      "boom",
	})
	code, b := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/logs", body)
	if code != http.StatusBadRequest {
		t.Errorf("bad level code=%d body=%s want 400", code, b)
	}
}

func TestAgentSucceedHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'c')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{"machine_uuid": in.MachineUUID})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/succeed", body)
	if code != http.StatusNoContent {
		t.Fatalf("succeed code=%d", code)
	}
	got, _ := f.store.Get(t.Context(), j.ID)
	if got.Status != "succeeded" {
		t.Errorf("status=%q", got.Status)
	}
}

func TestAgentSucceedFromPendingRejected(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'd')
	j, _ := f.store.Create(t.Context(), in) // pending, never claimed

	body := jsonBody(t, map[string]any{"machine_uuid": in.MachineUUID})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/succeed", body)
	if code != http.StatusConflict {
		t.Errorf("succeed-pending code=%d want 409", code)
	}
}

func TestAgentFailHappy(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'e')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"error":        "disk write failed: ENOSPC",
	})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/fail", body)
	if code != http.StatusNoContent {
		t.Fatalf("fail code=%d", code)
	}
	got, _ := f.store.Get(t.Context(), j.ID)
	if got.Status != "failed" || !bytes.Contains([]byte(got.Error), []byte("ENOSPC")) {
		t.Errorf("got=%+v", got)
	}
}

// TestAgentFailFromTerminalRejected: once succeeded, fail must 409.
func TestAgentFailFromTerminalRejected(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, 'f')
	j, _ := f.store.Create(t.Context(), in)
	_, _ = f.store.Claim(t.Context(), j.ID)
	_ = f.store.Succeed(t.Context(), j.ID)

	body := jsonBody(t, map[string]any{
		"machine_uuid": in.MachineUUID,
		"error":        "too late",
	})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+j.ID+"/fail", body)
	if code != http.StatusConflict {
		t.Errorf("fail-terminal code=%d want 409", code)
	}
}

// TestAgentEndpointsUUIDCaseInsensitive: agent might send the UUID upcased; the
// API normalises to lower so the ownership check succeeds.
func TestAgentEndpointsUUIDCaseInsensitive(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '~')
	j, _ := f.store.Create(t.Context(), in)

	body := jsonBody(t, map[string]any{
		"machine_uuid": strings.ToUpper(in.MachineUUID),
	})
	code, _ := agentDo(t, ts, "POST", "/api/v1/agent/jobs/"+strings.ToUpper(j.ID)+"/claim", body)
	if code != http.StatusOK {
		t.Errorf("upper-case code=%d want 200", code)
	}
}

// seedSpecFakes populates the fake fetchers with a binding/profile/image that
// reference the job's identifiers, returning the seeded values for assertion.
func seedSpecFakes(t *testing.T, j *Job, fb *fakeBindingFetcher, fp *fakeProfileFetcher, fi *fakeImageFetcher) (*bindings.Binding, *profiles.Profile, *images.Image) {
	t.Helper()
	b := &bindings.Binding{
		MachineUUID:  j.MachineUUID,
		ImageID:      j.ImageID,
		ProfileID:    j.ProfileID,
		DesiredState: "install",
		Hostname:     "node-test",
		UpdatedBy:    "admin",
	}
	p := &profiles.Profile{
		ID:               j.ProfileID,
		Name:             "ubuntu-default",
		HostnameTemplate: "node-{n}",
		RootPasswordHash: "$6$test$abc",
	}
	img := &images.Image{
		ID:     j.ImageID,
		Name:   "ubuntu.qcow2",
		Format: "qcow2",
		SHA256: "deadbeef" + strings.Repeat("0", 56),
	}
	fb.byMUUID[j.MachineUUID] = b
	fp.byID[j.ProfileID] = p
	fi.byID[j.ImageID] = img
	return b, p, img
}

func TestAgentSpecHappy(t *testing.T) {
	f, ts, fb, fp, fi := newTestAgentAPIWithFetchers(t)
	in := f.baseInput(t, '0')
	j, err := f.store.Create(t.Context(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, p, img := seedSpecFakes(t, j, fb, fp, fi)

	code, body := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusOK {
		t.Fatalf("spec code=%d body=%s", code, body)
	}
	var got InstallSpec
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.JobID != j.ID {
		t.Errorf("JobID=%q want %q", got.JobID, j.ID)
	}
	if got.MachineUUID != j.MachineUUID {
		t.Errorf("MachineUUID=%q want %q", got.MachineUUID, j.MachineUUID)
	}
	if got.ImageID != img.ID || got.ImageSHA256 != img.SHA256 || got.ImageFormat != img.Format {
		t.Errorf("image fields: %+v", got)
	}
	wantURL := "/api/v1/agent/images/" + img.ID + "/blob"
	if got.ImageBlobURL != wantURL {
		t.Errorf("ImageBlobURL=%q want %q", got.ImageBlobURL, wantURL)
	}
	if got.Profile.ID != p.ID || got.Profile.RootPasswordHash != p.RootPasswordHash {
		t.Errorf("profile=%+v", got.Profile)
	}
	if got.Binding.MachineUUID != b.MachineUUID || got.Binding.Hostname != b.Hostname {
		t.Errorf("binding=%+v", got.Binding)
	}
}

func TestAgentSpecMachineMismatch(t *testing.T) {
	f, ts, fb, fp, fi := newTestAgentAPIWithFetchers(t)
	in := f.baseInput(t, '1')
	j, _ := f.store.Create(t.Context(), in)
	seedSpecFakes(t, j, fb, fp, fi)
	otherMUUID := f.seedMachine(t, '2')

	code, _ := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+otherMUUID, "")
	if code != http.StatusForbidden {
		t.Errorf("mismatch code=%d want 403", code)
	}
}

func TestAgentSpecUnknownJob(t *testing.T) {
	f, ts, _, _, _ := newTestAgentAPIWithFetchers(t)
	muuid := f.seedMachine(t, '3')
	code, _ := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+strings.Repeat("0", 32)+"/spec?machine_uuid="+muuid, "")
	if code != http.StatusNotFound {
		t.Errorf("unknown job code=%d want 404", code)
	}
}

func TestAgentSpecMissingMachineUUID(t *testing.T) {
	f, ts, fb, fp, fi := newTestAgentAPIWithFetchers(t)
	in := f.baseInput(t, '4')
	j, _ := f.store.Create(t.Context(), in)
	seedSpecFakes(t, j, fb, fp, fi)

	code, _ := agentDo(t, ts, "GET", "/api/v1/agent/jobs/"+j.ID+"/spec", "")
	if code != http.StatusBadRequest {
		t.Errorf("missing muuid code=%d want 400", code)
	}
}

// TestAgentSpecBareAPIReturns503: the back-compat constructor leaves the
// fetchers nil; the spec endpoint must 503 (not panic, not 500).
func TestAgentSpecBareAPIReturns503(t *testing.T) {
	f, ts := newTestAgentAPI(t)
	in := f.baseInput(t, '5')
	j, _ := f.store.Create(t.Context(), in)

	code, _ := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusServiceUnavailable {
		t.Errorf("bare-api spec code=%d want 503", code)
	}
}

// newTestAgentAPIWithSubnets is the spec-test setup that also attaches a fake
// subnet fetcher so the M2.3-12 phase ④ overlay can be exercised.
func newTestAgentAPIWithSubnets(t *testing.T) (*fixture, *httptest.Server, *fakeBindingFetcher, *fakeProfileFetcher, *fakeImageFetcher, *fakeSubnetFetcher) {
	t.Helper()
	f := newFixture(t)
	fb := &fakeBindingFetcher{byMUUID: map[string]*bindings.Binding{}, passwd: map[string]string{}}
	fp := &fakeProfileFetcher{byID: map[string]*profiles.Profile{}}
	fi := &fakeImageFetcher{byID: map[string]*images.Image{}}
	fs := &fakeSubnetFetcher{byID: map[string]*subnets.Subnet{}}
	a := NewAgentAPIWithFetchers(f.store, fb, fp, fi, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithSubnets(fs)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts, fb, fp, fi, fs
}

// TestAgentSpecSubnetOverlay: when binding.subnet_id is set, the spec must
// expose subnet's CIDR/gateway/DNS (and vlan if any) on profile.network.
func TestAgentSpecSubnetOverlay(t *testing.T) {
	f, ts, fb, fp, fi, fs := newTestAgentAPIWithSubnets(t)
	in := f.baseInput(t, '6')
	j, _ := f.store.Create(t.Context(), in)
	b, p, _ := seedSpecFakes(t, j, fb, fp, fi)
	// Profile was seeded with no network method — give it a meaningful
	// starting point so we can prove the overlay replaced everything.
	p.Network = profiles.NetworkConfig{
		Method: "dhcp", NICSelector: "auto", VLAN: 999,
	}
	b.SubnetID = "11111111111111111111111111111111"
	fs.byID[b.SubnetID] = &subnets.Subnet{
		ID:      b.SubnetID,
		Name:    "lab",
		CIDR:    "10.20.0.0/22",
		Gateway: "10.20.0.1",
		DNS:     []string{"1.1.1.1", "8.8.8.8"},
		VLANID:  100,
	}

	code, body := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	var got InstallSpec
	_ = json.Unmarshal(body, &got)
	nc := got.Profile.Network
	if nc.Method != "static" {
		t.Errorf("method=%q want static", nc.Method)
	}
	if nc.PrefixLen != 22 {
		t.Errorf("prefix=%d want 22", nc.PrefixLen)
	}
	if nc.Gateway != "10.20.0.1" {
		t.Errorf("gateway=%q want 10.20.0.1", nc.Gateway)
	}
	if len(nc.DNS) != 2 || nc.DNS[0] != "1.1.1.1" || nc.DNS[1] != "8.8.8.8" {
		t.Errorf("dns=%v", nc.DNS)
	}
	if nc.VLAN != 100 {
		t.Errorf("vlan=%d want 100 (subnet's)", nc.VLAN)
	}
}

// TestAgentSpecVLANOverrideWinsOverSubnet: binding.vlan_override > 0 should
// take precedence over the subnet's vlan_id.
func TestAgentSpecVLANOverrideWinsOverSubnet(t *testing.T) {
	f, ts, fb, fp, fi, fs := newTestAgentAPIWithSubnets(t)
	in := f.baseInput(t, '7')
	j, _ := f.store.Create(t.Context(), in)
	b, _, _ := seedSpecFakes(t, j, fb, fp, fi)
	b.SubnetID = "22222222222222222222222222222222"
	b.VLANOverride = 200
	fs.byID[b.SubnetID] = &subnets.Subnet{
		ID:      b.SubnetID,
		CIDR:    "192.168.0.0/24",
		Gateway: "192.168.0.1",
		VLANID:  100,
	}

	code, body := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	var got InstallSpec
	_ = json.Unmarshal(body, &got)
	if got.Profile.Network.VLAN != 200 {
		t.Errorf("vlan=%d want 200 (binding override wins)", got.Profile.Network.VLAN)
	}
}

// TestAgentSpecNoSubnetLeavesProfileIntact: binding.subnet_id empty → the
// profile.network is sent through unmodified (legacy path).
func TestAgentSpecNoSubnetLeavesProfileIntact(t *testing.T) {
	f, ts, fb, fp, fi, _ := newTestAgentAPIWithSubnets(t)
	in := f.baseInput(t, '8')
	j, _ := f.store.Create(t.Context(), in)
	_, p, _ := seedSpecFakes(t, j, fb, fp, fi)
	p.Network = profiles.NetworkConfig{
		Method:    "static",
		PrefixLen: 24,
		Gateway:   "10.0.0.1",
		DNS:       []string{"10.0.0.53"},
		VLAN:      50,
	}

	code, body := agentDo(t, ts, "GET",
		"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+in.MachineUUID, "")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	var got InstallSpec
	_ = json.Unmarshal(body, &got)
	nc := got.Profile.Network
	if nc.Gateway != "10.0.0.1" || nc.PrefixLen != 24 || nc.VLAN != 50 {
		t.Errorf("profile mutated unexpectedly: %+v", nc)
	}
}

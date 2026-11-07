package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"metalkit/internal/sshkeys"
)

// fakeSSHKeyFetcher is the spec-handler test double for sshkeys.Store.
type fakeSSHKeyFetcher struct {
	byID map[string]*sshkeys.SSHKey
}

func (f *fakeSSHKeyFetcher) Get(_ context.Context, id string) (*sshkeys.SSHKey, error) {
	if k, ok := f.byID[id]; ok {
		return k, nil
	}
	return nil, sshkeys.ErrNotFound
}

// TestAgentSpecSSHPublicKeys: the binding's ssh_key_ids resolve to
// authorized_keys lines on the spec, in selection order; no selection (or a
// nil fetcher) means an absent field; a dangling id fails the whole spec
// rather than silently installing without the key.
func TestAgentSpecSSHPublicKeys(t *testing.T) {
	f, ts, fb, fp, fi := newTestAgentAPIWithFetchers(t)
	fk := &fakeSSHKeyFetcher{byID: map[string]*sshkeys.SSHKey{}}
	// Re-wire the server with the ssh key fetcher attached. The helper
	// returned a server without one; tests for other fields don't need it.
	a := NewAgentAPIWithFetchers(f.store, fb, fp, fi, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.WithSSHKeys(fk)
	mux := http.NewServeMux()
	a.RegisterRoutes(mux)
	ts2 := httptest.NewServer(mux)
	t.Cleanup(ts2.Close)
	ts.Close()

	in := f.baseInput(t, 'b')
	j, _ := f.store.Create(t.Context(), in)
	b, _, _ := seedSpecFakes(t, j, fb, fp, fi)

	fetch := func(mu string) (int, InstallSpec) {
		t.Helper()
		code, body := agentDo(t, ts2, "GET",
			"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+mu, "")
		var got InstallSpec
		_ = json.Unmarshal(body, &got)
		return code, got
	}

	line1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd ops@metalkit"
	line2 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIEeIM72OY68FIhL6i3KsXruAYy0C5matj1jacX/rDwd backup@metalkit"
	fk.byID["11111111111111111111111111111111"] = &sshkeys.SSHKey{ID: "11111111111111111111111111111111", PublicKey: line1}
	fk.byID["22222222222222222222222222222222"] = &sshkeys.SSHKey{ID: "22222222222222222222222222222222", PublicKey: line2}

	// 1. No selection → no field.
	code, spec := fetch(in.MachineUUID)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if len(spec.SSHPublicKeys) != 0 {
		t.Errorf("no selection: ssh_public_keys = %v, want empty", spec.SSHPublicKeys)
	}

	// 2. Two keys, selection order preserved.
	b.SSHKeyIDs = []string{"11111111111111111111111111111111", "22222222222222222222222222222222"}
	code, spec = fetch(in.MachineUUID)
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if len(spec.SSHPublicKeys) != 2 || spec.SSHPublicKeys[0] != line1 || spec.SSHPublicKeys[1] != line2 {
		t.Errorf("ssh_public_keys = %v, want the two lines in order", spec.SSHPublicKeys)
	}

	// 3. Dangling id → 500, never a partial install without the key.
	b.SSHKeyIDs = []string{"11111111111111111111111111111111", "33333333333333333333333333333333"}
	if code, _ = fetch(in.MachineUUID); code != http.StatusInternalServerError {
		t.Errorf("dangling ssh key id: code=%d, want 500", code)
	}
}

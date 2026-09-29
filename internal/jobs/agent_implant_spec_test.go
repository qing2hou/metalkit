package jobs

import (
	"encoding/json"
	"net/http"
	"testing"

	"metalkit/internal/profiles"
)

// TestAgentSpecAgentInstalledOverride: the spec's profile.agent_installed
// follows the per-binding override when set, and the profile value when
// the binding stays nil (inherit).
func TestAgentSpecAgentInstalledOverride(t *testing.T) {
	f, ts, fb, fp, fi := newTestAgentAPIWithFetchers(t)
	in := f.baseInput(t, 'a')
	j, _ := f.store.Create(t.Context(), in)
	b, p, _ := seedSpecFakes(t, j, fb, fp, fi)

	fetch := func(mu string) profiles.Profile {
		t.Helper()
		code, body := agentDo(t, ts, "GET",
			"/api/v1/agent/jobs/"+j.ID+"/spec?machine_uuid="+mu, "")
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s", code, body)
		}
		var got InstallSpec
		_ = json.Unmarshal(body, &got)
		return got.Profile
	}

	// 1. Inherit: binding nil, profile off.
	if pr := fetch(in.MachineUUID); pr.AgentInstalled {
		t.Error("no override + profile off should stay off")
	}

	// 2. Inherit: binding nil, profile on.
	p.AgentInstalled = true
	if pr := fetch(in.MachineUUID); !pr.AgentInstalled {
		t.Error("no override + profile on should stay on")
	}

	// 3. Binding override flips profile default off.
	off := false
	b.AgentInstalledOverride = &off
	if pr := fetch(in.MachineUUID); pr.AgentInstalled {
		t.Error("binding override false must win over profile true")
	}

	// 4. Binding override flips profile default on.
	p.AgentInstalled = false
	on := true
	b.AgentInstalledOverride = &on
	if pr := fetch(in.MachineUUID); !pr.AgentInstalled {
		t.Error("binding override true must win over profile false")
	}
}

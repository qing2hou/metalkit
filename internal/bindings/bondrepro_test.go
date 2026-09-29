package bindings

import (
	"context"
	"encoding/json"
	"testing"
)

// Regression: the UI serialised its "clear the bond override" sentinel as the
// JS string 'null', which arrives as the JSON string "null". That used to hit
// ValidateBondJSON and 400 every save (observed in production: body length 90
// = {"error":"bond: json: cannot unmarshal string into Go value of type
// profiles.BondConfig"}). Both spellings must now mean "clear".
func TestBondClearAcceptsStringSentinel(t *testing.T) {
	for i, sentinel := range []string{`null`, `"null"`, `{}`, `"{}"`, `""`} {
		f := newFixture(t)
		mu := f.seedMachine(t, 'c')
		im := f.seedImage(t, "c")
		suffix := string(rune('a' + i))
		pr := f.seedProfile(t, "p-bond-"+suffix, "static")
		sid := f.seedSubnet(t, "lab-bond-"+suffix, "10.11.0.0/24", "10.11.0.1")
		b, err := f.bindings.Upsert(context.Background(), UpsertInput{
			MachineUUID:  mu,
			ImageID:      im,
			ProfileID:    pr,
			DesiredState: "install",
			SubnetID:     &sid,
			Bond:         json.RawMessage(sentinel),
			UpdatedBy:    "admin",
		})
		if err != nil {
			t.Errorf("Bond=%s: Upsert failed: %v", sentinel, err)
			continue
		}
		if b.Bond != nil {
			t.Errorf("Bond=%s: override should be cleared, got %+v", sentinel, b.Bond)
		}
		if b.StaticAddress == "" {
			t.Errorf("Bond=%s: address should have been auto-allocated", sentinel)
		}
	}
}

// A real bond config must still be accepted and stored.
func TestBondRealConfigStillStored(t *testing.T) {
	f := newFixture(t)
	mu := f.seedMachine(t, 'd')
	im := f.seedImage(t, "d")
	pr := f.seedProfile(t, "p-bond-real", "static")
	sid := f.seedSubnet(t, "lab-bond-real", "10.12.0.0/24", "10.12.0.1")
	b, err := f.bindings.Upsert(context.Background(), UpsertInput{
		MachineUUID:  mu,
		ImageID:      im,
		ProfileID:    pr,
		DesiredState: "install",
		SubnetID:     &sid,
		Bond:         json.RawMessage(`{"mode":"active-backup","slaves":["eno1","eno2"],"miimon":100}`),
		UpdatedBy:    "admin",
	})
	if err != nil {
		t.Fatalf("Upsert with real bond: %v", err)
	}
	if b.Bond == nil || b.Bond.Mode != "active-backup" || len(b.Bond.Slaves) != 2 {
		t.Fatalf("bond not stored: %+v", b.Bond)
	}
}

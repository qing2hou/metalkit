package bindings

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateVLANOverride(t *testing.T) {
	tests := []struct {
		name      string
		in        *int
		wantNull  bool
		wantVal   int
		wantErr   bool
		errSubstr string
	}{
		{"nil_caller_bug", nil, false, 0, true, "nil"},
		{"zero_writes_null", ptrInt(0), true, 0, false, ""},
		{"min_valid", ptrInt(1), false, 1, false, ""},
		{"max_valid", ptrInt(4094), false, 4094, false, ""},
		{"negative_rejected", ptrInt(-1), false, 0, true, "must be 0"},
		{"too_large_rejected", ptrInt(4095), false, 0, true, "must be 0"},
		{"reserved_4095_rejected", ptrInt(4095), false, 0, true, "must be 0"},
		{"typical_lab_vlan", ptrInt(100), false, 100, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeNull, val, err := validateVLANOverride(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("err %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if writeNull != tc.wantNull {
				t.Errorf("writeNull = %v, want %v", writeNull, tc.wantNull)
			}
			if val != tc.wantVal {
				t.Errorf("val = %d, want %d", val, tc.wantVal)
			}
		})
	}
}

func TestValidateSubnetID(t *testing.T) {
	valid32 := strings.Repeat("a", 32)
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty_ok", "", "", false},
		{"whitespace_canonicalized_to_empty", "   ", "", false},
		{"valid_lower", valid32, valid32, false},
		{"valid_uppercase_lowered", strings.ToUpper(valid32), valid32, false},
		{"too_short", "abc", "", true},
		{"too_long", strings.Repeat("a", 33), "", true},
		{"non_hex", strings.Repeat("g", 32), "", true},
		{"hyphenated_uuid_rejected", "4c4c4544-0058-3210-8053-c5c04f463830", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateSubnetID(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil: got=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func ptrInt(v int) *int { return &v }

func ptrStr(v string) *string { return &v }

// TestValidateMachineUUID_TypedError: the API layer classifies 400 vs 500
// via errors.Is(err, ErrInvalidUUID) — the sentinel must survive wrapping.
func TestValidateMachineUUID_TypedError(t *testing.T) {
	_, err := validateMachineUUID("NOT-A-UUID")
	if err == nil {
		t.Fatal("expected error for non-UUID")
	}
	if !errors.Is(err, ErrInvalidUUID) {
		t.Errorf("error does not wrap ErrInvalidUUID: %v", err)
	}
	if !strings.Contains(err.Error(), "machine_uuid") && !strings.Contains(err.Error(), "must be canonical") {
		t.Errorf("message lost its diagnostic content: %v", err)
	}
	if _, err := validateMachineUUID("4c4c4544-0058-3210-8053-c5c04f463830"); err != nil {
		t.Errorf("valid UUID rejected: %v", err)
	}
}

package agentclient

import (
	"errors"
	"testing"
)

func TestAPIError_Error(t *testing.T) {
	cases := []struct {
		in   *APIError
		want string
	}{
		{&APIError{Code: 404, Message: "no job"}, "agentclient: http 404: no job"},
		{&APIError{Code: 500, Message: ""}, "agentclient: http 500"},
	}
	for _, tc := range cases {
		if got := tc.in.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

func TestErrNoCurrentJob_Identity(t *testing.T) {
	// errors.Is should match the sentinel even after fmt.Errorf wrap.
	wrapped := errors.Join(ErrNoCurrentJob, errors.New("ctx"))
	if !errors.Is(wrapped, ErrNoCurrentJob) {
		t.Fatal("errors.Is failed on wrapped sentinel")
	}
}

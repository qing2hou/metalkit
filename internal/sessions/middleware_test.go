package sessions_test

import (
	"context"
	"testing"

	"metalkit/internal/sessions"
)

func TestWithUserAndUserFromContext(t *testing.T) {
	ctx := context.Background()

	if got := sessions.UserFromContext(ctx); got != "" {
		t.Fatalf("UserFromContext on bare ctx = %q, want \"\"", got)
	}

	ctx2 := sessions.WithUser(ctx, "alice")
	if got := sessions.UserFromContext(ctx2); got != "alice" {
		t.Fatalf("UserFromContext = %q, want %q", got, "alice")
	}

	// Original ctx unchanged.
	if got := sessions.UserFromContext(ctx); got != "" {
		t.Fatalf("UserFromContext on original ctx after derive = %q, want \"\"", got)
	}

	// Overwriting works.
	ctx3 := sessions.WithUser(ctx2, "bob")
	if got := sessions.UserFromContext(ctx3); got != "bob" {
		t.Fatalf("UserFromContext after overwrite = %q, want %q", got, "bob")
	}
}

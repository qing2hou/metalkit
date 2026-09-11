package sessions

import "context"

// ctxKey is an unexported type so other packages can't collide with our key.
type ctxKey int

const userKey ctxKey = 0

// WithUser returns ctx with the username attached. Slice B's auth middleware
// calls this after validating the session cookie; downstream handlers read it
// back via UserFromContext (e.g. for the X-Auth-User audit field on writes).
func WithUser(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, userKey, username)
}

// UserFromContext returns the username attached by WithUser, or "" if none.
func UserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userKey).(string)
	return v
}

// roleKey carries the operator's role ("admin" | "operator") alongside the
// username. Attached by the httpd auth middleware; empty means "legacy
// single-admin deployment" — handlers treat empty as admin for back-compat.
const roleKey ctxKey = 1

// WithRole returns ctx with the operator's role attached.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleKey, role)
}

// RoleFromContext returns the role attached by WithRole, or "" when absent.
func RoleFromContext(ctx context.Context) string {
	v, _ := ctx.Value(roleKey).(string)
	return v
}

// IsAdmin reports whether the request context carries an admin (or legacy
// empty-role, i.e. single-admin deployments) identity. Operator-restricted
// handlers use this; everything else stays role-agnostic.
func IsAdmin(ctx context.Context) bool {
	switch RoleFromContext(ctx) {
	case "", "admin":
		return true
	}
	return false
}

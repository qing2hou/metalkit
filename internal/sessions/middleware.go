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

package sessions

import "time"

// SetClockForTest replaces s's clock. Test-only: this file is _test.go so it
// only compiles when running `go test`.
func SetClockForTest(s *Store, fn func() time.Time) {
	s.clock = fn
}

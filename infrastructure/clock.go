package infrastructure

import (
	"time"

	"github.com/prorochestvo/yarddog/services"
)

// SystemClock is the real wall clock: main wires it into services.Execute in
// production. Tests inject a fake instead so the recovery loop's up-to-15m
// timeout runs instantly.
type SystemClock struct{}

var _ services.Clock = SystemClock{}

// After waits for d to elapse, as time.After.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

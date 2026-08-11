package accelerated

import (
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// settings is immutable once published; the atomic pointer swap is the only
// mutation, so a reader can never see a factor paired with the other base.
type settings struct {
	factor int       // the last factor supplied, verbatim — may be <= 1
	base   time.Time // meaningful only when active
	active bool      // factor > 1 AND base usable
}

var current atomic.Pointer[settings] // nil ⇒ never configured: factor 1, inactive

// nowFn is the package's single wall-clock seam and the only time.Now in the
// repository. SetNowForTest swaps it; production never does.
var nowFn = time.Now //nolint:forbidigo // the one wrapper implementation

// load is the single atomic read. Every exported reader calls it exactly once
// per invocation, so no caller can observe two different configurations.
func load() *settings {
	return current.Load()
}

// GetCurrentTime returns the app clock: wall time, or base + factor×(now-base).
func GetCurrentTime() time.Time {
	s := load()
	now := nowFn()
	if s == nil || !s.active {
		return now
	}
	return s.base.Add(now.Sub(s.base) * time.Duration(s.factor))
}

// Configure sets the process clock from an explicit base. The base is
// TRUNCATED TO WHOLE SECONDS, so the RFC3339 timestamp this package later
// echoes is a lossless encoding of the exact value it computes from and a
// client's second-resolution copy of it never drifts. factor <= 1 disables
// acceleration but is still recorded verbatim for Snapshot. Safe from any
// goroutine.
func Configure(factor int, base time.Time) {
	current.Store(&settings{
		factor: factor,
		base:   base.Truncate(time.Second),
		active: factor > 1,
	})
}

// ConfigureNow anchors the base at the current wall clock, TRUNCATED TO WHOLE
// SECONDS for the same reason as Configure, and sets the factor, returning the
// new base. The package owns the wall clock (rule 1), so the settings handler
// must not capture its own.
func ConfigureNow(factor int) time.Time {
	base := nowFn().Truncate(time.Second)
	current.Store(&settings{
		factor: factor,
		base:   base,
		active: factor > 1,
	})
	return base
}

// ConfigureAtBoot applies settings from configuration. baseStr accepts Unix
// seconds or RFC3339; either way the base is truncated to whole seconds for
// the same reason as Configure. A missing or unparseable base with factor > 1
// records the factor, leaves the clock inactive, and returns an error
// describing why. Parameter types match config.RuntimeConfig exactly (int,
// string).
func ConfigureAtBoot(factor int, baseStr string) error {
	if factor <= 1 {
		current.Store(&settings{factor: factor})
		return nil
	}
	base, err := parseBase(baseStr)
	if err != nil {
		current.Store(&settings{factor: factor})
		return fmt.Errorf("time acceleration factor %d supplied with no usable base: %w", factor, err)
	}
	current.Store(&settings{
		factor: factor,
		base:   base.Truncate(time.Second),
		active: true,
	})
	return nil
}

// parseBase tries Unix seconds first (what SetTimeAcceleration/ConfigureNow
// produce), then falls back to RFC3339 (what the docs additionally promise).
func parseBase(baseStr string) (time.Time, error) {
	if baseStr == "" {
		return time.Time{}, fmt.Errorf("TIME_BASE is empty")
	}
	if unixSec, err := strconv.ParseInt(baseStr, 10, 64); err == nil {
		return time.Unix(unixSec, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, baseStr); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("TIME_BASE %q is neither Unix seconds nor RFC3339", baseStr)
}

// Snapshot reports the last accepted factor VERBATIM (including values <= 1),
// the base, and whether the clock is actually accelerated. Never configured ⇒
// (1, time.Time{}, false). One atomic load.
func Snapshot() (factor int, base time.Time, active bool) {
	s := load()
	if s == nil {
		return 1, time.Time{}, false
	}
	return s.factor, s.base, s.active
}

// SnapshotWithTime reports the app clock AND the settings that produced it,
// from ONE atomic load, so an HTTP response can never pair a current_time
// computed under one configuration with a factor/base/active from another.
// This is the only reader GetSystemTime may use.
func SnapshotWithTime() (now time.Time, factor int, base time.Time, active bool) {
	s := load()
	n := nowFn()
	if s == nil {
		return n, 1, time.Time{}, false
	}
	if !s.active {
		return n, s.factor, s.base, s.active
	}
	return s.base.Add(n.Sub(s.base) * time.Duration(s.factor)), s.factor, s.base, s.active
}

// Reset disables acceleration and clears the recorded factor.
func Reset() {
	current.Store(nil)
}

// SetNowForTest replaces the package wall clock and returns a restore func.
// Exported because handler tests in other packages must advance the clock
// deterministically; there is no other way to test re-anchoring without a
// sleep. Production code must never call it.
func SetNowForTest(fn func() time.Time) (restore func()) {
	prev := nowFn
	nowFn = fn
	return func() { nowFn = prev }
}

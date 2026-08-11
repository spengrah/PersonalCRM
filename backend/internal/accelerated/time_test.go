package accelerated

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestGetCurrentTime_ProcessState pins the package's read path against
// injected process state instead of environment variables. No subtest here
// calls time.Now() — every clock read comes from the injected nowFn, which is
// the rule this PR exists to make true of the package itself, not just its
// callers.
func TestGetCurrentTime_ProcessState(t *testing.T) {
	fakeNow := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("unconfigured returns wall clock", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fakeNow })
		t.Cleanup(restore)

		if got := GetCurrentTime(); !got.Equal(fakeNow) {
			t.Fatalf("GetCurrentTime() = %v, want %v", got, fakeNow)
		}
	})

	t.Run("factor of one is inactive", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fakeNow })
		t.Cleanup(restore)

		Configure(1, fakeNow.Add(-time.Hour))

		if got := GetCurrentTime(); !got.Equal(fakeNow) {
			t.Fatalf("GetCurrentTime() = %v, want wall clock %v", got, fakeNow)
		}
		if _, _, active := Snapshot(); active {
			t.Fatal("Snapshot() active = true, want false for factor 1")
		}
	})

	t.Run("accelerates elapsed time", func(t *testing.T) {
		t.Cleanup(Reset)
		base := fakeNow
		now := fakeNow
		restore := SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)

		Configure(10, base)
		now = now.Add(60 * time.Second)

		want := base.Add(600 * time.Second)
		if got := GetCurrentTime(); !got.Equal(want) {
			t.Fatalf("GetCurrentTime() = %v, want %v", got, want)
		}
	})

	t.Run("re-anchoring the base jumps the clock", func(t *testing.T) {
		t.Cleanup(Reset)
		now := fakeNow
		restore := SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)

		base := fakeNow.Add(-1000 * time.Second)
		Configure(60, base)
		first := GetCurrentTime()

		Configure(60, base.Add(-1000*time.Second))
		second := GetCurrentTime()

		wantDelta := 59 * 1000 * time.Second // (factor-1) * shift
		if gotDelta := second.Sub(first); gotDelta != wantDelta {
			t.Fatalf("second - first = %v, want %v", gotDelta, wantDelta)
		}
	})

	t.Run("boot accepts unix seconds and rfc3339", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fakeNow })
		t.Cleanup(restore)

		unixBase := fakeNow.Add(-500 * time.Second)
		if err := ConfigureAtBoot(60, fmt.Sprintf("%d", unixBase.Unix())); err != nil {
			t.Fatalf("ConfigureAtBoot(unix) returned error: %v", err)
		}
		if _, _, active := Snapshot(); !active {
			t.Fatal("Snapshot() active = false after unix-seconds boot config")
		}

		Reset()
		rfcBase := fakeNow.Add(-500 * time.Second)
		if err := ConfigureAtBoot(60, rfcBase.Format(time.RFC3339)); err != nil {
			t.Fatalf("ConfigureAtBoot(rfc3339) returned error: %v", err)
		}
		if _, _, active := Snapshot(); !active {
			t.Fatal("Snapshot() active = false after rfc3339 boot config")
		}
	})

	t.Run("boot without a base is inactive and errors", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fakeNow })
		t.Cleanup(restore)

		err := ConfigureAtBoot(60, "")
		if err == nil {
			t.Fatal("ConfigureAtBoot(60, \"\") returned nil error, want non-nil")
		}
		factor, _, active := Snapshot()
		if active {
			t.Fatal("Snapshot() active = true, want false")
		}
		if factor != 60 {
			t.Fatalf("Snapshot() factor = %d, want 60 (echoed verbatim)", factor)
		}
	})

	t.Run("snapshot echoes a non-positive factor", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fakeNow })
		t.Cleanup(restore)

		Configure(-5, fakeNow.Add(-time.Hour))

		factor, _, active := Snapshot()
		if factor != -5 {
			t.Fatalf("Snapshot() factor = %d, want -5", factor)
		}
		if active {
			t.Fatal("Snapshot() active = true, want false for a non-positive factor")
		}
		if got := GetCurrentTime(); !got.Equal(fakeNow) {
			t.Fatalf("GetCurrentTime() = %v, want wall clock %v", got, fakeNow)
		}
	})

	t.Run("snapshot agrees with the clock", func(t *testing.T) {
		cases := []struct {
			name       string
			configure  func()
			wantActive bool
		}{
			{"unconfigured", func() {}, false},
			{"factor one", func() { Configure(1, fakeNow.Add(-time.Hour)) }, false},
			{"accelerated", func() { Configure(10, fakeNow.Add(-time.Hour)) }, true},
			{"negative factor", func() { Configure(-5, fakeNow.Add(-time.Hour)) }, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Cleanup(Reset)
				restore := SetNowForTest(func() time.Time { return fakeNow })
				t.Cleanup(restore)

				tc.configure()
				_, _, active := Snapshot()
				if active != tc.wantActive {
					t.Fatalf("Snapshot() active = %v, want %v", active, tc.wantActive)
				}
				differsFromWall := !GetCurrentTime().Equal(fakeNow)
				if differsFromWall != active {
					t.Fatalf("GetCurrentTime() differs from wall clock = %v, want it to match active = %v", differsFromWall, active)
				}
			})
		}
	})
}

// TestConfigureNow_TruncatesBaseToSecond pins that every entry point stores
// a whole-second base so the server's RFC3339 echo is a lossless encoding of
// the value it actually computes from, and the browser's second-resolution
// copy never drifts from it.
func TestConfigureNow_TruncatesBaseToSecond(t *testing.T) {
	fractional := time.Date(2026, 3, 1, 12, 0, 0, 750_000_000, time.UTC)
	want := fractional.Truncate(time.Second)

	t.Run("ConfigureNow", func(t *testing.T) {
		t.Cleanup(Reset)
		restore := SetNowForTest(func() time.Time { return fractional })
		t.Cleanup(restore)

		got := ConfigureNow(60)
		if got.Nanosecond() != 0 {
			t.Fatalf("ConfigureNow returned base with Nanosecond() = %d, want 0", got.Nanosecond())
		}
		if !got.Equal(want) {
			t.Fatalf("ConfigureNow returned %v, want %v", got, want)
		}
		_, base, _ := Snapshot()
		if !base.Equal(want) {
			t.Fatalf("Snapshot() base = %v, want %v", base, want)
		}
	})

	t.Run("Configure", func(t *testing.T) {
		t.Cleanup(Reset)
		Configure(60, fractional)
		_, base, _ := Snapshot()
		if base.Nanosecond() != 0 {
			t.Fatalf("Snapshot() base Nanosecond() = %d, want 0", base.Nanosecond())
		}
		if !base.Equal(want) {
			t.Fatalf("Snapshot() base = %v, want %v", base, want)
		}
	})

	t.Run("ConfigureAtBoot", func(t *testing.T) {
		t.Cleanup(Reset)
		if err := ConfigureAtBoot(60, fractional.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("ConfigureAtBoot returned error: %v", err)
		}
		_, base, _ := Snapshot()
		if base.Nanosecond() != 0 {
			t.Fatalf("Snapshot() base Nanosecond() = %d, want 0", base.Nanosecond())
		}
		if !base.Equal(want) {
			t.Fatalf("Snapshot() base = %v, want %v", base, want)
		}
	})
}

// TestConfigureAtBoot_AppliesAndWarns pins the contract crm-api's and
// crm-admin's boot wiring both depend on: a good boot config activates, a
// missing or unparseable base echoes the factor but stays inactive and
// returns a non-nil error, and a factor <= 1 with no base is a silent no-op
// (nothing to warn about).
func TestConfigureAtBoot_AppliesAndWarns(t *testing.T) {
	fakeNow := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("good config activates", func(t *testing.T) {
		t.Cleanup(Reset)
		base := fakeNow.Add(-500 * time.Second)
		if err := ConfigureAtBoot(60, fmt.Sprintf("%d", base.Unix())); err != nil {
			t.Fatalf("ConfigureAtBoot returned error: %v", err)
		}
		factor, gotBase, active := Snapshot()
		if !active {
			t.Fatal("Snapshot() active = false, want true")
		}
		if factor != 60 {
			t.Fatalf("Snapshot() factor = %d, want 60", factor)
		}
		if !gotBase.Equal(base.Truncate(time.Second)) {
			t.Fatalf("Snapshot() base = %v, want %v", gotBase, base.Truncate(time.Second))
		}
	})

	t.Run("empty base errors and stays inactive", func(t *testing.T) {
		t.Cleanup(Reset)
		err := ConfigureAtBoot(60, "")
		if err == nil {
			t.Fatal("ConfigureAtBoot returned nil error, want non-nil naming the missing base")
		}
		factor, _, active := Snapshot()
		if active {
			t.Fatal("Snapshot() active = true, want false")
		}
		if factor != 60 {
			t.Fatalf("Snapshot() factor = %d, want 60 (echoed verbatim)", factor)
		}
	})

	t.Run("unparseable base errors and stays inactive", func(t *testing.T) {
		t.Cleanup(Reset)
		err := ConfigureAtBoot(60, "not-a-timestamp")
		if err == nil {
			t.Fatal("ConfigureAtBoot returned nil error, want non-nil naming the unparseable base")
		}
		factor, _, active := Snapshot()
		if active {
			t.Fatal("Snapshot() active = true, want false")
		}
		if factor != 60 {
			t.Fatalf("Snapshot() factor = %d, want 60 (echoed verbatim)", factor)
		}
	})

	t.Run("factor at or below one with no base is a silent no-op", func(t *testing.T) {
		t.Cleanup(Reset)
		if err := ConfigureAtBoot(1, ""); err != nil {
			t.Fatalf("ConfigureAtBoot(1, \"\") returned error: %v, want nil", err)
		}
		_, _, active := Snapshot()
		if active {
			t.Fatal("Snapshot() active = true, want false")
		}
	})
}

// TestGetCurrentTime_ConcurrentReconfigure is the proof this PR exists to
// make possible: under the old two-os.Getenv implementation, a reader
// landing between the factor write and the base write could pair a new
// factor with a stale base. The atomic.Pointer[settings] swap makes that
// structurally impossible, because every reader does exactly one load and
// therefore observes one configuration in full or the other in full, never a
// mix. The wall clock is held fixed (via the injected nowFn, per the
// package's no-time.Now()-in-tests rule) so each configuration produces one
// exact, precomputed value; any read that is neither value is a torn read.
// Run with -race so a regression to non-atomic package state is caught as a
// data race too, not just as a wrong value.
func TestGetCurrentTime_ConcurrentReconfigure(t *testing.T) {
	t.Cleanup(Reset)
	fixedNow := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	restore := SetNowForTest(func() time.Time { return fixedNow })
	t.Cleanup(restore)

	const factorA, factorB = 2, 1000
	baseA := fixedNow.Add(-100 * time.Second)
	baseB := fixedNow.Add(-500 * time.Second)
	valueA := baseA.Add(fixedNow.Sub(baseA) * time.Duration(factorA))
	valueB := baseB.Add(fixedNow.Sub(baseB) * time.Duration(factorB))

	// Seed synchronously so no reader can start before the package holds
	// either configuration — otherwise an early read legitimately observes
	// the unconfigured wall clock, which is a false positive for this test,
	// not the torn read it exists to catch.
	Configure(factorA, baseA)

	stop := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				Configure(factorA, baseA)
			} else {
				Configure(factorB, baseB)
			}
		}
	}()

	const readers = 8
	const iterations = 5000
	errCh := make(chan error, readers)
	var readerWG sync.WaitGroup
	readerWG.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer readerWG.Done()
			for i := 0; i < iterations; i++ {
				got := GetCurrentTime()
				if !got.Equal(valueA) && !got.Equal(valueB) {
					select {
					case errCh <- fmt.Errorf("torn read: got %v, want %v or %v", got, valueA, valueB):
					default:
					}
					return
				}
			}
		}()
	}

	readerWG.Wait()
	close(stop)
	writerWG.Wait()

	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

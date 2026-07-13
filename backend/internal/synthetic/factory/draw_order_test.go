package factory

import (
	"testing"
	"time"
)

// drawOrderAnchor is a fixed anchor for the draw-count tests; its value is
// irrelevant (these tests only compare PRNG draw positions, not timestamps).
var drawOrderAnchor = time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)

// baseDrawOrderOpts is the option set a catalog slot carries MINUS the age option,
// so the reference build differs from the option build only by that one option.
func baseDrawOrderOpts() []ContactOption {
	return []ContactOption{WithEmail(), WithCadence("weekly")}
}

// TestWithCreatedAge_DrawsZero proves WithCreatedAge adds NO generator draw over
// the identical base build: the overdue catalog slots drew zero before this change
// (the old WithOverdue used a fixed offset), so the shared-generator stream — and
// every downstream replay's ids — stays byte-identical.
func TestWithCreatedAge_DrawsZero(t *testing.T) {
	t.Parallel()

	gA := NewGeneratorAt(DefaultSeed, "draworder", drawOrderAnchor)
	_ = gA.Contact(append(baseDrawOrderOpts(), WithCreatedAge(90*24*time.Hour))...)
	withOption := gA.rng.Uint64()

	gB := NewGeneratorAt(DefaultSeed, "draworder", drawOrderAnchor)
	_ = gB.Contact(baseDrawOrderOpts()...)
	withoutOption := gB.rng.Uint64()

	if withOption != withoutOption {
		t.Fatalf("WithCreatedAge changed the PRNG stream position: got next-raw %d with the option vs %d without it (expected equal — zero extra draws)", withOption, withoutOption)
	}
}

// TestWithRecentCreation_DrawsOneRecentOffset proves WithRecentCreation's sole
// extra draw is exactly one recentOffset — the same draw the deleted WithRecent
// made at that stream position — so the shared-generator sequence is preserved.
func TestWithRecentCreation_DrawsOneRecentOffset(t *testing.T) {
	t.Parallel()

	const window = 48 * time.Hour

	gC := NewGeneratorAt(DefaultSeed, "draworder", drawOrderAnchor)
	_ = gC.Contact(append(baseDrawOrderOpts(), WithRecentCreation(window))...)
	withOption := gC.rng.Uint64()

	gD := NewGeneratorAt(DefaultSeed, "draworder", drawOrderAnchor)
	_ = gD.Contact(baseDrawOrderOpts()...)
	gD.recentOffset(window) // the option's single extra draw
	withoutOption := gD.rng.Uint64()

	if withOption != withoutOption {
		t.Fatalf("WithRecentCreation did not draw exactly one recentOffset: got next-raw %d with the option vs %d after one manual recentOffset (expected equal)", withOption, withoutOption)
	}
}

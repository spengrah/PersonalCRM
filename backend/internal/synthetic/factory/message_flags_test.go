package factory

import (
	"reflect"
	"testing"
)

// The same seed and namespace build the same window, flags, clearings and
// unjudged messages; another seed builds others.
func TestMessageFlags_SameSeedBuildsSameRecords(t *testing.T) {
	t.Parallel()

	keys := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	kinds := []string{"commitment", "big_moment", "claim"}
	reasons := []string{"call_failed", "message_over_limit", "before_window"}
	build := func(seed uint64) (MessageFlagsSpec, []ClearingSpec, []UnjudgedSpec) {
		g := NewGeneratorAt(seed, "flags", drawOrderAnchor)
		return g.MessageFlags(keys, kinds), g.Clearings(keys[:3]), g.UnjudgedMessages(keys, reasons)
	}

	flagsA, clearingsA, unjudgedA := build(DefaultSeed)
	flagsB, clearingsB, unjudgedB := build(DefaultSeed)

	if !reflect.DeepEqual(flagsA, flagsB) || !reflect.DeepEqual(clearingsA, clearingsB) || !reflect.DeepEqual(unjudgedA, unjudgedB) {
		t.Fatalf("the same seed built different records:\n%+v\n%+v", flagsA, flagsB)
	}
	if len(flagsA.Flags) != len(flagsA.NewMessages)*len(kinds) {
		t.Fatalf("got %d flags for %d new messages and %d kinds", len(flagsA.Flags), len(flagsA.NewMessages), len(kinds))
	}
	flagsC, clearingsC, unjudgedC := build(DefaultSeed + 1)
	if reflect.DeepEqual(flagsA, flagsC) && reflect.DeepEqual(clearingsA, clearingsC) && reflect.DeepEqual(unjudgedA, unjudgedC) {
		t.Fatal("another seed built the same records: the seed decides nothing")
	}
}

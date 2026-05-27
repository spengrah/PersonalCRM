package mac

import "testing"

// TestIsAllowedPushSource pins the daemon-push source allowlist. Sources
// outside this set are rejected with 400 at the cursor get/commit
// endpoints before any DB write happens; new entries land here as their
// daemon-side readers ship.
func TestIsAllowedPushSource(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"messages",
		"icloud_contacts",
		"anarlog_humans",
		"anarlog_sessions",
	}
	for _, src := range allowed {
		src := src
		t.Run("allowed/"+src, func(t *testing.T) {
			t.Parallel()
			if !IsAllowedPushSource(src) {
				t.Fatalf("IsAllowedPushSource(%q) = false, want true", src)
			}
		})
	}

	rejected := []string{
		"",
		"anarlog_unknown",
		"gcontacts",
		"telegram",
		"ANARLOG_HUMANS", // case sensitive
	}
	for _, src := range rejected {
		src := src
		t.Run("rejected/"+src, func(t *testing.T) {
			t.Parallel()
			if IsAllowedPushSource(src) {
				t.Fatalf("IsAllowedPushSource(%q) = true, want false", src)
			}
		})
	}
}

// TestAllowedPushSourcesMap pins the exact membership of the allowlist
// map so any future addition is caught by a failing test (forcing the
// author to update this test and think about ingest-layer + payload
// validators that also need to pick up the new source).
func TestAllowedPushSourcesMap(t *testing.T) {
	t.Parallel()

	expected := map[string]struct{}{
		"messages":         {},
		"icloud_contacts":  {},
		"anarlog_humans":   {},
		"anarlog_sessions": {},
	}
	if len(AllowedPushSources) != len(expected) {
		t.Fatalf("AllowedPushSources size = %d, want %d", len(AllowedPushSources), len(expected))
	}
	for src := range expected {
		if _, ok := AllowedPushSources[src]; !ok {
			t.Errorf("AllowedPushSources missing %q", src)
		}
	}
	for src := range AllowedPushSources {
		if _, ok := expected[src]; !ok {
			t.Errorf("AllowedPushSources has unexpected %q; update this test if new source is intentional", src)
		}
	}
}

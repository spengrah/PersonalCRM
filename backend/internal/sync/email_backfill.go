package sync

import (
	"strings"
	"time"
)

const (
	// EmailBackfillSinceDefault is the onboarding floor written into new email
	// sync state metadata and used when metadata has no valid override.
	EmailBackfillSinceDefault = "2026-01-01"

	emailBackfillSinceMetadataKey = "backfill_since"
	emailBackfillDateLayout       = "2006-01-02"
)

// EmailBackfillFloorEpoch reads metadata["backfill_since"] as YYYY-MM-DD and
// returns its UTC epoch seconds, falling back to EmailBackfillSinceDefault.
func EmailBackfillFloorEpoch(metadata map[string]any) int64 {
	since := EmailBackfillSinceDefault
	if metadata != nil {
		if v, ok := metadata[emailBackfillSinceMetadataKey].(string); ok && strings.TrimSpace(v) != "" {
			since = strings.TrimSpace(v)
		}
	}
	t, err := time.Parse(emailBackfillDateLayout, since)
	if err != nil {
		t, _ = time.Parse(emailBackfillDateLayout, EmailBackfillSinceDefault)
	}
	return t.Unix()
}

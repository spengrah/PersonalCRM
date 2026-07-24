package replay

import "sync"

// Tunables the batch adapters read, with test-only setters so an integration
// test can drive the failure shapes the constants exist to prevent — a GChat
// sweep with bucketing disabled, and a GCal drain that hits its iteration cap.
// Both are package-level, so a test that overrides one must not run in parallel
// with another batch test; the batch suite is serial for a separate reason (the
// per-test isolated-database connection budget).
var (
	batchTunablesMu sync.Mutex

	// gchatBatchSpacesPerSync is how many spaces ReplayGChatBatch presents per
	// Sync. Zero or less disables bucketing entirely (every space every Sync) —
	// the negative control, which the provider's shared page budget cannot
	// survive for a space count much past half the budget.
	gchatBatchSpacesPerSync = gchatBatchDefaultSpacesPerSync

	// gcalBatchMaxSyncsOverride, when positive, replaces the drain-loop iteration
	// cap ReplayGCalBatch otherwise derives from the batch size.
	gcalBatchMaxSyncsOverride = 0
)

// SetGChatBatchSpacesPerSyncForTest overrides the GChat batch bucket size and
// returns a restore func. Pass 0 (or less) to disable bucketing. Test-only.
func SetGChatBatchSpacesPerSyncForTest(n int) (restore func()) {
	batchTunablesMu.Lock()
	defer batchTunablesMu.Unlock()
	prev := gchatBatchSpacesPerSync
	gchatBatchSpacesPerSync = n
	return func() {
		batchTunablesMu.Lock()
		defer batchTunablesMu.Unlock()
		gchatBatchSpacesPerSync = prev
	}
}

// SetGCalBatchMaxSyncsForTest overrides the GCal drain-loop iteration cap and
// returns a restore func. Pass 0 to fall back to the size-derived cap.
// Test-only.
func SetGCalBatchMaxSyncsForTest(n int) (restore func()) {
	batchTunablesMu.Lock()
	defer batchTunablesMu.Unlock()
	prev := gcalBatchMaxSyncsOverride
	gcalBatchMaxSyncsOverride = n
	return func() {
		batchTunablesMu.Lock()
		defer batchTunablesMu.Unlock()
		gcalBatchMaxSyncsOverride = prev
	}
}

// gchatSpacesPerSync / gcalMaxSyncs read the tunables under the lock.
func gchatSpacesPerSync() int {
	batchTunablesMu.Lock()
	defer batchTunablesMu.Unlock()
	return gchatBatchSpacesPerSync
}

func gcalMaxSyncs(derived int) int {
	batchTunablesMu.Lock()
	defer batchTunablesMu.Unlock()
	if gcalBatchMaxSyncsOverride > 0 {
		return gcalBatchMaxSyncsOverride
	}
	return derived
}

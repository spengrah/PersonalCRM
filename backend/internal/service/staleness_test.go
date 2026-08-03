package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// fixedNow is a stable reference time for the pure-function tests. All
// fixtures are positioned relative to it; no wall clock is read.
var fixedNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// stalenessTestConfig returns a config with explicit, easy-to-reason-about
// thresholds. Source overrides cover the cases the matrix exercises.
func stalenessTestConfig() config.StalenessConfig {
	return config.StalenessConfig{
		HeartbeatThreshold: 15 * time.Minute,
		PullThreshold:      24 * time.Hour,
		PushThreshold:      48 * time.Hour,
		ErrorMinCount:      3,
		ErrorThreshold:     6 * time.Hour,
		SourceOverrides: map[string]time.Duration{
			"gcontacts":        72 * time.Hour,
			"phone_calls":      168 * time.Hour,
			"icloud_contacts":  336 * time.Hour,
			"anarlog_sessions": 168 * time.Hour,
			"anarlog_humans":   336 * time.Hour,
		},
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
func ptrStr(s string) *string        { return &s }

// findCandidate returns the candidate matching (source, breachType) or nil.
func findCandidate(cands []breachCandidate, source, breachType string) *breachCandidate {
	for i := range cands {
		if cands[i].source == source && cands[i].breachType == breachType {
			return &cands[i]
		}
	}
	return nil
}

func makeSyncState(source string, opts func(*repository.SyncState)) repository.SyncState {
	st := repository.SyncState{
		ID:        uuid.New(),
		Source:    source,
		Enabled:   true,
		Status:    repository.SyncStatusIdle,
		Strategy:  repository.SyncStrategyContactDriven,
		CreatedAt: fixedNow.Add(-365 * 24 * time.Hour), // ancient by default
	}
	if opts != nil {
		opts(&st)
	}
	return st
}

func makeHost(heartbeatAgo time.Duration, sourceHealth json.RawMessage) *repository.MacHost {
	hb := fixedNow.Add(-heartbeatAgo)
	return &repository.MacHost{
		ID:              uuid.New(),
		Hostname:        "synthetic-host",
		LastHeartbeatAt: &hb,
		SourceHealth:    sourceHealth,
		CreatedAt:       fixedNow.Add(-30 * 24 * time.Hour),
	}
}

// --- heartbeat ---

func TestEvaluateBreaches_HeartbeatFreshAndStale(t *testing.T) {
	cfg := stalenessTestConfig()

	fresh := makeHost(5*time.Minute, nil)
	if got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{fresh}); len(got) != 0 {
		t.Fatalf("fresh host should not breach, got %d candidates", len(got))
	}

	stale := makeHost(20*time.Minute, nil)
	got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{stale})
	c := findCandidate(got, macHostSource, repository.BreachTypeHeartbeat)
	if c == nil {
		t.Fatal("stale host should open a heartbeat breach")
	}
	if c.accountID != stale.ID.String() {
		t.Errorf("heartbeat breach account_id = %q, want host UUID %q", c.accountID, stale.ID.String())
	}
	if c.thresholdSeconds != int64((15 * time.Minute).Seconds()) {
		t.Errorf("threshold_seconds = %d, want 900", c.thresholdSeconds)
	}
}

func TestEvaluateBreaches_HeartbeatBoundaryExclusive(t *testing.T) {
	cfg := stalenessTestConfig()
	// age == threshold exactly → not stale.
	host := makeHost(15*time.Minute, nil)
	if got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}); len(got) != 0 {
		t.Fatalf("age == threshold must not breach, got %d", len(got))
	}
}

func TestEvaluateBreaches_HeartbeatNeverHeartbeatedUsesCreatedAt(t *testing.T) {
	cfg := stalenessTestConfig()
	host := &repository.MacHost{
		ID:              uuid.New(),
		LastHeartbeatAt: nil,
		CreatedAt:       fixedNow.Add(-1 * time.Hour), // older than 15m threshold
	}
	got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host})
	c := findCandidate(got, macHostSource, repository.BreachTypeHeartbeat)
	if c == nil {
		t.Fatal("never-heartbeated host past threshold should breach via created_at")
	}
	if !c.staleSince.Equal(host.CreatedAt.UTC()) {
		t.Errorf("stale_since = %v, want created_at %v", c.staleSince, host.CreatedAt.UTC())
	}
}

func TestEvaluateBreaches_HeartbeatDisabledByZeroThreshold(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.HeartbeatThreshold = 0
	host := makeHost(48*time.Hour, nil)
	if got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}); len(got) != 0 {
		t.Fatalf("zero heartbeat threshold disables the check, got %d", len(got))
	}
}

// --- sync_stale ---

func TestEvaluateBreaches_SyncStaleFreshAndStale(t *testing.T) {
	cfg := stalenessTestConfig()

	fresh := makeSyncState("gcal", func(st *repository.SyncState) {
		st.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-1 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{fresh}, nil); len(got) != 0 {
		t.Fatalf("fresh pull row should not breach, got %d", len(got))
	}

	stale := makeSyncState("gcal", func(st *repository.SyncState) {
		st.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-26 * time.Hour))
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{stale}, nil)
	if findCandidate(got, "gcal", repository.BreachTypeSyncStale) == nil {
		t.Fatal("stale pull row should open a sync_stale breach")
	}
}

func TestEvaluateBreaches_SyncStaleBoundaryExclusive(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-24 * time.Hour)) // exactly threshold
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("age == threshold must not breach, got %d", len(got))
	}
}

func TestEvaluateBreaches_SyncStaleNeverSucceededFallback(t *testing.T) {
	cfg := stalenessTestConfig()
	// Never succeeded; created 26h ago (> 24h pull threshold).
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-26 * time.Hour)
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil)
	c := findCandidate(got, "gcal", repository.BreachTypeSyncStale)
	if c == nil {
		t.Fatal("never-succeeded enabled row past threshold should breach via created_at")
	}
	if !c.staleSince.Equal(st.CreatedAt.UTC()) {
		t.Errorf("stale_since = %v, want created_at %v", c.staleSince, st.CreatedAt.UTC())
	}
}

func TestEvaluateBreaches_SyncStaleDisabledRowSkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Enabled = false
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("disabled row must be skipped, got %d", len(got))
	}
}

func TestEvaluateBreaches_SyncStalePushStrategySkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("messages", func(s *repository.SyncState) {
		s.Strategy = repository.SyncStrategyPush
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-100 * time.Hour)
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("push-strategy row must be excluded from sync_stale, got %d", len(got))
	}
}

func TestEvaluateBreaches_SyncStaleTelegramExcluded(t *testing.T) {
	cfg := stalenessTestConfig()
	// Telegram never writes last_successful_sync_at → ancient created_at; must
	// NOT produce a sync_stale (permanent false positive otherwise).
	st := makeSyncState("telegram", func(s *repository.SyncState) {
		s.Strategy = repository.SyncStrategyFetchAll
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-365 * 24 * time.Hour)
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil)
	if findCandidate(got, "telegram", repository.BreachTypeSyncStale) != nil {
		t.Fatal("telegram must be excluded from sync_stale")
	}
}

func TestEvaluateBreaches_SyncStaleFeatureFlagOff(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	// externalSyncEnabled=false → no pull check for non-telegram.
	if got := evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("feature flag off must skip pull checks, got %d", len(got))
	}
}

func TestEvaluateBreaches_SyncStalePerSourceOverride(t *testing.T) {
	cfg := stalenessTestConfig()
	// gcontacts override is 72h; a 50h-stale gcontacts row must NOT breach
	// (would breach under the 24h default).
	st := makeSyncState("gcontacts", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-50 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("gcontacts within its 72h override must not breach, got %d", len(got))
	}

	// Past the override → breach.
	st2 := makeSyncState("gcontacts", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-80 * time.Hour))
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st2}, nil)
	c := findCandidate(got, "gcontacts", repository.BreachTypeSyncStale)
	if c == nil {
		t.Fatal("gcontacts past its 72h override should breach")
	}
	if c.thresholdSeconds != int64((72 * time.Hour).Seconds()) {
		t.Errorf("threshold_seconds = %d, want override 259200", c.thresholdSeconds)
	}
}

func TestEvaluateBreaches_SyncStaleZeroOverrideDisablesSource(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.SourceOverrides["gcal"] = 0
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("zero override disables that source, got %d", len(got))
	}
}

func TestEvaluateBreaches_SyncStaleZeroPullThresholdDisablesClass(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.PullThreshold = 0
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("zero pull threshold disables sync_stale, got %d", len(got))
	}
}

// --- sync_error two-term predicate ---

func TestEvaluateBreaches_SyncErrorCountBoundary(t *testing.T) {
	cfg := stalenessTestConfig()
	// count 2 (below min 3) with a 7h-old anchor → no breach.
	st2 := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 2
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-7 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st2}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("error_count 2 (< min 3) must not breach")
	}

	// count 3 with a 7h-old anchor → breach.
	st3 := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-7 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st3}, nil), "gcal", repository.BreachTypeSyncError) == nil {
		t.Fatal("error_count 3 with 7h anchor must breach")
	}
}

func TestEvaluateBreaches_SyncErrorTransientBlipNoBreaches(t *testing.T) {
	cfg := stalenessTestConfig()
	// count >= 3 but last success 10m ago → the transient-blip case: NO breach
	// because the duration floor (6h) is not met.
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 5
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-10 * time.Minute))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("recent success with errors is a transient blip; must not breach")
	}
}

func TestEvaluateBreaches_SyncErrorTelegramCountOnlyAncientAnchor(t *testing.T) {
	cfg := stalenessTestConfig()
	// Telegram error row: NULL success, ancient created_at anchor → the
	// duration term is trivially satisfied; breach on the count.
	st := makeSyncState("telegram", func(s *repository.SyncState) {
		s.Strategy = repository.SyncStrategyFetchAll
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-30 * 24 * time.Hour)
	})
	// Telegram is evaluated even with externalSyncEnabled=false.
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{st}, nil), "telegram", repository.BreachTypeSyncError) == nil {
		t.Fatal("telegram error row must breach on count regardless of feature flag")
	}
}

func TestEvaluateBreaches_SyncErrorThresholdZeroCountOnly(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.ErrorThreshold = 0 // drop the duration term → count-only
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-1 * time.Minute)) // very recent
	})
	c := findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError)
	if c == nil {
		t.Fatal("ErrorThreshold=0 → count-only; count 3 must breach even with recent success")
	}
	if c.thresholdSeconds != 0 {
		t.Errorf("threshold_seconds = %d, want 0 in count-only mode", c.thresholdSeconds)
	}
}

func TestEvaluateBreaches_SyncErrorMinCountZeroDisablesClass(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.ErrorMinCount = 0
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 10
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("ErrorMinCount=0 disables the sync_error class")
	}
}

func TestEvaluateBreaches_SyncErrorIdleStatusNoBreaches(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusIdle
		s.ErrorCount = 5
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-7 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("idle status must not produce a sync_error breach")
	}
}

func TestEvaluateBreaches_SyncErrorFeatureFlagOffSkipsPullRow(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 5
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("feature flag off must skip sync_error for non-telegram pull rows")
	}
}

func TestEvaluateBreaches_SyncStaleAndSyncErrorCoexist(t *testing.T) {
	cfg := stalenessTestConfig()
	// Row that is both not-recently-successful AND erroring: both breaches.
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 4
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-30 * time.Hour))
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil)
	if findCandidate(got, "gcal", repository.BreachTypeSyncStale) == nil {
		t.Error("expected a sync_stale breach")
	}
	if findCandidate(got, "gcal", repository.BreachTypeSyncError) == nil {
		t.Error("expected a sync_error breach")
	}
}

func TestEvaluateBreaches_SyncErrorTruncatesMessage(t *testing.T) {
	cfg := stalenessTestConfig()
	longMsg := ""
	for i := 0; i < 500; i++ {
		longMsg += "x"
	}
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-7 * time.Hour))
		s.ErrorMessage = ptrStr(longMsg)
	})
	c := findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError)
	if c == nil {
		t.Fatal("expected sync_error breach")
	}
	// details = "erroring for ..., N consecutive errors: <truncated>…"
	if len([]rune(c.details)) > maxBreachDetailLen+100 {
		t.Errorf("details unexpectedly long (%d runes); truncation failed", len([]rune(c.details)))
	}
}

// --- push_stale ---

func sourceHealthJSON(t *testing.T, entries map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal source_health: %v", err)
	}
	return b
}

func TestEvaluateBreaches_PushFreshAndStale(t *testing.T) {
	cfg := stalenessTestConfig()

	fresh := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-1 * time.Hour)},
	}))
	got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{fresh})
	if findCandidate(got, "messages", repository.BreachTypePushStale) != nil {
		t.Fatal("fresh push must not breach")
	}

	stale := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-50 * time.Hour)},
	}))
	got = evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{stale})
	c := findCandidate(got, "messages", repository.BreachTypePushStale)
	if c == nil {
		t.Fatal("messages 50h stale (> 48h push threshold) should breach")
	}
	if c.accountID != stale.ID.String() {
		t.Errorf("push breach account_id = %q, want host UUID", c.accountID)
	}
}

func TestEvaluateBreaches_PushDisabledEntrySkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages": map[string]any{"enabled": false, "last_pushed_at": fixedNow.Add(-100 * time.Hour)},
	}))
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}), "messages", repository.BreachTypePushStale) != nil {
		t.Fatal("enabled:false source_health entry must be skipped")
	}
}

func TestEvaluateBreaches_PushAbsentLastPushedSkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages": map[string]any{"enabled": true}, // no last_pushed_at
	}))
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}), "messages", repository.BreachTypePushStale) != nil {
		t.Fatal("absent last_pushed_at must be skipped (can't compute age)")
	}
}

func TestEvaluateBreaches_PushNullLastPushedSkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	host := makeHost(5*time.Minute, json.RawMessage(`{"messages":{"enabled":true,"last_pushed_at":null}}`))
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}), "messages", repository.BreachTypePushStale) != nil {
		t.Fatal("null last_pushed_at must be skipped")
	}
}

func TestEvaluateBreaches_PushPerSourceOverride(t *testing.T) {
	cfg := stalenessTestConfig()
	// phone_calls override is 168h (7d). A 100h-stale push must NOT breach.
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"phone_calls": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-100 * time.Hour)},
	}))
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}), "phone_calls", repository.BreachTypePushStale) != nil {
		t.Fatal("phone_calls within its 168h override must not breach")
	}

	host2 := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"phone_calls": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-200 * time.Hour)},
	}))
	c := findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host2}), "phone_calls", repository.BreachTypePushStale)
	if c == nil {
		t.Fatal("phone_calls past its 168h override should breach")
	}
	if c.thresholdSeconds != int64((168 * time.Hour).Seconds()) {
		t.Errorf("threshold_seconds = %d, want override 604800", c.thresholdSeconds)
	}
}

func TestEvaluateBreaches_PushAnarlogSourcesResolveOverrides(t *testing.T) {
	cfg := stalenessTestConfig()
	// anarlog_sessions override 168h, anarlog_humans 336h. Both 50h-stale →
	// neither breaches (proves the named surface resolves through thresholdFor).
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"anarlog_sessions": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-50 * time.Hour)},
		"anarlog_humans":   map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-50 * time.Hour)},
	}))
	got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host})
	if len(got) != 0 {
		t.Fatalf("anarlog sources within overrides must not breach, got %d", len(got))
	}
}

func TestEvaluateBreaches_PushZeroThresholdDisablesClass(t *testing.T) {
	cfg := stalenessTestConfig()
	cfg.PushThreshold = 0
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-100 * time.Hour)},
	}))
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host}), "messages", repository.BreachTypePushStale) != nil {
		t.Fatal("zero push threshold disables the class for non-overridden sources")
	}
}

func TestEvaluateBreaches_PushMalformedSourceHealthSkippedNoError(t *testing.T) {
	cfg := stalenessTestConfig()
	host := makeHost(5*time.Minute, json.RawMessage(`{"messages": "not an object"}`))
	// Malformed JSON for the per-source value → unmarshal fails → host skipped,
	// no panic, no candidate. The heartbeat check (fresh here) still runs.
	got := evaluateBreaches(fixedNow, cfg, false, nil, []*repository.MacHost{host})
	if len(got) != 0 {
		t.Fatalf("malformed source_health must yield no push candidates, got %d", len(got))
	}
}

func TestEvaluateBreaches_PushSortedDeterministicOrder(t *testing.T) {
	cfg := stalenessTestConfig()
	// Two stale push sources; iterate twice and require identical order.
	host := makeHost(5*time.Minute, sourceHealthJSON(t, map[string]any{
		"messages":         map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-50 * time.Hour)},
		"anarlog_sessions": map[string]any{"enabled": true, "last_pushed_at": fixedNow.Add(-200 * time.Hour)},
	}))
	first := evaluatePushBreaches(fixedNow, cfg, []*repository.MacHost{host})
	for i := 0; i < 20; i++ {
		next := evaluatePushBreaches(fixedNow, cfg, []*repository.MacHost{host})
		if len(next) != len(first) {
			t.Fatalf("candidate count varied across runs: %d vs %d", len(first), len(next))
		}
		for j := range first {
			if first[j].source != next[j].source {
				t.Fatalf("push candidate order not deterministic: %q vs %q at %d", first[j].source, next[j].source, j)
			}
		}
	}
	// Sorted: anarlog_sessions before messages.
	if first[0].source != "anarlog_sessions" || first[1].source != "messages" {
		t.Errorf("expected sorted order [anarlog_sessions, messages], got [%q, %q]", first[0].source, first[1].source)
	}
}

// --- parser ---

func TestParseSourceHealth_EmptyAndAbsent(t *testing.T) {
	if got, ok := parseSourceHealth(nil); !ok || len(got) != 0 {
		t.Errorf("nil blob: got (%v, %v), want (empty, true)", got, ok)
	}
	if got, ok := parseSourceHealth(json.RawMessage(`{}`)); !ok || len(got) != 0 {
		t.Errorf("empty object: got (%v, %v), want (empty, true)", got, ok)
	}
}

func TestParseSourceHealth_Malformed(t *testing.T) {
	if _, ok := parseSourceHealth(json.RawMessage(`{not json`)); ok {
		t.Error("malformed JSON should report ok=false")
	}
}

func TestParseSourceHealth_IgnoresUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{"messages":{"enabled":true,"last_pushed_at":"2026-06-01T10:00:00Z","observed_cursor":42,"schema_version":"v9"}}`)
	got, ok := parseSourceHealth(raw)
	if !ok {
		t.Fatal("valid blob with extra fields should parse")
	}
	entry := got["messages"]
	if !entry.Enabled || entry.LastPushedAt == nil {
		t.Errorf("decoded entry lost known fields: %+v", entry)
	}
}

// --- helpers ---

func TestIsStaleBoundary(t *testing.T) {
	ref := fixedNow.Add(-10 * time.Minute)
	if isStale(fixedNow, ref, 10*time.Minute) {
		t.Error("age == threshold must be NOT stale")
	}
	if !isStale(fixedNow, ref, 10*time.Minute-time.Nanosecond) {
		t.Error("age > threshold must be stale")
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h30m"},
		{26 * time.Hour, "1d2h"},
		{-5 * time.Minute, "0m"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestStaleness_WhatsAppErrorEvaluatedAsManagerDriven pins the generalization of
// the manager-driven carve-out. WhatsApp's connection is owned by a long-lived
// in-process manager, so a disconnected session has to reach the watchdog even
// when external sync is off — that is what makes "degrades to a visible
// disconnected status" true through the watchdog and not only through the
// settings page.
// whatsAppTerminalRow is the row shape the WhatsApp terminal path ACTUALLY
// produces, and it is deliberately hostile to the ordinary error predicate:
//
//   - Enabled=false, because a manager-driven row must stay out of the polling
//     scheduler's queue (ListDueSyncStates selects enabled = TRUE);
//   - ErrorCount=1, because the terminal path writes the error exactly ONCE and
//     then stops — it disconnects, and the restart gate prevents retries, so the
//     count can never reach the default minimum of 3;
//   - a fresh created_at, because the row is written the moment the connection
//     ends, so the duration term is not satisfied either.
//
// Only the terminal-reason rule can open a breach on this row. Building it from
// the same metadata key the manager writes is what keeps the test honest.
func whatsAppTerminalRow(reason string) repository.SyncState {
	return makeSyncState("whatsapp", func(s *repository.SyncState) {
		s.Enabled = false
		s.Strategy = repository.SyncStrategyPush
		s.Status = repository.SyncStatusError
		s.ErrorCount = 1
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-1 * time.Minute)
		s.Metadata = map[string]any{repository.SyncStateMetadataTerminalReason: reason}
	})
}

func TestStaleness_WhatsAppErrorEvaluatedAsManagerDriven(t *testing.T) {
	cfg := stalenessTestConfig()
	for _, reason := range []string{"logged_out", "stream_replaced", "client_outdated", "temporary_ban"} {
		st := whatsAppTerminalRow(reason)
		got := findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{st}, nil), "whatsapp", repository.BreachTypeSyncError)
		if got == nil {
			t.Fatalf("%s: a terminal whatsapp row must breach regardless of the external-sync flag", reason)
		}
		if !strings.Contains(got.details, reason) {
			t.Fatalf("%s: the breach must name the reason, got %q", reason, got.details)
		}
	}
}

// TestStaleness_WhatsAppTerminalRowBreachesBelowTheErrorCountFloor is the
// discriminating half: the row the production path produces sits at one error,
// well under the three-error minimum, and would never breach on the ordinary
// predicate no matter how long it sat there.
func TestStaleness_WhatsAppTerminalRowBreachesBelowTheErrorCountFloor(t *testing.T) {
	cfg := stalenessTestConfig()

	terminal := whatsAppTerminalRow("logged_out")
	if int(terminal.ErrorCount) >= cfg.ErrorMinCount {
		t.Fatalf("fixture is not exercising the sub-threshold case: count=%d min=%d", terminal.ErrorCount, cfg.ErrorMinCount)
	}
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{terminal}, nil), "whatsapp", repository.BreachTypeSyncError) == nil {
		t.Fatal("a terminal reason must breach immediately, not wait for an error streak that never comes")
	}

	// Negative control: the SAME row without a terminal reason stays silent, so
	// the rule is the reason and not merely "manager-driven rows always breach".
	noReason := whatsAppTerminalRow("logged_out")
	noReason.Metadata = nil
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{noReason}, nil), "whatsapp", repository.BreachTypeSyncError) != nil {
		t.Fatal("without a terminal reason a single error must still be below the floor")
	}

	// And an empty reason is not a reason.
	empty := whatsAppTerminalRow("")
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{empty}, nil), "whatsapp", repository.BreachTypeSyncError) != nil {
		t.Fatal("an empty terminal reason must not open a breach")
	}
}

// TestStaleness_TerminalReasonOnAnIdleRowNeverBreaches is why the WhatsApp
// terminal decision has to be a single write.
//
// The immediate-breach rule needs BOTH halves: a terminal reason and status
// 'error'. A reason recorded on a row that is still idle is invisible forever —
// the reason rule does not apply, and the ordinary count and duration terms
// never fire either, because the terminal path writes once and then stops by
// design. Splitting the reason and the status into two writes made that row
// reachable whenever the second one failed.
func TestStaleness_TerminalReasonOnAnIdleRowNeverBreaches(t *testing.T) {
	cfg := stalenessTestConfig()

	stranded := whatsAppTerminalRow("logged_out")
	stranded.Status = repository.SyncStatusIdle
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{stranded}, nil), "whatsapp", repository.BreachTypeSyncError) != nil {
		t.Fatal("fixture is wrong: an idle row is not supposed to breach at all")
	}

	// The same row in error — the only difference — breaches immediately. That
	// difference is the entire cost of a half-written terminal decision.
	inError := whatsAppTerminalRow("logged_out")
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{inError}, nil), "whatsapp", repository.BreachTypeSyncError) == nil {
		t.Fatal("the same row in error must breach, or this test proves nothing about the missing half")
	}
}

// TestStaleness_TerminalReasonOnANonManagerRowIsIgnored bounds the rule: only
// manager-driven sources get the immediate breach.
func TestStaleness_TerminalReasonOnANonManagerRowIsIgnored(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 1
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-1 * time.Minute))
		s.Metadata = map[string]any{repository.SyncStateMetadataTerminalReason: "logged_out"}
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("an ordinary pull source must still obey the count and duration terms")
	}
}

// TestStaleness_WhatsAppExcludedFromSyncStale: WhatsApp never writes a
// last_successful_sync_at, so evaluating it for staleness would be a permanent
// false positive — the same reason Telegram is excluded.
func TestStaleness_WhatsAppExcludedFromSyncStale(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("whatsapp", func(s *repository.SyncState) {
		s.Strategy = repository.SyncStrategyFetchAll
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-365 * 24 * time.Hour)
	})
	got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil)
	if findCandidate(got, "whatsapp", repository.BreachTypeSyncStale) != nil {
		t.Fatal("whatsapp must be excluded from sync_stale")
	}
}

// TestStaleness_TelegramBehaviorUnchanged pins that widening the carve-out from
// a boolean to a set left Telegram bit-identical: excluded from sync_stale,
// always evaluated for sync_error.
func TestStaleness_TelegramBehaviorUnchanged(t *testing.T) {
	cfg := stalenessTestConfig()

	stale := makeSyncState("telegram", func(s *repository.SyncState) {
		s.Strategy = repository.SyncStrategyFetchAll
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-365 * 24 * time.Hour)
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{stale}, nil), "telegram", repository.BreachTypeSyncStale) != nil {
		t.Fatal("telegram must still be excluded from sync_stale")
	}

	// Enabled=false matches TelegramManager's own CreateSyncState call.
	errored := makeSyncState("telegram", func(s *repository.SyncState) {
		s.Enabled = false
		s.Strategy = repository.SyncStrategyFetchAll
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = nil
		s.CreatedAt = fixedNow.Add(-30 * 24 * time.Hour)
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{errored}, nil), "telegram", repository.BreachTypeSyncError) == nil {
		t.Fatal("telegram error row must still breach with the external-sync flag off")
	}

	// The negative control: an ordinary pull source is still gated on the flag.
	other := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if findCandidate(evaluateBreaches(fixedNow, cfg, false, []repository.SyncState{other}, nil), "gcal", repository.BreachTypeSyncError) != nil {
		t.Fatal("a non-manager-driven source must stay gated on the external-sync flag")
	}
}

// TestStaleness_DisabledNonManagerRowStillSkipped is the negative control for
// the manager-driven exception to the enabled filter: an ordinary disabled row
// must still be skipped entirely, so the exception cannot widen into "evaluate
// everything".
func TestStaleness_DisabledNonManagerRowStillSkipped(t *testing.T) {
	cfg := stalenessTestConfig()
	st := makeSyncState("gcal", func(s *repository.SyncState) {
		s.Enabled = false
		s.Status = repository.SyncStatusError
		s.ErrorCount = 3
		s.LastSuccessfulSyncAt = ptrTime(fixedNow.Add(-100 * time.Hour))
	})
	if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
		t.Fatalf("a disabled non-manager row must be skipped, got %d", len(got))
	}
}

// TestStaleness_ManagerDrivenHealthyRowRaisesNothing bounds the blast radius of
// evaluating disabled manager rows: only an error status breaches, so a healthy
// disconnected-but-idle integration is silent.
func TestStaleness_ManagerDrivenHealthyRowRaisesNothing(t *testing.T) {
	cfg := stalenessTestConfig()
	for _, source := range []string{"telegram", "whatsapp"} {
		st := makeSyncState(source, func(s *repository.SyncState) {
			s.Enabled = false
			s.Strategy = repository.SyncStrategyPush
			s.Status = repository.SyncStatusIdle
			s.LastSuccessfulSyncAt = nil
			s.CreatedAt = fixedNow.Add(-365 * 24 * time.Hour)
		})
		if got := evaluateBreaches(fixedNow, cfg, true, []repository.SyncState{st}, nil); len(got) != 0 {
			t.Fatalf("%s: a healthy manager row must raise nothing, got %d", source, len(got))
		}
	}
}

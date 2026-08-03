package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
)

// stalenessResolvedRetention is how long resolved breach rows are kept for
// forensic history before the watchdog prunes them. Hardcoded (mirrors the
// discarded-job retention pattern) — there is no operator knob.
const stalenessResolvedRetention = 90 * 24 * time.Hour

// macHostSource is the synthetic source value used for heartbeat breaches
// (the mac_host row has no per-source identity of its own).
const macHostSource = "mac_host"

// maxBreachDetailLen caps the truncated error_message embedded in a
// sync_error breach's details so a pathological provider error can't bloat
// the row.
const maxBreachDetailLen = 200

// StalenessService runs the sync-staleness watchdog: it compares existing
// freshness timestamps against config-backed thresholds and reconciles the
// resulting breach set against sync_staleness_breach.
type StalenessService struct {
	cfg                 config.StalenessConfig
	externalSyncEnabled bool
	syncRepo            *repository.SyncRepository
	macHostRepo         *repository.MacHostRepository
	breachRepo          *repository.StalenessRepository
	// now defaults to accelerated.GetCurrentTime; overridable in-package for
	// unit tests. No exported test hook.
	now func() time.Time
}

// NewStalenessService constructs the watchdog service. externalSyncEnabled
// mirrors cfg.Features.EnableExternalSync — when false the pull/error checks
// for non-telegram sources are skipped (the scheduler that drives those
// sources never runs, so an old status can neither recur nor clear).
func NewStalenessService(
	cfg config.StalenessConfig,
	externalSyncEnabled bool,
	syncRepo *repository.SyncRepository,
	macHostRepo *repository.MacHostRepository,
	breachRepo *repository.StalenessRepository,
) *StalenessService {
	return &StalenessService{
		cfg:                 cfg,
		externalSyncEnabled: externalSyncEnabled,
		syncRepo:            syncRepo,
		macHostRepo:         macHostRepo,
		breachRepo:          breachRepo,
		now:                 accelerated.GetCurrentTime,
	}
}

// breachCandidate is the in-memory result of evaluateBreaches: one current
// breach the reconcile loop will upsert-open. account_id follows the table
// convention (” = single/none; Google email; mac_host UUID string).
type breachCandidate struct {
	source           string
	accountID        string
	breachType       string
	staleSince       time.Time
	thresholdSeconds int64
	details          string
}

// key returns the identity tuple that matches the partial unique index, used
// to diff the candidate set against open rows during reconcile.
func (b breachCandidate) key() breachKey {
	return breachKey{source: b.source, accountID: b.accountID, breachType: b.breachType}
}

type breachKey struct {
	source     string
	accountID  string
	breachType string
}

// sourceHealthEntry is the tolerant view of one mac_host.source_health entry.
// Only the fields the watchdog needs are decoded; unknown keys are ignored so
// daemon version skew never breaks parsing.
type sourceHealthEntry struct {
	Enabled      bool       `json:"enabled"`
	LastPushedAt *time.Time `json:"last_pushed_at"`
}

// RunChecks executes one watchdog tick: load freshness state, compute the
// current breach set, then reconcile (open new / refresh existing / resolve
// missing) and prune resolved history. Each statement is individually
// idempotent; the next tick self-heals any partial run.
func (s *StalenessService) RunChecks(ctx context.Context) error {
	now := s.now()

	states, err := s.syncRepo.ListSyncStates(ctx)
	if err != nil {
		return fmt.Errorf("list sync states: %w", err)
	}
	hosts, err := s.macHostRepo.ListActiveHosts(ctx)
	if err != nil {
		return fmt.Errorf("list active mac hosts: %w", err)
	}

	candidates := evaluateBreaches(now, s.cfg, s.externalSyncEnabled, states, hosts)

	candidateKeys := make(map[breachKey]struct{}, len(candidates))
	openedCount := 0
	for _, cand := range candidates {
		candidateKeys[cand.key()] = struct{}{}
		row, upErr := s.breachRepo.UpsertOpenBreach(ctx, repository.UpsertOpenBreachParams{
			Source:           cand.source,
			AccountID:        cand.accountID,
			BreachType:       cand.breachType,
			StaleSince:       cand.staleSince,
			ThresholdSeconds: cand.thresholdSeconds,
			Details:          cand.details,
			ObservedAt:       now,
		})
		if upErr != nil {
			return fmt.Errorf("upsert breach (%s/%s/%s): %w", cand.source, cand.accountID, cand.breachType, upErr)
		}
		// A freshly-opened breach has detected_at == last_observed_at. Log it
		// loud (this is the v1 alert channel); subsequent ticks only bump
		// last_observed_at and stay quiet.
		if row.DetectedAt.Equal(row.LastObservedAt) {
			openedCount++
			logger.Warn().
				Str("source", cand.source).
				Str("account_id", cand.accountID).
				Str("breach_type", cand.breachType).
				Time("stale_since", cand.staleSince).
				Int64("threshold_seconds", cand.thresholdSeconds).
				Str("details", cand.details).
				Msg("sync staleness watchdog: breach opened")
		}
	}

	// Resolve any open breach whose identity is no longer a candidate. This
	// covers recovery, disable, host revocation, and the feature flag turning
	// a check off — all of which mean "not currently breaching".
	open, err := s.breachRepo.ListOpenBreaches(ctx)
	if err != nil {
		return fmt.Errorf("list open breaches: %w", err)
	}
	resolvedCount := 0
	for _, b := range open {
		if _, stillBreaching := candidateKeys[breachKey{source: b.Source, accountID: b.AccountID, breachType: b.BreachType}]; stillBreaching {
			continue
		}
		n, resErr := s.breachRepo.ResolveBreach(ctx, b.ID, now)
		if resErr != nil {
			return fmt.Errorf("resolve breach %s: %w", b.ID, resErr)
		}
		if n > 0 {
			resolvedCount++
			logger.Info().
				Str("source", b.Source).
				Str("account_id", b.AccountID).
				Str("breach_type", b.BreachType).
				Msg("sync staleness watchdog: breach resolved")
		}
	}

	// Retention prune of resolved history.
	if err := s.breachRepo.DeleteResolvedBefore(ctx, now.Add(-stalenessResolvedRetention)); err != nil {
		return fmt.Errorf("prune resolved breaches: %w", err)
	}

	logger.Info().
		Int("active", len(candidates)).
		Int("opened", openedCount).
		Int("resolved", resolvedCount).
		Msg("sync staleness watchdog: tick complete")
	return nil
}

// ListActiveBreaches returns all currently-open breaches. This is the cheap
// read the endpoint and the (future) /health component consume.
func (s *StalenessService) ListActiveBreaches(ctx context.Context) ([]repository.StalenessBreach, error) {
	return s.breachRepo.ListOpenBreaches(ctx)
}

// evaluateBreaches is the pure core: given the current time, config, the
// external-sync flag, and the loaded freshness state, it returns the set of
// breaches that currently hold. No DB access, no clock reads — every input is
// explicit so the unit tests can drive the full matrix.
func evaluateBreaches(
	now time.Time,
	cfg config.StalenessConfig,
	externalSyncEnabled bool,
	states []repository.SyncState,
	hosts []*repository.MacHost,
) []breachCandidate {
	var candidates []breachCandidate

	candidates = append(candidates, evaluateHeartbeatBreaches(now, cfg, hosts)...)
	candidates = append(candidates, evaluateSyncStateBreaches(now, cfg, externalSyncEnabled, states)...)
	candidates = append(candidates, evaluatePushBreaches(now, cfg, hosts)...)

	return candidates
}

// evaluateHeartbeatBreaches: per active mac host, breach when the host has
// not heartbeated within HeartbeatThreshold. A host that never heartbeated
// uses created_at as the reference.
func evaluateHeartbeatBreaches(now time.Time, cfg config.StalenessConfig, hosts []*repository.MacHost) []breachCandidate {
	if cfg.HeartbeatThreshold <= 0 {
		return nil
	}
	var out []breachCandidate
	for _, h := range hosts {
		ref := h.CreatedAt
		if h.LastHeartbeatAt != nil {
			ref = *h.LastHeartbeatAt
		}
		if !isStale(now, ref, cfg.HeartbeatThreshold) {
			continue
		}
		out = append(out, breachCandidate{
			source:           macHostSource,
			accountID:        h.ID.String(),
			breachType:       repository.BreachTypeHeartbeat,
			staleSince:       ref.UTC(),
			thresholdSeconds: int64(cfg.HeartbeatThreshold.Seconds()),
			details:          fmt.Sprintf("no heartbeat for %s (threshold %s)", humanizeAge(now.Sub(ref)), cfg.HeartbeatThreshold),
		})
	}
	return out
}

// evaluateSyncStateBreaches handles the pull-row classes (sync_stale +
// sync_error) over external_sync_state. Push-strategy rows and the telegram
// row are excluded from sync_stale (they never write last_successful_sync_at);
// telegram IS evaluated for sync_error (its manager runs independent of the
// external-sync flag).

// managerDrivenSources are the sources whose connection is owned by a
// long-lived in-process manager rather than by the polling scheduler. Their
// rows carry a connection status the user needs to see even when external sync
// is off, and they never have a "last successful sync" to go stale against.
var managerDrivenSources = map[string]struct{}{
	"telegram": {},
	"whatsapp": {},
}

func evaluateSyncStateBreaches(
	now time.Time,
	cfg config.StalenessConfig,
	externalSyncEnabled bool,
	states []repository.SyncState,
) []breachCandidate {
	var out []breachCandidate
	for _, st := range states {
		if !st.Enabled {
			continue
		}
		_, isManagerDriven := managerDrivenSources[st.Source]
		isPush := st.Strategy == repository.SyncStrategyPush

		// sync_stale: enabled, non-push, non-manager-driven pull rows, gated on
		// the external-sync feature flag.
		if cfg.PullThreshold != 0 && !isPush && !isManagerDriven && externalSyncEnabled {
			if c, ok := evalSyncStale(now, cfg, st); ok {
				out = append(out, c)
			}
		}

		// sync_error: enabled rows in 'error' status. Pull rows are gated on
		// the feature flag; a manager-driven row is always evaluated. Push rows
		// never reach status='error' (they have no Sync run) so they are
		// excluded structurally by the status check below.
		if cfg.ErrorMinCount != 0 && (externalSyncEnabled || isManagerDriven) {
			if c, ok := evalSyncError(now, cfg, st); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// evalSyncStale returns a sync_stale candidate when the row has not had a
// successful sync within its per-source threshold. The created_at fallback
// means a row that has never succeeded breaches once it is threshold-old (a
// never-working enabled source is exactly the silent failure we want).
func evalSyncStale(now time.Time, cfg config.StalenessConfig, st repository.SyncState) (breachCandidate, bool) {
	threshold := thresholdForPull(cfg, st.Source)
	if threshold <= 0 {
		return breachCandidate{}, false
	}
	ref := staleReference(st)
	if !isStale(now, ref, threshold) {
		return breachCandidate{}, false
	}
	return breachCandidate{
		source:           st.Source,
		accountID:        accountIDOf(st),
		breachType:       repository.BreachTypeSyncStale,
		staleSince:       ref.UTC(),
		thresholdSeconds: int64(threshold.Seconds()),
		details:          fmt.Sprintf("no successful sync for %s (threshold %s)", humanizeAge(now.Sub(ref)), threshold),
	}, true
}

// evalSyncError returns a sync_error candidate for a row in 'error' status
// that satisfies the two-term predicate: error_count >= ErrorMinCount AND
// (ErrorThreshold == 0 OR the stale anchor is older than ErrorThreshold). The
// duration term supplies the persistence floor that error_count alone cannot
// (count increments on every retry, seconds apart).
func evalSyncError(now time.Time, cfg config.StalenessConfig, st repository.SyncState) (breachCandidate, bool) {
	if st.Status != repository.SyncStatusError {
		return breachCandidate{}, false
	}
	if int(st.ErrorCount) < cfg.ErrorMinCount {
		return breachCandidate{}, false
	}
	ref := staleReference(st)
	if cfg.ErrorThreshold > 0 && !isStale(now, ref, cfg.ErrorThreshold) {
		return breachCandidate{}, false
	}
	details := fmt.Sprintf("erroring for %s, %d consecutive errors", humanizeAge(now.Sub(ref)), st.ErrorCount)
	if st.ErrorMessage != nil && *st.ErrorMessage != "" {
		details += ": " + truncate(*st.ErrorMessage, maxBreachDetailLen)
	}
	return breachCandidate{
		source:           st.Source,
		accountID:        accountIDOf(st),
		breachType:       repository.BreachTypeSyncError,
		staleSince:       ref.UTC(),
		thresholdSeconds: int64(cfg.ErrorThreshold.Seconds()),
		details:          details,
	}, true
}

// evaluatePushBreaches: per active host, per source_health entry with
// enabled:true and a non-null last_pushed_at, breach when the last push is
// older than the source's push threshold. Entries are iterated in sorted key
// order so candidate order and logs are deterministic. A host whose
// source_health JSON is malformed is skipped with one WARN (the heartbeat
// check still covers its liveness).
func evaluatePushBreaches(now time.Time, cfg config.StalenessConfig, hosts []*repository.MacHost) []breachCandidate {
	var out []breachCandidate
	for _, h := range hosts {
		entries, ok := parseSourceHealth(h.SourceHealth)
		if !ok {
			logger.Warn().
				Str("host_id", h.ID.String()).
				Msg("sync staleness watchdog: unparseable source_health; skipping push checks for host")
			continue
		}
		sources := make([]string, 0, len(entries))
		for src := range entries {
			sources = append(sources, src)
		}
		sort.Strings(sources)
		for _, src := range sources {
			entry := entries[src]
			if !entry.Enabled {
				continue
			}
			// Absent/null last_pushed_at: can't compute an age; skip (heartbeat
			// covers liveness — known v1 limitation).
			if entry.LastPushedAt == nil {
				continue
			}
			threshold := thresholdForPush(cfg, src)
			if threshold <= 0 {
				continue
			}
			ref := *entry.LastPushedAt
			if !isStale(now, ref, threshold) {
				continue
			}
			out = append(out, breachCandidate{
				source:           src,
				accountID:        h.ID.String(),
				breachType:       repository.BreachTypePushStale,
				staleSince:       ref.UTC(),
				thresholdSeconds: int64(threshold.Seconds()),
				details:          fmt.Sprintf("no push for %s (threshold %s)", humanizeAge(now.Sub(ref)), threshold),
			})
		}
	}
	return out
}

// parseSourceHealth tolerantly decodes a mac_host.source_health JSONB blob
// into per-source entries. Returns (nil, false) on malformed JSON so the
// caller can skip that host without failing the whole run. An empty/absent
// blob is valid (no push sources to check).
func parseSourceHealth(raw json.RawMessage) (map[string]sourceHealthEntry, bool) {
	if len(raw) == 0 {
		return map[string]sourceHealthEntry{}, true
	}
	var entries map[string]sourceHealthEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// staleReference is the COALESCE(last_successful_sync_at, created_at) anchor
// used by both sync_stale and sync_error.
func staleReference(st repository.SyncState) time.Time {
	if st.LastSuccessfulSyncAt != nil {
		return *st.LastSuccessfulSyncAt
	}
	return st.CreatedAt
}

// accountIDOf maps a sync state's nullable account_id to the breach table's
// non-null convention (” = single/none).
func accountIDOf(st repository.SyncState) string {
	if st.AccountID != nil {
		return *st.AccountID
	}
	return ""
}

// thresholdForPull returns the freshness threshold for a pull source: a
// per-source override hit wins, else PullThreshold.
func thresholdForPull(cfg config.StalenessConfig, source string) time.Duration {
	if d, ok := cfg.SourceOverrides[source]; ok {
		return d
	}
	return cfg.PullThreshold
}

// thresholdForPush returns the freshness threshold for a push source: a
// per-source override hit wins, else PushThreshold.
func thresholdForPush(cfg config.StalenessConfig, source string) time.Duration {
	if d, ok := cfg.SourceOverrides[source]; ok {
		return d
	}
	return cfg.PushThreshold
}

// isStale reports whether ref is older than threshold relative to now. The
// boundary is exclusive: age == threshold is NOT stale; age > threshold is.
func isStale(now, ref time.Time, threshold time.Duration) bool {
	return now.Sub(ref) > threshold
}

// humanizeAge renders a duration as a compact "Nd Nh" / "Nh Nm" / "Nm" string
// for breach details. Negative durations (clock skew) render as "0m".
func humanizeAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// truncate clips s to at most n runes, appending an ellipsis when clipped.
func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

package scheduler

// SchedulerTickArgs are the args for the 5-minute periodic job that enumerates
// due sync states and enqueues per-account jobs. See
// .ai/spec/event-bus-foundation.md §3.6 and §3.7.
type SchedulerTickArgs struct{}

// Kind implements river.JobArgs.
func (SchedulerTickArgs) Kind() string { return "scheduler_tick" }

// SyncProviderAccountArgs carry the source and account identifier for a
// single per-account sync run.
//
// The JSON keys below are load-bearing: the repository's
// CountInFlightSyncJobs query (see backend/internal/db/queries/external_sync.sql)
// matches river_job.args->>'source' and args->>'account_id' by these exact
// key names. TestSyncProviderAccountArgs_JSONContract guards them.
//
// AccountID is emitted via `omitempty` because some providers (e.g.,
// single-account sources) do not carry an account identifier. The dedup SQL
// uses COALESCE(args->>'account_id', ”) to treat missing vs empty as the
// same bucket.
type SyncProviderAccountArgs struct {
	Source    string  `json:"source"`
	AccountID *string `json:"account_id,omitempty"`
}

// Kind implements river.JobArgs. The literal string 'sync_provider_account'
// is matched by CountInFlightSyncJobs; keep them in lockstep.
func (SyncProviderAccountArgs) Kind() string { return "sync_provider_account" }

// PairingTokenJanitorArgs are the args for the periodic
// PairingTokenJanitorWorker. The worker holds no state; the args type
// is intentionally empty so the periodic-job scheduler can insert it
// without per-tick parameter resolution.
type PairingTokenJanitorArgs struct{}

// Kind implements river.JobArgs.
func (PairingTokenJanitorArgs) Kind() string { return "pairing_token_janitor" }

// StalenessWatchdogArgs are the args for the periodic
// StalenessWatchdogWorker. Stateless like the janitor — the worker computes
// the whole breach set from current DB state each tick, so no per-tick
// parameters are resolved.
type StalenessWatchdogArgs struct{}

// Kind implements river.JobArgs.
func (StalenessWatchdogArgs) Kind() string { return "sync_staleness_watchdog" }

// AssertionRolloverArgs are the args for the daily periodic
// AssertionRolloverWorker. Stateless — the worker terminalizes the whole
// due-bounded-successor set from current DB state each tick (a catch-up sweep),
// so no per-tick parameters are resolved.
type AssertionRolloverArgs struct{}

// Kind implements river.JobArgs.
func (AssertionRolloverArgs) Kind() string { return "assertion_rollover" }

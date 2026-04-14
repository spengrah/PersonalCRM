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

package jobsample

// TrimArgs are the args for the periodic job_sample_trim worker. The worker
// holds no per-tick state; the args type is intentionally empty so the
// periodic-job scheduler can insert it without per-tick parameter resolution.
type TrimArgs struct{}

// Kind implements river.JobArgs.
func (TrimArgs) Kind() string { return "job_sample_trim" }

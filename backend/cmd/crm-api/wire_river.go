package main

import (
	"github.com/riverqueue/river"
)

// riverRegistrar is a thin recording delegate over River's worker bundle
// and periodic-job bundle. River's public API does not enumerate
// registered workers or periodic jobs (river.Workers and PeriodicJobBundle
// are opaque; the startup log prints only concurrency), so the wire
// functions register through this delegate instead of calling
// river.AddWorker / PeriodicJobs().Add directly. Each registration is
// recorded as a kind string, which the golden-list test reads back to pin
// the worker/periodic registration set per config shape.
//
// The delegation is behavior-identical to the direct calls: addWorker does
// exactly what river.AddWorker does plus one slice append of the args
// type's Kind() (a side-effect-free value method on every args type), and
// addPeriodic does exactly what PeriodicJobs().Add does plus one append.
//
// Two-phase construction: the registrar is built around riverWorkers
// BEFORE river.NewClient (so the noopWorker registration is recorded), and
// its periodic field is attached immediately after river.NewClient returns
// (reg.periodic = riverClient.PeriodicJobs()). Every addPeriodic call
// happens later in the wire chain, so the late attach is safe by
// construction; the nil-check in addPeriodic guards misuse loudly.
type riverRegistrar struct {
	workers  *river.Workers
	periodic *river.PeriodicJobBundle

	workerKinds   []string
	periodicKinds []string
}

// newRiverRegistrar builds a registrar around an existing worker bundle.
// The periodic bundle is attached later (after river.NewClient).
func newRiverRegistrar(workers *river.Workers) *riverRegistrar {
	return &riverRegistrar{workers: workers}
}

// addWorker registers a worker on the underlying bundle and records its
// job-args Kind(). Pure delegation to river.AddWorker plus one append.
func addWorker[T river.JobArgs](r *riverRegistrar, w river.Worker[T]) {
	river.AddWorker(r.workers, w)
	var t T
	r.workerKinds = append(r.workerKinds, t.Kind())
}

// addPeriodic registers a periodic job on the attached bundle and records
// its kind. The kind is the args type's Kind() at the call site
// (source-locked to the job it registers). Panics if called before the
// periodic bundle is attached (a wiring-order bug, never reachable in the
// current construction order).
func (r *riverRegistrar) addPeriodic(kind string, job *river.PeriodicJob) {
	if r.periodic == nil {
		panic("riverRegistrar: addPeriodic before periodic bundle attached")
	}
	r.periodic.Add(job)
	r.periodicKinds = append(r.periodicKinds, kind)
}

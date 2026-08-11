package main

import (
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/riverqueue/river"
)

// macHostStack holds the mac-host repo, pool-wired sync repo, and handler
// consumed downstream by route registration + the staleness watchdog.
type macHostStack struct {
	Repo     *repository.MacHostRepository
	SyncRepo *repository.SyncRepository
	Handler  *handlers.MacHostHandler
}

// buildMacHost wires the Mac-daemon host management (pairing flow,
// heartbeat, cursor protocol, admin revoke) and registers the
// pairing-token janitor worker + its periodic job.
func buildMacHost(reg *riverRegistrar, database *db.Database, core coreRepos, ingest ingestRepos) macHostStack {
	contactMethodRepo := core.ContactMethod
	externalContactRepoForIngest := ingest.ExternalContact
	meetingNoteRepoForIngest := ingest.MeetingNote

	// macHostRepoForIngest was constructed earlier so the IngestService
	// could take it as a HostLivenessChecker dep. Reuse the same instance
	// here so the host-management service shares the same repository wrapper.
	macHostRepo := ingest.MacHost
	pairingTokenRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	// Mac cursor commit needs a tx — use the pool-wired SyncRepository.
	macSyncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macHostService := service.NewMacHostService(
		macHostRepo,
		pairingTokenRepo,
		macSyncRepo,
		contactMethodRepo,
		externalContactRepoForIngest, // /known-ids reader (external_contact)
		meetingNoteRepoForIngest,     // /known-ids reader (anarlog_sessions)
		database.Pool,
		0, // default bcrypt cost
	)
	pairingIPLimiter := auth.NewPairingIPRateLimiter()
	macHostHandler := handlers.NewMacHostHandler(macHostService, macHostRepo, pairingIPLimiter)

	// Register the pairing-token janitor periodic job (5 min). Worker
	// registered unconditionally; the periodic-job inserter triggers it
	// on the same River client. See
	// backend/internal/scheduler/pairing_token_janitor_worker.go.
	addWorker(reg, scheduler.NewPairingTokenJanitorWorker(pairingTokenRepo))
	reg.addPeriodic(scheduler.PairingTokenJanitorArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.PairingTokenJanitorArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	return macHostStack{
		Repo:     macHostRepo,
		SyncRepo: macSyncRepo,
		Handler:  macHostHandler,
	}
}

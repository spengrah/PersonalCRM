package main

import (
	"context"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/riverqueue/river"
)

// whatsappStack holds the WhatsApp manager + handler + the default history
// recorder. A zero whatsappStack (returned when WhatsApp sync is disabled, or
// when the device store could not be opened) is exactly a nil manager / nil
// handler, so nothing downstream needs a second flag check.
type whatsappStack struct {
	Manager *wapkg.Manager
	Handler *handlers.WhatsAppHandler
	// Recorder is the default history-notification recorder, built over the
	// same WhatsAppRepository the manager got. Keeping it on the stack gives
	// the recorder exactly one construction site.
	Recorder wapkg.HistoryNotificationRecorder
}

// whatsappPrereqs is the readiness gate expressed as a struct the caller must
// fill in.
//
// Start is the SOLE activation point, and the wiring calls it exactly once,
// textually after the setter block — so a later change that adds a prerequisite
// cannot forget to activate the integration: it fills a field in a struct it
// must already touch. A readiness transition never auto-connects; making a
// setter trigger the connect would spread activation across three call sites and
// give Start's carefully-ordered gate a second, undisciplined entrance.
type whatsappPrereqs struct {
	// Ingestor stores incoming messages. Nil until the ingest work lands.
	Ingestor wapkg.MessageIngestor
	// Recorder captures history-sync notifications durably. There is no default:
	// a no-op here is silent, unrecoverable history loss.
	Recorder wapkg.HistoryNotificationRecorder
	// DrainReady reports that the history drain worker is registered.
	DrainReady bool
}

// whatsappManagerSeam is the slice of *wapkg.Manager the activation sequence
// drives. It exists so the ORDER — every setter, then Start, once — is
// observable rather than merely intended.
type whatsappManagerSeam interface {
	SetIngestor(wapkg.MessageIngestor)
	SetHistoryRecorder(wapkg.HistoryNotificationRecorder)
	SetHistoryDrainReady()
	Start(context.Context) error
}

// newWhatsAppStack constructs the WhatsApp device store, manager, handler and
// default history recorder. It installs NO prerequisite and does NOT start the
// manager — activation is wireWhatsApp's job, textually after the drain worker
// is registered, because the readiness gate reads that registration.
//
// It does NOT register the Stop defer either: the caller (run()) owns that, so
// it fires on run() return rather than when this function returns.
//
// Nothing here is fatal to the process: a WhatsApp failure leaves the rest of
// the CRM running, exactly as a Telegram failure does.
func newWhatsAppStack(ctx context.Context, cfg *config.Config, database *db.Database) whatsappStack {
	waLog := wapkg.NewWALogger("whatsapp")

	container, err := wapkg.NewDeviceContainer(ctx, database.Pool, waLog)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to open device store; integration disabled for this boot")
		return whatsappStack{}
	}

	syncRepo := repository.NewSyncRepository(database.Queries)
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)

	manager := wapkg.NewManager(container, waLog, &cfg.WhatsApp, syncRepo, whatsappRepo)

	// The chat-settings service is deliberately NOT part of the manager: it
	// reads and writes ordinary gate rows, which the actor neither owns nor
	// serialises.
	chatSettings := wapkg.NewChatSettingsService(whatsappRepo, cfg.WhatsApp.GroupMaxMembers)

	logger.Info().Msg("WhatsApp integration initialized")

	return whatsappStack{
		Manager:  manager,
		Handler:  handlers.NewWhatsAppHandler(manager, chatSettings),
		Recorder: wapkg.NewHistoryRecorder(whatsappRepo),
	}
}

// registerWhatsAppHistoryDrain registers the history drain worker and its
// 1-minute periodic, and reports whether it did — which is what
// whatsappPrereqs.DrainReady is set from, so the readiness flag can never claim
// a worker that was not registered.
//
// The feature gate lives INSIDE the function, mirroring registerSyncScheduler,
// so the golden wire chain can call it unconditionally and its config-shape pin
// is not vacuous. Registration deliberately does not depend on a manager or an
// ingestor existing: a config shape's registered-kind list must not vary with
// device-store availability. The drainer normalizes a nil fetcher source into
// one that defers, which is exactly the boot where the device store failed.
func registerWhatsAppHistoryDrain(
	reg *riverRegistrar,
	cfg *config.Config,
	database *db.Database,
	ingestor wapkg.MessageIngestor,
	gate *wapkg.ChatGate,
	fetcherSource func() wapkg.HistoryFetcher,
) bool {
	if !cfg.Features.EnableWhatsAppSync {
		return false
	}

	drainer := wapkg.NewHistoryDrainer(repository.NewWhatsAppRepository(database.Queries), ingestor, gate, fetcherSource)
	addWorker(reg, wapkg.NewHistoryDrainWorker(drainer))
	reg.addPeriodic(consumerjobs.WhatsAppHistoryDrainArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(1*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return consumerjobs.WhatsAppHistoryDrainArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))
	return true
}

// wireWhatsApp is the whole WhatsApp composition, extracted from run() so the
// line that TURNS THE FEATURE ON has a call site a test can drive. It is a
// no-op returning a zero stack when the feature is off.
//
// The ingestor is a PARAMETER rather than built here: run() declares it outside
// the gated block because the post-import hook wiring further down references
// the same instance, so moving its construction in would break that hook.
func wireWhatsApp(
	ctx context.Context,
	cfg *config.Config,
	database *db.Database,
	reg *riverRegistrar,
	waIngestor *wapkg.Ingestor,
) whatsappStack {
	if !cfg.Features.EnableWhatsAppSync {
		return whatsappStack{}
	}

	stk := newWhatsAppStack(ctx, cfg, database)

	prereqs := whatsappPrereqs{Recorder: stk.Recorder}
	// The nil check is load-bearing: assigning a nil *Ingestor into the
	// interface field would produce a non-nil interface holding a nil pointer,
	// which the readiness gate would read as "wired".
	var gate *wapkg.ChatGate
	if waIngestor != nil {
		prereqs.Ingestor = waIngestor
		// The SAME gate instance the ingest path uses: Manager.SetIngestor
		// binds the group-info source on that instance and only that one.
		gate = waIngestor.ChatGate()
	}
	// Left nil when the device store failed to open. That is the case
	// NewHistoryDrainer normalizes, not something this function papers over.
	var fetcherSource func() wapkg.HistoryFetcher
	if stk.Manager != nil {
		fetcherSource = stk.Manager.HistoryFetcher
	}

	prereqs.DrainReady = registerWhatsAppHistoryDrain(reg, cfg, database, prereqs.Ingestor, gate, fetcherSource)

	if stk.Manager != nil {
		activateWhatsApp(ctx, stk.Manager, prereqs)
	}
	return stk
}

// buildWhatsAppIngestor constructs the live-message ingest path.
//
// It is built by run() rather than inside wireWhatsApp because its
// dependencies — the identity and enrichment services — are composed earlier
// than the WhatsApp stack, AND because the post-import hook wiring further down
// run() references the same instance. It reaches the manager through
// whatsappPrereqs.Ingestor, the readiness field it satisfies; its manager seam
// (the group-info source) is bound by Manager.SetIngestor, before Start.
//
// The WhatsAppRepository built here is a stateless wrapper over the shared
// db.Querier, so the second instance newWhatsAppStack constructs for the
// manager is the same thing; there is no state to share.
//
// engine is the SAME aggregation engine instance buildAggregationEngines wires
// in — built earlier, by buildWhatsAppAggregationEngine, so the matcher's
// post-link aggregation and the periodic sweep share one engine rather than
// two. It is never nil in production: the caller builds it unconditionally.
func buildWhatsAppIngestor(
	cfg *config.Config,
	database *db.Database,
	commsMessageRepo *repository.CommsMessageRepository,
	identityService *service.IdentityService,
	externalContactRepo *repository.ExternalContactRepository,
	enricher *service.EnrichmentService,
	engine *aggregation.Engine,
) *wapkg.Ingestor {
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)
	gate := wapkg.NewChatGate(whatsappRepo, cfg.WhatsApp.GroupMaxMembers)
	matcher := wapkg.NewPeerMatcher(
		identityService,
		commsMessageRepo,
		externalContactRepo,
		enricher,
		engine,
		cfg.WhatsApp.DiscoveryMinMessages,
	)
	return wapkg.NewIngestor(commsMessageRepo, gate, matcher)
}

// activateWhatsApp installs every prerequisite that is present and THEN calls
// Start, exactly once. It is the only Start call site in the binary.
func activateWhatsApp(ctx context.Context, manager whatsappManagerSeam, prereqs whatsappPrereqs) {
	if prereqs.Ingestor != nil {
		manager.SetIngestor(prereqs.Ingestor)
	}
	if prereqs.Recorder != nil {
		manager.SetHistoryRecorder(prereqs.Recorder)
	}
	if prereqs.DrainReady {
		manager.SetHistoryDrainReady()
	}

	if err := manager.Start(ctx); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to start")
	}
}

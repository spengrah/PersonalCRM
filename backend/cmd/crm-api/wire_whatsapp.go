package main

import (
	"context"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	wapkg "personal-crm/backend/internal/whatsapp"
)

// whatsappStack holds the WhatsApp manager + handler. A zero whatsappStack
// (returned when WhatsApp sync is disabled) is exactly a nil manager / nil
// handler, so nothing downstream needs a second flag check.
type whatsappStack struct {
	Manager *wapkg.Manager
	Handler *handlers.WhatsAppHandler
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

// buildWhatsApp constructs the WhatsApp device store, manager and handler,
// installs whichever prerequisites are present, and starts the manager. It does
// NOT register the Stop defer — the caller (run()) owns that, so it fires on
// run() return rather than when this function returns.
//
// Nothing here is fatal to the process: a WhatsApp failure leaves the rest of
// the CRM running, exactly as a Telegram failure does.
func buildWhatsApp(ctx context.Context, cfg *config.Config, database *db.Database, prereqs whatsappPrereqs) whatsappStack {
	waLog := wapkg.NewWALogger("whatsapp")

	container, err := wapkg.NewDeviceContainer(ctx, database.Pool, waLog)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to open device store; integration disabled for this boot")
		return whatsappStack{}
	}

	syncRepo := repository.NewSyncRepository(database.Queries)
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)

	manager := wapkg.NewManager(container, waLog, &cfg.WhatsApp, syncRepo, whatsappRepo)

	if prereqs.Recorder == nil {
		prereqs.Recorder = wapkg.NewHistoryRecorder(whatsappRepo)
	}
	activateWhatsApp(ctx, manager, prereqs)

	// NOTE: manager.Stop() is deferred by the caller (run()) as a nil-guarded
	// defer — a defer here would fire when THIS function returns, stopping the
	// client at boot.

	logger.Info().Msg("WhatsApp integration initialized")

	return whatsappStack{
		Manager: manager,
		Handler: handlers.NewWhatsAppHandler(manager),
	}
}

// buildWhatsAppIngestor constructs the live-message ingest path.
//
// It is built by the caller rather than inside buildWhatsApp because its
// dependencies — the identity and enrichment services — are composed earlier
// than the WhatsApp stack, and it reaches the manager through
// whatsappPrereqs.Ingestor, the readiness field it satisfies. Its manager seam
// (the group-info source) is bound by Manager.SetIngestor, before Start.
//
// The WhatsAppRepository built here is a stateless wrapper over the shared
// db.Querier, so the second instance buildWhatsApp constructs for the manager is
// the same thing; there is no state to share.
func buildWhatsAppIngestor(
	cfg *config.Config,
	database *db.Database,
	commsMessageRepo *repository.CommsMessageRepository,
	identityService *service.IdentityService,
	externalContactRepo *repository.ExternalContactRepository,
	enricher *service.EnrichmentService,
) *wapkg.Ingestor {
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)
	gate := wapkg.NewChatGate(whatsappRepo, cfg.WhatsApp.GroupMaxMembers)
	matcher := wapkg.NewPeerMatcher(
		identityService,
		commsMessageRepo,
		externalContactRepo,
		enricher,
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

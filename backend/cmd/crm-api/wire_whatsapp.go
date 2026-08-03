package main

import (
	"context"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	wapkg "personal-crm/backend/internal/whatsapp"
)

// whatsappStack holds the WhatsApp manager + handler. A zero whatsappStack
// (returned when WhatsApp sync is disabled) is exactly a nil manager / nil
// handler, so nothing downstream needs a second flag check.
type whatsappStack struct {
	Manager *wapkg.Manager
	Handler *handlers.WhatsAppHandler
}

// buildWhatsApp constructs the WhatsApp device store, manager and handler, and
// starts the manager. It does NOT register the Stop defer — the caller (run())
// owns that, so it fires on run() return rather than when this function
// returns.
//
// Nothing here is fatal to the process: a WhatsApp failure leaves the rest of
// the CRM running, exactly as a Telegram failure does.
func buildWhatsApp(ctx context.Context, cfg *config.Config, database *db.Database) whatsappStack {
	waLog := wapkg.NewWALogger("whatsapp")

	container, err := wapkg.NewDeviceContainer(ctx, database.Pool, waLog)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to open device store; integration disabled for this boot")
		return whatsappStack{}
	}

	syncRepo := repository.NewSyncRepository(database.Queries)
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)

	manager := wapkg.NewManager(container, waLog, &cfg.WhatsApp, syncRepo, whatsappRepo)

	// The recorder is installed BEFORE Start, because Start refuses to connect
	// without it. A wiring omission therefore fails loudly at boot ("not
	// ready") rather than silently discarding one-shot history.
	manager.SetHistoryRecorder(wapkg.NewHistoryRecorder(whatsappRepo))

	if err := manager.Start(ctx); err != nil {
		logger.Warn().Err(err).Msg("whatsapp: failed to start")
	}
	// NOTE: manager.Stop() is deferred by the caller (run()) as a nil-guarded
	// defer — a defer here would fire when THIS function returns, stopping the
	// client at boot.

	logger.Info().Msg("WhatsApp integration initialized")

	return whatsappStack{
		Manager: manager,
		Handler: handlers.NewWhatsAppHandler(manager),
	}
}

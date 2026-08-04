package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedUnmatchedWhatsAppPeer stages two unmatched rows for a peer whose recovered
// phone number is e164, mirroring what the ingest path writes for a peer with no
// contact yet.
func (e *whatsappTestEnv) seedUnmatchedWhatsAppPeer(t *testing.T, chatJID, e164, prefix string) {
	t.Helper()
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)
	for i, at := range []time.Time{base, base.Add(20 * time.Minute)} {
		body := "whatsapp body"
		peer := chatJID
		normalized := e164
		_, err := e.commsRepo.UpsertChatMessage(e.ctx, repository.UpsertChatMessageParams{
			Source:         repository.InteractionSourceWhatsApp,
			ExternalID:     prefix + string(rune('a'+i)),
			ThreadID:       chatJID,
			Body:           &body,
			PeerHandle:     &peer,
			PeerNormalized: &normalized,
			Direction:      repository.InteractionDirectionInbound,
			SentAt:         at,
		})
		require.NoError(t, err)
	}
}

// whatsappPeerFixture is the (chat JID, normalized phone) pair a synthetic
// contact's phone number produces on the ingest path.
func whatsappPeerFixture(t *testing.T, rawPhone string) (chatJID, e164 string) {
	t.Helper()
	e164 = identity.Normalize(rawPhone, identity.IdentifierTypeWhatsApp)
	require.NotEmpty(t, e164)
	require.Equal(t, "+", e164[:1], "the normalized WhatsApp identifier is E.164")
	return e164[1:] + "@s.whatsapp.net", e164
}

// spec: WHA-042.attach-by-recovered-number
// spec: WHA-042.interactions-appear-without-waiting-for-a-sweep
func TestWhatsAppRematch_OnPhoneMethodAdded(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	e.commsRepo.SetPool(e.database.Pool)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Rematch Phone "+suffix)
	chatJID, e164 := whatsappPeerFixture(t, "+1-204-555-0142")
	e.seedUnmatchedWhatsAppPeer(t, chatJID, e164, "wa-rm-phone-"+suffix+"-")

	handler := wapkg.NewPhoneRematchHandler(e.commsRepo, e.engine)
	require.Equal(t, "phone", handler.IdentifierType())

	n, err := handler.Rematch(e.ctx, contact.ID, e164)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both staged rows for that number must attach")

	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, repository.InteractionSourceWhatsApp, interactions[0].Source)
}

// spec: WHA-042.attach-by-recovered-number
func TestWhatsAppRematch_OnWhatsAppMethodAdded(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	e.commsRepo.SetPool(e.database.Pool)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Rematch Method "+suffix)
	chatJID, e164 := whatsappPeerFixture(t, "+1-204-555-0143")
	e.seedUnmatchedWhatsAppPeer(t, chatJID, e164, "wa-rm-wa-"+suffix+"-")

	handler := wapkg.NewWhatsAppMethodRematchHandler(e.commsRepo, e.engine)
	require.Equal(t, "whatsapp", handler.IdentifierType())

	n, err := handler.Rematch(e.ctx, contact.ID, e164)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, repository.InteractionSourceWhatsApp, interactions[0].Source)
}

// TestWhatsAppRematch_TelegramPhoneHandlerUnaffected is R5.4's guard. Adding a
// WhatsApp handler for the 'phone' type means TWO handlers now run for a phone
// method; RematchService fans out to every one and FAILS FAST on the first
// error, so a WhatsApp attach failure would stop Telegram's handler running in
// the same pass. This asserts both run and neither errors.
func TestWhatsAppRematch_TelegramPhoneHandlerUnaffected(t *testing.T) {
	t.Parallel()
	e := setupWhatsAppEngineTest(t)
	e.commsRepo.SetPool(e.database.Pool)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newWhatsAppContact(t, "WhatsApp Rematch Coexist "+suffix)
	chatJID, e164 := whatsappPeerFixture(t, "+1-204-555-0144")
	e.seedUnmatchedWhatsAppPeer(t, chatJID, e164, "wa-rm-coexist-"+suffix+"-")

	telegramRepo := repository.NewTelegramMessageRepository(e.database.Queries)
	identitySvc := service.NewIdentityService(repository.NewIdentityRepository(e.database.Queries))
	telegramMatcher := tgpkg.NewPeerMatcher(
		identitySvc, telegramRepo, repository.NewExternalContactRepository(e.database.Queries), nil, 3,
	)
	telegramEngine := tgpkg.NewAggregationEngine(
		2, 48, telegramRepo, e.interactionRepo, nil, nil, nil, e.database.Pool, nil,
	)

	svc := service.NewRematchService()
	// Registration order mirrors the composition root: telegram (main.go builds
	// it first) then whatsapp.
	svc.Register(tgpkg.NewPhoneRematchHandler(telegramRepo, telegramMatcher, telegramEngine))
	svc.Register(wapkg.NewPhoneRematchHandler(e.commsRepo, e.engine))

	require.Len(t, svc.EligibleMethods([]service.Method{{Type: "phone", Value: e164}}), 1)

	err := svc.Run(e.ctx, uuid.New(), contact.ID, []service.Method{{Type: "phone", Value: e164}})
	require.NoError(t, err, "neither phone handler may error when both are registered")

	// The WhatsApp handler did its own work: the staged rows became an interaction.
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, repository.InteractionSourceWhatsApp, interactions[0].Source)
}

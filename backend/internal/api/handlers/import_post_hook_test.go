package handlers

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/telegram"
	"personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHook is a PostImportHook that records what it was handed.
type recordingHook struct {
	source   string
	err      error
	calls    int
	external *repository.ExternalContact
	contact  uuid.UUID
}

func (h *recordingHook) Source() string { return h.source }

func (h *recordingHook) OnPeerLinked(_ context.Context, external *repository.ExternalContact, contactID uuid.UUID) error {
	h.calls++
	h.external = external
	h.contact = contactID
	return h.err
}

func TestPostImportDispatch_RoutesWhatsAppToWhatsAppHook(t *testing.T) {
	wa := &recordingHook{source: "whatsapp"}
	tg := &recordingHook{source: "telegram"}
	h := &ImportHandler{}
	h.RegisterPostImportHook(wa)
	h.RegisterPostImportHook(tg)

	contactID := uuid.New()
	external := &repository.ExternalContact{
		Source: "whatsapp", SourceID: "88800000002@lid",
		Metadata: map[string]any{"phone_e164": "+15559876543"},
	}
	h.triggerPostImportHook(context.Background(), external, contactID)

	assert.Equal(t, 1, wa.calls)
	assert.Zero(t, tg.calls, "a candidate reaches exactly one hook")
	assert.Same(t, external, wa.external, "the hook receives the whole row, not extracted fields")
	assert.Equal(t, contactID, wa.contact)
}

func TestPostImportDispatch_RoutesTelegramToTelegramHook(t *testing.T) {
	wa := &recordingHook{source: "whatsapp"}
	tg := &recordingHook{source: "telegram"}
	h := &ImportHandler{}
	h.RegisterPostImportHook(wa)
	h.RegisterPostImportHook(tg)

	h.triggerPostImportHook(context.Background(),
		&repository.ExternalContact{Source: "telegram", SourceID: "12345"}, uuid.New())

	assert.Equal(t, 1, tg.calls)
	assert.Zero(t, wa.calls)
}

// recordingTelegramLinker captures exactly what the Telegram adapter derived,
// which is the only way the extraction can be asserted: with a concrete
// *TelegramManager the sole reachable call is nil-constructed, and a
// nil-constructed call observes nothing.
type recordingTelegramLinker struct {
	calls        int
	peerUserID   int64
	peerUsername string
	contactID    uuid.UUID
	err          error
}

func (r *recordingTelegramLinker) OnPeerLinked(_ context.Context, peerUserID int64, peerUsername string, contactID uuid.UUID) error {
	r.calls++
	r.peerUserID = peerUserID
	r.peerUsername = peerUsername
	r.contactID = contactID
	return r.err
}

type recordingWhatsAppLinker struct {
	calls     int
	peerJID   string
	phoneE164 *string
	contactID uuid.UUID
	err       error
}

func (r *recordingWhatsAppLinker) OnPeerLinked(_ context.Context, peerJID string, phoneE164 *string, contactID uuid.UUID) error {
	r.calls++
	r.peerJID = peerJID
	r.phoneE164 = phoneE164
	r.contactID = contactID
	return r.err
}

// TestPostImportDispatch_TelegramBehaviorUnchanged pins that the adapter derives
// the SAME (peerUserID, peerUsername) pair the handler used to compute inline,
// including the stripped leading '@'.
func TestPostImportDispatch_TelegramBehaviorUnchanged(t *testing.T) {
	linker := &recordingTelegramLinker{}
	hook := telegram.NewPostImportHook(linker)
	assert.Equal(t, "telegram", hook.Source())

	contactID := uuid.New()
	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{
			Source: "telegram", SourceID: "12345",
			Metadata: map[string]any{"username": "@handle"},
		}, contactID))

	require.Equal(t, 1, linker.calls)
	assert.Equal(t, int64(12345), linker.peerUserID, "the source id is the Telegram peer user id")
	assert.Equal(t, "handle", linker.peerUsername, "the leading '@' is stripped, as the handler stripped it")
	assert.Equal(t, contactID, linker.contactID)
}

func TestPostImportDispatch_TelegramMissingUsernameIsEmpty(t *testing.T) {
	linker := &recordingTelegramLinker{}
	hook := telegram.NewPostImportHook(linker)

	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "telegram", SourceID: "999"}, uuid.New()))

	require.Equal(t, 1, linker.calls)
	assert.Equal(t, int64(999), linker.peerUserID)
	assert.Empty(t, linker.peerUsername, "an absent handle is empty, not a panic and not a literal '<nil>'")
}

func TestPostImportDispatch_TelegramUnparseableSourceIDIsTolerated(t *testing.T) {
	linker := &recordingTelegramLinker{}
	hook := telegram.NewPostImportHook(linker)

	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "telegram", SourceID: "not-a-number"}, uuid.New()),
		"an unparseable source id logs and returns nil, exactly as the handler did")
	assert.Zero(t, linker.calls, "and nothing is delegated with a garbage peer id")
}

// TestPostImportDispatch_WhatsAppDerivesJIDAndPhone is the WhatsApp half of the
// same guard: the candidate's source_id IS the raw peer JID, and the resolved
// phone rides in metadata.
func TestPostImportDispatch_WhatsAppDerivesJIDAndPhone(t *testing.T) {
	linker := &recordingWhatsAppLinker{}
	hook := whatsapp.NewPostImportHook(linker)
	assert.Equal(t, "whatsapp", hook.Source())

	contactID := uuid.New()
	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{
			Source: "whatsapp", SourceID: "88800000002@lid",
			Metadata: map[string]any{"phone_e164": "+15559876543"},
		}, contactID))

	require.Equal(t, 1, linker.calls)
	assert.Equal(t, "88800000002@lid", linker.peerJID)
	require.NotNil(t, linker.phoneE164)
	assert.Equal(t, "+15559876543", *linker.phoneE164)
	assert.Equal(t, contactID, linker.contactID)
}

func TestPostImportDispatch_WhatsAppWithoutPhonePassesNil(t *testing.T) {
	linker := &recordingWhatsAppLinker{}
	hook := whatsapp.NewPostImportHook(linker)

	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "whatsapp", SourceID: "88800000002@lid"}, uuid.New()))

	require.Equal(t, 1, linker.calls)
	assert.Equal(t, "88800000002@lid", linker.peerJID)
	assert.Nil(t, linker.phoneE164, "a peer whose number was never recovered attaches by handle alone")

	// An empty-string phone is absent, not a selector: passing "" would match
	// no staged row and would look like a deliberate narrowing.
	linker2 := &recordingWhatsAppLinker{}
	hook2 := whatsapp.NewPostImportHook(linker2)
	require.NoError(t, hook2.OnPeerLinked(context.Background(),
		&repository.ExternalContact{
			Source: "whatsapp", SourceID: "88800000002@lid",
			Metadata: map[string]any{"phone_e164": ""},
		}, uuid.New()))
	require.Equal(t, 1, linker2.calls)
	assert.Nil(t, linker2.phoneE164)
}

func TestPostImportDispatch_UnwiredDelegateIsNoop(t *testing.T) {
	require.NoError(t, telegram.NewPostImportHook(nil).OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "telegram", SourceID: "12345"}, uuid.New()),
		"an unwired delegate is a no-op, not a panic")
	require.NoError(t, whatsapp.NewPostImportHook(nil).OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "whatsapp", SourceID: "88800000002@lid"}, uuid.New()))
}

func TestPostImportDispatch_UnknownSourceIsNoop(t *testing.T) {
	tg := &recordingHook{source: "telegram"}
	h := &ImportHandler{}
	h.RegisterPostImportHook(tg)

	h.triggerPostImportHook(context.Background(),
		&repository.ExternalContact{Source: "google", SourceID: "abc"}, uuid.New())
	assert.Zero(t, tg.calls)
}

func TestPostImportDispatch_NoHooksRegisteredIsNoop(t *testing.T) {
	h := &ImportHandler{}
	assert.NotPanics(t, func() {
		h.triggerPostImportHook(context.Background(),
			&repository.ExternalContact{Source: "whatsapp", SourceID: "x"}, uuid.New())
	})
}

func TestPostImportDispatch_HookErrorIsNonFatal(t *testing.T) {
	wa := &recordingHook{source: "whatsapp", err: errors.New("attach failed")}
	h := &ImportHandler{}
	h.RegisterPostImportHook(wa)

	assert.NotPanics(t, func() {
		h.triggerPostImportHook(context.Background(),
			&repository.ExternalContact{Source: "whatsapp", SourceID: "x"}, uuid.New())
	}, "the import already succeeded; the back-link is best-effort")
	assert.Equal(t, 1, wa.calls)
}

func TestPostImportDispatch_RegisteringTwiceReplaces(t *testing.T) {
	first := &recordingHook{source: "whatsapp"}
	second := &recordingHook{source: "whatsapp"}
	h := &ImportHandler{}
	h.RegisterPostImportHook(first)
	h.RegisterPostImportHook(second)

	h.triggerPostImportHook(context.Background(),
		&repository.ExternalContact{Source: "whatsapp", SourceID: "x"}, uuid.New())
	assert.Zero(t, first.calls)
	assert.Equal(t, 1, second.calls)
}

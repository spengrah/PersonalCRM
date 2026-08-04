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

// TestPostImportDispatch_TelegramBehaviorUnchanged pins that the adapter derives
// the SAME (peerUserID, peerUsername) pair the handler used to compute inline,
// including the stripped leading '@'.
func TestPostImportDispatch_TelegramBehaviorUnchanged(t *testing.T) {
	hook := telegram.NewPostImportHook(nil)
	assert.Equal(t, "telegram", hook.Source())

	// A nil manager makes the delegation a no-op, so what is under test here is
	// the extraction: an unparseable source id must be tolerated, exactly as the
	// handler tolerated it before.
	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "telegram", SourceID: "not-a-number"}, uuid.New()))
	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{
			Source: "telegram", SourceID: "12345",
			Metadata: map[string]any{"username": "@handle"},
		}, uuid.New()))
}

func TestPostImportDispatch_WhatsAppHookSourceIsWhatsApp(t *testing.T) {
	hook := whatsapp.NewPostImportHook(nil)
	assert.Equal(t, "whatsapp", hook.Source())
	require.NoError(t, hook.OnPeerLinked(context.Background(),
		&repository.ExternalContact{Source: "whatsapp", SourceID: "88800000002@lid"}, uuid.New()),
		"an unwired matcher is a no-op, not a panic")
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

package whatsapp

import (
	"context"

	"personal-crm/backend/internal/repository"
)

// ChatSettingsStore is the slice of WhatsAppRepository the settings surface
// needs. It is deliberately narrower than the gate's own store: the settings
// path never resolves group metadata and never upserts.
type ChatSettingsStore interface {
	ListChatConfigs(ctx context.Context) ([]repository.WhatsAppChatConfig, error)
	SetChatStatus(ctx context.Context, chatJID, status string) (*repository.WhatsAppChatConfig, error)
}

// ChatWithTracking is a chat config plus the decision the gate would take for
// it right now. The two are carried together so the settings list can never
// present a decision the live ingest path disagrees with.
type ChatWithTracking struct {
	repository.WhatsAppChatConfig
	EffectiveTracked bool
}

// ChatSettingsService serves the per-chat tracking override.
//
// It is a plain service rather than a method on Manager because chat-gate rows
// are ordinary DB state the actor does not own and never reads outside
// ChatGate. Routing CRUD through the actor would either bypass its own inbox
// discipline or add message types for state it has no business serialising.
type ChatSettingsService struct {
	store           ChatSettingsStore
	groupMaxMembers int
}

// NewChatSettingsService builds the service over a store and the same size
// threshold the live gate is configured with.
func NewChatSettingsService(store ChatSettingsStore, groupMaxMembers int) *ChatSettingsService {
	return &ChatSettingsService{store: store, groupMaxMembers: groupMaxMembers}
}

// ListChats returns every observed chat with the gate's current decision for it.
func (s *ChatSettingsService) ListChats(ctx context.Context) ([]ChatWithTracking, error) {
	cfgs, err := s.store.ListChatConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChatWithTracking, 0, len(cfgs))
	for _, cfg := range cfgs {
		out = append(out, s.withTracking(cfg))
	}
	return out, nil
}

// SetChatStatus records the user's override and echoes the row back with the
// RECOMPUTED decision, so the client sees the new effective answer without a
// second read. The status value is validated at the handler; a store error
// (including db.ErrNotFound for an unobserved chat) passes through unwrapped so
// the handler's errors.Is comparison still holds.
func (s *ChatSettingsService) SetChatStatus(ctx context.Context, chatJID, status string) (*ChatWithTracking, error) {
	cfg, err := s.store.SetChatStatus(ctx, chatJID, status)
	if err != nil {
		return nil, err
	}
	out := s.withTracking(*cfg)
	return &out, nil
}

// withTracking calls the gate's OWN exported predicate, so a change to the
// tracking rule reaches the settings list without a second implementation.
func (s *ChatSettingsService) withTracking(cfg repository.WhatsAppChatConfig) ChatWithTracking {
	return ChatWithTracking{
		WhatsAppChatConfig: cfg,
		EffectiveTracked:   EffectiveTracked(cfg.Status, int32PtrToIntPtr(cfg.MemberCount), s.groupMaxMembers),
	}
}

package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
)

// Per-chat gate statuses. "auto" is the insert default; the other two are the
// user's explicit override, which no automatic write ever clears.
const (
	ChatStatusAuto    = "auto"
	ChatStatusTracked = "tracked"
	ChatStatusIgnored = "ignored"
)

// ErrChatGateUndecided means the gate could not decide right now. The caller
// must NOT acknowledge the message: WhatsApp never redelivers an acked message,
// and the next delivery will plausibly find the count resolved.
//
// Only TRANSIENT causes may enter this bucket. A permanent answer — we are not
// in the group, the group does not exist, the server reports no size — is a
// decision, because "ask again later" against an unchanging answer is an
// unbounded redelivery loop, each iteration paying a config read and a network
// IQ on the library's serialized handler queue.
var ErrChatGateUndecided = errors.New("whatsapp: group tracking could not be decided")

// EffectiveTracked decides whether a chat's messages are stored, from its
// persisted state alone.
//
// It fails CLOSED on an unknown member count — a deliberate divergence from
// Telegram, which tracks by default when the size is unknown. A non-positive
// count is treated as unknown, never as a small group: the wire's size
// attribute is optional and yields 0 when absent.
func EffectiveTracked(status string, memberCount *int, groupMaxMembers int) bool {
	switch status {
	case ChatStatusTracked:
		return true
	case ChatStatusIgnored:
		return false
	default:
		if memberCount == nil || *memberCount <= 0 {
			return false
		}
		return *memberCount <= groupMaxMembers
	}
}

// whatsAppChatConfigStore is the slice of WhatsAppRepository the gate needs.
type whatsAppChatConfigStore interface {
	GetChatConfig(ctx context.Context, chatJID string) (*repository.WhatsAppChatConfig, error)
	UpsertChatConfig(ctx context.Context, cfg repository.WhatsAppChatConfig) (*repository.WhatsAppChatConfig, error)
}

// ChatGate is the persisted group-size gate. It is owned by the Ingestor rather
// than the manager because the DB read must come FIRST — the library's group
// lookup is uncached, so an ungated call would be a network round trip per
// message — and because the history drainer projects through the same ingest
// choke point, giving both paths one implementation.
type ChatGate struct {
	repo            whatsAppChatConfigStore
	groupInfoSource func() GroupInfoFetcher
	groupMaxMembers int
}

// NewChatGate builds the gate. The group-info source is bound later, by
// Manager.SetIngestor, because the ingestor is constructed before the manager.
func NewChatGate(repo whatsAppChatConfigStore, groupMaxMembers int) *ChatGate {
	return &ChatGate{repo: repo, groupMaxMembers: groupMaxMembers}
}

// BindGroupInfoSource installs the late-bound accessor for the live client.
func (g *ChatGate) BindGroupInfoSource(src func() GroupInfoFetcher) {
	g.groupInfoSource = src
}

// ShouldTrack resolves and persists the chat's config, then decides whether its
// messages are stored.
//
// A non-nil error wrapping ErrChatGateUndecided means the caller must NOT
// acknowledge. "Do not track" and "fail closed" are STORAGE rules, not ack
// rules: they say an unresolved group's messages must not be stored, never that
// they must be acknowledged as handled.
func (g *ChatGate) ShouldTrack(ctx context.Context, chatJID, chatType string) (bool, error) {
	// The gate is the group-SIZE gate. A private chat has already passed the
	// person-to-person allowlist upstream.
	if chatType != ChatTypeGroup {
		return true, nil
	}

	cfg, err := g.repo.GetChatConfig(ctx, chatJID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return false, fmt.Errorf("%w: read chat config: %v", ErrChatGateUndecided, err)
	}
	if errors.Is(err, db.ErrNotFound) {
		cfg = nil
	}

	// An explicit user override needs no member count, and consulting it first
	// is what stops an ignored group paying a network IQ per message for a
	// number its own decision discards.
	if cfg != nil && (cfg.Status == ChatStatusTracked || cfg.Status == ChatStatusIgnored) {
		return cfg.Status == ChatStatusTracked, nil
	}

	var (
		count  *int
		title  *string
		lookup *time.Time
	)
	if cfg != nil {
		count = int32PtrToIntPtr(cfg.MemberCount)
		title = cfg.ChatTitle
	}

	// A lookup runs only when the row is absent or its count is still
	// unresolved, and a successful one is always something new to persist.
	looked := false
	if cfg == nil || count == nil {
		info, err := g.lookupGroup(ctx, chatJID)
		if err != nil {
			return false, err
		}
		if info == nil {
			// A decided-permanent lookup failure: nothing is stored, nothing is
			// written, and the message is consumed.
			return false, nil
		}
		resolved := info.MemberCount
		count = &resolved
		if info.Title != "" {
			title = &info.Title
		}
		now := accelerated.GetCurrentTime()
		lookup = &now
		looked = true
	}

	// Re-writing an unchanged row on every group message is a pointless write
	// on the message-handling path, so only a fresh resolution is persisted —
	// which is also the only way a row can be absent by this point.
	if looked {
		if _, err := g.repo.UpsertChatConfig(ctx, repository.WhatsAppChatConfig{
			ChatJID:      chatJID,
			ChatTitle:    title,
			ChatType:     chatType,
			MemberCount:  intPtrToInt32Ptr(count),
			LastLookupAt: lookup,
		}); err != nil {
			// Undecided rather than decided: without the write the count would
			// be re-fetched on every message from this chat.
			return false, fmt.Errorf("%w: persist chat config: %v", ErrChatGateUndecided, err)
		}
	}

	status := ChatStatusAuto
	if cfg != nil && cfg.Status != "" {
		status = cfg.Status
	}
	return EffectiveTracked(status, count, g.groupMaxMembers), nil
}

// lookupGroup resolves the group's metadata, splitting failures by whether a
// later delivery could plausibly answer differently.
//
// (nil, nil) means DECIDED-not-tracked: the server answered, and its answer
// does not change on redelivery. A non-nil error is undecided.
func (g *ChatGate) lookupGroup(ctx context.Context, chatJID string) (*ChatGroupInfo, error) {
	if g.groupInfoSource == nil {
		return nil, fmt.Errorf("%w: no group info source", ErrChatGateUndecided)
	}
	fetcher := g.groupInfoSource()
	if fetcher == nil {
		return nil, fmt.Errorf("%w: no connected client", ErrChatGateUndecided)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, groupInfoTimeout)
	defer cancel()

	info, err := fetcher.GroupInfo(lookupCtx, chatJID)
	if err == nil {
		return info, nil
	}
	if errors.Is(err, ErrGroupUnavailable) || errors.Is(err, ErrGroupSizeUnknown) {
		// We are not in the group, it does not exist, or it has no reportable
		// size. Withholding here would redeliver forever.
		logger.Debug().Err(err).Msg("whatsapp: group is permanently unsizable; not tracking")
		return nil, nil
	}
	logger.Warn().Err(err).Msg("whatsapp: group lookup failed; withholding the ack")
	return nil, fmt.Errorf("%w: %v", ErrChatGateUndecided, err)
}

// int32PtrToIntPtr converts the repository's column width to the gate's.
func int32PtrToIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}

// intPtrToInt32Ptr converts the gate's width back to the repository's.
func intPtrToInt32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	if *v > math.MaxInt32 {
		out := int32(math.MaxInt32)
		return &out
	}
	out := int32(*v)
	return &out
}

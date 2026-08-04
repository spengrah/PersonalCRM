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
	// lookupTimeout bounds the group metadata call. It is a field rather than
	// the package constant read directly so tests can shrink it without a
	// mutable global — two tests otherwise spend the full production bound in
	// real wall-clock time proving the bound exists.
	lookupTimeout time.Duration
}

// NewChatGate builds the gate. The group-info source is bound later, by
// Manager.SetIngestor, because the ingestor is constructed before the manager.
func NewChatGate(repo whatsAppChatConfigStore, groupMaxMembers int) *ChatGate {
	return &ChatGate{repo: repo, groupMaxMembers: groupMaxMembers, lookupTimeout: groupInfoTimeout}
}

// BindGroupInfoSource installs the late-bound accessor for the live client.
func (g *ChatGate) BindGroupInfoSource(src func() GroupInfoFetcher) {
	g.groupInfoSource = src
}

// ShouldTrack resolves and persists the chat's config, then decides whether its
// messages are stored.
//
// accountJID is the linked account the MESSAGE was observed by, in the non-AD
// form the parser stamps. It is compared against the account the connected
// client belongs to, because the two can differ mid-re-pair; an empty value
// skips the comparison, which is what a caller that cannot know its account
// gets.
//
// A non-nil error wrapping ErrChatGateUndecided means the caller must NOT
// acknowledge. "Do not track" and "fail closed" are STORAGE rules, not ack
// rules: they say an unresolved group's messages must not be stored, never that
// they must be acknowledged as handled.
func (g *ChatGate) ShouldTrack(ctx context.Context, chatJID, chatType, accountJID string) (bool, error) {
	return g.decide(ctx, chatJID, chatType, accountJID, nil)
}

// ShouldTrackHistoryChat is the history drainer's entry point. It is
// ShouldTrack with ONE narrow addition: a participant snapshot the chunk itself
// carried, usable only when the live client reports that we are NOT IN the
// group at all.
//
// The two permanent answers lookupGroup can take are materially different and
// must not be collapsed:
//
//   - permanentUnavailable — we are not in the group, or it does not exist.
//     Such a group produces no live messages, so a count adopted from the
//     chunk can only ever gate the historical conversation that carried it.
//     This is the case the fallback exists for: without it, every group the
//     user has left is dropped from the backfill.
//   - permanentSizeUnknown — we ARE in the group; the server simply did not
//     report a size. Adopting a historical count here would persist it, and
//     because the gate re-looks-up only while member_count is NULL, it would
//     freeze as the answer for every FUTURE LIVE message from a group whose
//     real size is unknown. That is the fail-open this gate exists to refuse,
//     so it stays fail-closed.
//
// snapshot may be nil (an empty participant list), in which case the decision
// is byte-identical to ShouldTrack's.
func (g *ChatGate) ShouldTrackHistoryChat(ctx context.Context, chatJID string, snapshot *ChatGroupInfo, accountJID string) (bool, error) {
	return g.decide(ctx, chatJID, ChatTypeGroup, accountJID, snapshot)
}

// decide is the gate's whole body. ShouldTrack passes a nil fallback, which
// makes every branch below identical to the live path's.
func (g *ChatGate) decide(ctx context.Context, chatJID, chatType, accountJID string, fallback *ChatGroupInfo) (bool, error) {
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
		info, reason, err := g.lookupGroup(ctx, chatJID, accountJID)
		if err != nil {
			return false, err
		}
		switch {
		case info != nil:
			resolved := info.MemberCount
			count = &resolved
			if info.Title != "" {
				title = &info.Title
			}
			now := accelerated.GetCurrentTime()
			lookup = &now
			looked = true

		case reason == permanentUnavailable && fallback != nil && fallback.MemberCount > 0:
			// The one permitted fallback: the live client says we are not in
			// this group, so its size can never gate a live message, and the
			// chunk carried WhatsApp's own participant list for it.
			//
			// LastLookupAt stays nil deliberately — no live lookup produced
			// this count, and the column is the record of one.
			resolved := fallback.MemberCount
			count = &resolved
			if fallback.Title != "" {
				fallbackTitle := fallback.Title
				title = &fallbackTitle
			}
			looked = true

		default:
			// A decided-permanent lookup failure: nothing is stored, nothing is
			// written, and the message is consumed.
			return false, nil
		}
	}

	// Re-writing an unchanged row on every group message is a pointless write
	// on the message-handling path, so only a fresh resolution is persisted —
	// which is also the only way a row can be absent by this point.
	//
	// LastLookupAt is written for OBSERVABILITY only: nothing reads it, and
	// nothing re-resolves on a TTL, so once a size is recorded it is frozen
	// until the row's member_count is cleared. A group that grows past the
	// threshold therefore keeps being tracked. That is a deliberate limit of
	// this PR, not an oversight — a TTL re-resolve would put a network round
	// trip back on the message path, and the settings surface that will let a
	// user override the decision is where a re-resolve belongs.
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

// permanentReason names WHICH decided-permanent answer a lookup took, so a
// caller can distinguish "we are not in this group" from "we are in it and the
// server did not say how big it is". Only the FIRST may be superseded by a
// historical snapshot; the second must stay fail-closed, because the group is
// live and its real size is merely unreported.
type permanentReason string

const (
	permanentNone        permanentReason = ""
	permanentUnavailable permanentReason = "group_unavailable"
	permanentSizeUnknown permanentReason = "group_size_unknown"
)

// lookupGroup resolves the group's metadata, splitting failures by whether a
// later delivery could plausibly answer differently.
//
// (nil, <reason>, nil) means DECIDED-not-tracked: the server answered, and its
// answer does not change on redelivery. A non-nil error is undecided, and its
// reason is always permanentNone.
func (g *ChatGate) lookupGroup(ctx context.Context, chatJID, accountJID string) (*ChatGroupInfo, permanentReason, error) {
	if g.groupInfoSource == nil {
		return nil, permanentNone, fmt.Errorf("%w: no group info source", ErrChatGateUndecided)
	}
	fetcher := g.groupInfoSource()
	if fetcher == nil {
		return nil, permanentNone, fmt.Errorf("%w: no connected client", ErrChatGateUndecided)
	}

	// The fetcher comes from the PUBLISHED session; the message came from its
	// EMITTING one. Mid-re-pair those are different accounts, and asking the new
	// account about a group only the old one was in returns "not in that group"
	// — a PERMANENT answer, which this gate would consume and acknowledge,
	// losing the message for good. A mismatch is transient by construction (the
	// re-pair settles), so it is undecided, and no lookup is made at all.
	if accountJID != "" {
		if live := fetcher.AccountJID(); live != "" && live != accountJID {
			return nil, permanentNone, fmt.Errorf("%w: the connected client is a different account than the one that observed this message", ErrChatGateUndecided)
		}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, g.lookupTimeout)
	defer cancel()

	info, err := fetcher.GroupInfo(lookupCtx, chatJID)
	if err == nil {
		return info, permanentNone, nil
	}
	if errors.Is(err, ErrGroupUnavailable) {
		// We are not in the group, or it does not exist. Withholding here would
		// redeliver forever.
		logger.Debug().Err(err).Msg("whatsapp: group is unavailable to this account; not tracking")
		return nil, permanentUnavailable, nil
	}
	if errors.Is(err, ErrGroupSizeUnknown) {
		// We ARE in the group; the server just did not report a size. Also
		// permanent — but never superseded by a historical count, because this
		// group's live messages keep arriving.
		logger.Debug().Err(err).Msg("whatsapp: group size is unreported; not tracking")
		return nil, permanentSizeUnknown, nil
	}
	logger.Warn().Err(err).Msg("whatsapp: group lookup failed; withholding the ack")
	return nil, permanentNone, fmt.Errorf("%w: %v", ErrChatGateUndecided, err)
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

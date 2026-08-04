package whatsapp

import (
	"context"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

var (
	// ErrGroupSizeUnknown means the server answered without a usable size. It
	// is deliberately NOT the same as a small group: an absent size attribute
	// yields 0 with no error from the library, and 0 satisfies any
	// "<= threshold" test, so a resolved-looking 0 would make the fail-closed
	// gate fail OPEN forever.
	ErrGroupSizeUnknown = errors.New("whatsapp: group size unknown")

	// ErrGroupUnavailable wraps the library's "not in the group" (403) and
	// "group does not exist" (404) answers. Both are answers, not failures:
	// neither changes on a redelivery.
	ErrGroupUnavailable = errors.New("whatsapp: group unavailable")
)

// GroupInfoFetcher returns the seam bound to the live client, or nil when no
// client is connected — in which case the member count stays unresolved and the
// gate reports ErrChatGateUndecided rather than deciding.
//
// It reads the published snapshot rather than taking a turn, exactly as
// HistoryFetcher does: a per-message lookup must never wait on the actor loop.
func (m *Manager) GroupInfoFetcher() GroupInfoFetcher {
	var sess *session
	if s := m.snap.Load(); s != nil {
		sess = s.sess
	}

	if sess == nil || sess.wa == nil || !sess.client.IsConnected() {
		return nil
	}
	return &clientGroupInfoFetcher{cli: sess.wa}
}

// clientGroupInfoFetcher wraps the one group-metadata call this integration
// makes.
type clientGroupInfoFetcher struct {
	cli *whatsmeow.Client
}

var _ GroupInfoFetcher = (*clientGroupInfoFetcher)(nil)

// AccountJID reports the linked account this client belongs to.
//
// It goes through canonicalAccountJID — the SAME derivation the parser uses to
// stamp each message — so the comparison in the gate cannot fail merely because
// the two sides preferred different forms of one identity.
func (f *clientGroupInfoFetcher) AccountJID() string {
	if f.cli == nil || f.cli.Store == nil {
		return ""
	}
	return canonicalAccountJID(f.cli.Store.GetJID(), f.cli.Store.GetLID())
}

// GroupInfo resolves a group's title and member count.
//
// The library's GetGroupInfo never reads its cache — it sends a group IQ every
// call — so this is a network round trip and the caller must gate it on
// persisted state.
//
// The count is derived DEFENSIVELY: ParticipantCount is an optional wire
// attribute that yields 0 when absent, so a non-positive value falls back to
// the participant list's length and, failing that, is reported as unknown
// rather than as a resolved zero.
func (f *clientGroupInfoFetcher) GroupInfo(ctx context.Context, chatJID string) (*ChatGroupInfo, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: parse group JID: %w", err)
	}

	info, err := f.cli.GetGroupInfo(ctx, jid)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrNotInGroup) || errors.Is(err, whatsmeow.ErrGroupNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrGroupUnavailable, err)
		}
		return nil, fmt.Errorf("whatsapp: get group info: %w", err)
	}
	return projectGroupInfo(info)
}

// projectGroupInfo derives the member count DEFENSIVELY, which is why it is a
// pure function rather than four lines inline: it is the single place a
// resolved-looking zero is refused, and it is unit-testable without a client.
//
// ParticipantCount is an OPTIONAL wire attribute — an absent size yields 0 with
// no error — so a non-positive value falls back to the participant list's length
// and, failing that, is reported as unknown. Without this, an absent size
// persists as a resolved 0, satisfies any "<= threshold" test, and makes the
// fail-closed gate fail OPEN forever, because the retry never re-runs once the
// count is no longer NULL.
func projectGroupInfo(info *types.GroupInfo) (*ChatGroupInfo, error) {
	if info == nil {
		return nil, fmt.Errorf("%w: empty group info", ErrGroupSizeUnknown)
	}
	count := info.ParticipantCount
	if count <= 0 {
		count = len(info.Participants)
	}
	if count <= 0 {
		return nil, ErrGroupSizeUnknown
	}
	return &ChatGroupInfo{Title: info.Name, MemberCount: count}, nil
}

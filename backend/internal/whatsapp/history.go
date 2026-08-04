package whatsapp

import (
	"context"
	"errors"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// HistoryFetcher returns the seam the history drainer uses to reach the live
// client, or nil when no client is connected — in which case the drainer defers
// the chunk without claiming it. The manager is the only production
// implementation; the drainer never holds a *whatsmeow.Client.
func (m *Manager) HistoryFetcher() HistoryFetcher {
	// The published snapshot, not a query message: a status-adjacent read must
	// never wait on a turn. The live IsConnected check stays at call time — a
	// read-only library call touching no manager state — so the "nil when not
	// connected" contract is behaviourally identical.
	var sess *session
	if s := m.snap.Load(); s != nil {
		sess = s.sess
	}

	if sess == nil || sess.wa == nil || !sess.client.IsConnected() {
		return nil
	}
	return &clientHistoryFetcher{cli: sess.wa, sess: sess, mgr: m}
}

// clientHistoryFetcher wraps the history-owned whatsmeow calls.
//
// It carries three references rather than one client because projection needs
// all three: cli for the library calls, sess because ownIdentityFor resolves
// the EMITTING session's identity from it, and mgr so the unresolved-LID
// counter keeps exactly ONE owner across the live and history paths.
type clientHistoryFetcher struct {
	cli  *whatsmeow.Client
	sess *session
	mgr  *Manager
}

var _ HistoryFetcher = (*clientHistoryFetcher)(nil)

func unmarshalNotification(notification []byte) (*waE2E.HistorySyncNotification, error) {
	var notif waE2E.HistorySyncNotification
	if err := proto.Unmarshal(notification, &notif); err != nil {
		return nil, fmt.Errorf("%w: unmarshal: %v", ErrHistoryNotificationMalformed, err)
	}
	return &notif, nil
}

// AccountJID reports the linked account this client belongs to.
//
// It goes through canonicalAccountJID — the SAME derivation the parser uses to
// stamp each message and the group-info seam uses to report its account — so
// the gate's account comparison cannot fail merely because the two sides
// preferred different forms of one identity.
func (f *clientHistoryFetcher) AccountJID() string {
	if f.cli == nil || f.cli.Store == nil {
		return ""
	}
	return canonicalAccountJID(f.cli.Store.GetJID(), f.cli.Store.GetLID())
}

// ProjectHistoryMessage projects one history message through the live parser.
//
// ParseWebMessage is a pure local decode: JID parsing, field copies, an
// in-memory own-ID read and a proto walk. It performs no I/O and mutates no
// store, which is why the read-only fence admits it.
//
// The error/eligibility split matches the seam contract: a decode failure is
// this ONE message's problem and the caller skips it, while eligible=false is
// an ordinary drop the parser already decided (ineligible chat, non-turn).
func (f *clientHistoryFetcher) ProjectHistoryMessage(ctx context.Context, chatJID string, webMsg *waWeb.WebMessageInfo) (IngestedMessage, bool, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return IngestedMessage{}, false, fmt.Errorf("whatsapp: parse history chat JID %q: %w", chatJID, err)
	}
	evt, err := f.cli.ParseWebMessage(jid, webMsg)
	if err != nil {
		return IngestedMessage{}, false, fmt.Errorf("whatsapp: parse history message: %w", err)
	}

	own, resolver := f.mgr.ownIdentityFor(f.sess)
	if !own.ok() {
		// Not a per-message failure: without an own JID no message in this
		// chunk can be attributed, so the caller's retry is the right response.
		return IngestedMessage{}, false, errors.New("whatsapp: own identity unknown; cannot project history")
	}

	msg, unresolvedLID, eligible := parseMessage(ctx, evt, own, resolver, altJIDTimeout)
	if unresolvedLID != "" {
		f.mgr.noteUnresolvedLID(unresolvedLID)
	}
	return msg, eligible, nil
}

// DownloadHistorySync downloads the chunk and then verifies that every PN-LID
// mapping it carried actually reads back out of the client's own LID store.
//
// synchronousStorage=true is mandatory (with false the library's storage runs
// detached), but it is not sufficient: whatsmeow logs and swallows a failure to
// persist the mappings while still reporting the download as a success. Without
// the read-back a transient database error would let the caller project
// LID-only peers as permanently unmatched.
//
// Verification is EQUALITY on the normalized pair, never mere presence: a stale
// row mapping a LID to a different phone number would satisfy a presence check
// and silently mis-attribute every message from that peer.
func (f *clientHistoryFetcher) DownloadHistorySync(ctx context.Context, notification []byte) (*waHistorySync.HistorySync, error) {
	notif, err := unmarshalNotification(notification)
	if err != nil {
		return nil, err
	}

	chunk, err := f.cli.DownloadHistorySync(ctx, notif, true)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: download history sync: %w", err)
	}

	if f.cli.Store == nil {
		return nil, fmt.Errorf("%w: no device store", ErrLIDMappingsIncomplete)
	}
	if err := VerifyHistoryLIDMappings(ctx, f.cli.Store.LIDs, chunk); err != nil {
		return nil, err
	}
	return chunk, nil
}

// expectedStoredLIDMappings reduces a chunk's PN-LID mappings to the set the
// library will actually have persisted, mirroring PutManyLIDMappings.
//
// The raw chunk contents are the WRONG expectation, because the library declines
// to store some of what a chunk carries and says nothing about it at a level
// this process can see:
//
//   - It SKIPS a pair whose phone number or LID does not parse, and IGNORES one
//     whose LID is not on the hidden-user server or whose phone number is not on
//     the default user server (the legacy server is rewritten first, here as
//     there).
//   - The store holds one LID per phone number and one phone number per LID —
//     whatsmeow_lid_map is UNIQUE on both columns — and each write DELETEs the
//     rows that collide with it. So when one chunk carries two LIDs for the same
//     phone number, the later write EVICTS the earlier one, and only the last
//     pair survives.
//
// Demanding the raw list read back therefore fails PERMANENTLY on a chunk the
// library handled correctly: the verification stalls a one-shot bootstrap that
// no retry can change, because every retry re-downloads the same chunk and
// reproduces the same end state.
//
// Reducing first keeps the guarantee that matters — every mapping still expected
// must read back as an exact pair, so a stale row can never mis-attribute a peer
// — over the set that is actually capable of existing. Chunk order is preserved
// so a failure names the same mapping on every run.
func expectedStoredLIDMappings(chunk *waHistorySync.HistorySync) []store.LIDMapping {
	raw := chunk.GetPhoneNumberToLidMappings()
	accepted := make([]store.LIDMapping, 0, len(raw))
	for _, mapping := range raw {
		pn, err := types.ParseJID(mapping.GetPnJID())
		if err != nil {
			continue
		}
		if pn.Server == types.LegacyUserServer {
			pn.Server = types.DefaultUserServer
		}
		lid, err := types.ParseJID(mapping.GetLidJID())
		if err != nil {
			continue
		}
		if lid.Server != types.HiddenUserServer || pn.Server != types.DefaultUserServer {
			continue
		}
		accepted = append(accepted, store.LIDMapping{LID: lid.ToNonAD(), PN: pn.ToNonAD()})
	}

	// Replay the writes in order to land on the same final state the store does,
	// evicting on both unique columns exactly as its delete-then-upsert pair.
	lidToPN := make(map[types.JID]types.JID, len(accepted))
	pnToLID := make(map[types.JID]types.JID, len(accepted))
	for _, m := range accepted {
		if prevLID, ok := pnToLID[m.PN]; ok && prevLID != m.LID {
			delete(lidToPN, prevLID)
		}
		if prevPN, ok := lidToPN[m.LID]; ok && prevPN != m.PN {
			delete(pnToLID, prevPN)
		}
		lidToPN[m.LID] = m.PN
		pnToLID[m.PN] = m.LID
	}

	survivors := make([]store.LIDMapping, 0, len(lidToPN))
	seen := make(map[types.JID]struct{}, len(lidToPN))
	for _, m := range accepted {
		if _, done := seen[m.LID]; done {
			continue
		}
		if finalPN, ok := lidToPN[m.LID]; ok && finalPN == m.PN {
			seen[m.LID] = struct{}{}
			survivors = append(survivors, m)
		}
	}
	return survivors
}

// VerifyHistoryLIDMappings checks that every PN-LID mapping a history chunk
// carried AND the library would have kept actually reads back out of the LID
// store, as an equality on the normalized pair.
//
// It is exported because it is the boundary the whole LID-attribution guarantee
// rests on: whatsmeow logs and swallows a failure to persist these mappings
// while still reporting the download as a success, so nothing downstream may
// assume the mappings landed. See expectedStoredLIDMappings for why the
// expectation is the reduced set rather than the chunk's raw list.
func VerifyHistoryLIDMappings(ctx context.Context, lids store.LIDStore, chunk *waHistorySync.HistorySync) error {
	if lids == nil {
		return fmt.Errorf("%w: no LID store", ErrLIDMappingsIncomplete)
	}

	for _, expected := range expectedStoredLIDMappings(chunk) {
		stored, err := lids.GetPNForLID(ctx, expected.LID)
		if err != nil {
			return fmt.Errorf("%w: reading back %s: %w", ErrLIDMappingsIncomplete, expected.LID, err)
		}
		if stored.IsEmpty() {
			return fmt.Errorf("%w: no stored phone number for %s", ErrLIDMappingsIncomplete, expected.LID)
		}
		if stored.ToNonAD() != expected.PN {
			return fmt.Errorf("%w: %s maps to %s, expected %s", ErrLIDMappingsIncomplete, expected.LID, stored.ToNonAD(), expected.PN)
		}
	}
	return nil
}

// AckHistorySync sends the protocol receipt for a handled chunk. In its default
// mode the library sends this itself, unconditionally, before the download —
// manual mode moves WHEN it fires, not whether.
//
// An empty id is REFUSED rather than passed through: SendProtocolMessageReceipt
// returns nil immediately for one, so the chunk would advance to done having
// told WhatsApp nothing — an inbox that reads complete while WhatsApp
// redelivers the chunk indefinitely. The column is NOT NULL UNIQUE but not
// constrained non-empty, so this guard is what closes it.
func (f *clientHistoryFetcher) AckHistorySync(ctx context.Context, protocolMsgID string) error {
	if protocolMsgID == "" {
		return errors.New("whatsapp: refusing to acknowledge a history chunk with no protocol message id")
	}
	if err := f.cli.SendProtocolMessageReceipt(ctx, protocolMsgID, types.ReceiptTypeHistorySync); err != nil {
		return fmt.Errorf("whatsapp: ack history sync: %w", err)
	}
	return nil
}

// DeleteHistoryMedia removes our own history payload from WhatsApp's media
// server. It acts on our blob, never on a conversation partner's state.
func (f *clientHistoryFetcher) DeleteHistoryMedia(ctx context.Context, notification []byte) error {
	notif, err := unmarshalNotification(notification)
	if err != nil {
		return err
	}
	if err := f.cli.DeleteMedia(ctx, whatsmeow.MediaHistory, notif.GetDirectPath(), notif.GetFileEncSHA256(), notif.GetEncHandle()); err != nil {
		return fmt.Errorf("whatsapp: delete history media: %w", err)
	}
	return nil
}

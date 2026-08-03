package whatsapp

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// HistoryFetcher returns the seam the history drainer uses to reach the live
// client, or nil when no client is connected — in which case the drainer defers
// the chunk without claiming it. The manager is the only production
// implementation; the drainer never holds a *whatsmeow.Client.
func (m *Manager) HistoryFetcher() HistoryFetcher {
	m.mu.RLock()
	sess := m.sess
	m.mu.RUnlock()

	if sess == nil || sess.wa == nil || !sess.client.IsConnected() {
		return nil
	}
	return &clientHistoryFetcher{cli: sess.wa}
}

// clientHistoryFetcher wraps the three history-owned whatsmeow calls.
type clientHistoryFetcher struct {
	cli *whatsmeow.Client
}

var _ HistoryFetcher = (*clientHistoryFetcher)(nil)

func unmarshalNotification(notification []byte) (*waE2E.HistorySyncNotification, error) {
	var notif waE2E.HistorySyncNotification
	if err := proto.Unmarshal(notification, &notif); err != nil {
		return nil, fmt.Errorf("whatsapp: unmarshal history notification: %w", err)
	}
	return &notif, nil
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

// VerifyHistoryLIDMappings checks that every PN-LID mapping a history chunk
// carried actually reads back out of the LID store, as an equality on the
// normalized pair.
//
// It is exported because it is the boundary the whole LID-attribution guarantee
// rests on: whatsmeow logs and swallows a failure to persist these mappings
// while still reporting the download as a success, so nothing downstream may
// assume the mappings landed.
func VerifyHistoryLIDMappings(ctx context.Context, lids store.LIDStore, chunk *waHistorySync.HistorySync) error {
	if lids == nil {
		return fmt.Errorf("%w: no LID store", ErrLIDMappingsIncomplete)
	}

	for _, mapping := range chunk.GetPhoneNumberToLidMappings() {
		expectedPN, err := types.ParseJID(mapping.GetPnJID())
		if err != nil {
			return fmt.Errorf("%w: unparseable phone JID %q: %w", ErrLIDMappingsIncomplete, mapping.GetPnJID(), err)
		}
		// The library applies the same rewrite before storing, so the
		// comparison must apply it too or every legacy-server pair would
		// mismatch.
		if expectedPN.Server == types.LegacyUserServer {
			expectedPN.Server = types.DefaultUserServer
		}
		lid, err := types.ParseJID(mapping.GetLidJID())
		if err != nil {
			return fmt.Errorf("%w: unparseable LID %q: %w", ErrLIDMappingsIncomplete, mapping.GetLidJID(), err)
		}

		stored, err := lids.GetPNForLID(ctx, lid)
		if err != nil {
			return fmt.Errorf("%w: reading back %s: %w", ErrLIDMappingsIncomplete, lid, err)
		}
		if stored.IsEmpty() {
			return fmt.Errorf("%w: no stored phone number for %s", ErrLIDMappingsIncomplete, lid)
		}
		if stored.ToNonAD() != expectedPN.ToNonAD() {
			return fmt.Errorf("%w: %s maps to %s, expected %s", ErrLIDMappingsIncomplete, lid, stored.ToNonAD(), expectedPN.ToNonAD())
		}
	}
	return nil
}

// AckHistorySync sends the protocol receipt for a handled chunk. In its default
// mode the library sends this itself, unconditionally, before the download —
// manual mode moves WHEN it fires, not whether.
func (f *clientHistoryFetcher) AckHistorySync(ctx context.Context, protocolMsgID string) error {
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

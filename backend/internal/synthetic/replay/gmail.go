package replay

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	gmailapi "google.golang.org/api/gmail/v1"
)

// GmailResult is the settled outcome of a Gmail replay.
type GmailResult struct {
	ContactID  uuid.UUID
	ExternalID string
	Matched    bool // false for MatchUnknown (match-only)
}

// ReplayGmail feeds a synthetic Gmail message through the REAL GmailSyncProvider
// (fake fetcher + injected me-set, no OAuth) and settles the graph. For
// MatchSeeded the seeded contact's email matches → comms_message + email.* event
// → email_interaction_consumer → interaction. For MatchUnknown it is match-only
// (no contact, no interaction, no row).
//
// contactID is the seeded contact this message targets (for MatchSeeded). The
// caller seeds the contact first via Harness.SeedContact.
func (h *Harness) ReplayGmail(ctx context.Context, contactID uuid.UUID, spec factory.GmailMessageSpec) (GmailResult, error) {
	provider := google.NewGmailSyncProvider(nil, h.commsRepo, h.bus, h.database.Pool)
	provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, _, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			return []google.GmailMessageRefForTest{{ID: spec.Message.Id, ThreadID: spec.Message.ThreadId}}, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			if id == spec.Message.Id {
				return spec.Message, nil
			}
			return nil, fmt.Errorf("synthetic gmail: message %s not found", id)
		},
	}))
	provider.SetMeSetForTest(map[string]struct{}{spec.AccountID: {}})

	// Floor the scan window just before THIS message's own send time so a message
	// replayed at any age (the interaction temporal spread) is still inside the
	// scanned window. InternalDate is the wire epoch the factory stamped from the
	// (aged) anchor-relative sentAt.
	sentAt := time.UnixMilli(spec.Message.InternalDate).UTC()
	state := &repository.SyncState{
		Source:    repository.InteractionSourceEmail,
		AccountID: &spec.AccountID,
		Metadata:  map[string]any{"backfill_since": gmailBackfillSince(sentAt)},
	}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return GmailResult{}, fmt.Errorf("gmail sync: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		// Match-only: the provider writes NO comms_message row and publishes no
		// email.* event for an unknown correspondent, so there is nothing to
		// track for cleanup and nothing to wait on. Gate B is trivially 0 (no
		// seeded contact).
		if err := h.Settle(ctx, func(context.Context) (bool, error) { return true, nil }, ""); err != nil {
			return GmailResult{}, err
		}
		return GmailResult{ExternalID: spec.ExternalID, Matched: false}, nil
	}

	// Seeded path: the provider publishes an email.* root event (source_id is
	// synth-<ns>-prefixed) — track the source so cleanup captures the root event.
	h.track(func(c *created) { c.addDirectSource(repository.InteractionSourceEmail) })

	// Gate A: THIS replay's email message row is linked to an interaction.
	if err := h.Settle(ctx, h.gmailSettled(spec.ExternalID), ""); err != nil {
		return GmailResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceEmail); err != nil {
		return GmailResult{}, err
	}
	return GmailResult{ContactID: contactID, ExternalID: spec.ExternalID, Matched: true}, nil
}

// GmailBatchItem is one Gmail payload in a batch: the seeded contact it targets
// and the message itself. PairKey marks the two items of a promotion pair (0 =
// not part of one) — the inbound member of a pair is driven in a later
// dependency generation so its outbound's interaction already exists.
type GmailBatchItem struct {
	ContactID uuid.UUID
	Spec      factory.GmailMessageSpec
	PairKey   int
}

// ReplayGmailBatch drives N Gmail payloads through ONE GmailSyncProvider Sync
// per dependency generation and settles once per generation, instead of N single
// replays each performing a full Settle. Items must be in chronological replay
// order (oldest first); the adapter never reorders them.
//
// The fake fetcher returns the whole generation on every scan window and the
// provider filters by exact internalDate, so one Sync covers every item inside
// the reachable span — which is what gmailBatchMaxSpan bounds.
func (h *Harness) ReplayGmailBatch(ctx context.Context, items []GmailBatchItem) (BatchResult, error) {
	const source = "gmail"

	entries := gmailBatchEntries(items)
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	accountID, err := gmailBatchAccount(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := gmailBatchSpanWithinReach(items); err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}
	before := h.snapshotInteractionIDs(ctx, contactIDs)

	// The provider publishes email.* root events whose source_id is
	// synth-<ns>-prefixed; track the source so cleanup captures them.
	h.track(func(c *created) { c.addDirectSource(repository.InteractionSourceEmail) })

	for _, generation := range partitionGenerations(entries) {
		genItems := make([]GmailBatchItem, 0, len(generation))
		for _, i := range generation {
			genItems = append(genItems, items[i])
		}

		provider := google.NewGmailSyncProvider(nil, h.commsRepo, h.bus, h.database.Pool)
		provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(gmailBatchFetcherFuncs(genItems)))
		provider.SetMeSetForTest(map[string]struct{}{accountID: {}})

		// Floor the scan window just before the generation's OLDEST message, the
		// same rule the single adapter applies to its one message.
		state := &repository.SyncState{
			Source:    repository.InteractionSourceEmail,
			AccountID: &accountID,
			Metadata:  map[string]any{"backfill_since": gmailBackfillSince(oldestInstant(gmailBatchInstants(genItems)))},
		}
		if _, err := provider.Sync(ctx, state, nil); err != nil {
			return res, h.drainPartial(ctx, source, "", contactIDs, fmt.Errorf("gmail sync: %w", err))
		}
		res.SyncCalls++

		// Gate A is scoped to THIS generation's identifiers. Demanding all N after
		// driving only generation 0 would time out by construction.
		if err := h.Settle(ctx, h.gmailBatchSettled(gmailBatchExternalIDs(genItems)), ""); err != nil {
			return res, h.drainPartial(ctx, source, "", contactIDs, err)
		}
		res.SettleCalls++
	}

	res.Interactions = h.trackBatchInteractions(ctx, contactIDs, before)
	for _, contactID := range contactIDs {
		if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceEmail); err != nil {
			return res, err
		}
	}
	return res, nil
}

// gmailBatchSettled is the batch Gate A: every one of these external ids has an
// interaction-linked comms_message row. COUNT(DISTINCT external_id) over the
// passed ids cannot exceed the batch size, so >= is the equality the plan calls
// for, stated in a form that cannot be tripped by a duplicate row.
func (h *Harness) gmailBatchSettled(externalIDs []string) gateA {
	want := int64(len(externalIDs))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountSettledGmailMessagesByExternalIDs(ctx, externalIDs)
		return n >= want, err
	}
}

// gmailBatchFetcherFuncs builds the fake fetcher for one generation: every
// message on the first page, each resolvable by id.
func gmailBatchFetcherFuncs(items []GmailBatchItem) google.FakeGmailFetcherFuncs {
	refs := make([]google.GmailMessageRefForTest, 0, len(items))
	byID := make(map[string]*gmailapi.Message, len(items))
	for _, it := range items {
		refs = append(refs, google.GmailMessageRefForTest{ID: it.Spec.Message.Id, ThreadID: it.Spec.Message.ThreadId})
		byID[it.Spec.Message.Id] = it.Spec.Message
	}
	return google.FakeGmailFetcherFuncs{
		ListMessageIDs: func(_ context.Context, _, pageToken string) ([]google.GmailMessageRefForTest, string, error) {
			if pageToken != "" {
				return nil, "", nil
			}
			return refs, "", nil
		},
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			msg, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("synthetic gmail: message %s not found", id)
			}
			return msg, nil
		},
	}
}

// gmailBatchEntries projects the typed items into the source-neutral view the
// shared preflight and partition helpers work over. Direction comes from the
// SENT label (what the provider itself reads), and the addressed identifier is
// the peer side of the message — the value that decides whether the contact
// matches at all.
func gmailBatchEntries(items []GmailBatchItem) []batchEntry {
	out := make([]batchEntry, 0, len(items))
	for _, it := range items {
		outbound := gmailSpecOutbound(it.Spec)
		peerHeader := "From"
		if outbound {
			peerHeader = "To"
		}
		out = append(out, batchEntry{
			contactID:     it.ContactID,
			identifier:    it.Spec.ExternalID,
			seeded:        it.Spec.Intent == factory.MatchSeeded,
			outbound:      outbound,
			pairKey:       it.PairKey,
			addressed:     gmailSpecHeader(it.Spec, peerHeader),
			addressedType: identity.IdentifierTypeEmail,
		})
	}
	return out
}

// gmailSpecOutbound reports whether the provider will derive OUTBOUND from this
// payload. The provider decides on From ∈ meSet; the factory keeps the SENT
// label in lockstep with that swap, and the label is the cheap read.
func gmailSpecOutbound(spec factory.GmailMessageSpec) bool {
	if spec.Message == nil {
		return false
	}
	for _, label := range spec.Message.LabelIds {
		if label == "SENT" {
			return true
		}
	}
	return false
}

// gmailSpecHeader returns a header value from a Gmail payload ("" if absent).
func gmailSpecHeader(spec factory.GmailMessageSpec, name string) string {
	if spec.Message == nil || spec.Message.Payload == nil {
		return ""
	}
	for _, hdr := range spec.Message.Payload.Headers {
		if hdr.Name == name {
			return hdr.Value
		}
	}
	return ""
}

// gmailBatchInstants returns each item's send instant (the wire epoch the
// factory stamped from the aged, anchor-relative sentAt).
func gmailBatchInstants(items []GmailBatchItem) []time.Time {
	out := make([]time.Time, 0, len(items))
	for _, it := range items {
		out = append(out, time.UnixMilli(it.Spec.Message.InternalDate).UTC())
	}
	return out
}

// gmailBatchExternalIDs returns the batch's comms_message external ids.
func gmailBatchExternalIDs(items []GmailBatchItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Spec.ExternalID)
	}
	return out
}

// gmailBatchSpanWithinReach rejects a batch wider than one Sync can reach. The
// provider scans forward from backfill_since in 7-day windows, at most 24 of
// them, and the floor is the OLDEST payload — so a payload newer than
// oldest + 168d is never listed. It is dropped SILENTLY, surfacing 30 seconds
// later as a Gate A timeout naming the wrong cause, which is exactly what this
// check converts into an immediate, self-describing error. Callers bucket.
func gmailBatchSpanWithinReach(items []GmailBatchItem) error {
	span := ageSpan(gmailBatchInstants(items))
	if span > gmailBatchMaxSpan {
		return fmt.Errorf("gmail: batch spans %s from oldest to newest payload but one sync reaches only %s: %w",
			span, gmailBatchMaxSpan, ErrBatchGmailSpanExceeded)
	}
	return nil
}

// gmailBatchAccount returns the single connected account the batch is driven
// under. The me-set and sync state are per account, so a mixed-account batch
// would silently drop every payload belonging to the other account.
func gmailBatchAccount(items []GmailBatchItem) (string, error) {
	account := items[0].Spec.AccountID
	for i, it := range items {
		if it.Spec.AccountID != account {
			return "", fmt.Errorf("gmail: item %d is on account %q but the batch is on %q: %w",
				i, it.Spec.AccountID, account, ErrBatchMixedAccounts)
		}
	}
	return account, nil
}

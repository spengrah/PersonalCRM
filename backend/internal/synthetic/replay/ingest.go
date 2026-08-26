package replay

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
)

// MeetingNoteRecordedSpec is the production-shaped input for one linked
// meeting note replay. The adapter wraps it in a meeting_note.recorded
// envelope and sends it through IngestService, preserving the same writer and
// linkage decisions as the daemon path.
type MeetingNoteRecordedSpec struct {
	SessionID            uuid.UUID
	Title, Summary, Memo *string
	MeetingAt            time.Time
}

// ReplayMeetingNoteRecorded feeds a meeting_note.recorded envelope through the
// real ingest service. The event source id includes the production content
// hash, while the event row id is tracked explicitly because the source id is
// uuid-shaped and cannot be found by the generator namespace prefix.
func (h *Harness) ReplayMeetingNoteRecorded(ctx context.Context, spec MeetingNoteRecordedSpec) error {
	payload := events.MeetingNoteRecordedPayload{
		Version: 1, HostID: h.macHostID, Source: "anarlog_sessions",
		SourceID: spec.SessionID.String(), Title: spec.Title, Summary: spec.Summary,
		Memo: spec.Memo, MeetingAt: spec.MeetingAt,
	}
	raw, err := events.Marshal(events.KindMeetingNoteRecorded, payload)
	if err != nil {
		return fmt.Errorf("marshal meeting note: %w", err)
	}
	hash, err := service.ComputeContentHash(raw)
	if err != nil {
		return fmt.Errorf("compute meeting note content hash: %w", err)
	}
	env := &events.Envelope{
		Source: "anarlog_sessions", SourceID: spec.SessionID.String() + "@" + hash,
		Kind: events.KindMeetingNoteRecorded, Payload: raw,
		ObservedAt: accelerated.GetCurrentTime(),
	}
	accepted, _, rejections, _, err := h.ingestService.IngestBatch(ctx, []*events.Envelope{env}, []int{0}, &h.macHostID)
	if err != nil {
		return fmt.Errorf("ingest meeting note: %w", err)
	}
	if accepted != 1 {
		return fmt.Errorf("ingest meeting note: %d accepted (rejections=%v)", accepted, rejections)
	}
	root, err := repository.NewEventRepository(h.database.Queries).FindEventBySource(ctx, env.Source, env.SourceID)
	if err != nil {
		return fmt.Errorf("find meeting note root event: %w", err)
	}
	h.track(func(c *created) { c.eventIDs = append(c.eventIDs, root.ID) })
	return nil
}

// MacContactResult is the settled outcome of a Mac-contact replay.
type MacContactResult struct {
	ContactID uuid.UUID
	EntityID  string
	Matched   bool // false for MatchUnknown (unmatched import candidate)
}

// ReplayMacContacts feeds a synthetic external_contact.upserted envelope through
// IngestService.IngestBatch (hostLiveness=nil). For MatchSeeded the email matches
// the seeded contact → external_contact linked (match_status='matched'). For
// MatchUnknown → external_contact.match_status='unmatched' (the Imports queue).
// Settles synchronously inside the tx (no River cascade for the basic case).
//
// The host is the one the SPEC declares (factory.MacContactSpec.HostID) — usually
// the harness's revoked marker, but a declared world with a LIVE paired host
// builds its payloads against that host instead, because the stored row's
// host_id is what the per-host source-count route reads and Upsert never
// reassigns an existing owner.
func (h *Harness) ReplayMacContacts(ctx context.Context, contactID uuid.UUID, spec factory.MacContactSpec) (MacContactResult, error) {
	// raw external_contact.upserted root events carry no CRM contact id; track
	// the synthetic source so cleanup captures the root event.
	h.track(func(c *created) { c.addDirectSource(spec.Envelope.Source) })

	// Finalize the source_id to <entity_id>@<sha256(JCS(payload\host_id))>, the
	// invariant IngestBatch enforces for external_contact.upserted. The factory
	// (a leaf package) cannot import service to compute the hash, so the adapter
	// does it here over the factory-marshalled payload.
	hash, err := service.ComputeContentHash(spec.Envelope.Payload)
	if err != nil {
		return MacContactResult{}, fmt.Errorf("compute external_contact content hash: %w", err)
	}
	spec.Envelope.SourceID = spec.EntityID + "@" + hash

	hostID := spec.HostID
	accepted, _, rejections, _, err := h.ingestService.IngestBatch(ctx, []*events.Envelope{spec.Envelope}, []int{0}, &hostID)
	if err != nil {
		return MacContactResult{}, fmt.Errorf("ingest mac contact: %w", err)
	}
	if accepted == 0 {
		return MacContactResult{}, fmt.Errorf("ingest mac contact: 0 accepted (rejections=%v) — host wiring regression", rejections)
	}

	if spec.Intent == factory.MatchUnknown {
		predicate := func(ctx context.Context) (bool, error) {
			n, err := h.support.CountUnmatchedExternalContactBySourceID(ctx, spec.EntityID)
			return n > 0, err
		}
		if err := h.Settle(ctx, predicate, repository.InteractionSourceMessages); err != nil {
			return MacContactResult{}, err
		}
		return MacContactResult{EntityID: spec.EntityID, Matched: false}, nil
	}

	predicate := func(ctx context.Context) (bool, error) {
		n, err := h.support.CountMatchedExternalContactBySourceID(ctx, spec.EntityID)
		return n > 0, err
	}
	if err := h.Settle(ctx, predicate, repository.InteractionSourceMessages); err != nil {
		return MacContactResult{}, err
	}
	return MacContactResult{ContactID: contactID, EntityID: spec.EntityID, Matched: true}, nil
}

// IMessageResult is the settled outcome of an iMessage replay.
type IMessageResult struct {
	ContactID uuid.UUID
	Guid      string
	Matched   bool // false for MatchUnknown (stranded staging row)
}

// ReplayIMessage feeds a synthetic raw_message.received envelope through
// IngestService.IngestBatch (revoked host id, hostLiveness=nil, harness
// riverClient so the end-of-batch MessagingAggregateForContactArgs enqueue
// succeeds). For MatchSeeded the phone matches the seeded contact → matched
// interaction (via the messaging aggregate worker). For MatchUnknown →
// messages_message.matched_contact_id IS NULL (stranded, awaits rematch).
func (h *Harness) ReplayIMessage(ctx context.Context, contactID uuid.UUID, spec factory.IMessageSpec) (IMessageResult, error) {
	// raw_message.* root events carry no CRM contact id; track the source.
	h.track(func(c *created) { c.addDirectSource(spec.Envelope.Source) })

	accepted, _, rejections, _, err := h.ingestService.IngestBatch(ctx, []*events.Envelope{spec.Envelope}, []int{0}, &h.macHostID)
	if err != nil {
		return IMessageResult{}, fmt.Errorf("ingest imessage: %w", err)
	}
	if accepted == 0 {
		return IMessageResult{}, fmt.Errorf("ingest imessage: 0 accepted (rejections=%v) — host wiring regression", rejections)
	}

	if spec.Intent == factory.MatchUnknown {
		predicate := func(ctx context.Context) (bool, error) {
			n, err := h.support.CountStrandedMessagesMessageByGuid(ctx, spec.Guid)
			return n > 0, err
		}
		if err := h.Settle(ctx, predicate, repository.InteractionSourceMessages); err != nil {
			return IMessageResult{}, err
		}
		return IMessageResult{Guid: spec.Guid, Matched: false}, nil
	}

	// Gate A: THIS replay's staging row is linked to an interaction.
	if err := h.Settle(ctx, h.imessageSettled(spec.Guid), repository.InteractionSourceMessages); err != nil {
		return IMessageResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceMessages); err != nil {
		return IMessageResult{}, err
	}
	return IMessageResult{ContactID: contactID, Guid: spec.Guid, Matched: true}, nil
}

// IMessageBatchItem is one iMessage payload in a batch: the seeded contact it
// targets and the raw_message envelope. PairKey marks the two items of a
// promotion pair (0 = not part of one).
type IMessageBatchItem struct {
	ContactID uuid.UUID
	Spec      factory.IMessageSpec
	PairKey   int
}

// ReplayIMessageBatch drives N iMessage payloads through ONE IngestBatch call
// per dependency generation and settles once per generation. IngestBatch already
// enqueues the messaging aggregate ONCE per (contact, source) at the end of the
// batch, so a whole generation costs one aggregate pass per contact rather than
// one per payload. Items must be in chronological replay order.
func (h *Harness) ReplayIMessageBatch(ctx context.Context, items []IMessageBatchItem) (BatchResult, error) {
	const source = "imessage"

	entries, err := h.imessageBatchEntries(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}
	before, err := h.snapshotInteractionIDs(ctx, contactIDs)
	if err != nil {
		return res, err
	}

	// raw_message.* root events carry no CRM contact id; track the source so
	// cleanup captures them.
	h.track(func(c *created) { c.addDirectSource(items[0].Spec.Envelope.Source) })

	for _, generation := range partitionGenerations(entries) {
		envelopes := make([]*events.Envelope, 0, len(generation))
		guids := make([]string, 0, len(generation))
		indices := make([]int, 0, len(generation))
		for n, i := range generation {
			envelopes = append(envelopes, items[i].Spec.Envelope)
			guids = append(guids, items[i].Spec.Guid)
			indices = append(indices, n)
		}

		accepted, _, rejections, _, err := h.ingestService.IngestBatch(ctx, envelopes, indices, &h.macHostID)
		if err != nil {
			return res, h.drainPartial(ctx, source, repository.InteractionSourceMessages, contactIDs, fmt.Errorf("ingest imessage batch: %w", err))
		}
		if accepted != len(envelopes) {
			return res, h.drainPartial(ctx, source, repository.InteractionSourceMessages, contactIDs,
				fmt.Errorf("ingest imessage batch: %d of %d accepted (rejections=%v) — host wiring regression", accepted, len(envelopes), rejections))
		}
		res.SyncCalls++

		// Gate A is scoped to THIS generation's guids, never the whole batch.
		if err := h.Settle(ctx, h.imessageBatchSettled(guids), repository.InteractionSourceMessages); err != nil {
			return res, h.drainPartial(ctx, source, repository.InteractionSourceMessages, contactIDs, err)
		}
		res.SettleCalls++
	}

	res.Interactions = h.trackBatchInteractions(ctx, contactIDs, before)
	for _, contactID := range contactIDs {
		if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceMessages); err != nil {
			return res, err
		}
	}
	return res, nil
}

// imessageBatchSettled is the batch Gate A: every one of these guids has an
// interaction-linked staging row.
func (h *Harness) imessageBatchSettled(guids []string) gateA {
	want := int64(len(guids))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountSettledMessagesMessagesByGUIDs(ctx, guids)
		return n >= want, err
	}
}

// imessageBatchEntries projects the typed items into the source-neutral view.
// Direction and the addressed handle both come from the envelope: the kind
// carries the direction the inline ingest handler reads, and the payload carries
// the peer handle that decides whether the contact matches at all.
func (h *Harness) imessageBatchEntries(items []IMessageBatchItem) ([]batchEntry, error) {
	out := make([]batchEntry, 0, len(items))
	for i, it := range items {
		if it.Spec.Envelope == nil {
			return nil, fmt.Errorf("imessage: item %d has a nil envelope", i)
		}
		handle, err := imessagePeerHandle(it.Spec.Envelope)
		if err != nil {
			return nil, fmt.Errorf("imessage: item %d (%s): %w", i, it.Spec.Guid, err)
		}
		out = append(out, batchEntry{
			contactID:     it.ContactID,
			identifier:    it.Spec.Guid,
			seeded:        it.Spec.Intent == factory.MatchSeeded,
			outbound:      it.Spec.Envelope.Kind == events.KindRawMessageSent,
			pairKey:       it.PairKey,
			addressed:     handle,
			addressedType: identity.DetectIdentifierType(handle),
		})
	}
	return out, nil
}

// imessagePeerHandle decodes the peer handle out of a raw_message envelope. The
// sent and received kinds share a field shape but decode to distinct types, so
// events.Unmarshal's kind-vs-type check requires dispatching on the kind.
func imessagePeerHandle(env *events.Envelope) (string, error) {
	switch env.Kind {
	case events.KindRawMessageSent:
		var payload events.RawMessageSentPayload
		if err := events.Unmarshal(env, &payload); err != nil {
			return "", err
		}
		return payload.PeerHandle, nil
	case events.KindRawMessageReceived:
		var payload events.RawMessageReceivedPayload
		if err := events.Unmarshal(env, &payload); err != nil {
			return "", err
		}
		return payload.PeerHandle, nil
	default:
		return "", fmt.Errorf("unexpected envelope kind %q for an iMessage payload", env.Kind)
	}
}

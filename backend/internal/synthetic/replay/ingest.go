package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
)

// MacContactResult is the settled outcome of a Mac-contact replay.
type MacContactResult struct {
	ContactID uuid.UUID
	EntityID  string
	Matched   bool // false for MatchUnknown (unmatched import candidate)
}

// ReplayMacContacts feeds a synthetic external_contact.upserted envelope through
// IngestService.IngestBatch (revoked synthetic host id, hostLiveness=nil). For
// MatchSeeded the email matches the seeded contact → external_contact linked
// (match_status='matched'). For MatchUnknown → external_contact.match_status=
// 'unmatched' (the Imports queue). Settles synchronously inside the tx (no River
// cascade for the basic case).
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

	accepted, _, rejections, _, err := h.ingestService.IngestBatch(ctx, []*events.Envelope{spec.Envelope}, []int{0}, &h.macHostID)
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
	return IMessageResult{ContactID: contactID, Guid: spec.Guid, Matched: true}, nil
}

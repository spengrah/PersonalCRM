package service

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
)

// rematchMethodsToRefs converts service.Method (from diffNewMethods) to
// the event-layer ref type.
func rematchMethodsToRefs(methods []Method) []events.ContactMethodRef {
	out := make([]events.ContactMethodRef, len(methods))
	for i, m := range methods {
		out[i] = events.ContactMethodRef{Type: m.Type, Value: m.Value}
	}
	return out
}

// buildContactMethodsAddedEnvelope composes a ready-to-publish envelope
// for the given (contactID, methods, jobID) tuple. jobID doubles as the
// event's SourceID so a publisher retry under the same intent dedupes
// at the event.source_id unique index.
func buildContactMethodsAddedEnvelope(
	source string,
	contactID uuid.UUID,
	methods []events.ContactMethodRef,
	jobID uuid.UUID,
) (*events.Envelope, error) {
	payload, err := events.Marshal(events.KindContactMethodsAdded, events.ContactMethodsAddedPayload{
		Version:      1,
		ContactID:    contactID,
		Methods:      methods,
		RematchJobID: jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal contact_methods.added: %w", err)
	}
	return &events.Envelope{
		Source:     source,
		SourceID:   jobID.String(),
		Kind:       events.KindContactMethodsAdded,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}, nil
}

// RescanRematch triggers a rematch over ALL methods currently attached
// to a contact. Used by POST /rematch/contacts/:id/rescan. Publishes
// one contact_methods.added via the event bus (same path as
// CreateContact / UpdateContact), registers the in-memory job entry,
// and returns the jobID for the client poll loop.
//
// Returns uuid.Nil when the contact has no methods (200 with
// rematch_job_id=null, matching the handler contract). Returns
// db.ErrNotFound when the contact doesn't exist.
//
// Uses bus.Publish (not PublishTx) because Rescan has no sibling
// writes to atomically couple — the bus opens its own short-lived tx
// covering event-insert + river-InsertTx.
func (s *ContactService) RescanRematch(ctx context.Context, contactID uuid.UUID) (uuid.UUID, error) {
	if s.bus == nil {
		return uuid.Nil, fmt.Errorf("rescan rematch: event bus not wired")
	}
	contact, err := s.GetContact(ctx, contactID)
	if err != nil {
		return uuid.Nil, err // propagates db.ErrNotFound
	}
	// Filter to eligible methods so a contact with no matchable methods
	// (no registered handler for any method type) returns uuid.Nil
	// instead of enqueueing a no-op rematch job.
	var eligible []Method
	if s.rematchRegistry != nil {
		all := make([]Method, 0, len(contact.Methods))
		for _, m := range contact.Methods {
			all = append(all, Method{Type: m.Type, Value: m.ValueNormalized})
		}
		eligible = s.rematchRegistry.EligibleMethods(all)
	}
	if len(eligible) == 0 {
		return uuid.Nil, nil
	}
	jobID := uuid.New()
	env, err := buildContactMethodsAddedEnvelope("manual", contactID, rematchMethodsToRefs(eligible), jobID)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.bus.Publish(ctx, env); err != nil {
		return uuid.Nil, fmt.Errorf("publish contact_methods.added: %w", err)
	}
	s.rematchRegistry.RegisterPending(jobID, contactID, eligible)
	return jobID, nil
}

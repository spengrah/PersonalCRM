package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// verifyCallInvariants enforces the cross-field consistency properties
// for call.* envelopes. Returns a *IngestPerEventRejection (caller fills
// in Index) on any violation, nil on success.
//
// Properties checked:
//  1. Payload decodes cleanly into CallPayload.
//  2. payload.HostID matches the authenticated host (no host
//     cross-impersonation).
//  3. env.Source is in the call family's allowed sources.
//  4. payload.Source matches env.Source.
//  5. payload.CallUniqueID is non-empty and equals env.SourceID (so
//     event-log dedup key and staging-table dedup key are the same
//     string).
//  6. payload.PeerNormalized matches a fresh re-canonicalization of
//     payload.PeerHandle. The daemon canonicalized at emit-time; the Pi
//     re-verifies defensively — a mismatch indicates a compromised /
//     buggy daemon and we refuse to trust its normalized value.
func verifyCallInvariants(env *events.Envelope, authenticatedHostID uuid.UUID) *IngestPerEventRejection {
	var p events.CallPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode call payload: %s", err.Error()),
		}
	}
	if p.HostID != authenticatedHostID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload host_id does not match authenticated host",
		}
	}
	if _, ok := callAllowedSources[env.Source]; !ok {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("env.source %q not supported on call kinds", env.Source),
		}
	}
	if p.Source != env.Source {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload source does not match envelope source",
		}
	}
	if p.CallUniqueID == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload call_unique_id is required",
		}
	}
	if p.CallUniqueID != env.SourceID {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload call_unique_id must equal envelope source_id",
		}
	}
	if p.PeerNormalized == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "payload peer_normalized is required",
		}
	}
	// Defence-in-depth: re-canonicalize peer_handle on the Pi and verify
	// it matches what the daemon emitted. A divergence rejects the
	// event; either the daemon used a different canonicalization or the
	// payload was tampered with.
	idType := identity.DetectIdentifierType(p.PeerHandle)
	repaNormalized := identity.Normalize(p.PeerHandle, idType)
	if repaNormalized == "" {
		return &IngestPerEventRejection{
			Code:    ingestRejectIdentityMatchFailed,
			Message: fmt.Sprintf("peer_handle %q is not normalizable", p.PeerHandle),
		}
	}
	if repaNormalized != p.PeerNormalized {
		return &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: fmt.Sprintf("payload peer_normalized %q does not match Pi re-canonicalization %q", p.PeerNormalized, repaNormalized),
		}
	}
	return nil
}

// handleCall runs the per-event domain logic for call.* envelopes inside
// the per-event savepoint:
//
//  1. Decode the payload (already cross-checked by verifyCallInvariants).
//  2. Identity-match the canonicalized peer; unmatched → reject so the
//     daemon retries (matches handleRawMessage semantics).
//  3. Upsert the phone_call staging row.
//  4. Apply the interaction-creation decision table (content-delivered
//     cadence; spec §`phone_calls` source).
//  5. If qualified: RecordInteractionTx → publish interaction.recorded
//     in the same tx → inline cadence.HandleEvent → inline
//     followUp.HandleEvent. Mirrors consumer/interaction_recorder.go's
//     HandleEvent. Returns the follow-up's post-commit closure (if
//     any) so the caller can invoke it after tx.Commit.
//  6. MarkProcessedTx with the interaction_id (or nil for missed-
//     no-voicemail rows).
//
// Returns (postCommit, rejection). postCommit may be nil even on
// success (no follow-up refresh branch fired). rejection != nil
// indicates a domain refusal; the caller rolls back the savepoint.
//
// isOutbound is true for KindCallSent, false for KindCallReceived. Used
// to apply the decision table without re-checking the kind.
func (s *IngestService) handleCall(
	ctx context.Context,
	tx pgx.Tx,
	env *events.Envelope,
	hostID uuid.UUID,
	isOutbound bool,
) (func(context.Context), *IngestPerEventRejection) {
	if s.identity == nil || s.phoneCalls == nil || s.contactRecorder == nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured for call processing",
		}
	}

	var p events.CallPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvalid,
			Message: fmt.Sprintf("decode call payload: %s", err.Error()),
		}
	}

	// Identity match using the daemon-supplied normalized peer. The
	// MatchOrCreateTx call uses RawIdentifier; identity.Normalize is
	// invoked inside the match path so the raw + type pair are
	// sufficient. The result.ContactID is nil on unmatched peer.
	//
	// FailEmpty: a call carries exactly one peer handle, so an
	// un-normalizable handle is fatal data — reject the event (the
	// daemon holds its cursor for retry) rather than dropping it.
	idType := identity.DetectIdentifierType(p.PeerHandle)
	matchReq := MatchRequest{
		RawIdentifier: p.PeerHandle,
		Type:          idType,
		Source:        env.Source,
		SourceID:      &p.CallUniqueID,
	}
	matchResult, err := s.identity.MatchOrCreateTx(ctx, tx, matchReq, NormalizationFailEmpty)
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectIdentityMatchFailed,
			Message: fmt.Sprintf("identity match: %s", err.Error()),
		}
	}
	if matchResult.ContactID == nil {
		// Daemon-side filter (known-identifiers) should have prevented
		// this in practice; rejecting matches handleRawMessage semantics
		// so the daemon doesn't advance its cursor.
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectIdentityMatchFailed,
			Message: fmt.Sprintf("no contact matched for peer %q", p.PeerHandle),
		}
	}
	contactID := *matchResult.ContactID

	// Normalize per-direction invariants before staging. Outbound rows
	// ignore the daemon's `answered` value (force NULL) and force
	// has_voicemail FALSE. The daemon should already comply; we enforce
	// here as defence-in-depth.
	var direction string
	var answeredForStaging *bool
	hasVoicemail := p.HasVoicemail
	if isOutbound {
		direction = repository.PhoneCallDirectionOutbound
		answeredForStaging = nil
		hasVoicemail = false
	} else {
		direction = repository.PhoneCallDirectionInbound
		answeredForStaging = p.Answered
	}

	stagingCall, err := s.phoneCalls.UpsertCallTx(ctx, tx, repository.UpsertPhoneCallParams{
		CallUniqueID:     p.CallUniqueID,
		PeerHandle:       p.PeerHandle,
		PeerNormalized:   p.PeerNormalized,
		Service:          p.Service,
		Direction:        direction,
		Answered:         answeredForStaging,
		HasVoicemail:     hasVoicemail,
		DurationSeconds:  p.DurationSeconds,
		StartedAt:        p.StartedAt,
		MatchedContactID: &contactID,
		MacHostID:        &hostID,
	})
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("upsert phone_call: %s", err.Error()),
		}
	}

	// Apply the interaction-creation decision table — content-delivered
	// cadence. Rows return nil interaction when no interaction should
	// be created (missed inbound with no voicemail); the staging row
	// still exists for audit + the future contact-timeline UI's union
	// projection.
	createInteraction, interactionDirection, description := decideCallInteraction(
		isOutbound,
		answeredForStaging,
		hasVoicemail,
		p.DurationSeconds,
		p.Service,
	)

	if !createInteraction {
		// Mark the staging row processed with interaction_id = NULL.
		if err := s.phoneCalls.MarkProcessedTx(ctx, tx, repository.MarkProcessedParams{
			ID:            stagingCall.ID,
			InteractionID: nil,
		}); err != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectStagingUpsertFailed,
				Message: fmt.Sprintf("mark phone_call processed: %s", err.Error()),
			}
		}
		return nil, nil
	}

	// Qualified — record an interaction, publish interaction.recorded,
	// inline-apply cadence + follow-up. Mirrors
	// consumer/interaction_recorder.go HandleEvent.
	descCopy := description
	recReq := repository.RecordInteractionRequest{
		ContactID:   contactID,
		Source:      repository.InteractionSourcePhoneCalls,
		SourceRef:   &p.CallUniqueID,
		OccurredAt:  p.StartedAt,
		Description: &descCopy,
		Direction:   interactionDirection,
	}
	// Resolve the call venue from the call's unique id, set atomically with the
	// insert. Best-effort: a resolution error leaves venue_id NULL rather than
	// rejecting the call.
	if s.venue != nil {
		venueID, venueErr := s.venue.ResolveVenueForInteractionTx(
			ctx, tx, repository.InteractionSourcePhoneCalls, repository.VenueKindCall, p.CallUniqueID, "")
		if venueErr != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectStagingUpsertFailed,
				Message: fmt.Sprintf("resolve call venue: %s", venueErr.Error()),
			}
		}
		recReq.VenueID = &venueID
	}

	res, err := s.contactRecorder.RecordInteractionTx(ctx, tx, true, recReq)
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("record interaction: %s", err.Error()),
		}
	}
	if res == nil || res.Interaction == nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: "record interaction returned nil result",
		}
	}

	// Link the staging row to the (existing or newly-created)
	// interaction. Done BEFORE the publish so a publish failure rolls
	// back the link via the savepoint and avoids stranding a
	// phone_call row with a dangling interaction_id.
	if err := s.phoneCalls.MarkProcessedTx(ctx, tx, repository.MarkProcessedParams{
		ID:            stagingCall.ID,
		InteractionID: &res.Interaction.ID,
	}); err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("mark phone_call processed: %s", err.Error()),
		}
	}

	// Replay: skip the interaction.recorded emit. The staging row is
	// linked to the existing interaction; cadence/follow-up already
	// fired on the original write.
	if res.IsReplay {
		return nil, nil
	}

	// Fresh write: build the V3 InteractionRecordedPayload + envelope
	// and publish in the same tx.
	recordedPayload, err := buildCallRecordedPayload(res.Interaction, interactionDirection, recReq, res.PrevCadence, res.CadenceAtEmit)
	if err != nil {
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("marshal interaction.recorded: %s", err.Error()),
		}
	}
	recordedEnv := &events.Envelope{
		Source:     env.Source,
		SourceID:   res.Interaction.ID.String(),
		Kind:       events.KindInteractionRecorded,
		Payload:    recordedPayload,
		ObservedAt: p.StartedAt,
	}
	if s.bus == nil {
		// Production wires the bus; this guard exists for unit tests
		// that don't reach the publish path. Reaching it on a fresh
		// write without a bus is a wiring bug.
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectPayloadInvariant,
			Message: "ingest service was not configured with an event bus",
		}
	}
	if err := s.bus.PublishTx(ctx, tx, recordedEnv); err != nil {
		// PublishTx infrastructure failures are unexpected; surface
		// as a rejection so the savepoint rolls back. The outer batch
		// continues with the next event.
		return nil, &IngestPerEventRejection{
			Code:    ingestRejectStagingUpsertFailed,
			Message: fmt.Sprintf("publish interaction.recorded: %s", err.Error()),
		}
	}

	// Inline cadence apply — same tx, same atomicity. CadenceUpdater
	// looks up the contact, applies the direction-driven bump
	// (last_contacted / last_outreach_at), and marks the event
	// consumed via event_consumer_claim so a queued re-delivery (if
	// the event router ever fires one) becomes a no-op.
	if s.cadence != nil {
		if err := s.cadence.HandleEvent(ctx, tx, recordedEnv); err != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectStagingUpsertFailed,
				Message: fmt.Sprintf("inline apply cadence: %s", err.Error()),
			}
		}
	}

	// Inline follow-up apply. Returns a post-commit closure (non-nil on
	// the refresh branch). For v1.5 phone_calls has no Todoist
	// integration, so the closure is expected to be nil — but the
	// machinery is wired through correctly so when a follow-up source
	// eventually attaches to phone_calls contacts the path works.
	var postCommit func(context.Context)
	if s.followUp != nil {
		pc, err := s.followUp.HandleEvent(ctx, tx, recordedEnv)
		if err != nil {
			return nil, &IngestPerEventRejection{
				Code:    ingestRejectStagingUpsertFailed,
				Message: fmt.Sprintf("inline apply follow-up: %s", err.Error()),
			}
		}
		postCommit = pc
	}

	return postCommit, nil
}

// decideCallInteraction applies the content-delivered decision table
// (spec §`phone_calls` source). Returns
// (createInteraction, direction, description).
//
//	| direction | answered | duration | has_voicemail | create | direction      |
//	|-----------|----------|----------|---------------|--------|----------------|
//	| inbound   | true     | (any)    | (any)         | yes    | inbound        |
//	| inbound   | false    | (any)    | true          | yes    | inbound        |
//	| inbound   | false/NULL| (any)   | false         | no     | —              |
//	| outbound  | (ignored)| > 0      | (forced false)| yes    | outbound       |
//	| outbound  | (ignored)| 0        | (forced false)| yes    | outbound       |
//
// Note: outbound ALWAYS creates an interaction (the user's
// "attempted to reach" signal). Description distinguishes
// connected / voicemail / missed for the future contact-timeline UI.
func decideCallInteraction(
	isOutbound bool,
	answered *bool,
	hasVoicemail bool,
	durationSeconds int32,
	service string,
) (createInteraction bool, direction string, description string) {
	serviceLabel := callServiceLabel(service)
	if isOutbound {
		if durationSeconds > 0 {
			return true, repository.InteractionDirectionOutbound,
				fmt.Sprintf("%s call (%d sec)", serviceLabel, durationSeconds)
		}
		return true, repository.InteractionDirectionOutbound,
			fmt.Sprintf("%s call (missed)", serviceLabel)
	}
	// inbound
	answeredVal := answered != nil && *answered
	if answeredVal {
		return true, repository.InteractionDirectionInbound,
			fmt.Sprintf("%s call (%d sec)", serviceLabel, durationSeconds)
	}
	if hasVoicemail {
		return true, repository.InteractionDirectionInbound,
			fmt.Sprintf("voicemail (%d sec)", durationSeconds)
	}
	// Missed-no-voicemail: no interaction.
	return false, "", ""
}

// callServiceLabel renders a phone_call.service enum value as a human-
// readable label for interaction.description. Frozen mapping; matches
// the daemon-side ServiceDerivation table.
func callServiceLabel(service string) string {
	switch service {
	case repository.PhoneCallServiceFaceTimeAudio:
		return "FaceTime audio"
	case repository.PhoneCallServiceFaceTimeVideo:
		return "FaceTime video"
	case repository.PhoneCallServiceVoice:
		return "voice"
	default:
		return service
	}
}

// buildCallRecordedPayload constructs the V3 InteractionRecordedPayload
// for a phone_calls-source interaction. Mirrors the recorder's
// marshalRecordedPayload but lives here so the service package doesn't
// import consumer. CadenceUpdater and FollowUpManager both consume the
// V3 shape; PrevCadenceSnapshot + PrevCadenceValue come from
// ContactService.RecordInteractionTx's publishesEvent=true output.
func buildCallRecordedPayload(
	interaction *repository.Interaction, direction string, req repository.RecordInteractionRequest,
	prev *repository.ContactCadenceFields, cadenceAtEmit *string,
) (json.RawMessage, error) {
	if interaction == nil {
		return nil, errors.New("buildCallRecordedPayload: nil interaction")
	}
	payload := events.InteractionRecordedPayload{
		Version:          3,
		ContactID:        interaction.ContactID,
		InteractionID:    interaction.ID,
		Direction:        direction,
		OccurredAt:       interaction.OccurredAt,
		Source:           interaction.Source,
		SourceRef:        req.SourceRef,
		PrevCadenceValue: cadenceAtEmit,
		// SuppressFollowUp not set: only task.completed (V2+) sets this
		// for kind=send. Phone calls never set it.
	}
	if prev != nil {
		payload.PrevCadenceSnapshot = &events.CadenceFieldsSnapshot{
			LastContacted:  prev.LastContacted,
			LastOutreachAt: prev.LastOutreachAt,
			LastResponseAt: prev.LastResponseAt,
			ContactBy:      prev.ContactBy,
		}
	}
	return events.Marshal(events.KindInteractionRecorded, payload)
}

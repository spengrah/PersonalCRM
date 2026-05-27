package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Ingestion limits. Chosen per plan/spec §3.5:
//   - maxBatchSize: hard ceiling on the number of events per POST.
//   - maxIngestBodyBytes: 8 MiB body cap wrapped via http.MaxBytesReader
//     at the top of the handler — DoS mitigation.
//   - maxSourceLen / maxSourceIDLen: bounded keys for the partial unique
//     index; avoids pathological btree entries.
//   - maxPayloadBytes: 64 KiB guard so even JSON-valid payloads can't
//     pressure jsonb storage.
const (
	maxBatchSize       = 500
	maxIngestBodyBytes = 8 << 20 // 8 MiB
	maxSourceLen       = 64
	maxSourceIDLen     = 255
	maxPayloadBytes    = 64 << 10 // 64 KiB
)

// Ingestion error codes surfaced in the per-event errors[] array. HTTP
// status for the batch remains 200 when only per-event rejections occur;
// request-level failures (batch too large, malformed JSON, body too big)
// use the standard api.Send* helpers.
const (
	ingestCodeMissingField    = "MISSING_FIELD"
	ingestCodeFieldTooLong    = "FIELD_TOO_LONG"
	ingestCodeUnknownKind     = "UNKNOWN_KIND"
	ingestCodePayloadInvalid  = "PAYLOAD_INVALID"
	ingestCodePayloadTooLarge = "PAYLOAD_TOO_LARGE"
)

// eventsRawMessageMaxKnownVersion is the highest raw_message.* payload
// Version the Pi knows how to process. Daemons that ship a Version
// higher than this are rejected at the handler with PAYLOAD_INVALID so
// the operator sees a clear "upgrade Pi" error rather than a silent
// dropped-field decode.
const eventsRawMessageMaxKnownVersion = 1

// eventsExternalContactMaxKnownVersion is the highest
// external_contact.* payload Version the Pi knows how to process. Same
// "upgrade Pi" rejection semantics as raw_message.* (above).
const eventsExternalContactMaxKnownVersion = 1

// eventsMeetingNoteMaxKnownVersion is the highest meeting_note.*
// payload Version the Pi knows how to process. Same "upgrade Pi"
// rejection semantics as raw_message.* / external_contact.* above.
const eventsMeetingNoteMaxKnownVersion = 1

// IngestHandler exposes POST /api/v1/ingest/events. Registered only when
// cfg.Features.EnableEventBusIngest is true (spec §3.9).
type IngestHandler struct {
	ingestService *service.IngestService
}

// NewIngestHandler builds an IngestHandler over the given service.
func NewIngestHandler(s *service.IngestService) *IngestHandler {
	return &IngestHandler{ingestService: s}
}

// IngestEventRequest matches the per-event wire shape in spec §3.5.
//
// Fields are deliberately NOT tagged with `binding:"required"`: a single
// bad event (missing source, unknown kind, empty payload, missing
// observed_at) must NOT cause gin's bind step to fail the whole batch
// with 400. Per-event validation runs in validateIngestEvent after bind
// and surfaces each bad event in the response's errors[] — spec §3.5
// "the batch continues" contract.
//
// ObservedAt is *time.Time so (a) an absent key is distinguishable from
// a zero timestamp; (b) a syntactically invalid RFC3339 string still
// fails the whole batch at bind (that's a wire-protocol error, not a
// per-event logical error).
type IngestEventRequest struct {
	Source     string          `json:"source"`
	SourceID   string          `json:"source_id,omitempty"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	ObservedAt *time.Time      `json:"observed_at"`
}

// IngestBatchRequest is the top-level POST body.
//
// The Events slice is NOT `binding:"required"` — gin treats an explicit
// empty array as satisfying that tag, so we'd check length explicitly
// anyway. A missing `events` key decodes to a nil slice, which falls
// into the same empty-batch path.
type IngestBatchRequest struct {
	Events []IngestEventRequest `json:"events"`
}

// IngestError is a per-event validation failure reported in the response.
// Index is the 0-based position in the original request's events array.
type IngestError struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IngestResponse matches the spec §3.5 happy-path shape literally —
// deliberately NOT wrapped in api.APIResponse. External daemons are
// contract consumers and the wrapping would be a spec deviation.
//
// NeedsAttention is populated by the meeting_note.recorded inline
// handler when a session lands in a state requiring user attention
// (conflict_pending or orphan_needs_review). Emitted with `omitempty`
// so existing daemons unaware of the field see a backward-compatible
// response; the Mac daemon consumes it in phase 2 PR 6.
type IngestResponse struct {
	Accepted       int                  `json:"accepted"`
	Duplicate      int                  `json:"duplicate"`
	Rejected       int                  `json:"rejected"`
	Errors         []IngestError        `json:"errors"`
	NeedsAttention []NeedsAttentionItem `json:"needs_attention,omitempty"`
}

// NeedsAttentionItem is the per-session attention record surfaced in
// IngestResponse. SessionID is the anarlog session UUID; Reason is
// one of "orphan" | "conflict" (mirrors the service-layer constants).
type NeedsAttentionItem struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// IngestEvents is the HTTP handler for POST /api/v1/ingest/events.
//
// Flow:
//  1. Wrap the body in http.MaxBytesReader (8 MiB cap). Reject oversize
//     bodies with 413 before parsing JSON.
//  2. Bind the JSON body. Malformed → 400.
//  3. Enforce batch shape: non-empty, ≤ maxBatchSize.
//  4. Validate each event (required fields, length bounds, known kind,
//     payload shape, raw_message-specific field checks). Failures land
//     in errors[]; the batch continues.
//  5. If any events pass validation, forward them to the service as a
//     single atomic batch. Return spec-shaped success response. If the
//     service errors (unexpected DB failure), return 500.
//
// Auth dispatch: the route runs behind IngestAuthMiddleware which
// branches on the X-Mac-Host-ID header. When present and validated by
// MacHostAuthMiddleware, the parsed UUID is stashed in gin context and
// the handler reads it via auth.MacHostIDFromContext to pass through to
// the service (so raw_message.* events can be processed and stamped
// with mac_host_id). Absent header → API-key path → hostID is nil.
func (h *IngestHandler) IngestEvents(c *gin.Context) {
	// Body size cap. MaxBytesReader returns *http.MaxBytesError (Go 1.21+)
	// which propagates through json.Decoder without re-wrapping; detect it
	// with errors.As rather than string-matching the message.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxIngestBodyBytes)

	var req IngestBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			api.SendError(c, http.StatusRequestEntityTooLarge, api.ErrCodeValidation,
				"Request body too large",
				fmt.Sprintf("body exceeds %d bytes", maxIngestBodyBytes))
			return
		}
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}

	// Empty batch: spec-silent; plan Decision 9 chose 400 for client-bug
	// visibility. gin's binding:"required" tag catches nil/missing events
	// but NOT an explicit empty array, so check here.
	if len(req.Events) == 0 {
		api.SendValidationError(c, "events must be non-empty", "")
		return
	}

	if len(req.Events) > maxBatchSize {
		api.SendValidationError(c,
			fmt.Sprintf("batch exceeds maximum size of %d events", maxBatchSize),
			fmt.Sprintf("got %d events", len(req.Events)))
		return
	}

	// Validate each event pre-tx. Valid ones become envelopes to publish.
	// originalIndices[i] is the caller-original request-array position of
	// envsToIngest[i]; passed to the service so per-event rejections
	// surface with caller-original indexing.
	envsToIngest := make([]*events.Envelope, 0, len(req.Events))
	originalIndices := make([]int, 0, len(req.Events))
	ingestErrors := make([]IngestError, 0)
	for i, ev := range req.Events {
		if ierr := validateIngestEvent(i, ev); ierr != nil {
			ingestErrors = append(ingestErrors, *ierr)
			continue
		}
		envsToIngest = append(envsToIngest, &events.Envelope{
			Source:     ev.Source,
			SourceID:   ev.SourceID,
			Kind:       events.Kind(ev.Kind),
			Payload:    ev.Payload,
			ObservedAt: *ev.ObservedAt, // validator guarantees non-nil + non-zero
		})
		originalIndices = append(originalIndices, i)
	}

	// Host ID is set by MacHostAuthMiddleware when the host-auth path
	// fired; nil on the global-API-key path. Service uses it to enforce
	// the per-path kind allowlist and stamp staging rows.
	var hostID *uuid.UUID
	if id, ok := auth.MacHostIDFromContext(c); ok {
		hostID = &id
	}

	// All events failed validation — still a success response (spec §3.5
	// distinguishes "validation failure per event" from "batch-level
	// failure"). No tx work needed.
	if len(envsToIngest) == 0 {
		logger.Info().
			Int("batch_size", len(req.Events)).
			Int("accepted", 0).
			Int("duplicate", 0).
			Int("rejected", len(ingestErrors)).
			Msg("event batch ingested (all rejected)")
		c.JSON(http.StatusOK, IngestResponse{
			Accepted:  0,
			Duplicate: 0,
			Rejected:  len(ingestErrors),
			Errors:    ingestErrors,
		})
		return
	}

	accepted, duplicate, perEventRejections, needsAttention, err := h.ingestService.IngestBatch(c.Request.Context(), envsToIngest, originalIndices, hostID)
	if err != nil {
		// Host revoked between auth-middleware validation and the
		// batch's tx-internal FOR UPDATE lock — return 401 so the
		// daemon stops retrying. Matches the cursor-commit precedent
		// (MacHostHandler.CommitCursor).
		if errors.Is(err, service.ErrHostRevokedDuringBatch) {
			api.SendError(c, http.StatusUnauthorized, "UNKNOWN_HOST",
				"host revoked during ingest batch", "")
			return
		}
		logger.Error().
			Err(err).
			Int("batch_size", len(req.Events)).
			Int("validated_batch_size", len(envsToIngest)).
			Msg("event batch ingest failed (tx rolled back)")
		api.SendInternalError(c, "Failed to ingest events")
		return
	}

	for _, r := range perEventRejections {
		ingestErrors = append(ingestErrors, IngestError{
			Index:   r.Index,
			Code:    r.Code,
			Message: r.Message,
		})
	}

	// Map the service-layer NeedsAttentionItem to the handler-layer
	// JSON shape. omitempty on the response field means nil/empty
	// slice produces a wire-compatible response for pre-PR-6 daemons.
	var responseNeedsAttention []NeedsAttentionItem
	if len(needsAttention) > 0 {
		responseNeedsAttention = make([]NeedsAttentionItem, len(needsAttention))
		for i, na := range needsAttention {
			responseNeedsAttention[i] = NeedsAttentionItem{
				SessionID: na.SessionID,
				Reason:    na.Reason,
			}
		}
	}

	logger.Info().
		Int("batch_size", len(req.Events)).
		Int("accepted", accepted).
		Int("duplicate", duplicate).
		Int("rejected", len(ingestErrors)).
		Int("needs_attention", len(responseNeedsAttention)).
		Msg("event batch ingested")

	c.JSON(http.StatusOK, IngestResponse{
		Accepted:       accepted,
		Duplicate:      duplicate,
		Rejected:       len(ingestErrors),
		Errors:         ingestErrors,
		NeedsAttention: responseNeedsAttention,
	})
}

// validateIngestEvent runs the per-event validation chain. Returns nil
// when the event passes; otherwise returns a populated *IngestError. The
// caller appends rejected events to the response's errors[] array.
//
// This function owns ALL per-event rejections — no binding:"required" tag
// fires at gin-bind time, because that would 400 the whole batch and
// violate spec §3.5's "batch continues" contract.
func validateIngestEvent(index int, ev IngestEventRequest) *IngestError {
	if ev.Source == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField, Message: "source is required"}
	}
	if len(ev.Source) > maxSourceLen {
		return &IngestError{Index: index, Code: ingestCodeFieldTooLong,
			Message: fmt.Sprintf("source exceeds %d chars", maxSourceLen)}
	}
	if len(ev.SourceID) > maxSourceIDLen {
		return &IngestError{Index: index, Code: ingestCodeFieldTooLong,
			Message: fmt.Sprintf("source_id exceeds %d chars", maxSourceIDLen)}
	}
	if ev.Kind == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField, Message: "kind is required"}
	}
	// *time.Time lets us distinguish "observed_at absent" (nil pointer)
	// from "observed_at: 0001-01-01..." (IsZero). Both are rejected with
	// MISSING_FIELD.
	if ev.ObservedAt == nil || ev.ObservedAt.IsZero() {
		return &IngestError{Index: index, Code: ingestCodeMissingField, Message: "observed_at is required"}
	}
	if len(ev.Payload) == 0 {
		return &IngestError{Index: index, Code: ingestCodeMissingField, Message: "payload is required"}
	}
	if len(ev.Payload) > maxPayloadBytes {
		return &IngestError{Index: index, Code: ingestCodePayloadTooLarge,
			Message: fmt.Sprintf("payload exceeds %d bytes", maxPayloadBytes)}
	}

	kind := events.Kind(ev.Kind)
	if !events.IsKnownKind(kind) {
		return &IngestError{Index: index, Code: ingestCodeUnknownKind,
			Message: fmt.Sprintf("unknown kind %q", ev.Kind)}
	}

	// Structural payload check via reflection-backed helper. See
	// events.ValidatePayload for the exact contract (lenient unknown
	// fields; absent fields decode to zero values). ValidatePayload also
	// rejects a literal JSON `null` payload — we don't want to persist a
	// row whose payload decodes to an all-zero canonical struct.
	tmp := &events.Envelope{Kind: kind, Payload: ev.Payload}
	if err := events.ValidatePayload(tmp); err != nil {
		return &IngestError{
			Index:   index,
			Code:    ingestCodePayloadInvalid,
			Message: fmt.Sprintf("payload failed structural validation: %s", err.Error()),
		}
	}

	// Raw_message.* kinds require strict field-level checks beyond the
	// lenient ValidatePayload contract. Zero-valued chat_id, peer_handle,
	// or sent_at would cause downstream identity-match + staging-upsert
	// to fail mid-tx in non-obvious ways. Catch them at the handler.
	if kind == events.KindRawMessageReceived || kind == events.KindRawMessageSent {
		if ev.SourceID == "" {
			return &IngestError{
				Index:   index,
				Code:    ingestCodeMissingField,
				Message: "source_id is required for raw_message.* kinds (must equal payload.guid)",
			}
		}
		if rerr := validateRawMessagePayload(index, ev); rerr != nil {
			return rerr
		}
	}

	// external_contact.* kinds: the payload version envelope is the
	// daemon's "upgrade Pi" signal; reject zero/missing or too-high so
	// the operator sees a clear PAYLOAD_INVALID rather than a silent
	// dropped-field decode. The service-layer verifier enforces the
	// source_id regex shape and source-allowlist (we don't duplicate
	// that here).
	if kind == events.KindExternalContactUpserted || kind == events.KindExternalContactDeleted {
		if ev.SourceID == "" {
			return &IngestError{
				Index:   index,
				Code:    ingestCodeMissingField,
				Message: "source_id is required for external_contact.* kinds",
			}
		}
		if rerr := validateExternalContactPayloadVersion(index, kind, ev); rerr != nil {
			return rerr
		}
	}

	// meeting_note.* kinds: same "upgrade Pi" version-envelope check as
	// the other daemon-emitted kinds. ValidatePayload already enforces
	// source_id parses as a UUID and host_id is non-zero — we add the
	// payload-version check + empty source_id guard here so the daemon
	// gets a clear MISSING_FIELD / PAYLOAD_INVALID rather than a
	// service-layer PAYLOAD_INVARIANT for the same root cause.
	if kind == events.KindMeetingNoteRecorded || kind == events.KindMeetingNoteDeleted {
		if ev.SourceID == "" {
			return &IngestError{
				Index:   index,
				Code:    ingestCodeMissingField,
				Message: "source_id is required for meeting_note.* kinds",
			}
		}
		if rerr := validateMeetingNotePayloadVersion(index, kind, ev); rerr != nil {
			return rerr
		}
	}

	return nil
}

// validateExternalContactPayloadVersion enforces the version envelope
// on both external_contact.* kinds. Runs after ValidatePayload has
// already passed (so the payload decodes into the typed struct).
func validateExternalContactPayloadVersion(index int, kind events.Kind, ev IngestEventRequest) *IngestError {
	// Both kinds carry Version at the same JSON key; decode just enough
	// to read it.
	var versionEnvelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(ev.Payload, &versionEnvelope); err != nil {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid, Message: err.Error()}
	}
	if versionEnvelope.Version < 1 {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("%s payload: version must be >=1 (got %d)", kind, versionEnvelope.Version)}
	}
	if versionEnvelope.Version > eventsExternalContactMaxKnownVersion {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("%s payload version %d exceeds max known %d; upgrade Pi",
				kind, versionEnvelope.Version, eventsExternalContactMaxKnownVersion)}
	}
	return nil
}

// validateMeetingNotePayloadVersion enforces the version envelope on
// both meeting_note.* kinds. Same shape and behaviour as the
// external_contact variant above.
func validateMeetingNotePayloadVersion(index int, kind events.Kind, ev IngestEventRequest) *IngestError {
	var versionEnvelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(ev.Payload, &versionEnvelope); err != nil {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid, Message: err.Error()}
	}
	if versionEnvelope.Version < 1 {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("%s payload: version must be >=1 (got %d)", kind, versionEnvelope.Version)}
	}
	if versionEnvelope.Version > eventsMeetingNoteMaxKnownVersion {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("%s payload version %d exceeds max known %d; upgrade Pi",
				kind, versionEnvelope.Version, eventsMeetingNoteMaxKnownVersion)}
	}
	return nil
}

// validateRawMessagePayload runs the raw_message.*-specific field-level
// validation that ValidatePayload's lenient decode does not enforce.
// Runs only when the kind is a raw_message.* kind and ValidatePayload
// has already passed (structural).
func validateRawMessagePayload(index int, ev IngestEventRequest) *IngestError {
	var p events.RawMessageReceivedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid, Message: err.Error()}
	}
	if p.Version < 1 {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("raw_message payload: version must be >=1 (got %d)", p.Version)}
	}
	if p.Version > eventsRawMessageMaxKnownVersion {
		return &IngestError{Index: index, Code: ingestCodePayloadInvalid,
			Message: fmt.Sprintf("raw_message payload version %d exceeds max known %d; upgrade Pi",
				p.Version, eventsRawMessageMaxKnownVersion)}
	}
	if p.HostID == uuid.Nil {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: host_id is required"}
	}
	if p.Source == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: source is required"}
	}
	if p.Guid == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: guid is required"}
	}
	if p.ChatID == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: chat_id is required"}
	}
	if p.PeerHandle == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: peer_handle is required"}
	}
	if p.SentAt.IsZero() {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: sent_at is required and must not be zero"}
	}
	if p.MessageType == "" {
		return &IngestError{Index: index, Code: ingestCodeMissingField,
			Message: "raw_message payload: message_type is required"}
	}
	// ValidatePayload already rejects message_type values outside the
	// canonical set when non-empty; that check fires above.
	return nil
}

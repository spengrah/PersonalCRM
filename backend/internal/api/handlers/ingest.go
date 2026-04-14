package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
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
type IngestEventRequest struct {
	Source     string          `json:"source"      binding:"required"`
	SourceID   string          `json:"source_id,omitempty"`
	Kind       string          `json:"kind"        binding:"required"`
	Payload    json.RawMessage `json:"payload"     binding:"required"`
	ObservedAt time.Time       `json:"observed_at" binding:"required"`
}

// IngestBatchRequest is the top-level POST body. events must be a non-empty
// array; an empty array is rejected with 400 (spec deliberately unspecified;
// plan Decision 9 chose 400 for client-bug visibility).
type IngestBatchRequest struct {
	Events []IngestEventRequest `json:"events" binding:"required"`
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
type IngestResponse struct {
	Accepted  int           `json:"accepted"`
	Duplicate int           `json:"duplicate"`
	Rejected  int           `json:"rejected"`
	Errors    []IngestError `json:"errors"`
}

// IngestEvents is the HTTP handler for POST /api/v1/ingest/events.
//
// Flow:
//  1. Wrap the body in http.MaxBytesReader (8 MiB cap). Reject oversize
//     bodies with 413 before parsing JSON.
//  2. Bind the JSON body. Malformed → 400.
//  3. Enforce batch shape: non-empty, ≤ maxBatchSize.
//  4. Validate each event (required fields, length bounds, known kind,
//     payload shape). Failures land in errors[]; the batch continues.
//  5. If any events pass validation, forward them to the service as a
//     single atomic batch. Return spec-shaped success response. If the
//     service errors (unexpected DB failure), return 500.
func (h *IngestHandler) IngestEvents(c *gin.Context) {
	// Body size cap. MaxBytesReader returns an error from the JSON decoder
	// when the body exceeds the limit; we detect that via string match
	// since gin/pgx don't expose a typed error for it.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxIngestBodyBytes)

	var req IngestBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
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
	envsToIngest := make([]*events.Envelope, 0, len(req.Events))
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
			ObservedAt: ev.ObservedAt,
		})
	}

	rejected := len(ingestErrors)

	// All events failed validation — still a success response (spec §3.5
	// distinguishes "validation failure per event" from "batch-level
	// failure"). No tx work needed.
	if len(envsToIngest) == 0 {
		logger.Info().
			Int("batch_size", len(req.Events)).
			Int("accepted", 0).
			Int("duplicate", 0).
			Int("rejected", rejected).
			Msg("event batch ingested (all rejected)")
		c.JSON(http.StatusOK, IngestResponse{
			Accepted:  0,
			Duplicate: 0,
			Rejected:  rejected,
			Errors:    ingestErrors,
		})
		return
	}

	accepted, duplicate, err := h.ingestService.IngestBatch(c.Request.Context(), envsToIngest)
	if err != nil {
		logger.Error().
			Err(err).
			Int("batch_size", len(req.Events)).
			Int("validated_batch_size", len(envsToIngest)).
			Msg("event batch ingest failed (tx rolled back)")
		api.SendInternalError(c, "Failed to ingest events")
		return
	}

	logger.Info().
		Int("batch_size", len(req.Events)).
		Int("accepted", accepted).
		Int("duplicate", duplicate).
		Int("rejected", rejected).
		Msg("event batch ingested")

	c.JSON(http.StatusOK, IngestResponse{
		Accepted:  accepted,
		Duplicate: duplicate,
		Rejected:  rejected,
		Errors:    ingestErrors,
	})
}

// validateIngestEvent runs the per-event validation chain. Returns nil
// when the event passes; otherwise returns a populated *IngestError. The
// caller appends rejected events to the response's errors[] array.
func validateIngestEvent(index int, ev IngestEventRequest) *IngestError {
	// Required-field bounds. binding:"required" catches missing keys and
	// empty strings for source/kind, but ObservedAt's zero-time passes
	// the tag since Go's zero-value time.Time isn't "empty" to gin's
	// validator — we check IsZero() below.
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
	if ev.ObservedAt.IsZero() {
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
	// fields; absent fields decode to zero values).
	tmp := &events.Envelope{Kind: kind, Payload: ev.Payload}
	if err := events.ValidatePayload(tmp); err != nil {
		return &IngestError{
			Index:   index,
			Code:    ingestCodePayloadInvalid,
			Message: fmt.Sprintf("payload failed structural validation: %s", err.Error()),
		}
	}

	return nil
}

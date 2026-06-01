package anarlog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// asciiTitleCase lower-cases the input then upper-cases the first byte
// if it's a-z. Concurrency-safe with no shared mutable state. The
// extractor's keep regex (^[A-Z][a-zA-Z]{1,29}$) guarantees ASCII
// input, so this byte-level approach is correct for every token the
// discovery writer can receive.
func asciiTitleCase(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	first := lower[0]
	if first >= 'a' && first <= 'z' {
		b := []byte(lower)
		b[0] = first - 'a' + 'A'
		return string(b)
	}
	return lower
}

// externalContactUpserter is the narrow interface the discovery writer
// needs from ExternalContactRepository — UpsertTx only. Lets tests
// hand-roll a mock without dragging in a full repository.
type externalContactUpserter interface {
	UpsertTx(ctx context.Context, tx pgx.Tx, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
}

// DiscoveryWriter persists anarlog_title weak-candidate rows via
// ExternalContactRepository.UpsertTx. The repository is bound at
// construction time.
type DiscoveryWriter struct {
	externalContactRepo externalContactUpserter
}

// NewDiscoveryWriter constructs a DiscoveryWriter bound to the given
// repository (or any externalContactUpserter).
func NewDiscoveryWriter(externalContactRepo externalContactUpserter) *DiscoveryWriter {
	return &DiscoveryWriter{externalContactRepo: externalContactRepo}
}

// UpsertTitleCandidateTx writes an anarlog_title external_contact row
// for the given session+token pair. The source_id is deterministic
// (sha256 of normalizedToken || session_uuid string) so re-emit of the
// same (session, token) hits ON CONFLICT and only refreshes
// updated_at/synced_at.
//
// displayToken is the original-case token from ExtractNameTokens; the
// writer internally title-cases it for the on-disk display_name + the
// metadata.token_display field, so callers don't have to reason about
// casing.
func (w *DiscoveryWriter) UpsertTitleCandidateTx(
	ctx context.Context,
	tx pgx.Tx,
	sessionUUID uuid.UUID,
	normalizedToken, displayToken string,
) error {
	if normalizedToken == "" {
		return fmt.Errorf("normalizedToken must be non-empty")
	}
	displayTitleCased := asciiTitleCase(displayToken)
	sourceID := computeAnarlogTitleSourceID(normalizedToken, sessionUUID)
	now := accelerated.GetCurrentTime()

	dn := displayTitleCased
	req := repository.UpsertExternalContactRequest{
		Source:      "anarlog_title",
		SourceID:    sourceID,
		DisplayName: &dn,
		Metadata: map[string]any{
			"session_uuid":     sessionUUID.String(),
			"token_normalized": normalizedToken,
			"token_display":    displayTitleCased,
			"extracted_at":     now.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		},
		SyncedAt: &now,
		// HostID intentionally nil: anarlog_title rows are Pi-generated
		// weak candidates, never daemon-claimed. The UpsertExternalContact
		// query's COALESCE(host_id, EXCLUDED.host_id) preserves any prior
		// nil on update.
		// LastContentHash intentionally nil: the deterministic source_id
		// already encodes uniqueness; there is no daemon delete flow for
		// these rows.
	}
	if _, err := w.externalContactRepo.UpsertTx(ctx, tx, req); err != nil {
		return fmt.Errorf("upsert anarlog_title row: %w", err)
	}
	return nil
}

// ComputeAnarlogTitleSourceIDForTest exposes the deterministic
// source_id recipe to integration tests so they can read back a seeded
// anarlog_title row by (token, session). TEST ONLY — production code
// uses the unexported computeAnarlogTitleSourceID via UpsertTitleCandidateTx.
func ComputeAnarlogTitleSourceIDForTest(normalizedToken string, sessionUUID uuid.UUID) string {
	return computeAnarlogTitleSourceID(normalizedToken, sessionUUID)
}

// computeAnarlogTitleSourceID returns the lowercase-hex SHA-256 of
// (normalizedToken || sessionUUID.String()) per the spec recipe.
// `||` is byte concatenation with NO separator (faithful to the spec
// wording). normalizedToken is expected to be lowercased; the caller
// is responsible for that contract.
func computeAnarlogTitleSourceID(normalizedToken string, sessionUUID uuid.UUID) string {
	var buf bytes.Buffer
	buf.WriteString(normalizedToken)
	buf.WriteString(sessionUUID.String())
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

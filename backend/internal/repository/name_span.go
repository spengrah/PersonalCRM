package repository

import (
	"context"

	"personal-crm/backend/internal/db"
)

// NameSpanKey identifies one text of a message: the message's key and a
// SHA-256 digest of the text.
type NameSpanKey struct {
	MessageKey string
	Digest     []byte
}

// StoredNameSpans are the name model's spans of one text under one model
// version: UTF-8 byte ranges [Starts[i], Ends[i]) into the text.
type StoredNameSpans struct {
	NameSpanKey
	Starts, Ends []int32
}

// NameSpanRepository stores the name model's spans of message texts. It holds
// no text: only message keys, text digests, model versions and offsets.
type NameSpanRepository struct {
	queries db.Querier
}

// NewNameSpanRepository builds the repository.
func NewNameSpanRepository(queries db.Querier) *NameSpanRepository {
	return &NameSpanRepository{queries: queries}
}

// ListNameSpans returns the stored spans of the model version for the keys;
// a key with none stored is left out.
func (r *NameSpanRepository) ListNameSpans(ctx context.Context, modelVersion string, keys []NameSpanKey) ([]StoredNameSpans, error) {
	params := db.ListMessageNameSpansParams{ModelVersion: modelVersion, MessageKeys: make([]string, len(keys)), TextDigests: make([][]byte, len(keys))}
	for i, k := range keys {
		params.MessageKeys[i], params.TextDigests[i] = k.MessageKey, k.Digest
	}
	rows, err := r.queries.ListMessageNameSpans(ctx, params)
	if err != nil {
		return nil, err
	}
	out := make([]StoredNameSpans, len(rows))
	for i, row := range rows {
		out[i] = StoredNameSpans{NameSpanKey: NameSpanKey{MessageKey: row.MessageKey, Digest: row.TextDigest}, Starts: row.Starts, Ends: row.Ends}
	}
	return out, nil
}

// SaveNameSpans stores the spans of each text under the model version,
// keeping any already stored.
func (r *NameSpanRepository) SaveNameSpans(ctx context.Context, modelVersion string, spans []StoredNameSpans) error {
	for _, s := range spans {
		if err := r.queries.InsertMessageNameSpan(ctx, db.InsertMessageNameSpanParams{
			MessageKey: s.MessageKey, TextDigest: s.Digest, ModelVersion: modelVersion,
			Starts: s.Starts, Ends: s.Ends,
		}); err != nil {
			return err
		}
	}
	return nil
}

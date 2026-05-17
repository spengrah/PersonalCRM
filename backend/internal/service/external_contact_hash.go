// Package service contains the JCS+SHA-256 content-hash helper used by
// the Mac daemon ingest path for external_contact.upserted events.
//
// The Pi recomputes JCS(payload \ {host_id}) → SHA-256 → lowercase hex
// and compares to the hash suffix embedded in env.SourceID
// ("<entity_uuid>@<hex>"). A mismatch signals a stale daemon cache, a
// JCS-library bug, or protocol drift; the ingest layer rejects the
// event as PAYLOAD_INVARIANT rather than silently storing inconsistent
// state.
//
// The recipe must match the Swift daemon byte-for-byte:
//
//  1. Strip the top-level "host_id" key from the raw JSON payload.
//  2. JCS-canonicalize per RFC 8785 (lexicographic UTF-16 key order,
//     no insignificant whitespace, ECMAScript Number→String, NO Unicode
//     normalization).
//  3. SHA-256, hex-encode lowercase.
//
// `host_id` is stripped at the raw byte level via sjson.DeleteBytes so
// numeric fields in Metadata (which may exceed 2^53) are not lost to a
// json.Unmarshal → map[string]any → json.Marshal round-trip.
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/gowebpki/jcs"
	"github.com/tidwall/sjson"
)

// ComputeContentHash returns the lowercase-hex SHA-256 of the JCS-
// canonicalized payload with the top-level host_id field removed.
//
// payload must be valid JSON. The function returns an error for
// malformed JSON, sjson failures, or JCS canonicalization failures.
//
// host_id is removed at the raw-bytes level: sjson.DeleteBytes
// preserves the original numeric fidelity for every other field, so
// integers larger than 2^53 in Metadata round-trip without precision
// loss. The Swift daemon must produce byte-identical canonical JSON.
func ComputeContentHash(payload []byte) (string, error) {
	stripped, err := sjson.DeleteBytes(payload, "host_id")
	if err != nil {
		return "", fmt.Errorf("strip host_id: %w", err)
	}
	canonical, err := jcs.Transform(stripped)
	if err != nil {
		return "", fmt.Errorf("jcs canonicalize: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

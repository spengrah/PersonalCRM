// dump-external-contact-hash-fixture regenerates the cross-language
// JCS+SHA-256 parity fixture consumed by both the Go side
// (external_contact_hash_parity_test.go) and the Swift side
// (JCSParityFixtureTests.swift in CRMMacCoreTests).
//
// For each entry, the helper records:
//   - name         — human-readable identifier for the test case
//   - input        — the canonical UTF-8 source JSON bytes
//   - strip_host   — whether to strip the top-level "host_id" key
//     before canonicalizing (true mirrors
//     service.ComputeContentHash; false exercises raw JCS)
//   - canonical    — JCS-canonicalized output bytes (verbatim)
//   - sha256       — lowercase-hex SHA-256 of canonical
//
// The fixture is exhaustive over the subset of RFC 8785 the Swift
// canonicalizer supports (object/array root, string/bool/null/integer
// values within the JavaScript safe-integer range). It deliberately
// excludes floats + top-level fragments — both are rejected by the
// Swift implementation via precondition, and including them here would
// fail the parity contract.
//
// Regenerate:
//
//	go run ./backend/cmd/dump-external-contact-hash-fixture \
//	    > backend/internal/service/testdata/external_contact_hash_parity.json
//
// The companion test (external_contact_hash_parity_test.go) reads the
// resulting file and asserts that gowebpki/jcs still produces the
// documented bytes — guarding against silent library swaps.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gowebpki/jcs"
	"github.com/tidwall/sjson"
)

type fixtureEntry struct {
	Name      string          `json:"name"`
	Note      string          `json:"note,omitempty"`
	Input     json.RawMessage `json:"input"`
	StripHost bool            `json:"strip_host"`
	Canonical string          `json:"canonical"`
	SHA256    string          `json:"sha256"`
}

type fixtureFile struct {
	Meta    fixtureMeta    `json:"_meta"`
	Entries []fixtureEntry `json:"entries"`
}

type fixtureMeta struct {
	Recipe      string   `json:"recipe"`
	Subset      []string `json:"subset"`
	Unsupported []string `json:"unsupported"`
	Regenerate  string   `json:"regenerate"`
}

// inputs returns the source payloads in order. Each is a triple of
// (name, raw JSON bytes, strip_host?). The Swift test reads the same
// file and compares its canonicalizer output byte-for-byte against
// the Canonical field of each entry.
//
// Keep entries small + descriptive: every entry becomes a documented
// invariant cross-language test failures cite.
func inputs() []fixtureEntry {
	// Build escape-sensitive JSON literals via regular (non-raw)
	// strings so JSON escape sequences (\n, \r, \t, \b, \f, \uXXXX)
	// reach the parser intact — a raw Go string literal would inline
	// raw control bytes and the test wouldn't exercise the escape
	// branch of the canonicalizer.
	controlChars := "{\"s\":\"a\\nb\\rc\\td\\be\\ff\"}"
	quoteBackslash := "{\"s\":\"a\\\"b\\\\c/d\"}"
	lowCtrl := "{\"s\":\"a\\u0001b\"}"

	return []fixtureEntry{
		{
			Name:      "empty_object",
			Note:      "trivial empty-object canonical form",
			Input:     mustJSON(`{}`),
			StripHost: false,
		},
		{
			Name:      "empty_array",
			Note:      "trivial empty-array canonical form",
			Input:     mustJSON(`[]`),
			StripHost: false,
		},
		{
			Name:      "single_ascii_string",
			Note:      "object with one ASCII string field",
			Input:     mustJSON(`{"name":"Contact A"}`),
			StripHost: false,
		},
		{
			Name:      "key_sort_swap",
			Note:      "two keys in reverse order canonicalize to sorted form",
			Input:     mustJSON(`{"b":1,"a":2}`),
			StripHost: false,
		},
		{
			Name:      "non_ascii_string_bmp",
			Note:      "BMP non-ASCII string (accented latin, CJK)",
			Input:     mustJSON(`{"name":"São Paulo","city":"中国"}`),
			StripHost: false,
		},
		{
			Name: "non_ascii_string_nfd",
			Note: "NFD decomposed form (e + U+0301 combining acute) must stay byte-distinct from NFC; no normalization is performed",
			// "e" followed by U+0301 COMBINING ACUTE ACCENT (two code
			// points, three UTF-8 bytes: 0x65 0xCC 0x81).
			Input:     mustJSON("{\"name\":\"e\\u0301\"}"),
			StripHost: false,
		},
		{
			Name: "non_ascii_string_nfc",
			Note: "NFC precomposed form (U+00E9) must stay byte-distinct from NFD; no normalization is performed",
			// U+00E9 LATIN SMALL LETTER E WITH ACUTE (one code point,
			// two UTF-8 bytes: 0xC3 0xA9).
			Input:     mustJSON("{\"name\":\"\\u00e9\"}"),
			StripHost: false,
		},
		{
			Name:      "supplementary_plane_emoji",
			Note:      "Supplementary-plane emoji (U+1F602) emits as literal UTF-8, NOT surrogate-escape",
			Input:     mustJSON(`{"face":"😂"}`),
			StripHost: false,
		},
		{
			Name:      "control_chars_escaped",
			Note:      "Named control escapes (\\n, \\r, \\t, \\b, \\f) emit short forms",
			Input:     mustJSON(controlChars),
			StripHost: false,
		},
		{
			Name:      "quote_and_backslash_escapes",
			Note:      "Quote and backslash MUST escape; slash MUST NOT",
			Input:     mustJSON(quoteBackslash),
			StripHost: false,
		},
		{
			Name:      "low_control_unicode_escape",
			Note:      "Control char < 0x20 outside the named set uses \\u00XX form",
			Input:     mustJSON(lowCtrl),
			StripHost: false,
		},
		{
			Name:      "booleans_and_null",
			Note:      "true / false / null literal forms",
			Input:     mustJSON(`{"t":true,"f":false,"n":null}`),
			StripHost: false,
		},
		{
			Name:      "integers_boundary",
			Note:      "Zero, positive, negative, and the JavaScript safe-integer boundary 2^53-1",
			Input:     mustJSON(`{"zero":0,"one":1,"neg":-1,"max_safe":9007199254740991,"min_safe":-9007199254740991}`),
			StripHost: false,
		},
		{
			Name:      "nested_object_with_mixed_keys",
			Note:      "Nested object with ASCII + non-ASCII keys to validate UTF-16 sort",
			Input:     mustJSON(`{"outer":{"z":1,"é":2,"a":3},"prefix":"x"}`),
			StripHost: false,
		},
		{
			Name:      "array_of_objects",
			Note:      "Array preserves input order; per-element canonicalization",
			Input:     mustJSON(`{"items":[{"b":1,"a":2},{"d":4,"c":3}]}`),
			StripHost: false,
		},
		{
			Name: "external_contact_upserted_synthetic",
			Note: "Representative ExternalContactUpsertedPayload-shaped object with synthetic identifiers",
			Input: mustJSON(`{
				"version":1,
				"host_id":"00000000-0000-0000-0000-000000000001",
				"source":"icloud_contacts",
				"entity_id":"contact-A",
				"display_name":"Contact A",
				"first_name":"Contact",
				"last_name":"A",
				"emails":[{"value":"a@example.com","type":"home","primary":true}],
				"phones":[{"value":"+10000000001","type":"mobile","primary":true}],
				"addresses":[{"formatted":"100 Synthetic St, Nowhere","type":"home"}],
				"organization":"Org X",
				"job_title":"Engineer",
				"birthday":"1990-01-01",
				"metadata":{"container_identifier":"00000000-0000-0000-0000-000000000002"}
			}`),
			StripHost: true,
		},
		{
			Name: "external_contact_upserted_strip_host_parity",
			Note: "Same payload as the prior entry without host_id; canonical + hash MUST equal the strip_host=true case",
			Input: mustJSON(`{
				"version":1,
				"source":"icloud_contacts",
				"entity_id":"contact-A",
				"display_name":"Contact A",
				"first_name":"Contact",
				"last_name":"A",
				"emails":[{"value":"a@example.com","type":"home","primary":true}],
				"phones":[{"value":"+10000000001","type":"mobile","primary":true}],
				"addresses":[{"formatted":"100 Synthetic St, Nowhere","type":"home"}],
				"organization":"Org X",
				"job_title":"Engineer",
				"birthday":"1990-01-01",
				"metadata":{"container_identifier":"00000000-0000-0000-0000-000000000002"}
			}`),
			StripHost: false,
		},
		{
			Name: "external_contact_deleted_synthetic",
			Note: "Representative ExternalContactDeletedPayload-shaped object with synthetic identifiers",
			Input: mustJSON(`{
				"version":1,
				"host_id":"00000000-0000-0000-0000-000000000001",
				"source":"icloud_contacts",
				"entity_id":"contact-A"
			}`),
			StripHost: true,
		},
	}
}

func mustJSON(s string) json.RawMessage {
	// Re-marshal to compact form: ensures the embedded literal in this
	// source file is byte-precise. Strips whitespace.
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(fmt.Sprintf("invalid fixture JSON literal: %v: %s", err, s))
	}
	out, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("re-marshal failed: %v", err))
	}
	return out
}

func main() {
	entries := inputs()
	for i := range entries {
		raw := []byte(entries[i].Input)
		if entries[i].StripHost {
			stripped, err := sjson.DeleteBytes(raw, "host_id")
			if err != nil {
				fmt.Fprintf(os.Stderr, "strip host_id: %v\n", err)
				os.Exit(1)
			}
			raw = stripped
		}
		canonical, err := jcs.Transform(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "jcs transform %q: %v\n", entries[i].Name, err)
			os.Exit(1)
		}
		sum := sha256.Sum256(canonical)
		entries[i].Canonical = string(canonical)
		entries[i].SHA256 = hex.EncodeToString(sum[:])
	}

	file := fixtureFile{
		Meta: fixtureMeta{
			Recipe: "JCS(payload \\ {host_id when strip_host==true}) -> SHA-256 -> lowercase hex",
			Subset: []string{
				"top-level object or array",
				"string values (no Unicode normalization)",
				"boolean true/false",
				"null",
				"integer numbers within [-2^53+1, +2^53-1]",
				"nested objects and arrays",
			},
			Unsupported: []string{
				"floating-point numbers",
				"top-level fragments (string/number/bool/null without object or array)",
				"integers outside the JavaScript safe-integer range",
			},
			Regenerate: "go run ./backend/cmd/dump-external-contact-hash-fixture > backend/internal/service/testdata/external_contact_hash_parity.json",
		},
		Entries: entries,
	}

	out, err := marshalIndentNoHTML(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write([]byte("\n")); err != nil {
		fmt.Fprintf(os.Stderr, "write trailing newline: %v\n", err)
		os.Exit(1)
	}
}

// marshalIndentNoHTML emits two-space-indented JSON without escaping
// <, >, &. The fixture must round-trip verbatim through reader code
// on both sides — HTML-escaped characters would diverge from the
// literal bytes the canonicalizer produces.
func marshalIndentNoHTML(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder appends a trailing newline; trim it so the main()
	// caller can manage whitespace deterministically.
	out := sb.String()
	out = strings.TrimRight(out, "\n")
	return []byte(out), nil
}

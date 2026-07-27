// Package spec parses and validates the behavior SSOT corpus that lives at the
// repository's top-level spec/ directory (one spec/<domain>.yaml file per
// domain). It is the shared foundation for the spec-lint CLI and the
// spec-coverage traceability scanner — so the typed schema structs and the
// parse/validate logic live here, not in the CLIs.
//
// The exported File and Behavior structs are the plain OUTPUT of the parser
// (no yaml struct tags or custom unmarshalers): the parser walks the decoded
// yaml.Node tree by hand so that absent-vs-present-but-empty is decidable and
// violation messages can carry line numbers. See parser.go for the walker and
// validate.go for the semantic checks.
package spec

// File is one parsed spec/<domain>.yaml document.
type File struct {
	Domain   string
	Prefix   string
	Maturity string // draft | reviewed | ratified
	// Settled: surfaces whose orphans block instead of warn; absent = none
	// block. Elements ∈ {ui, api} (none reserved — not yet supported).
	Settled   []string
	Behaviors []Behavior
	Path      string // set by the parser; used in violation messages
}

// Behavior is one entry in a File's behaviors list.
type Behavior struct {
	ID         string
	Title      string
	Type       string   // business-logic | api | ux | invariant | data | intent
	Status     string   // current | proposed | retired
	Surface    string   // ui | api | none; required for non-intent, non-retired behaviors (intents are judge-only and take no surface)
	Given      []string // scalar input normalized to a one-element list; nil = absent
	When       string
	Then       []ThenItem // same normalization as Given (scalar → one-element list; nil = absent)
	Statement  string     // invariant and intent types only; mutually exclusive with GWT
	Serves     []string   // ux and intent types only; targets must be intent behavior IDs
	Waivers    []Waiver   // ui- or api-surface behaviors only: then-items deliberately excluded from deterministic coverage
	Provenance []string
	Notes      string
	Line       int // 1-based source line of the behavior mapping; set by the parser, used in drift annotations
}

// ThenItem is one observable outcome in a behavior's then list. Key is the
// stable citation handle — empty when the item carries none (uncited items stay
// plain strings in YAML). Line is the item's 1-based source line.
//
// A keyed item is written as a {key, text} mapping; an unkeyed one stays a
// plain scalar. Key is what a // spec: ID.key citation binds to, so it survives
// reordering, insertion, and deletion of sibling items — the positional index
// does not.
type ThenItem struct {
	Key  string
	Text string
	Line int
}

// ThenTexts projects the then list down to its assertion texts, dropping keys
// and source lines. It is the only sanctioned way to compare two behaviors'
// then lists as assertions: a key is a citation handle, not an assertion, and
// Line moves whenever anything above it in the file does.
func (b *Behavior) ThenTexts() []string {
	if b.Then == nil {
		return nil
	}
	out := make([]string, len(b.Then))
	for i, it := range b.Then {
		out[i] = it.Text
	}
	return out
}

// waiverStatementKey is the reserved token a waiver uses to address a statement
// behavior's single implicit coverage item, which has no then list to key. It
// is rejected as a then-item key so the two meanings can never collide.
const waiverStatementKey = "statement"

// Waiver records the DROP verdict for one then-item of a ui- or api-surface
// behavior: the item is neither deterministically provable nor worth a judge
// intent, so the coverage scanner reports it as waived (with the reason)
// instead of orphaned.
//
// A waiver addresses its item by key (the current form) or by 0-based index
// (the legacy form); Keyed says which. Index and Keyed are TRANSITIONAL — they
// exist only while the corpus still carries index-form waivers and are removed
// once it does not.
type Waiver struct {
	// Key is a then-item key of the same behavior, or the reserved token
	// "statement" addressing a statement behavior's single implicit item.
	// Valid only when Keyed.
	Key string
	// Index is the legacy 0-based then-item index. Valid only when !Keyed. It
	// may legitimately be NEGATIVE — a negative index is a linted violation
	// (out of range), never a sentinel for "the keyed form is in use". That is
	// what Keyed is for.
	Index int
	// Keyed discriminates which of Key/Index carries the reference.
	Keyed  bool
	Reason string
}

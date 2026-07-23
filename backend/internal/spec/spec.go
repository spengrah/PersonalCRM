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
	Then       []string // same normalization as Given
	Statement  string   // invariant and intent types only; mutually exclusive with GWT
	Serves     []string // ux and intent types only; targets must be intent behavior IDs
	Waivers    []Waiver // ui- or api-surface behaviors only: then-items deliberately excluded from deterministic coverage
	Provenance []string
	Notes      string
	Line       int // 1-based source line of the behavior mapping; set by the parser, used in drift annotations
}

// Waiver records the DROP verdict for one then-item of a ui- or api-surface
// behavior: the item is neither deterministically provable nor worth a judge
// intent, so the coverage scanner reports it as waived (with the reason)
// instead of orphaned.
type Waiver struct {
	Then   int // 0-based index into the behavior's then list
	Reason string
}

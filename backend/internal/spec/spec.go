// Package spec parses and validates the behavior SSOT corpus that lives at the
// repository's top-level spec/ directory (one spec/<domain>.yaml file per
// domain). It is the shared foundation for the spec-lint CLI and, later,
// Piece 3's traceability scanner — so the typed schema structs and the
// parse/validate logic live here, not in the CLI.
//
// The exported File and Behavior structs are the plain OUTPUT of the parser
// (no yaml struct tags or custom unmarshalers): the parser walks the decoded
// yaml.Node tree by hand so that absent-vs-present-but-empty is decidable and
// violation messages can carry line numbers. See parser.go for the walker and
// validate.go for the semantic checks.
package spec

// File is one parsed spec/<domain>.yaml document.
type File struct {
	Domain    string
	Prefix    string
	Maturity  string // draft | reviewed | ratified
	Behaviors []Behavior
	Path      string // set by the parser; used in violation messages
}

// Behavior is one entry in a File's behaviors list.
type Behavior struct {
	ID         string
	Title      string
	Type       string   // business-logic | api | ux | invariant | data
	Status     string   // current | proposed | retired
	Given      []string // scalar input normalized to a one-element list; nil = absent
	When       string
	Then       []string // same normalization as Given
	Statement  string   // invariant type only; mutually exclusive with GWT
	Provenance []string
	Notes      string
}

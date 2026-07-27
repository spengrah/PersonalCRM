package spec

import (
	"fmt"
	"regexp"
	"sort"
)

// Violation is one linter finding. String() renders it as "path:line: ref: msg"
// (line omitted when 0, ref omitted when empty — file-level findings).
type Violation struct {
	Path string
	Ref  string // behavior ID when present, else "behaviors[i]"; empty for file-level
	Line int    // 1-based source line; 0 when unavailable
	Msg  string
}

func (v Violation) String() string {
	switch {
	case v.Line > 0 && v.Ref != "":
		return fmt.Sprintf("%s:%d: %s: %s", v.Path, v.Line, v.Ref, v.Msg)
	case v.Line > 0:
		return fmt.Sprintf("%s:%d: %s", v.Path, v.Line, v.Msg)
	case v.Ref != "":
		return fmt.Sprintf("%s: %s: %s", v.Path, v.Ref, v.Msg)
	default:
		return fmt.Sprintf("%s: %s", v.Path, v.Msg)
	}
}

// scopedViolation carries a Violation plus internal sort keys so the aggregate
// output is deterministic: by file path, then behavior index (-1 =
// file-level, sorts first), then check order, then line, then message.
type scopedViolation struct {
	v     Violation
	bIdx  int
	order int
}

// Check-order ranks give violations within one scope a stable order.
const (
	orderStructural       = 0  // parse/structural (YAML, node shape, string typing, when-singular)
	orderLegacyKey        = 9  // retired file keys (e2e_settled); sorts first among file findings
	orderRequiredFile     = 10 // required file fields present + non-empty
	orderMaturityEnum     = 11 // maturity enum membership
	orderPrefixUnique     = 12 // prefixes unique across files
	orderPrefixFormat     = 13 // prefix charset matches the citation grammar
	orderSettledEnum      = 14 // settled surface-list membership / duplicates
	orderRequiredBehavior = 20 // required behavior fields present + non-empty
	orderTypeEnum         = 21 // type enum membership
	orderStatusEnum       = 22 // status enum membership
	orderIDFormat         = 23 // id matches <prefix>-NNN
	orderIDDupFile        = 24 // ids unique within a file
	orderIDDupGlobal      = 25 // ids unique across files
	orderGWT              = 26 // GWT xor statement, by type
	orderListItems        = 27 // given/then/serves list items non-empty
	orderServesType       = 28 // serves only on ux/intent behaviors
	orderServesResolve    = 29 // serves targets resolve to intent behaviors
	orderSurface          = 30 // surface enum + required for non-intent non-retired + forbidden on intent
	orderWaivers          = 31 // waivers only on ui- or api-surface; index in range; no dups; reason non-empty
	// orderThenKeys is APPENDED rather than slotted next to orderListItems so
	// the 20-31 ranks (and the report-order assertions that pin them) do not
	// renumber. Consequence: a bad or duplicated then-item key sorts AFTER the
	// orderWaivers finding it may have caused, so the root cause reads second.
	orderThenKeys = 32 // then-item key charset, reserved words, uniqueness
)

var (
	validMaturity = map[string]bool{"draft": true, "reviewed": true, "ratified": true}
	validType     = map[string]bool{"business-logic": true, "api": true, "ux": true, "invariant": true, "data": true, "intent": true}
	validStatus   = map[string]bool{"current": true, "proposed": true, "retired": true}
	validSurface  = map[string]bool{"ui": true, "api": true, "none": true}
	// statementTypes use statement instead of GWT (mutually exclusive).
	statementTypes = map[string]bool{"invariant": true, "intent": true}
	// servesTypes may carry a serves list of intent-behavior targets.
	servesTypes = map[string]bool{"ux": true, "intent": true}
)

// idRegex builds the ID pattern for a file's declared prefix: the prefix
// literally (regexp.QuoteMeta), a hyphen, then a 3-digit number or a 4+-digit
// number with no leading zero.
func idRegex(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-(\d{3}|[1-9]\d{3,})$`)
}

// prefixRegex is the charset the citation grammar (coverage.go citationRef)
// can parse back out of a // spec: marker — keep the two in lockstep.
var prefixRegex = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// thenKeyRegex is the then-item key charset: lowercase alphanumeric words
// separated by single hyphens. It must stay in lockstep with the key
// alternative of coverage.go's citationRef — a key outside it could never be
// cited.
var thenKeyRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Lint parses and validates the spec corpus in dir, returning the parsed files,
// every violation (parse/structural + semantic + cross-file) in deterministic
// order, and an IO-level error only when the directory (or a file) is
// unreadable. It never fails fast on the first violation.
func Lint(dir string) ([]*File, []Violation, error) {
	parsed, err := parseDirInternal(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []*File
	var all []scopedViolation
	for _, pf := range parsed {
		if pf.file != nil {
			files = append(files, pf.file)
		}
		all = append(all, pf.parseViol...)
	}
	all = append(all, semanticChecks(parsed)...)
	return files, extractSorted(all), nil
}

type collector struct{ items []scopedViolation }

func (c *collector) add(pf *parsedFile, order, bIdx, line int, ref, msg string) {
	c.items = append(c.items, scopedViolation{
		v:     Violation{Path: pf.path, Ref: ref, Line: line, Msg: msg},
		bIdx:  bIdx,
		order: order,
	})
}

// semanticChecks runs the semantic + cross-file checks over the parsed files.
// File-tier-broken files are excluded entirely — they register no
// prefixes/IDs (see the tiered rule on parsedFile).
func semanticChecks(parsed []*parsedFile) []scopedViolation {
	c := &collector{}

	prefixFiles := map[string][]*parsedFile{}    // prefix -> files declaring it
	idFilePaths := map[string]map[string]bool{}  // id -> set of file paths declaring it
	idToBehavior := map[string]*parsedBehavior{} // id -> first registered behavior (serves resolution)

	for _, pf := range parsed {
		if pf.fatal {
			continue
		}
		checkFile(pf, c)

		if prefixUsable(pf) {
			prefixFiles[pf.file.Prefix] = append(prefixFiles[pf.file.Prefix], pf)
		}

		var idRe *regexp.Regexp
		if prefixUsable(pf) {
			idRe = idRegex(pf.file.Prefix)
		}

		idInFile := map[string][]*parsedBehavior{}
		for _, pb := range pf.behaviors {
			if pb.skip {
				continue
			}
			checkBehavior(pf, pb, idRe, c)
			if pb.keys["id"] && !pb.broken["id"] && pb.b.ID != "" {
				idInFile[pb.b.ID] = append(idInFile[pb.b.ID], pb)
				if idFilePaths[pb.b.ID] == nil {
					idFilePaths[pb.b.ID] = map[string]bool{}
				}
				idFilePaths[pb.b.ID][pf.path] = true
				if _, exists := idToBehavior[pb.b.ID]; !exists {
					idToBehavior[pb.b.ID] = pb
				}
			}
		}
		checkIDDupInFile(pf, idInFile, c)
	}

	checkPrefixDup(prefixFiles, c)
	checkIDDupGlobal(parsed, idFilePaths, c)

	// serves resolution is corpus-wide (cross-domain references are legal), so
	// it needs the full ID registry — a second pass after every file's IDs
	// have registered.
	for _, pf := range parsed {
		if pf.fatal {
			continue
		}
		for _, pb := range pf.behaviors {
			if pb.skip {
				continue
			}
			checkServesResolve(pf, pb, idToBehavior, c)
		}
	}
	return c.items
}

// checkServesResolve enforces that every serves target names an existing
// intent behavior somewhere in the corpus. A target whose own type is broken
// or invalid is not re-reported here (its own violation already flags it).
// Under a globally-duplicated ID (itself a violation) resolution binds to the
// first-registered behavior, so the serves message may name the wrong twin —
// the dup violation is the root cause to fix first.
func checkServesResolve(pf *parsedFile, pb *parsedBehavior, idToBehavior map[string]*parsedBehavior, c *collector) {
	if !pb.keys["serves"] || pb.broken["serves"] {
		return
	}
	for _, target := range pb.b.Serves {
		if target == "" {
			continue // already reported by the list-items check
		}
		if target == pb.b.ID {
			c.add(pf, orderServesResolve, pb.idx, pb.line, pb.ref,
				fmt.Sprintf("serves target %q is the behavior itself", target))
			continue
		}
		tpb, ok := idToBehavior[target]
		if !ok {
			c.add(pf, orderServesResolve, pb.idx, pb.line, pb.ref,
				fmt.Sprintf("serves target %q does not exist in the corpus", target))
			continue
		}
		if tpb.keys["type"] && !tpb.broken["type"] && validType[tpb.b.Type] && tpb.b.Type != "intent" {
			c.add(pf, orderServesResolve, pb.idx, pb.line, pb.ref,
				fmt.Sprintf("serves target %q is not an intent behavior (type %q)", target, tpb.b.Type))
		}
	}
}

func prefixUsable(pf *parsedFile) bool {
	return pf.fileKeys["prefix"] && !pf.fileBroken["prefix"] && pf.file.Prefix != ""
}

func checkFile(pf *parsedFile, c *collector) {
	// Required file fields present + non-empty.
	checkRequiredFileField(pf, c, "domain", pf.file.Domain)
	checkRequiredFileField(pf, c, "prefix", pf.file.Prefix)
	checkRequiredFileField(pf, c, "maturity", pf.file.Maturity)
	if !pf.fileKeys["behaviors"] {
		c.add(pf, orderRequiredFile, -1, 0, "", `missing required field "behaviors"`)
	}
	// Maturity enum (only when present + non-empty + not broken).
	if pf.fileKeys["maturity"] && !pf.fileBroken["maturity"] && pf.file.Maturity != "" && !validMaturity[pf.file.Maturity] {
		c.add(pf, orderMaturityEnum, -1, 0, "",
			fmt.Sprintf("invalid maturity %q (want draft|reviewed|ratified)", pf.file.Maturity))
	}
	// Prefix charset must match the citation grammar (the coverage scanner
	// parses references as [A-Z][A-Z0-9]*-N); a prefix outside it would mint
	// behaviors that can never be validly cited.
	if prefixUsable(pf) && !prefixRegex.MatchString(pf.file.Prefix) {
		c.add(pf, orderPrefixFormat, -1, 0, "",
			fmt.Sprintf("prefix %q must be uppercase alphanumeric starting with a letter", pf.file.Prefix))
	}
	checkSettled(pf, c)
}

// checkSettled enforces the settled surface list: the retired e2e_settled key
// is rejected outright (the parser silently ignores unknown root keys, so a
// lingering legacy line would otherwise disable ui blocking unseen), an
// explicit-but-empty settled (null or []) is rejected rather than read as
// "nothing blocks", and every element of settled must be a supported surface
// with no duplicates. `none` is reserved — rejected with a forward-looking
// message until none-orphan detection ships. Structurally broken settled fields
// are skipped (already reported by the parser).
func checkSettled(pf *parsedFile, c *collector) {
	if pf.fileKeys["e2e_settled"] {
		c.add(pf, orderLegacyKey, -1, 0, "", "e2e_settled is retired; use a settled: [ui] list")
	}
	if pf.fileBroken["settled"] {
		return
	}
	// An explicit settled: key that declares nothing (null or an empty list) is
	// rejected, not read as "nothing blocks": that reading is indistinguishable
	// from an absent key yet hides an incomplete edit. Omit the key entirely for
	// a genuinely unsettled domain; otherwise list its surfaces.
	if pf.fileKeys["settled"] && len(pf.file.Settled) == 0 {
		c.add(pf, orderSettledEnum, -1, 0, "",
			"settled must list at least one surface; omit the key entirely for a genuinely unsettled domain")
		return
	}
	seen := map[string]bool{}
	for _, s := range pf.file.Settled {
		switch {
		case s == "none":
			c.add(pf, orderSettledEnum, -1, 0, "",
				`settled surface "none" is not yet supported (none-orphan detection is future work)`)
		case !validSurface[s]:
			c.add(pf, orderSettledEnum, -1, 0, "",
				fmt.Sprintf("invalid settled surface %q (want ui|api)", s))
		case seen[s]:
			c.add(pf, orderSettledEnum, -1, 0, "",
				fmt.Sprintf("duplicate settled surface %q", s))
		}
		seen[s] = true
	}
}

func checkRequiredFileField(pf *parsedFile, c *collector, key, val string) {
	if pf.fileBroken[key] {
		return // structural violation already reported; a missing-field report would double-count it
	}
	if !pf.fileKeys[key] || val == "" {
		c.add(pf, orderRequiredFile, -1, 0, "", fmt.Sprintf("missing required field %q", key))
	}
}

func checkBehavior(pf *parsedFile, pb *parsedBehavior, idRe *regexp.Regexp, c *collector) {
	// Required behavior fields present + non-empty.
	checkRequiredBehaviorField(pf, pb, c, "id", pb.b.ID)
	checkRequiredBehaviorField(pf, pb, c, "title", pb.b.Title)
	checkRequiredBehaviorField(pf, pb, c, "type", pb.b.Type)
	checkRequiredBehaviorField(pf, pb, c, "status", pb.b.Status)

	// Type enum.
	if pb.keys["type"] && !pb.broken["type"] && pb.b.Type != "" && !validType[pb.b.Type] {
		c.add(pf, orderTypeEnum, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("invalid type %q (want business-logic|api|ux|invariant|data|intent)", pb.b.Type))
	}
	// Status enum.
	if pb.keys["status"] && !pb.broken["status"] && pb.b.Status != "" && !validStatus[pb.b.Status] {
		c.add(pf, orderStatusEnum, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("invalid status %q (want current|proposed|retired)", pb.b.Status))
	}
	// ID format (only when the id is clean and the prefix is usable).
	if idRe != nil && pb.keys["id"] && !pb.broken["id"] && pb.b.ID != "" && !idRe.MatchString(pb.b.ID) {
		c.add(pf, orderIDFormat, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("id %q must match %s-NNN (NNN = 3 digits, or 4+ with no leading zero)", pb.b.ID, pf.file.Prefix))
	}
	// GWT xor statement, presence-based by type.
	checkGWT(pf, pb, c)
	// given/then/serves list items non-empty. then is projected to its texts:
	// emptiness is a property of the assertion, not of the citation handle, and
	// the message + reported line stay exactly as they were.
	checkListItems(pf, pb, c, "given", pb.b.Given, pb.broken["given"])
	checkListItems(pf, pb, c, "then", pb.b.ThenTexts(), pb.broken["then"])
	checkListItems(pf, pb, c, "serves", pb.b.Serves, pb.broken["serves"])
	// then-item key charset / reserved words / uniqueness.
	checkThenKeys(pf, pb, c)

	// serves is only for ux and intent behaviors (decidable only when the
	// type is a known enum value; an absent/invalid type is already reported).
	if pb.keys["serves"] && !pb.broken["serves"] &&
		pb.keys["type"] && !pb.broken["type"] && validType[pb.b.Type] && !servesTypes[pb.b.Type] {
		c.add(pf, orderServesType, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("serves is only for ux and intent behaviors (type %q)", pb.b.Type))
	}

	checkSurface(pf, pb, c)
	checkWaivers(pf, pb, c)
}

// checkSurface enforces the surface field: valid enum value when present;
// required for non-intent, non-retired behaviors; forbidden on intents (they
// are judge-only, so deterministic coverage classification does not apply).
// Requiredness/forbiddenness are decidable only when type (and, for the
// retired exemption, status) parsed clean with a valid enum value — an
// absent/invalid type or status is already reported by its own checks.
func checkSurface(pf *parsedFile, pb *parsedBehavior, c *collector) {
	surfacePresent := pb.keys["surface"] && !pb.broken["surface"] && pb.b.Surface != ""
	if surfacePresent && !validSurface[pb.b.Surface] {
		c.add(pf, orderSurface, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("invalid surface %q (want ui|api|none)", pb.b.Surface))
		return
	}
	if !pb.keys["type"] || pb.broken["type"] || !validType[pb.b.Type] {
		return
	}
	if pb.b.Type == "intent" {
		if pb.keys["surface"] && !pb.broken["surface"] {
			c.add(pf, orderSurface, pb.idx, pb.line, pb.ref,
				"surface is not for intent behaviors (intents are judge-only)")
		}
		return
	}
	if pb.broken["surface"] {
		return // structural violation already reported
	}
	// The retired exemption makes requiredness decidable only when status is
	// present with a valid enum value — an absent/invalid status is already
	// reported by its own checks.
	if !pb.keys["status"] || pb.broken["status"] || !validStatus[pb.b.Status] {
		return
	}
	if pb.b.Status != "retired" && !surfacePresent {
		c.add(pf, orderSurface, pb.idx, pb.line, pb.ref, `missing required field "surface" (want ui|api|none)`)
	}
}

// checkThenKeys enforces then-item key semantics: the charset, the reserved
// "statement" token, and uniqueness within the behavior. A structurally broken
// then list is skipped, the same tiered guard checkListItems carries. Both
// guards are currently UNREACHABLE, not live filters — behaviorThen returns nil
// whenever it marks the field broken, so the loop below has nothing to iterate
// either way — and both stay because the guard, not the walker's present return
// value, is what states the tier contract.
//
// Uniqueness is checked among NON-EMPTY keys only. An unkeyed item is a plain
// string with an empty key, and a behavior routinely has several of those —
// treating them as colliding would fail the entire corpus.
func checkThenKeys(pf *parsedFile, pb *parsedBehavior, c *collector) {
	if pb.broken["then"] {
		return
	}
	seen := map[string]bool{}
	for _, it := range pb.b.Then {
		if it.Key == "" {
			continue // an unkeyed (plain-string) item
		}
		switch {
		case !thenKeyRegex.MatchString(it.Key):
			c.add(pf, orderThenKeys, pb.idx, it.Line, pb.ref,
				fmt.Sprintf("then item key %q must be lowercase alphanumeric words separated by hyphens", it.Key))
		case it.Key == waiverStatementKey:
			c.add(pf, orderThenKeys, pb.idx, it.Line, pb.ref,
				`then item key "statement" is reserved for statement-behavior waivers`)
		case seen[it.Key]:
			c.add(pf, orderThenKeys, pb.idx, it.Line, pb.ref,
				fmt.Sprintf("duplicate then item key %q", it.Key))
		}
		seen[it.Key] = true
	}
}

// checkWaivers enforces waiver semantics: waivers may appear only on ui- or
// api-surface behaviors (never on none, never on intents), every waived
// reference must resolve to an item of this behavior (a key that names one, or
// an index in range), references must be unique, and every reason must be
// non-empty. Placement is decidable only when the fields it depends on (type,
// surface) parsed clean; resolution needs a clean then list.
//
// Duplicate detection keys on the RESOLVED index, so a key-form waiver and an
// index-form waiver naming the same item are one duplicate, not two waivers.
func checkWaivers(pf *parsedFile, pb *parsedBehavior, c *collector) {
	if !pb.keys["waivers"] || pb.broken["waivers"] || len(pb.b.Waivers) == 0 {
		return
	}
	waiverLine := func(i int) int {
		if i < len(pb.waiverLines) {
			return pb.waiverLines[i]
		}
		return pb.line
	}

	// Placement: intents never carry waivers; other types only with surface ui
	// or api. An illegal placement is the root cause — the per-item checks below
	// would only add noise about a list that shouldn't exist, so report and stop.
	typeClean := pb.keys["type"] && !pb.broken["type"] && validType[pb.b.Type]
	if typeClean && pb.b.Type == "intent" {
		c.add(pf, orderWaivers, pb.idx, pb.line, pb.ref,
			"waivers are not for intent behaviors (intents are judge-only)")
		return
	}
	if pb.keys["surface"] && !pb.broken["surface"] && validSurface[pb.b.Surface] && pb.b.Surface != "ui" && pb.b.Surface != "api" {
		c.add(pf, orderWaivers, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("waivers are only for ui- or api-surface behaviors (surface %q)", pb.b.Surface))
		return
	}

	// A statement behavior (ui invariant) has exactly one waivable coverage
	// item, addressed as index 0.
	statement := typeClean && statementTypes[pb.b.Type]

	seen := map[int]bool{}
	for i, w := range pb.b.Waivers {
		if w.Reason == "" {
			c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref, "waiver reason must be non-empty")
		}
		if w.Keyed {
			// A key that names nothing is the root cause — reporting it and
			// stopping keeps one finding per bad reference.
			switch {
			case statement:
				if w.Key != waiverStatementKey {
					c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
						fmt.Sprintf("waiver then %q is not valid for a statement behavior (use the reserved key %q)", w.Key, waiverStatementKey))
					continue
				}
			case pb.broken["then"]:
				continue // the then list never parsed; a key cannot be resolved
			case w.Key == waiverStatementKey:
				// The mirror of the case above, and it needs its own message:
				// "statement" is reserved rather than merely absent, so the
				// generic unknown-key wording would send the author looking for
				// a then item they never meant to name.
				c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
					fmt.Sprintf("waiver then %q is reserved for statement behaviors; waive a then item of this behavior by its key", waiverStatementKey))
				continue
			default:
				if _, ok := resolveWaiverIndex(pb.b, w); !ok {
					c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
						fmt.Sprintf("waiver then %q names no then-item key of this behavior", w.Key))
					continue
				}
			}
		}
		idx, _ := resolveWaiverIndex(pb.b, w)
		if seen[idx] {
			c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
				fmt.Sprintf("duplicate waiver for then item %d", idx))
		}
		seen[idx] = true
		if w.Keyed {
			continue // a resolved key is in range by construction
		}
		switch {
		case statement:
			if idx != 0 {
				c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
					fmt.Sprintf("waiver then index %d out of range (a statement behavior has one implicit item, index 0)", idx))
			}
		case !pb.broken["then"] && (idx < 0 || idx >= len(pb.b.Then)):
			c.add(pf, orderWaivers, pb.idx, waiverLine(i), pb.ref,
				fmt.Sprintf("waiver then index %d out of range (behavior has %d then items)", idx, len(pb.b.Then)))
		}
	}
}

// resolveWaiverIndex resolves a waiver's reference to a 0-based then-item
// index. It reports ok=false only for a keyed waiver whose key names nothing:
// the reserved "statement" token on a behavior that has no statement, or a key
// that matches no then item. The legacy index form always resolves to itself —
// whether the index is IN RANGE is checkWaivers' job, not this function's, so
// a negative or past-the-end index still comes back ok.
func resolveWaiverIndex(b *Behavior, w Waiver) (int, bool) {
	if !w.Keyed {
		return w.Index, true
	}
	if w.Key == waiverStatementKey {
		// A statement behavior's single implicit item occupies index 0, the
		// same slot the legacy index form addressed.
		return 0, b.Statement != ""
	}
	for i, it := range b.Then {
		if it.Key != "" && it.Key == w.Key {
			return i, true
		}
	}
	return 0, false
}

func checkRequiredBehaviorField(pf *parsedFile, pb *parsedBehavior, c *collector, key, val string) {
	if pb.broken[key] {
		return // structural violation already reported; a missing-field report would double-count it
	}
	if !pb.keys[key] || val == "" {
		c.add(pf, orderRequiredBehavior, pb.idx, pb.line, pb.ref, fmt.Sprintf("missing required field %q", key))
	}
}

// checkGWT enforces the GWT-xor-statement rule. It is decidable only when the
// type is a known enum value; an absent/invalid type is already reported by
// the required-field and enum checks.
func checkGWT(pf *parsedFile, pb *parsedBehavior, c *collector) {
	if !pb.keys["type"] || pb.broken["type"] || !validType[pb.b.Type] {
		return
	}
	givenPresent := pb.keys["given"]
	whenPresent := pb.keys["when"]
	thenPresent := pb.keys["then"]
	stmtPresent := pb.keys["statement"]

	if statementTypes[pb.b.Type] {
		// statement present + non-empty AND none of given/when/then present.
		// Each presence test is suppressed for a structurally broken field:
		// the parser already reported that field, and re-reporting it here
		// would be two violations for one root cause.
		if !pb.broken["statement"] && (!stmtPresent || pb.b.Statement == "") {
			c.add(pf, orderGWT, pb.idx, pb.line, pb.ref,
				fmt.Sprintf("%s behavior must have a non-empty statement", pb.b.Type))
		}
		if (givenPresent && !pb.broken["given"]) || (whenPresent && !pb.broken["when"]) || (thenPresent && !pb.broken["then"]) {
			c.add(pf, orderGWT, pb.idx, pb.line, pb.ref,
				fmt.Sprintf("%s behavior must not use given/when/then (statement replaces GWT)", pb.b.Type))
		}
		return
	}

	// Every other type: statement absent AND when present+non-empty AND then present with >=1 item.
	if stmtPresent && !pb.broken["statement"] {
		c.add(pf, orderGWT, pb.idx, pb.line, pb.ref,
			"statement is only for invariant and intent behaviors; other types use given/when/then")
	}
	if !pb.broken["when"] && (!whenPresent || pb.b.When == "") {
		c.add(pf, orderGWT, pb.idx, pb.line, pb.ref, "behavior must have a non-empty when")
	}
	if !pb.broken["then"] && (!thenPresent || len(pb.b.Then) == 0) {
		c.add(pf, orderGWT, pb.idx, pb.line, pb.ref, "behavior must have a then with at least one outcome")
	}
}

func checkListItems(pf *parsedFile, pb *parsedBehavior, c *collector, key string, items []string, broken bool) {
	if broken {
		return // structural violation already reported; the list never parsed
	}
	for _, item := range items {
		if item == "" {
			c.add(pf, orderListItems, pb.idx, pb.line, pb.ref, fmt.Sprintf("%s list items must be non-empty strings", key))
		}
	}
}

// checkIDDupInFile enforces within-file ID uniqueness: one violation per ID
// that occurs more than once in a single file, reported at the second
// occurrence.
func checkIDDupInFile(pf *parsedFile, idInFile map[string][]*parsedBehavior, c *collector) {
	ids := make([]string, 0, len(idInFile))
	for id := range idInFile {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		occ := idInFile[id]
		if len(occ) > 1 {
			second := occ[1]
			c.add(pf, orderIDDupFile, second.idx, second.line, second.ref, fmt.Sprintf("duplicate id %q within file", id))
		}
	}
}

// checkPrefixDup enforces cross-file prefix uniqueness: one file-level
// violation per file for any prefix declared by more than one file.
func checkPrefixDup(prefixFiles map[string][]*parsedFile, c *collector) {
	prefixes := make([]string, 0, len(prefixFiles))
	for p := range prefixFiles {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		files := prefixFiles[p]
		if len(files) <= 1 {
			continue
		}
		sorted := append([]*parsedFile(nil), files...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].path < sorted[j].path })
		for _, pf := range sorted {
			c.add(pf, orderPrefixUnique, -1, 0, "", fmt.Sprintf("prefix %q is not unique across files", p))
		}
	}
}

// checkIDDupGlobal enforces cross-file ID uniqueness: one file-level violation
// per file for any ID declared in more than one file.
func checkIDDupGlobal(parsed []*parsedFile, idFilePaths map[string]map[string]bool, c *collector) {
	byPath := map[string]*parsedFile{}
	for _, pf := range parsed {
		byPath[pf.path] = pf
	}
	ids := make([]string, 0, len(idFilePaths))
	for id := range idFilePaths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		paths := idFilePaths[id]
		if len(paths) <= 1 {
			continue
		}
		plist := make([]string, 0, len(paths))
		for p := range paths {
			plist = append(plist, p)
		}
		sort.Strings(plist)
		for _, p := range plist {
			c.add(byPath[p], orderIDDupGlobal, -1, 0, "", fmt.Sprintf("id %q is not unique across files", id))
		}
	}
}

// extractSorted orders scoped violations deterministically and projects them
// down to the exported Violation slice.
func extractSorted(items []scopedViolation) []Violation {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.v.Path != b.v.Path {
			return a.v.Path < b.v.Path
		}
		if a.bIdx != b.bIdx {
			return a.bIdx < b.bIdx
		}
		if a.order != b.order {
			return a.order < b.order
		}
		if a.v.Line != b.v.Line {
			return a.v.Line < b.v.Line
		}
		return a.v.Msg < b.v.Msg
	})
	out := make([]Violation, len(items))
	for i, it := range items {
		out[i] = it.v
	}
	return out
}

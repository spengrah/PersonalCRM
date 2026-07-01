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
// output is deterministic (D15): by file path, then behavior index (-1 =
// file-level, sorts first), then check order, then line, then message.
type scopedViolation struct {
	v     Violation
	bIdx  int
	order int
}

// Check-order ranks give violations within one scope a stable order (D15).
const (
	orderStructural       = 0  // parse/structural (checks 1–2, when-singular)
	orderRequiredFile     = 10 // check 3
	orderMaturityEnum     = 11 // check 4
	orderPrefixUnique     = 12 // check 5
	orderRequiredBehavior = 20 // check 6
	orderTypeEnum         = 21 // check 7
	orderStatusEnum       = 22 // check 8
	orderIDFormat         = 23 // check 9
	orderIDDupFile        = 24 // check 10
	orderIDDupGlobal      = 25 // check 11
	orderGWT              = 26 // check 13
	orderListItems        = 27 // check 14
)

var (
	validMaturity = map[string]bool{"draft": true, "reviewed": true, "ratified": true}
	validType     = map[string]bool{"business-logic": true, "api": true, "ux": true, "invariant": true, "data": true}
	validStatus   = map[string]bool{"current": true, "proposed": true, "retired": true}
)

// idRegex builds the ID pattern for a file's declared prefix (D4): the prefix
// literally (regexp.QuoteMeta), a hyphen, then a 3-digit number or a 4+-digit
// number with no leading zero.
func idRegex(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-(\d{3}|[1-9]\d{3,})$`)
}

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

// semanticChecks runs checks 3–14 over the parsed files. File-tier-broken files
// (D18 tier 1) are excluded entirely — they register no prefixes/IDs.
func semanticChecks(parsed []*parsedFile) []scopedViolation {
	c := &collector{}

	prefixFiles := map[string][]*parsedFile{}   // prefix -> files declaring it
	idFilePaths := map[string]map[string]bool{} // id -> set of file paths declaring it

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
			}
		}
		checkIDDupInFile(pf, idInFile, c)
	}

	checkPrefixDup(prefixFiles, c)
	checkIDDupGlobal(parsed, idFilePaths, c)
	return c.items
}

func prefixUsable(pf *parsedFile) bool {
	return pf.fileKeys["prefix"] && !pf.fileBroken["prefix"] && pf.file.Prefix != ""
}

func checkFile(pf *parsedFile, c *collector) {
	// check 3: required file fields present + non-empty.
	checkRequiredFileField(pf, c, "domain", pf.file.Domain)
	checkRequiredFileField(pf, c, "prefix", pf.file.Prefix)
	checkRequiredFileField(pf, c, "maturity", pf.file.Maturity)
	if !pf.fileKeys["behaviors"] {
		c.add(pf, orderRequiredFile, -1, 0, "", `missing required field "behaviors"`)
	}
	// check 4: maturity enum (only when present + non-empty + not broken).
	if pf.fileKeys["maturity"] && !pf.fileBroken["maturity"] && pf.file.Maturity != "" && !validMaturity[pf.file.Maturity] {
		c.add(pf, orderMaturityEnum, -1, 0, "",
			fmt.Sprintf("invalid maturity %q (want draft|reviewed|ratified)", pf.file.Maturity))
	}
}

func checkRequiredFileField(pf *parsedFile, c *collector, key, val string) {
	if pf.fileBroken[key] {
		return // structural violation already reported; don't cascade (D18 tier 2)
	}
	if !pf.fileKeys[key] || val == "" {
		c.add(pf, orderRequiredFile, -1, 0, "", fmt.Sprintf("missing required field %q", key))
	}
}

func checkBehavior(pf *parsedFile, pb *parsedBehavior, idRe *regexp.Regexp, c *collector) {
	// check 6: required behavior fields present + non-empty.
	checkRequiredBehaviorField(pf, pb, c, "id", pb.b.ID)
	checkRequiredBehaviorField(pf, pb, c, "title", pb.b.Title)
	checkRequiredBehaviorField(pf, pb, c, "type", pb.b.Type)
	checkRequiredBehaviorField(pf, pb, c, "status", pb.b.Status)

	// check 7: type enum.
	if pb.keys["type"] && !pb.broken["type"] && pb.b.Type != "" && !validType[pb.b.Type] {
		c.add(pf, orderTypeEnum, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("invalid type %q (want business-logic|api|ux|invariant|data)", pb.b.Type))
	}
	// check 8: status enum.
	if pb.keys["status"] && !pb.broken["status"] && pb.b.Status != "" && !validStatus[pb.b.Status] {
		c.add(pf, orderStatusEnum, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("invalid status %q (want current|proposed|retired)", pb.b.Status))
	}
	// check 9: id format (only when id is clean and the prefix is usable).
	if idRe != nil && pb.keys["id"] && !pb.broken["id"] && pb.b.ID != "" && !idRe.MatchString(pb.b.ID) {
		c.add(pf, orderIDFormat, pb.idx, pb.line, pb.ref,
			fmt.Sprintf("id %q must match %s-NNN (NNN = 3 digits, or 4+ with no leading zero)", pb.b.ID, pf.file.Prefix))
	}
	// check 13: GWT xor statement, presence-based by type.
	checkGWT(pf, pb, c)
	// check 14: given/then list items non-empty.
	checkListItems(pf, pb, c, "given", pb.b.Given, pb.broken["given"])
	checkListItems(pf, pb, c, "then", pb.b.Then, pb.broken["then"])
}

func checkRequiredBehaviorField(pf *parsedFile, pb *parsedBehavior, c *collector, key, val string) {
	if pb.broken[key] {
		return // structural violation already reported; don't cascade (D18 tier 4)
	}
	if !pb.keys[key] || val == "" {
		c.add(pf, orderRequiredBehavior, pb.idx, pb.line, pb.ref, fmt.Sprintf("missing required field %q", key))
	}
}

// checkGWT enforces check 13. It is decidable only when the type is a known
// enum value; an absent/invalid type is already reported by checks 6/7.
func checkGWT(pf *parsedFile, pb *parsedBehavior, c *collector) {
	if !pb.keys["type"] || pb.broken["type"] || !validType[pb.b.Type] {
		return
	}
	givenPresent := pb.keys["given"]
	whenPresent := pb.keys["when"]
	thenPresent := pb.keys["then"]
	stmtPresent := pb.keys["statement"]

	if pb.b.Type == "invariant" {
		// statement present + non-empty AND none of given/when/then present.
		if !pb.broken["statement"] && (!stmtPresent || pb.b.Statement == "") {
			c.add(pf, orderGWT, pb.idx, pb.line, pb.ref, "invariant behavior must have a non-empty statement")
		}
		if givenPresent || whenPresent || thenPresent {
			c.add(pf, orderGWT, pb.idx, pb.line, pb.ref,
				"invariant behavior must not use given/when/then (statement replaces GWT)")
		}
		return
	}

	// Every other type: statement absent AND when present+non-empty AND then present with >=1 item.
	if stmtPresent {
		c.add(pf, orderGWT, pb.idx, pb.line, pb.ref,
			"statement is only for invariant behaviors; other types use given/when/then")
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
		return // structural violation already reported (D18 tier 4)
	}
	for _, item := range items {
		if item == "" {
			c.add(pf, orderListItems, pb.idx, pb.line, pb.ref, fmt.Sprintf("%s list items must be non-empty strings", key))
		}
	}
}

// checkIDDupInFile enforces check 10: one violation per ID that occurs more than
// once within a single file, reported at the second occurrence.
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

// checkPrefixDup enforces check 5: one file-level violation per file for any
// prefix declared by more than one file.
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

// checkIDDupGlobal enforces check 11: one file-level violation per file for any
// ID declared in more than one file.
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

// extractSorted orders scoped violations deterministically (D15) and projects
// them down to the exported Violation slice.
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

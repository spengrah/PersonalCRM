package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// YAML tags the walker distinguishes. yaml.v3 assigns these to scalar nodes.
const (
	tagStr   = "!!str"
	tagNull  = "!!null"
	tagInt   = "!!int"
	tagFloat = "!!float"
)

// parsedFile is the walker's rich output for one document: the typed File plus
// the presence/breakage bookkeeping the semantic pass needs. It is
// package-internal; the exported entrypoints project it down to *File +
// []Violation.
//
// Structural failures degrade by tier — each broken scope reports exactly its
// own violation and suppresses only its own downstream checks, so one root
// cause is never double-reported and a broken field never hides an unrelated
// finding elsewhere:
//
//   - file-tier (syntax error; root not a mapping; duplicate key in the root
//     mapping; behaviors present but not a sequence): the file reports only
//     that violation and is excluded from semantic AND cross-file checks —
//     its prefix and IDs never register.
//   - file-field-tier (a file-level field with a structurally wrong node):
//     that field's own semantic checks are skipped. If prefix is the broken
//     field, per-behavior ID-format checks are skipped too (they need the
//     prefix), but cleanly-parsed IDs still register for global uniqueness.
//   - entry-tier (a behaviors[i] that is not a mapping, or with a duplicate
//     key inside it): the entry is skipped entirely — no semantic checks, no
//     ID registration; sibling behaviors validate normally.
//   - behavior-field-tier (a behavior field with a structurally wrong node):
//     only that field's semantic checks are skipped; the behavior's other
//     fields validate normally and its cleanly-parsed ID registers.
type parsedFile struct {
	file       *File
	path       string
	fatal      bool // file-tier break: excluded from semantic + cross-file checks
	fileKeys   map[string]bool
	fileBroken map[string]bool // file-field-tier: structurally broken file fields
	behaviors  []*parsedBehavior
	parseViol  []scopedViolation
}

// parsedBehavior carries one behavior's typed value plus per-behavior presence
// and breakage tracking.
type parsedBehavior struct {
	b           *Behavior
	idx         int
	ref         string          // behavior ID when present, else "behaviors[i]"
	line        int             // behavior mapping line
	keys        map[string]bool // present keys
	broken      map[string]bool // fields with a structural/typing violation (suppress their semantic checks)
	skip        bool            // entry-tier break: skip all semantic checks + no ID registration
	waiverLines []int           // source line of each parsed waiver entry, parallel to b.Waivers
}

func (pf *parsedFile) emit(order, bIdx, line int, ref, msg string) {
	pf.parseViol = append(pf.parseViol, scopedViolation{
		v:     Violation{Path: pf.path, Ref: ref, Line: line, Msg: msg},
		bIdx:  bIdx,
		order: order,
	})
}

// ParseFile parses a single spec YAML file. It returns the typed File, any
// parse/structural violations (YAML syntax, node shape, string typing, and the
// when-singular rule), and an IO-level error if the file cannot be read.
// Semantic and cross-file checks are NOT run here — use Lint for those.
// Exported for the traceability scanner's parse-only needs.
func ParseFile(path string) (*File, []Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read spec file %s: %w", path, err)
	}
	pf := parseFileNode(path, data)
	return pf.file, extractSorted(pf.parseViol), nil
}

// ParseDir globs <dir>/*.yaml (top-level only; README.md, subdirectories, and
// .yml are ignored) and parses each file. It returns the typed files that
// parsed, all per-file parse/structural violations, and an IO-level error only
// if the directory (or a file within it) cannot be read. Per-file parse
// failures surface as violations so one broken file does not hide the others.
func ParseDir(dir string) ([]*File, []Violation, error) {
	parsed, err := parseDirInternal(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []*File
	var viol []scopedViolation
	for _, pf := range parsed {
		if pf.file != nil {
			files = append(files, pf.file)
		}
		viol = append(viol, pf.parseViol...)
	}
	return files, extractSorted(viol), nil
}

// parseDirInternal is the shared glob+read+walk used by both ParseDir and Lint.
// The returned error is IO-level only (directory or file unreadable).
func parseDirInternal(dir string) ([]*parsedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read spec directory %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".yaml" {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths) // deterministic file order

	parsed := make([]*parsedFile, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read spec file %s: %w", p, err)
		}
		parsed = append(parsed, parseFileNode(p, data))
	}
	return parsed, nil
}

// parseFileNode walks the decoded yaml.Node tree of one file into a parsedFile,
// emitting parse/structural violations and degrading broken scopes per the
// tiered rule documented on parsedFile.
func parseFileNode(path string, data []byte) *parsedFile {
	pf := &parsedFile{
		path:       path,
		fileKeys:   map[string]bool{},
		fileBroken: map[string]bool{},
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// YAML syntax error — file-tier. yaml.v3 error text already
		// carries the line.
		pf.fatal = true
		pf.emit(orderStructural, -1, 0, "", fmt.Sprintf("YAML parse error: %v", err))
		return pf
	}

	// Unwrap the document node to the top-level content.
	doc := &root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			pf.fatal = true
			pf.emit(orderStructural, -1, 0, "", "file is empty; expected a mapping")
			return pf
		}
		doc = root.Content[0]
	}

	if doc.Kind != yaml.MappingNode {
		pf.fatal = true
		pf.emit(orderStructural, -1, doc.Line, "", "document root must be a mapping")
		return pf
	}

	fields, dupRoot := mappingFields(doc)
	if len(dupRoot) > 0 {
		// Root-level duplicate key — file-tier.
		pf.fatal = true
		pf.emit(orderStructural, -1, doc.Line, "",
			fmt.Sprintf("duplicate key %q in document mapping", dupRoot[0]))
		return pf
	}

	file := &File{Path: path}

	// behaviors: presence + must be a sequence when present (file-tier if not).
	if bn, ok := fields["behaviors"]; ok {
		pf.fileKeys["behaviors"] = true
		if bn.Kind != yaml.SequenceNode {
			pf.fatal = true
			pf.emit(orderStructural, -1, bn.Line, "", "behaviors must be a sequence")
			return pf
		}
		for i, item := range bn.Content {
			pf.behaviors = append(pf.behaviors, pf.walkBehavior(i, item))
		}
	}

	// domain / prefix / maturity: any scalar, string-coerced. A non-scalar
	// node is a file-field-tier structural violation.
	file.Domain = pf.fileScalar(fields, "domain")
	file.Prefix = pf.fileScalar(fields, "prefix")
	file.Maturity = pf.fileScalar(fields, "maturity")

	// e2e_settled is the retired boolean form of settled. The parser reads no
	// value from it — it only records presence so the semantic pass can reject
	// a lingering legacy key (an unread unknown root key would otherwise lint
	// green and silently disable ui blocking).
	if _, ok := fields["e2e_settled"]; ok {
		pf.fileKeys["e2e_settled"] = true
	}

	// settled: optional list of surfaces whose orphans block. A scalar (or any
	// non-sequence) node is a file-field-tier structural violation — fail-closed,
	// not silently normalized.
	file.Settled = pf.fileSurfaceList(fields, "settled")

	file.Behaviors = make([]Behavior, 0, len(pf.behaviors))
	for _, pb := range pf.behaviors {
		// A structurally unusable entry (non-mapping / duplicate key) never
		// populated its Behavior; exporting the zero-value stub would hand
		// downstream consumers a phantom all-empty behavior. It is already
		// reported as a violation, so drop it from the typed output.
		if pb.skip {
			continue
		}
		file.Behaviors = append(file.Behaviors, *pb.b)
	}
	pf.file = file
	return pf
}

// fileScalar extracts a file-level scalar field, recording presence and
// emitting a file-field-tier structural violation for a non-scalar node.
func (pf *parsedFile) fileScalar(fields map[string]*yaml.Node, key string) string {
	node, ok := fields[key]
	if !ok {
		return ""
	}
	pf.fileKeys[key] = true
	if node.Kind != yaml.ScalarNode {
		pf.fileBroken[key] = true
		pf.emit(orderStructural, -1, node.Line, "", fmt.Sprintf("%s must be a scalar", key))
		return ""
	}
	return node.Value
}

// fileSurfaceList extracts an optional file-level list-of-surfaces field: a
// sequence of !!str scalars (the list), or !!null (nil). A non-null scalar
// (e.g. `settled: ui`) is rejected as a structural violation — fail-closed, NOT
// normalized into a one-element list, so a mis-typed settlement can never
// silently under-declare its surfaces. A non-!!str sequence item or a mapping is
// also a file-field-tier structural violation. Presence is recorded regardless,
// so the semantic pass can reject an explicit-but-empty settled (null or []) as
// distinct from an absent key. Element membership (ui|api; none reserved) and
// duplicates are also the semantic pass's job, not the parser's.
func (pf *parsedFile) fileSurfaceList(fields map[string]*yaml.Node, key string) []string {
	node, ok := fields[key]
	if !ok {
		return nil
	}
	pf.fileKeys[key] = true

	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == tagNull {
			return nil // present but null — absent-equivalent value
		}
		pf.fileBroken[key] = true
		pf.emit(orderStructural, -1, node.Line, "", fmt.Sprintf("%s must be a list of surfaces", key))
		return nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != tagStr {
				pf.fileBroken[key] = true
				pf.emit(orderStructural, -1, item.Line, "", fmt.Sprintf("%s items must be surfaces", key))
				return nil
			}
			out = append(out, item.Value)
		}
		return out
	default:
		pf.fileBroken[key] = true
		pf.emit(orderStructural, -1, node.Line, "", fmt.Sprintf("%s must be a list of surfaces", key))
		return nil
	}
}

// walkBehavior walks one behaviors[i] node into a parsedBehavior, degrading
// per the tiered rule on parsedFile (entry-tier for a non-mapping or duplicate
// key; behavior-field-tier for a structurally broken field).
func (pf *parsedFile) walkBehavior(idx int, node *yaml.Node) *parsedBehavior {
	pb := &parsedBehavior{
		b:      &Behavior{Line: node.Line},
		idx:    idx,
		ref:    fmt.Sprintf("behaviors[%d]", idx),
		line:   node.Line,
		keys:   map[string]bool{},
		broken: map[string]bool{},
	}

	if node.Kind != yaml.MappingNode {
		// Entry-tier: not a mapping.
		pb.skip = true
		pf.emit(orderStructural, idx, node.Line, pb.ref, "behavior entry must be a mapping")
		return pb
	}

	fields, dup := mappingFields(node)
	if len(dup) > 0 {
		// Behavior-level duplicate key — entry-tier.
		pb.skip = true
		pf.emit(orderStructural, idx, node.Line, pb.ref,
			fmt.Sprintf("duplicate key %q in behavior mapping", dup[0]))
		return pb
	}

	// Scalar-coerced fields (any scalar; non-scalar = behavior-field-tier).
	pb.b.ID = pf.behaviorScalar(pb, fields, "id")
	pb.b.Title = pf.behaviorScalar(pb, fields, "title")
	pb.b.Type = pf.behaviorScalar(pb, fields, "type")
	pb.b.Status = pf.behaviorScalar(pb, fields, "status")
	pb.b.Surface = pf.behaviorScalar(pb, fields, "surface")
	pb.b.Notes = pf.behaviorScalar(pb, fields, "notes")

	if pb.keys["id"] && !pb.broken["id"] && pb.b.ID != "" {
		pb.ref = pb.b.ID
	}

	// Fields the schema mandates as strings: !!str (or !!null → empty) only.
	pb.b.When = pf.behaviorWhen(pb, fields)
	pb.b.Statement = pf.behaviorString(pb, fields, "statement")

	// given / then / serves: !!str scalar (→ one-element list) or a sequence
	// of !!str.
	pb.b.Given = pf.behaviorStrList(pb, fields, "given")
	pb.b.Then = pf.behaviorStrList(pb, fields, "then")
	pb.b.Serves = pf.behaviorStrList(pb, fields, "serves")

	// waivers: a sequence of {then: <int>, reason: <string>} mappings.
	pb.b.Waivers = pf.behaviorWaivers(pb, fields)

	// provenance: best-effort scalar list, no checks (non-load-bearing).
	pb.b.Provenance = provenance(fields)

	return pb
}

// behaviorScalar extracts a behavior scalar field (any scalar, string-coerced),
// recording presence and emitting a behavior-field-tier structural violation for
// a non-scalar node.
func (pf *parsedFile) behaviorScalar(pb *parsedBehavior, fields map[string]*yaml.Node, key string) string {
	node, ok := fields[key]
	if !ok {
		return ""
	}
	pb.keys[key] = true
	if node.Kind != yaml.ScalarNode {
		pb.broken[key] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref, fmt.Sprintf("%s must be a scalar", key))
		return ""
	}
	return node.Value
}

// behaviorString extracts a behavior field the schema mandates as a string: a
// !!str scalar (value, possibly empty) or !!null (empty). Any other scalar tag
// or a non-scalar node is a behavior-field-tier structural violation. (Fields
// the schema does NOT call strings go through behaviorScalar instead and
// string-coerce any scalar.)
func (pf *parsedFile) behaviorString(pb *parsedBehavior, fields map[string]*yaml.Node, key string) string {
	node, ok := fields[key]
	if !ok {
		return ""
	}
	pb.keys[key] = true
	val, strOK := strScalar(node)
	if !strOK {
		pb.broken[key] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref, fmt.Sprintf("%s must be a string", key))
		return ""
	}
	return val
}

// behaviorWhen extracts the when field. A when sequence is reported exactly
// once, HERE, phrased as the schema rule — the field is then marked broken so
// the semantic pass does not re-report it as a missing when.
func (pf *parsedFile) behaviorWhen(pb *parsedBehavior, fields map[string]*yaml.Node) string {
	node, ok := fields["when"]
	if !ok {
		return ""
	}
	pb.keys["when"] = true
	if node.Kind == yaml.SequenceNode {
		pb.broken["when"] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref,
			"when must be a single string (a behavior needing two whens is two behaviors)")
		return ""
	}
	val, strOK := strScalar(node)
	if !strOK {
		pb.broken["when"] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref, "when must be a string")
		return ""
	}
	return val
}

// behaviorStrList extracts a given/then field: a !!str scalar (normalized to a
// one-element list), !!null scalar (nil — absent-equivalent), or a sequence of
// !!str scalars. Any other scalar tag, a non-!!str sequence item, or a mapping
// is a behavior-field-tier structural violation.
func (pf *parsedFile) behaviorStrList(pb *parsedBehavior, fields map[string]*yaml.Node, key string) []string {
	node, ok := fields[key]
	if !ok {
		return nil
	}
	pb.keys[key] = true

	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == tagNull {
			return nil // present but null — absent-equivalent value
		}
		if node.Tag != tagStr {
			pb.broken[key] = true
			pf.emit(orderStructural, pb.idx, node.Line, pb.ref,
				fmt.Sprintf("%s items must be strings", key))
			return nil
		}
		return []string{node.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != tagStr {
				pb.broken[key] = true
				pf.emit(orderStructural, pb.idx, item.Line, pb.ref,
					fmt.Sprintf("%s items must be strings", key))
				return nil
			}
			out = append(out, item.Value)
		}
		return out
	default:
		pb.broken[key] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref,
			fmt.Sprintf("%s must be a string or a list of strings", key))
		return nil
	}
}

// behaviorWaivers extracts the waivers field: a sequence of mappings, each with
// an integer `then` and a string `reason`. Any shape deviation (non-sequence,
// non-mapping item, missing/mistyped sub-field, duplicate key in an item) is a
// behavior-field-tier structural violation that marks the whole field broken —
// the semantic pass (index range, duplicates, reason non-empty, ui-or-api
// placement) never sees a partially-parsed waiver list.
func (pf *parsedFile) behaviorWaivers(pb *parsedBehavior, fields map[string]*yaml.Node) []Waiver {
	node, ok := fields["waivers"]
	if !ok {
		return nil
	}
	pb.keys["waivers"] = true

	if node.Kind == yaml.ScalarNode && node.Tag == tagNull {
		return nil // present but null — absent-equivalent value
	}
	if node.Kind != yaml.SequenceNode {
		pb.broken["waivers"] = true
		pf.emit(orderStructural, pb.idx, node.Line, pb.ref,
			"waivers must be a list of {then, reason} mappings")
		return nil
	}

	out := make([]Waiver, 0, len(node.Content))
	lines := make([]int, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, item.Line, pb.ref,
				"waivers items must be {then, reason} mappings")
			return nil
		}
		wf, dup := mappingFields(item)
		if len(dup) > 0 {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, item.Line, pb.ref,
				fmt.Sprintf("duplicate key %q in waiver mapping", dup[0]))
			return nil
		}
		thenNode, thenOK := wf["then"]
		if !thenOK || thenNode.Kind != yaml.ScalarNode || thenNode.Tag != tagInt {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, item.Line, pb.ref,
				"waiver then must be an integer then-item index")
			return nil
		}
		idx, err := strconv.Atoi(thenNode.Value)
		if err != nil {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, thenNode.Line, pb.ref,
				"waiver then must be an integer then-item index")
			return nil
		}
		reasonNode, reasonOK := wf["reason"]
		if !reasonOK {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, item.Line, pb.ref, "waiver must have a reason")
			return nil
		}
		reason, strOK := strScalar(reasonNode)
		if !strOK {
			pb.broken["waivers"] = true
			pf.emit(orderStructural, pb.idx, reasonNode.Line, pb.ref, "waiver reason must be a string")
			return nil
		}
		out = append(out, Waiver{Then: idx, Reason: reason})
		lines = append(lines, item.Line)
	}
	pb.waiverLines = lines
	return out
}

// provenance best-effort collects a scalar list; it carries no schema checks
// (non-load-bearing, allowed to rot).
func provenance(fields map[string]*yaml.Node) []string {
	node, ok := fields["provenance"]
	if !ok {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == tagNull {
			return nil
		}
		return []string{node.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode {
				out = append(out, item.Value)
			}
		}
		return out
	default:
		return nil
	}
}

// strScalar reports the string value of a scalar node when it is a !!str (any
// value, including "") or !!null (→ ""). It returns ok=false for a non-string
// scalar (!!int / !!float / !!bool) or a non-scalar node. A !!null node yields
// "" regardless of its source text: yaml.v3 preserves the literal token in
// Node.Value (so `when: null` has Value "null"), but null means "no value".
func strScalar(node *yaml.Node) (string, bool) {
	if node.Kind != yaml.ScalarNode {
		return "", false
	}
	switch node.Tag {
	case tagNull:
		return "", true
	case tagStr:
		return node.Value, true
	default:
		return "", false
	}
}

// mappingFields turns a mapping node's content into a key→value-node map and
// returns the list of any duplicate keys. yaml.v3's yaml.Node decode path does
// NOT enforce mapping-key uniqueness (unlike its struct/map decode paths), so
// the walker restores parity here while building the presence map it needs — a
// silent last-win duplicate would make presence tracking unsound.
func mappingFields(node *yaml.Node) (map[string]*yaml.Node, []string) {
	m := make(map[string]*yaml.Node, len(node.Content)/2)
	var dups []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if _, exists := m[key]; exists {
			dups = append(dups, key)
			continue // keep the first binding; the duplicate is a violation
		}
		m[key] = node.Content[i+1]
	}
	return m, dups
}

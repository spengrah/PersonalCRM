package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// YAML tags the walker distinguishes. yaml.v3 assigns these to scalar nodes.
const (
	tagStr   = "!!str"
	tagNull  = "!!null"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagBool  = "!!bool"
)

// parsedFile is the walker's rich output for one document: the typed File plus
// the presence/breakage bookkeeping the semantic pass needs (D16). It is
// package-internal; the exported entrypoints project it down to *File +
// []Violation.
type parsedFile struct {
	file       *File
	path       string
	fatal      bool // file-tier (D18 tier 1): excluded from semantic + cross-file checks
	fileKeys   map[string]bool
	fileBroken map[string]bool // file-field-tier (D18 tier 2): structurally broken file fields
	behaviors  []*parsedBehavior
	parseViol  []scopedViolation
}

// parsedBehavior carries one behavior's typed value plus per-behavior presence
// and breakage tracking.
type parsedBehavior struct {
	b      *Behavior
	idx    int
	ref    string          // behavior ID when present, else "behaviors[i]"
	line   int             // behavior mapping line
	keys   map[string]bool // present keys
	broken map[string]bool // fields with a structural/typing violation (suppress their semantic checks)
	skip   bool            // entry-tier (D18 tier 3): skip all semantic checks + no ID registration
}

func (pf *parsedFile) emit(order, bIdx, line int, ref, msg string) {
	pf.parseViol = append(pf.parseViol, scopedViolation{
		v:     Violation{Path: pf.path, Ref: ref, Line: line, Msg: msg},
		bIdx:  bIdx,
		order: order,
	})
}

// ParseFile parses a single spec YAML file. It returns the typed File, any
// parse/structural violations (checks 1–2 and the when-singular rule), and an
// IO-level error if the file cannot be read. Semantic and cross-file checks are
// NOT run here — use Lint for those. Exported for Piece 3's parse-only needs.
func ParseFile(path string) (*File, []Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read spec file %s: %w", path, err)
	}
	pf := parseFileNode(path, data)
	return pf.file, extractSorted(pf.parseViol), nil
}

// ParseDir globs <dir>/*.yaml (top-level only; README.md, subdirectories, and
// .yml are ignored — D11) and parses each file. It returns the typed files that
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
	sort.Strings(paths) // deterministic file order (D15)

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
// emitting parse/structural violations and tiering per D18.
func parseFileNode(path string, data []byte) *parsedFile {
	pf := &parsedFile{
		path:       path,
		fileKeys:   map[string]bool{},
		fileBroken: map[string]bool{},
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// YAML syntax error — file-tier (D18 tier 1). yaml.v3 error text
		// already carries the line.
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
		// Root-level duplicate key — file-tier (D18 tier 1).
		pf.fatal = true
		pf.emit(orderStructural, -1, doc.Line, "",
			fmt.Sprintf("duplicate key %q in document mapping", dupRoot[0]))
		return pf
	}

	file := &File{Path: path}

	// behaviors: presence + must be a sequence when present (D18 tier 1 if not).
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

	// domain / prefix / maturity: any scalar, string-coerced (D16). A non-scalar
	// node is a file-field-tier structural violation (D18 tier 2).
	file.Domain = pf.fileScalar(fields, "domain")
	file.Prefix = pf.fileScalar(fields, "prefix")
	file.Maturity = pf.fileScalar(fields, "maturity")

	file.Behaviors = make([]Behavior, 0, len(pf.behaviors))
	for _, pb := range pf.behaviors {
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

// walkBehavior walks one behaviors[i] node into a parsedBehavior, tiering per
// D18 (entry-tier for a non-mapping or duplicate key; behavior-field-tier for a
// structurally broken field).
func (pf *parsedFile) walkBehavior(idx int, node *yaml.Node) *parsedBehavior {
	pb := &parsedBehavior{
		b:      &Behavior{},
		idx:    idx,
		ref:    fmt.Sprintf("behaviors[%d]", idx),
		line:   node.Line,
		keys:   map[string]bool{},
		broken: map[string]bool{},
	}

	if node.Kind != yaml.MappingNode {
		// Entry-tier (D18 tier 3): not a mapping.
		pb.skip = true
		pf.emit(orderStructural, idx, node.Line, pb.ref, "behavior entry must be a mapping")
		return pb
	}

	fields, dup := mappingFields(node)
	if len(dup) > 0 {
		// Behavior-level duplicate key — entry-tier (D18 tier 3).
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
	pb.b.Notes = pf.behaviorScalar(pb, fields, "notes")

	if pb.keys["id"] && !pb.broken["id"] && pb.b.ID != "" {
		pb.ref = pb.b.ID
	}

	// String-typed fields: !!str (or !!null → empty) only (D16).
	pb.b.When = pf.behaviorWhen(pb, fields)
	pb.b.Statement = pf.behaviorString(pb, fields, "statement")

	// given / then: !!str scalar (→ one-element list) or a sequence of !!str.
	pb.b.Given = pf.behaviorStrList(pb, fields, "given")
	pb.b.Then = pf.behaviorStrList(pb, fields, "then")

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

// behaviorString extracts a !!str-typed behavior field (D16): a !!str scalar
// (value, possibly empty) or !!null (empty). Any other scalar tag or a
// non-scalar node is a behavior-field-tier structural violation.
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

// behaviorWhen extracts the when field with the schema-rule message for a
// sequence (check 12's single emission site: a when list is reported once, at
// parse time, phrased as the schema rule).
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

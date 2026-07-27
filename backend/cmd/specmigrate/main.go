// Command specmigrate is the one-shot migration and verification tool for the
// spec-citation-by-key arc (GH #760, PR2). It keys the cited-and-waived
// then-items of the behavior corpus, rewrites every positional citation and
// waiver to the keyed form, and carries the three mechanical guards (G1-G3)
// that prove the rewrite faithful.
//
// It is wired into nothing — no CI job, no git hook, and exactly one make
// target (test-unit's package list, which runs slug_test.go). PR3 deletes the
// whole directory. See README.md.
//
// Exit codes are meaningful and are read directly by the caller:
//
//	0  ok
//	1  a violation was found (guards) — the migration is not faithful
//	2  operational error (bad arguments, dirty tree, unresolvable reference)
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"personal-crm/backend/internal/spec"
)

// Re-measured at this PR's base (1dd9b72) with the scanner's own code. The tool
// aborts rather than proceeding if the corpus has moved underneath it.
const (
	// expectedKeyedItems is the number of cited-or-waived then-items:
	// 380 distinct cited (behavior, index) pairs + 13 waived-but-uncited.
	expectedKeyedItems = 393
	// expectedSubstitutions is the number of positional references Pass B
	// rewrites: 597 in-root citations + 2 out-of-root I4 citations + 17
	// waivers.
	expectedSubstitutions = 616
)

// extraCitationPaths are real whole-line `// spec:` markers that live OUTSIDE
// CollectCitations' two scan roots (backend/**/*_test.go and
// frontend/tests/e2e/**/*.spec.ts) and are therefore invisible to the scanner.
// Pass B rewrites them from the same key map as everything else, and G1 folds
// them into its citation leg — otherwise they would be the only positional
// references in the whole migration with no mechanical guard at all.
var extraCitationPaths = []string{
	"frontend/src/lib/__tests__/contact-recency.test.ts",
}

// markerLine and refPattern mirror coverage.go's unexported citationLine and
// citationRef. They exist here because this tool must parse marker lines it
// also has to REWRITE (byte spans, not just parsed values) and must apply the
// scanner's own rules to the extra paths above, which the scanner cannot see.
// Keep them in lockstep with coverage.go for the life of this tool (PR3).
var (
	markerLine = regexp.MustCompile(`^\s*// spec: (.+?)\s*$`)
	refPattern = regexp.MustCompile(`^([A-Z][A-Z0-9]*-\d+)(?:\[(\d+)\]|\.([a-z0-9]+(?:-[a-z0-9]+)*))?$`)
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "keys":
		code = cmdKeys(os.Args[2:])
	case "cite":
		code = cmdCite(os.Args[2:])
	case "g1":
		code = cmdG1(os.Args[2:])
	case "g2":
		code = cmdG2(os.Args[2:])
	case "g3":
		code = cmdG3(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `specmigrate — one-shot spec citation key migration (GH #760, PR2)

  specmigrate keys -out <dir> [--write] <root>            pass A: key the cited/waived then-items
  specmigrate cite -out <dir> [--write] <root>            pass B: rewrite waivers and citations
  specmigrate g1 <base-ref> <root>                        map faithfulness over all 616 references
  specmigrate g2 [-subs <f>] <base> <tool> <passB> <head> <root>   diff shape + commit disjointness
  specmigrate g3 <base-ref> <root>                        corpus text preservation
`)
}

// newFlagSet returns a subcommand flag set that reports its own usage rather
// than the package-level default.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = usage
	return fs
}

func opErr(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "specmigrate: "+format+"\n", a...)
	return 2
}

// ---------------------------------------------------------------- helpers

func absRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// requireCleanTree refuses to write into a tree with uncommitted changes. It is
// what makes the rollback recipe unambiguous: pass A runs right after the tool
// commit and pass B right after the pass-A commit, so `git checkout --` always
// restores exactly the pass that failed.
func requireCleanTree(root string) error {
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("working tree is dirty; --write refuses to run:\n%s", out)
	}
	return nil
}

// requireOutsideRoot rejects an artifact directory inside the repository:
// artifacts written into the working tree would be untracked files that break
// the `git status --short` must-be-empty checks the guards depend on.
func requireOutsideRoot(out, root string) error {
	abs, err := absRoot(out)
	if err != nil {
		return err
	}
	if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return fmt.Errorf("-out %s resolves inside the repository at %s; choose a path outside it", abs, root)
	}
	return os.MkdirAll(abs, 0o755)
}

// loadCorpus parses <dir>/spec and fails on any lint violation: every pass and
// guard reasons over a corpus the linter accepts.
func loadCorpus(dir string) ([]*spec.File, error) {
	files, viols, err := spec.Lint(filepath.Join(dir, "spec"))
	if err != nil {
		return nil, err
	}
	if len(viols) > 0 {
		var b strings.Builder
		for _, v := range viols {
			fmt.Fprintf(&b, "  %s:%d: %s\n", v.Path, v.Line, v.Msg)
		}
		return nil, fmt.Errorf("corpus at %s has %d lint violation(s):\n%s", dir, len(viols), b.String())
	}
	return files, nil
}

func behaviorsByID(files []*spec.File) map[string]*spec.Behavior {
	out := map[string]*spec.Behavior{}
	for _, f := range files {
		for i := range f.Behaviors {
			out[f.Behaviors[i].ID] = &f.Behaviors[i]
		}
	}
	return out
}

// keyAtIndex maps (behavior ID, then index) to the item's key.
func keyAtIndex(files []*spec.File) map[string]map[int]string {
	out := map[string]map[int]string{}
	for _, f := range files {
		for _, b := range f.Behaviors {
			m := map[int]string{}
			for i, it := range b.Then {
				if it.Key != "" {
					m[i] = it.Key
				}
			}
			out[b.ID] = m
		}
	}
	return out
}

// indexOfKey maps (behavior ID, then-item key) to the item's 0-based index —
// derived from the FINAL corpus through the exported parser, never from a
// ledger. It is what G1 checks the old indexes against.
func indexOfKey(files []*spec.File) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, f := range files {
		for _, b := range f.Behaviors {
			m := map[string]int{}
			for i, it := range b.Then {
				if it.Key != "" {
					m[it.Key] = i
				}
			}
			out[b.ID] = m
		}
	}
	return out
}

// ref is one reference parsed out of a marker line, with the byte span it
// occupies so a rewrite can replace exactly those bytes and leave every other
// byte of the line — indentation, spacing, separators — untouched.
type ref struct {
	Text       string
	ID         string
	Index      int // -1 when the reference carries no index
	Key        string
	Start, End int
}

// parseMarkerRefs returns the references on a whole-line `// spec:` marker, in
// source order. ok=false when the line is not such a marker.
func parseMarkerRefs(line string) ([]ref, bool) {
	m := markerLine.FindStringSubmatchIndex(line)
	if m == nil {
		return nil, false
	}
	base, content := m[2], line[m[2]:m[3]]
	var out []ref
	start := 0
	for i := 0; i <= len(content); i++ {
		if i != len(content) && content[i] != ',' {
			continue
		}
		seg := content[start:i]
		ls, rs := 0, len(seg)
		for ls < rs && (seg[ls] == ' ' || seg[ls] == '\t') {
			ls++
		}
		for rs > ls && (seg[rs-1] == ' ' || seg[rs-1] == '\t') {
			rs--
		}
		if rs > ls {
			r := ref{Text: seg[ls:rs], Index: -1, Start: base + start + ls, End: base + start + rs}
			if rm := refPattern.FindStringSubmatch(r.Text); rm != nil {
				r.ID = rm[1]
				if rm[2] != "" {
					n, err := strconv.Atoi(rm[2])
					if err == nil {
						r.Index = n
					}
				}
				r.Key = rm[3]
			}
			out = append(out, r)
		}
		start = i + 1
	}
	return out, true
}

// replaceSpans rewrites the given byte spans of line, right to left so earlier
// offsets stay valid.
func replaceSpans(line string, spans []ref, repl []string) string {
	out := line
	for k := len(spans) - 1; k >= 0; k-- {
		out = out[:spans[k].Start] + repl[k] + out[spans[k].End:]
	}
	return out
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// yamlMapValue returns the value node for key in a mapping node.
func yamlMapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// yamlMapKeyNode returns the KEY node for key in a mapping node. Its Line is
// the source line the sub-key is written on.
func yamlMapKeyNode(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i]
		}
	}
	return nil
}

// waiverThenLines discovers, per behavior ID, the 1-based source line of each
// waiver's `then:` entry, with a READ-ONLY yaml.Node decode.
//
// The node tree is never re-encoded: a re-encode would reflow the corpus's long
// notes: scalars and relocate its 97 comments, producing an unreviewable diff.
// Reading line numbers and then editing bytes line-by-line keeps the diff
// minimal. An indentation search would not do — a waiver's `- then: 1` sits at
// exactly the same six-space indent as a then item.
func waiverThenLines(path string) (map[string][]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("decode %s: empty document", path)
	}
	behaviors := yamlMapValue(doc.Content[0], "behaviors")
	if behaviors == nil || behaviors.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("decode %s: no behaviors sequence", path)
	}
	out := map[string][]int{}
	for _, bn := range behaviors.Content {
		idNode := yamlMapValue(bn, "id")
		wv := yamlMapValue(bn, "waivers")
		if idNode == nil || wv == nil || wv.Kind != yaml.SequenceNode {
			continue
		}
		lines := make([]int, 0, len(wv.Content))
		for _, wn := range wv.Content {
			kn := yamlMapKeyNode(wn, "then")
			if kn == nil {
				return nil, fmt.Errorf("decode %s: waiver of %s has no then", path, idNode.Value)
			}
			lines = append(lines, kn.Line)
		}
		out[idNode.Value] = lines
	}
	return out, nil
}

var waiverIndexLine = regexp.MustCompile(`^      - then: (\d+)$`)

// ---------------------------------------------------------------- pass A

type slugRow struct {
	ID    string
	Index int
	Key   string
	Text  string
	Path  string
	Line  int
	Flags []string
}

func cmdKeys(args []string) int {
	fs := newFlagSet("keys")
	out := fs.String("out", "", "artifact output directory (must resolve outside <root>)")
	write := fs.Bool("write", false, "write the corpus edits (default: dry run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *out == "" {
		usage()
		return 2
	}
	root, err := absRoot(fs.Arg(0))
	if err != nil {
		return opErr("%v", err)
	}
	if err := requireOutsideRoot(*out, root); err != nil {
		return opErr("%v", err)
	}
	outDir, _ := absRoot(*out)
	if *write {
		if err := requireCleanTree(root); err != nil {
			return opErr("%v", err)
		}
	}

	files, err := loadCorpus(root)
	if err != nil {
		return opErr("%v", err)
	}
	byID := behaviorsByID(files)

	// Which items must be keyed: every cited (behavior, index) pair — from the
	// two scan roots AND the extra out-of-root paths — plus every waived item.
	need := map[string]map[int]bool{}
	mark := func(id string, n int) {
		if need[id] == nil {
			need[id] = map[int]bool{}
		}
		need[id][n] = true
	}
	cites, probs, err := spec.CollectCitations(filepath.Join(root, "backend"), filepath.Join(root, "frontend", "tests", "e2e"))
	if err != nil {
		return opErr("collect citations: %v", err)
	}
	if len(probs) > 0 {
		return opErr("citation problems in the scan roots: %v", probs)
	}
	for _, c := range cites {
		if c.Key == "" && c.Then >= 0 {
			mark(c.ID, c.Then)
		}
	}
	extra, err := scanExtraPaths(root)
	if err != nil {
		return opErr("%v", err)
	}
	for _, c := range extra {
		if c.Index >= 0 {
			mark(c.ID, c.Index)
		}
	}
	for _, f := range files {
		for i := range f.Behaviors {
			b := &f.Behaviors[i]
			for _, w := range b.Waivers {
				n, ok := resolveWaiver(b, w)
				if !ok {
					return opErr("waiver of %s resolves to no then item", b.ID)
				}
				mark(b.ID, n)
			}
		}
	}
	total := 0
	for id, m := range need {
		b, ok := byID[id]
		if !ok {
			return opErr("cited behavior %s is not in the corpus", id)
		}
		for n := range m {
			if n < 0 || n >= len(b.Then) {
				return opErr("%s[%d] is out of range (behavior has %d then items)", id, n, len(b.Then))
			}
			total++
		}
	}
	if total != expectedKeyedItems {
		return opErr("cited-or-waived then items = %d, expected %d — the corpus moved; re-measure before proceeding",
			total, expectedKeyedItems)
	}

	// Preconditions, asserted across the WHOLE corpus before any file is
	// written: the transform moves a text from sequence-entry position to
	// mapping-value position, and a plain scalar's legality differs between
	// them by its first character.
	if err := checkPreconditions(files); err != nil {
		return opErr("%v", err)
	}

	// Mint. Deterministic order: files in the linter's resolution order,
	// behaviors in file order, items in list order. Uniqueness is over the
	// NON-EMPTY keys of one behavior — an unkeyed sibling is a plain string
	// with an empty key and is not a collision.
	var rows []slugRow
	edits := map[string]map[int][]string{} // path -> 1-based line -> replacement lines
	minted, skipped := 0, 0
	for _, f := range files {
		for bi := range f.Behaviors {
			b := &f.Behaviors[bi]
			taken := map[string]bool{}
			for _, it := range b.Then {
				if it.Key != "" {
					taken[it.Key] = true
				}
			}
			for i, it := range b.Then {
				if !need[b.ID][i] {
					continue
				}
				if it.Key != "" {
					// Never re-key an item that already has one — that rule,
					// not any property of the slug function, is what makes the
					// migration idempotent. The row is still emitted so the
					// table always covers every cited-or-waived item, and a
					// partially-migrated corpus cannot yield a short table.
					skipped++
					rows = append(rows, slugRow{ID: b.ID, Index: i, Key: it.Key, Text: it.Text,
						Path: f.Path, Line: it.Line, Flags: flagSlug(it.Key, it.Text)})
					continue
				}
				key, err := mintSlug(it.Text, taken)
				if err != nil {
					return opErr("%s[%d]: %v", b.ID, i, err)
				}
				taken[key] = true
				minted++
				rows = append(rows, slugRow{ID: b.ID, Index: i, Key: key, Text: it.Text,
					Path: f.Path, Line: it.Line, Flags: flagSlug(key, it.Text)})
				if edits[f.Path] == nil {
					edits[f.Path] = map[int][]string{}
				}
				edits[f.Path][it.Line] = []string{"      - key: " + key, "        text: " + it.Text}
			}
		}
	}

	// Validate-all-then-write-any: build every file's new content in memory and
	// only then write, so a failure on file 7 of 12 leaves nothing on disk.
	staged := map[string][]string{}
	for _, f := range files {
		fileEdits := edits[f.Path]
		if len(fileEdits) == 0 {
			continue
		}
		lines, err := readLines(f.Path)
		if err != nil {
			return opErr("%v", err)
		}
		nums := make([]int, 0, len(fileEdits))
		for n := range fileEdits {
			nums = append(nums, n)
		}
		// Bottom-up within the file: pass A is the only pass that changes line
		// counts, and descending order needs no offset bookkeeping.
		sort.Sort(sort.Reverse(sort.IntSlice(nums)))
		for _, n := range nums {
			repl := fileEdits[n]
			text := strings.TrimPrefix(repl[1], "        text: ")
			if lines[n-1] != "      - "+text {
				return opErr("%s:%d: expected %q, found %q — refusing to write", f.Path, n, "      - "+text, lines[n-1])
			}
			lines = append(lines[:n-1], append(append([]string{}, repl...), lines[n:]...)...)
		}
		staged[f.Path] = lines
	}

	if len(rows) != total {
		return opErr("slug table has %d rows for %d cited-or-waived items", len(rows), total)
	}
	if err := writeSlugArtifacts(outDir, rows); err != nil {
		return opErr("%v", err)
	}
	fmt.Printf("keys: %d cited-or-waived items; %d slugs minted, %d already keyed (skipped)\n", total, minted, skipped)
	fmt.Printf("keys: %d corpus files to rewrite; artifacts in %s\n", len(staged), outDir)
	if !*write {
		fmt.Println("keys: DRY RUN — pass --write to apply the corpus edits")
		return 0
	}
	for path, lines := range staged {
		if err := writeLines(path, lines); err != nil {
			return opErr("write %s: %v", path, err)
		}
	}
	fmt.Printf("keys: wrote %d corpus files\n", len(staged))
	return 0
}

// resolveWaiver mirrors the linter's waiver resolution for the two forms this
// migration sees: an index-form waiver resolves to itself, a key-form one to
// the index of the item carrying that key.
func resolveWaiver(b *spec.Behavior, w spec.Waiver) (int, bool) {
	if !w.Keyed {
		return w.Index, true
	}
	for i, it := range b.Then {
		if it.Key != "" && it.Key == w.Key {
			return i, true
		}
	}
	return 0, false
}

// yamlIndicator matches the YAML indicator characters that can change how a
// plain scalar resolves — or make it a parse error — in MAPPING-VALUE position,
// which is where the transform moves every then-item text.
var yamlIndicator = regexp.MustCompile("^[-?:,\\[\\]{}#&*!|>'\"%@`]")

func checkPreconditions(files []*spec.File) error {
	var probs []string
	items := 0
	for _, f := range files {
		lines, err := readLines(f.Path)
		if err != nil {
			return err
		}
		for _, b := range f.Behaviors {
			seen := map[string]bool{}
			for _, it := range b.Then {
				items++
				if seen[it.Text] {
					probs = append(probs, fmt.Sprintf("%s:%d: %s has two then items with the same text", f.Path, it.Line, b.ID))
				}
				seen[it.Text] = true
				if it.Key != "" {
					continue // already a mapping; the transform does not touch it
				}
				if it.Line < 1 || it.Line > len(lines) {
					probs = append(probs, fmt.Sprintf("%s:%d: then item line out of file range", f.Path, it.Line))
					continue
				}
				raw := lines[it.Line-1]
				switch {
				case !strings.HasPrefix(raw, "      - "):
					probs = append(probs, fmt.Sprintf("%s:%d: then item is not a six-space `      - ` sequence entry: %q", f.Path, it.Line, raw))
				case raw[len("      - "):] != it.Text:
					probs = append(probs, fmt.Sprintf("%s:%d: then item is not a single-line plain scalar (raw %q != parsed %q)", f.Path, it.Line, raw[8:], it.Text))
				}
				if strings.Contains(it.Text, ": ") {
					probs = append(probs, fmt.Sprintf("%s:%d: then item text contains %q, illegal in mapping-value position", f.Path, it.Line, ": "))
				}
				if strings.Contains(it.Text, " #") {
					probs = append(probs, fmt.Sprintf("%s:%d: then item text contains %q, which would open a comment", f.Path, it.Line, " #"))
				}
				if yamlIndicator.MatchString(it.Text) {
					probs = append(probs, fmt.Sprintf("%s:%d: then item text begins with a YAML indicator character: %q", f.Path, it.Line, it.Text))
				}
				if it.Text != strings.TrimSpace(it.Text) {
					probs = append(probs, fmt.Sprintf("%s:%d: then item text has leading or trailing whitespace", f.Path, it.Line))
				}
			}
		}
	}
	if len(probs) > 0 {
		return fmt.Errorf("corpus preconditions failed (%d):\n  %s", len(probs), strings.Join(probs, "\n  "))
	}
	fmt.Printf("keys: corpus preconditions OK over %d then items\n", items)
	return nil
}

// flagSlug labels the weak-slug classes the human review checkpoint works
// through first. These are advisory: the checkpoint reads every row, but a
// flagged one is where hand-editing pays off.
func flagSlug(key, text string) []string {
	var flags []string
	toks := strings.Split(key, "-")
	has := func(t string) bool {
		for _, k := range toks {
			if k == t {
				return true
			}
		}
		return false
	}
	for _, frag := range apostropheFragments(text) {
		if has(frag) {
			flags = append(flags, "apostrophe")
			break
		}
	}
	if key != slugBase(text) {
		flags = append(flags, "disambiguated")
	}
	for _, tok := range toks {
		if _, err := strconv.Atoi(tok); err == nil {
			flags = append(flags, "numeric-token")
			break
		}
	}
	if n := len(toks); n < 4 {
		flags = append(flags, fmt.Sprintf("short-%d-tokens", n))
	}
	if len(key) >= 38 {
		flags = append(flags, fmt.Sprintf("long-%d-chars", len(key)))
	}
	for _, tok := range toks {
		switch tok {
		case "no", "not", "never", "without", "unless", "nothing", "none":
			flags = append(flags, "negation:"+tok)
		}
	}
	return flags
}

func writeSlugArtifacts(outDir string, rows []slugRow) error {
	var table, review strings.Builder
	flagged := 0
	for _, r := range rows {
		fmt.Fprintf(&table, "%s[%d]\t%s\t%s\n", r.ID, r.Index, r.Key, r.Text)
		if len(r.Flags) > 0 {
			flagged++
			fmt.Fprintf(&review, "%-24s %-40s [%s]\n    %s:%d  %s\n",
				fmt.Sprintf("%s[%d]", r.ID, r.Index), r.Key, strings.Join(r.Flags, " "),
				filepath.Base(r.Path), r.Line, r.Text)
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "slug-table.txt"), []byte(table.String()), 0o644); err != nil {
		return err
	}
	header := fmt.Sprintf("# %d of %d minted slugs carry a review flag (advisory — read the full table too)\n\n", flagged, len(rows))
	return os.WriteFile(filepath.Join(outDir, "slug-review.txt"), []byte(header+review.String()), 0o644)
}

// ---------------------------------------------------------------- pass B

type substitution struct {
	Path string // repository-relative
	Line int
	Old  string
	New  string
}

type extraCite struct {
	Path  string
	Line  int
	ID    string
	Index int
}

// scanExtraPaths applies the scanner's own marker rules to the out-of-root
// citation paths.
func scanExtraPaths(root string) ([]extraCite, error) {
	var out []extraCite
	for _, rel := range extraCitationPaths {
		lines, err := readLines(filepath.Join(root, rel))
		if err != nil {
			return nil, fmt.Errorf("read extra citation path %s: %w", rel, err)
		}
		for i, line := range lines {
			refs, ok := parseMarkerRefs(line)
			if !ok {
				continue
			}
			for _, r := range refs {
				if r.ID == "" {
					return nil, fmt.Errorf("%s:%d: malformed reference %q", rel, i+1, r.Text)
				}
				out = append(out, extraCite{Path: rel, Line: i + 1, ID: r.ID, Index: r.Index})
			}
		}
	}
	return out, nil
}

func cmdCite(args []string) int {
	fs := newFlagSet("cite")
	out := fs.String("out", "", "artifact output directory (must resolve outside <root>)")
	write := fs.Bool("write", false, "write the rewrites (default: dry run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *out == "" {
		usage()
		return 2
	}
	root, err := absRoot(fs.Arg(0))
	if err != nil {
		return opErr("%v", err)
	}
	if err := requireOutsideRoot(*out, root); err != nil {
		return opErr("%v", err)
	}
	outDir, _ := absRoot(*out)
	if *write {
		if err := requireCleanTree(root); err != nil {
			return opErr("%v", err)
		}
	}

	// Re-read the FINAL corpus: one (ID, index) -> key map serves both the
	// waivers and the citations, so the human's checkpoint hand-edits are what
	// every reference points at.
	files, err := loadCorpus(root)
	if err != nil {
		return opErr("%v", err)
	}
	keyOf := keyAtIndex(files)

	var subs []substitution
	var unresolved []string
	staged := map[string][]string{}
	resolve := func(id string, n int) (string, bool) {
		k, ok := keyOf[id][n]
		return k, ok
	}

	// Marker lines: the two scan roots (via the scanner itself, so the scoping
	// is identical) plus the explicit extra paths.
	cites, probs, err := spec.CollectCitations(filepath.Join(root, "backend"), filepath.Join(root, "frontend", "tests", "e2e"))
	if err != nil {
		return opErr("collect citations: %v", err)
	}
	if len(probs) > 0 {
		return opErr("citation problems in the scan roots: %v", probs)
	}
	markerFiles := map[string]map[int]bool{}
	touch := func(path string, line int) {
		if markerFiles[path] == nil {
			markerFiles[path] = map[int]bool{}
		}
		markerFiles[path][line] = true
	}
	for _, c := range cites {
		if c.Key == "" && c.Then >= 0 {
			touch(c.Path, c.Line)
		}
	}
	extra, err := scanExtraPaths(root)
	if err != nil {
		return opErr("%v", err)
	}
	for _, c := range extra {
		if c.Index >= 0 {
			touch(filepath.Join(root, c.Path), c.Line)
		}
	}

	paths := make([]string, 0, len(markerFiles))
	for p := range markerFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		lines, err := readLines(path)
		if err != nil {
			return opErr("%v", err)
		}
		nums := make([]int, 0, len(markerFiles[path]))
		for n := range markerFiles[path] {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		rel, _ := filepath.Rel(root, path)
		for _, n := range nums {
			refs, ok := parseMarkerRefs(lines[n-1])
			if !ok {
				return opErr("%s:%d is not a whole-line spec marker", rel, n)
			}
			var spans []ref
			var repl []string
			for _, r := range refs {
				if r.Index < 0 {
					continue
				}
				key, found := resolve(r.ID, r.Index)
				if !found {
					unresolved = append(unresolved, fmt.Sprintf("%s has no key for %s[%d]  (cited at %s:%d)", r.ID, r.ID, r.Index, rel, n))
					continue
				}
				spans = append(spans, r)
				repl = append(repl, r.ID+"."+key)
				subs = append(subs, substitution{Path: rel, Line: n, Old: r.Text, New: r.ID + "." + key})
			}
			if len(spans) > 0 {
				lines[n-1] = replaceSpans(lines[n-1], spans, repl)
			}
		}
		staged[path] = lines
	}

	// Waivers: line discovery through a read-only node decode, cross-checked
	// against the parsed Waiver.Index before a byte is touched.
	for _, f := range files {
		lines, ok := staged[f.Path]
		if !ok {
			lines, err = readLines(f.Path)
			if err != nil {
				return opErr("%v", err)
			}
		}
		wl, err := waiverThenLines(f.Path)
		if err != nil {
			return opErr("%v", err)
		}
		rel, _ := filepath.Rel(root, f.Path)
		changed := false
		for bi := range f.Behaviors {
			b := &f.Behaviors[bi]
			if len(b.Waivers) == 0 {
				continue
			}
			ls := wl[b.ID]
			if len(ls) != len(b.Waivers) {
				return opErr("%s: %s has %d waivers but %d discovered lines", rel, b.ID, len(b.Waivers), len(ls))
			}
			for wi, w := range b.Waivers {
				if w.Keyed {
					continue // already migrated
				}
				n := ls[wi]
				m := waiverIndexLine.FindStringSubmatch(lines[n-1])
				if m == nil {
					return opErr("%s:%d: waiver line %q does not match the index form", rel, n, lines[n-1])
				}
				got, _ := strconv.Atoi(m[1])
				if got != w.Index {
					return opErr("%s:%d: waiver line names index %d but the parser read %d", rel, n, got, w.Index)
				}
				key, found := resolve(b.ID, w.Index)
				if !found {
					unresolved = append(unresolved, fmt.Sprintf("%s has no key for %s[%d]  (waived at %s:%d)", b.ID, b.ID, w.Index, rel, n))
					continue
				}
				lines[n-1] = "      - then: " + key
				subs = append(subs, substitution{Path: rel, Line: n,
					Old: fmt.Sprintf("%s[%d]", b.ID, w.Index), New: b.ID + "." + key})
				changed = true
			}
		}
		if changed {
			staged[f.Path] = lines
		}
	}

	// Fail closed BEFORE writing a byte: a reference with no key is an
	// operational error, not something to half-apply.
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		fmt.Fprintf(os.Stderr, "specmigrate: cite aborted — %d reference(s) have no key in the corpus; nothing was written:\n", len(unresolved))
		for _, u := range unresolved {
			fmt.Fprintf(os.Stderr, "  %s\n", u)
		}
		return 2
	}
	if len(subs) != expectedSubstitutions {
		return opErr("built %d substitutions, expected %d — the corpus or the citing files moved; nothing was written",
			len(subs), expectedSubstitutions)
	}

	// STABLE, and on (path, line) only: the rows for a single marker line must
	// stay in SOURCE order, because G2a replays them positionally against the
	// line's references to prove the rewrite was exactly the substitution.
	sort.SliceStable(subs, func(i, j int) bool {
		if subs[i].Path != subs[j].Path {
			return subs[i].Path < subs[j].Path
		}
		return subs[i].Line < subs[j].Line
	})
	var ledger strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&ledger, "%s\t%d\t%s\t%s\n", s.Path, s.Line, s.Old, s.New)
	}
	if err := os.WriteFile(filepath.Join(outDir, "substitutions.txt"), []byte(ledger.String()), 0o644); err != nil {
		return opErr("%v", err)
	}
	fmt.Printf("cite: %d substitutions across %d files; ledger in %s\n", len(subs), len(staged), outDir)
	if !*write {
		fmt.Println("cite: DRY RUN — pass --write to apply the rewrites")
		return 0
	}
	for path, lines := range staged {
		if err := writeLines(path, lines); err != nil {
			return opErr("write %s: %v", path, err)
		}
	}
	fmt.Printf("cite: wrote %d files\n", len(staged))
	return 0
}

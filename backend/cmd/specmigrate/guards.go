package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"personal-crm/backend/internal/spec"
)

// The three mechanical guards that carry PR2's correctness proof. The
// before/after spec-coverage report diff is explicitly NOT the proof: report()
// prints nothing for covered items, so any permutation of the (ID, n) -> key
// map inside a fully-covered behavior is byte-invisible to it.

// Counts re-measured at this PR's base with the scanner's own code. A guard
// that silently checks fewer references than it should is indistinguishable
// from one that passes.
const (
	expectG1InRoot  = 597 // indexed citations inside the two scan roots
	expectG1Extra   = 2   // indexed citations in extraCitationPaths
	expectG1Waivers = 17  // index-form waivers
	expectG1Total   = expectG1InRoot + expectG1Extra + expectG1Waivers

	expectG2aFiles = 68  // scan-root files whose marker lines are rewritten
	expectG2aPairs = 510 // marker lines rewritten (one - and one + each)

	expectG2cRemoved   = 410 // 393 then-item lines + 17 waiver lines
	expectG2cAdded     = 803 // 786 keyed then-item lines + 17 waiver lines
	expectG2cItemPairs = 393
	expectG2cWaivers   = 17
)

// i4Path is the single non-scan-root, non-corpus file the mechanical range may
// touch — the hand-sweep target from the plan's D5.
const i4Path = "frontend/src/lib/__tests__/contact-recency.test.ts"

// guard accumulates violations so one run reports every failure rather than the
// first.
type guard struct {
	name  string
	viols []string
}

func (g *guard) fail(format string, a ...any) {
	g.viols = append(g.viols, fmt.Sprintf(format, a...))
}

func (g *guard) opFail(format string, a ...any) int {
	return opErr("%s: "+format, append([]any{g.name}, a...)...)
}

func (g *guard) report() int {
	if len(g.viols) == 0 {
		fmt.Printf("%s: PASS\n", g.name)
		return 0
	}
	fmt.Printf("%s: FAIL — %d violation(s)\n", g.name, len(g.viols))
	for _, v := range g.viols {
		fmt.Printf("  %s\n", v)
	}
	return 1
}

// ---------------------------------------------------------------- git access

// archivePaths are the trees the guards need from a historical ref: the corpus,
// the two scan roots, and the out-of-root citation file's directory.
var archivePaths = []string{"spec", "backend", "frontend/tests/e2e", "frontend/src/lib/__tests__"}

// materialize extracts a ref's tree into a temporary directory with git
// archive. It deliberately avoids `git worktree add`, which would write
// bookkeeping into the repository the guards are inspecting.
func materialize(root, ref string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "specmigrate-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	tarPath := filepath.Join(dir, "tree.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	args := append([]string{"-C", root, "archive", "--format=tar", ref, "--"}, archivePaths...)
	cmd := exec.Command("git", args...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("git archive %s: %w", ref, err)
	}
	// cmd.Stdout is an *os.File, so os/exec hands the descriptor straight to
	// the child and the parent buffers nothing — a Close error here cannot
	// truncate the archive, and this check is not what makes a short archive
	// safe. That chain is `tar -xf` failing plus G1's per-leg count
	// assertions. Checked anyway because ignoring it would be a silent
	// discard of a real error (and errcheck agrees).
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write archive of %s: %w", ref, err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if out, err := exec.Command("tar", "-xf", tarPath, "-C", tree).CombinedOutput(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("tar -xf: %w: %s", err, out)
	}
	return tree, cleanup, nil
}

func gitLines(root string, args ...string) ([]string, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			res = append(res, l)
		}
	}
	return res, nil
}

func gitNames(root, rng, filter string) ([]string, error) {
	args := []string{"diff", "--name-only"}
	if filter != "" {
		args = append(args, "--diff-filter="+filter)
	}
	return gitLines(root, append(args, rng)...)
}

func gitShow(root, spec string) (string, error) {
	out, err := exec.Command("git", "-C", root, "show", spec).Output()
	if err != nil {
		return "", fmt.Errorf("git show %s: %w", spec, err)
	}
	return string(out), nil
}

// diffHunk is one -U0 hunk: the removed lines and the added lines, with the
// 1-based line each group starts at.
type diffHunk struct {
	Path               string
	OldStart, NewStart int
	Removed, Added     []string
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// gitDiffHunks assumes no changed line's own text begins with "-- " or "++ ",
// which would render as "--- ..." / "+++ ..." and be mistaken for a file
// header. That cannot occur in this migration's diff — every changed line is a
// `// spec:` marker or a six-space YAML sequence entry — and this tool does not
// outlive PR3, so the parser stays simple rather than tracking header position.

func gitDiffHunks(root, rng string) ([]diffHunk, error) {
	out, err := exec.Command("git", "-C", root, "diff", "-U0", "--no-color", "--no-ext-diff", rng).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", rng, err)
	}
	var hunks []diffHunk
	path := ""
	cur := -1
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			p := strings.TrimPrefix(l, "+++ ")
			path = strings.TrimPrefix(p, "b/")
			cur = -1
		case strings.HasPrefix(l, "--- "):
			cur = -1
		case strings.HasPrefix(l, "@@"):
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("unparsable hunk header %q", l)
			}
			oldStart, _ := strconv.Atoi(m[1])
			newStart, _ := strconv.Atoi(m[2])
			hunks = append(hunks, diffHunk{Path: path, OldStart: oldStart, NewStart: newStart})
			cur = len(hunks) - 1
		case cur >= 0 && strings.HasPrefix(l, "-"):
			hunks[cur].Removed = append(hunks[cur].Removed, l[1:])
		case cur >= 0 && strings.HasPrefix(l, "+"):
			hunks[cur].Added = append(hunks[cur].Added, l[1:])
		}
	}
	return hunks, nil
}

// inScanRoots reports whether a repository-relative path is one of the two
// surfaces the coverage scanner reads.
func inScanRoots(p string) bool {
	return (strings.HasPrefix(p, "backend/") && strings.HasSuffix(p, "_test.go")) ||
		(strings.HasPrefix(p, "frontend/tests/e2e/") && strings.HasSuffix(p, ".spec.ts"))
}

func inCorpus(p string) bool {
	return strings.HasPrefix(p, "spec/") && strings.HasSuffix(p, ".yaml")
}

// ---------------------------------------------------------------- G1

// lineCache reads a file once and serves its lines.
type lineCache map[string][]string

func (c lineCache) get(path string) ([]string, error) {
	if l, ok := c[path]; ok {
		return l, nil
	}
	l, err := readLines(path)
	if err != nil {
		return nil, err
	}
	c[path] = l
	return l, nil
}

// cmdG1 proves map faithfulness: for EVERY rewritten positional reference —
// citation and waiver — index_of_key(behavior, new_key) equals the old index.
// The old index comes from a materialized checkout of the base, never from the
// substitutions ledger: the ledger is the thing under test.
func cmdG1(args []string) int {
	if len(args) != 2 {
		usage()
		return 2
	}
	g := &guard{name: "g1"}
	root, err := absRoot(args[1])
	if err != nil {
		return g.opFail("%v", err)
	}
	baseDir, cleanup, err := materialize(root, args[0])
	defer cleanup()
	if err != nil {
		return g.opFail("%v", err)
	}

	newFiles, err := loadCorpus(root)
	if err != nil {
		return g.opFail("%v", err)
	}
	idxOf := indexOfKey(newFiles)
	cache := lineCache{}

	// checkLine pairs the references on one marker line, old against new, and
	// returns how many indexed references it checked. The pairing is positional
	// and sound because no marker line in the corpus mixes indexed with
	// non-indexed references, and the rewrite preserves reference order.
	checkLine := func(rel string, line int) int {
		oldLines, err := cache.get(filepath.Join(baseDir, rel))
		if err != nil {
			g.fail("%s: %v", rel, err)
			return 0
		}
		newLines, err := cache.get(filepath.Join(root, rel))
		if err != nil {
			g.fail("%s: %v", rel, err)
			return 0
		}
		if line > len(oldLines) || line > len(newLines) {
			g.fail("%s:%d: line is past the end of one of the two trees", rel, line)
			return 0
		}
		oldRefs, ok1 := parseMarkerRefs(oldLines[line-1])
		newRefs, ok2 := parseMarkerRefs(newLines[line-1])
		if !ok1 || !ok2 {
			g.fail("%s:%d: not a whole-line spec marker in both trees", rel, line)
			return 0
		}
		if len(oldRefs) != len(newRefs) {
			g.fail("%s:%d: reference count changed (%d -> %d)", rel, line, len(oldRefs), len(newRefs))
			return 0
		}
		checked := 0
		for k := range oldRefs {
			o, n := oldRefs[k], newRefs[k]
			if o.ID != n.ID {
				g.fail("%s:%d: reference %d changed behavior (%s -> %s)", rel, line, k, o.Text, n.Text)
				continue
			}
			if o.Index < 0 {
				if o.Text != n.Text {
					g.fail("%s:%d: non-indexed reference %q was rewritten to %q", rel, line, o.Text, n.Text)
				}
				continue
			}
			checked++
			if n.Key == "" {
				g.fail("%s:%d: %s was not rewritten to a keyed reference (now %q)", rel, line, o.Text, n.Text)
				continue
			}
			got, ok := idxOf[n.ID][n.Key]
			if !ok {
				g.fail("%s:%d: %s names key %q, which no then item of %s carries", rel, line, n.Text, n.Key, n.ID)
				continue
			}
			if got != o.Index {
				g.fail("%s:%d: %s -> %s MIS-REPOINTED: key sits at index %d, old index was %d",
					rel, line, o.Text, n.Text, got, o.Index)
			}
		}
		return checked
	}

	// Leg 1 — the two scan roots, discovered with the scanner's own code.
	baseCites, probs, err := spec.CollectCitations(filepath.Join(baseDir, "backend"), filepath.Join(baseDir, "frontend", "tests", "e2e"))
	if err != nil {
		return g.opFail("collect base citations: %v", err)
	}
	if len(probs) > 0 {
		return g.opFail("base tree has citation problems: %v", probs)
	}
	type pl struct {
		path string
		line int
	}
	done := map[pl]bool{}
	var order []pl
	for _, c := range baseCites {
		if c.Key != "" || c.Then < 0 {
			continue
		}
		rel, err := filepath.Rel(baseDir, c.Path)
		if err != nil {
			return g.opFail("%v", err)
		}
		k := pl{rel, c.Line}
		if !done[k] {
			done[k] = true
			order = append(order, k)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].path != order[j].path {
			return order[i].path < order[j].path
		}
		return order[i].line < order[j].line
	})
	inRoot := 0
	for _, k := range order {
		inRoot += checkLine(k.path, k.line)
	}

	// Leg 2 — the out-of-root markers the scanner cannot see. Without this leg
	// they would be the only positional references in the migration with no
	// mechanical guard at all.
	extraChecked := 0
	for _, rel := range extraCitationPaths {
		lines, err := cache.get(filepath.Join(baseDir, rel))
		if err != nil {
			return g.opFail("%v", err)
		}
		for i, l := range lines {
			refs, ok := parseMarkerRefs(l)
			if !ok {
				continue
			}
			indexed := false
			for _, r := range refs {
				if r.Index >= 0 {
					indexed = true
				}
			}
			if indexed {
				extraChecked += checkLine(rel, i+1)
			}
		}
	}

	// Leg 3 — waivers.
	baseFiles, err := loadCorpus(baseDir)
	if err != nil {
		return g.opFail("base corpus: %v", err)
	}
	newByID := behaviorsByID(newFiles)
	waivers := 0
	for _, f := range baseFiles {
		for bi := range f.Behaviors {
			ob := &f.Behaviors[bi]
			if len(ob.Waivers) == 0 {
				continue
			}
			nb, ok := newByID[ob.ID]
			if !ok {
				g.fail("%s: behavior disappeared from the corpus", ob.ID)
				continue
			}
			if len(nb.Waivers) != len(ob.Waivers) {
				g.fail("%s: waiver count changed (%d -> %d)", ob.ID, len(ob.Waivers), len(nb.Waivers))
				continue
			}
			for wi := range ob.Waivers {
				ow, nw := ob.Waivers[wi], nb.Waivers[wi]
				if ow.Keyed {
					continue // already keyed at the base — nothing was rewritten
				}
				waivers++
				if !nw.Keyed {
					g.fail("%s waiver %d: still in the index form after the migration", ob.ID, wi)
					continue
				}
				got, ok := idxOf[ob.ID][nw.Key]
				if !ok {
					g.fail("%s waiver %d: key %q matches no then item", ob.ID, wi, nw.Key)
					continue
				}
				if got != ow.Index {
					g.fail("%s waiver %d: MIS-REPOINTED: key %q sits at index %d, old index was %d",
						ob.ID, wi, nw.Key, got, ow.Index)
				}
			}
		}
	}

	if inRoot != expectG1InRoot {
		g.fail("in-root citation leg checked %d references, expected %d", inRoot, expectG1InRoot)
	}
	if extraChecked != expectG1Extra {
		g.fail("out-of-root citation leg checked %d references, expected %d", extraChecked, expectG1Extra)
	}
	if waivers != expectG1Waivers {
		g.fail("waiver leg checked %d references, expected %d", waivers, expectG1Waivers)
	}
	total := inRoot + extraChecked + waivers
	if total != expectG1Total {
		g.fail("checked %d references in total, expected %d", total, expectG1Total)
	}
	fmt.Printf("g1: %d references checked (%d in-root citations + %d out-of-root citations + %d waivers)\n",
		total, inRoot, extraChecked, waivers)
	return g.report()
}

// ---------------------------------------------------------------- G2

var (
	keyedItemLine  = regexp.MustCompile(`^      - key: ([a-z0-9]+(?:-[a-z0-9]+)*)$`)
	waiverKeyLine  = regexp.MustCompile(`^      - then: ([a-z0-9]+(?:-[a-z0-9]+)*)$`)
	markerDiffLine = regexp.MustCompile(`^\s*// spec: `)
)

func cmdG2(args []string) int {
	fs := newFlagSet("g2")
	subsPath := fs.String("subs", "/tmp/pr2-artifacts/substitutions.txt", "the pass-B substitutions ledger")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 5 {
		usage()
		return 2
	}
	g := &guard{name: "g2"}
	base, tool, passB, head := fs.Arg(0), fs.Arg(1), fs.Arg(2), fs.Arg(3)
	root, err := absRoot(fs.Arg(4))
	if err != nil {
		return g.opFail("%v", err)
	}
	mech := tool + ".." + passB
	branch := base + ".." + head

	ledger, err := loadLedger(*subsPath)
	if err != nil {
		return g.opFail("%v", err)
	}
	hunks, err := gitDiffHunks(root, mech)
	if err != nil {
		return g.opFail("%v", err)
	}

	g2aShape(g, root, mech, hunks, ledger)
	g2bPaths(g, root, mech, branch)
	g2cCorpus(g, root, tool, hunks)
	g2Disjoint(g, root, base, tool, passB, head)
	return g.report()
}

func loadLedger(path string) (map[string][]substitution, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read substitutions ledger: %w", err)
	}
	out := map[string][]substitution{}
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" {
			continue
		}
		parts := strings.Split(l, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("malformed ledger row %q", l)
		}
		line, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("malformed ledger row %q", l)
		}
		k := fmt.Sprintf("%s:%d", parts[0], line)
		out[k] = append(out[k], substitution{Path: parts[0], Line: line, Old: parts[2], New: parts[3]})
		n++
	}
	if n != expectedSubstitutions {
		return nil, fmt.Errorf("ledger has %d rows, expected %d", n, expectedSubstitutions)
	}
	return out, nil
}

// g2aShape proves the bulk rewrite in the two scan roots touched nothing but
// marker lines, and that each marker line's change was EXACTLY the reference
// substitution — a shape regex alone would pass a one-character edit to a key.
func g2aShape(g *guard, root, mech string, hunks []diffHunk, ledger map[string][]substitution) {
	files := map[string]bool{}
	removed, added := 0, 0
	for _, h := range hunks {
		if !inScanRoots(h.Path) {
			continue
		}
		files[h.Path] = true
		removed += len(h.Removed)
		added += len(h.Added)
		for _, l := range h.Removed {
			if !markerDiffLine.MatchString(l) {
				g.fail("g2a %s: removed a non-marker line: %q", h.Path, l)
			}
		}
		for _, l := range h.Added {
			if !markerDiffLine.MatchString(l) {
				g.fail("g2a %s: added a non-marker line: %q", h.Path, l)
			}
		}
		if len(h.Removed) != len(h.Added) {
			g.fail("g2a %s@%d: hunk is not a 1:1 replacement (%d removed, %d added)",
				h.Path, h.OldStart, len(h.Removed), len(h.Added))
			continue
		}
		for k := range h.Removed {
			oldLine, newLine := h.Removed[k], h.Added[k]
			lineNo := h.NewStart + k
			if h.OldStart+k != lineNo {
				g.fail("g2a %s:%d: line number moved (old %d, new %d)", h.Path, lineNo, h.OldStart+k, lineNo)
			}
			rows := ledger[fmt.Sprintf("%s:%d", h.Path, lineNo)]
			if len(rows) == 0 {
				g.fail("g2a %s:%d: marker line changed but the ledger has no substitution for it", h.Path, lineNo)
				continue
			}
			got, err := applySubs(oldLine, rows)
			if err != nil {
				g.fail("g2a %s:%d: %v", h.Path, lineNo, err)
				continue
			}
			if got != newLine {
				g.fail("g2a %s:%d: the change was not exactly the ledger substitution\n      ledger yields %q\n      tree has     %q",
					h.Path, lineNo, got, newLine)
			}
		}
	}
	if len(files) != expectG2aFiles {
		g.fail("g2a: %d scan-root files changed, expected %d", len(files), expectG2aFiles)
	}
	if removed != expectG2aPairs || added != expectG2aPairs {
		g.fail("g2a: %d removed / %d added scan-root lines, expected %d / %d",
			removed, added, expectG2aPairs, expectG2aPairs)
	}
	adr, err := gitNames(root, mech, "ADR")
	if err != nil {
		g.fail("g2a: %v", err)
	}
	for _, p := range adr {
		if inScanRoots(p) {
			g.fail("g2a: %s was added, deleted, or renamed in the mechanical range", p)
		}
	}
	fmt.Printf("g2a: %d files, %d removed + %d added marker lines (%d total), substitution-exact\n",
		len(files), removed, added, removed+added)
}

// applySubs replays the ledger rows for one line against the OLD line and
// returns what the rewrite should have produced, byte for byte.
func applySubs(oldLine string, rows []substitution) (string, error) {
	refs, ok := parseMarkerRefs(oldLine)
	if !ok {
		return "", fmt.Errorf("old line is not a whole-line spec marker")
	}
	var indexed []ref
	for _, r := range refs {
		if r.Index >= 0 {
			indexed = append(indexed, r)
		}
	}
	if len(indexed) != len(rows) {
		return "", fmt.Errorf("line has %d indexed references but the ledger has %d rows", len(indexed), len(rows))
	}
	repl := make([]string, len(rows))
	for k, r := range rows {
		if r.Old != indexed[k].Text {
			return "", fmt.Errorf("ledger row %d names %q, the old line has %q", k, r.Old, indexed[k].Text)
		}
		repl[k] = r.New
	}
	return replaceSpans(oldLine, indexed, repl), nil
}

// g2bPaths is the hand-sweep control plus the production-untouched assertion.
// The mechanical range may touch exactly one path outside the scan roots and
// the corpus; across the whole branch, PR2 MODIFIES no production source.
func g2bPaths(g *guard, root, mech, branch string) {
	changed, err := gitNames(root, mech, "")
	if err != nil {
		g.fail("g2b: %v", err)
		return
	}
	var outside []string
	for _, p := range changed {
		if !inScanRoots(p) && !inCorpus(p) {
			outside = append(outside, p)
		}
	}
	if len(outside) != 1 || outside[0] != i4Path {
		g.fail("g2b: the mechanical range touches %v outside the scan roots and the corpus, expected exactly [%s]",
			outside, i4Path)
	}

	modified, err := gitNames(root, branch, "M")
	if err != nil {
		g.fail("g2b: %v", err)
		return
	}
	for _, p := range modified {
		switch {
		case p == "Makefile", inCorpus(p), inScanRoots(p), p == i4Path:
			continue
		}
		g.fail("g2b: %s is MODIFIED across the branch, which the allowlist does not permit", p)
	}
	for _, p := range modified {
		if strings.HasPrefix(p, "frontend/src/") && p != i4Path {
			g.fail("g2b: %s is a modified frontend/src path other than the one hand-swept test file", p)
		}
		if strings.HasPrefix(p, "backend/") && !strings.HasSuffix(p, "_test.go") {
			g.fail("g2b: %s is a modified backend path that is not a _test.go file", p)
		}
	}

	addedFiles, err := gitNames(root, branch, "A")
	if err != nil {
		g.fail("g2b: %v", err)
		return
	}
	for _, p := range addedFiles {
		if strings.HasPrefix(p, "backend/cmd/specmigrate/") ||
			strings.HasPrefix(p, "backend/internal/spec/testdata/invalid/waiver-key-empty/") {
			continue
		}
		g.fail("g2b: %s is ADDED across the branch, which the allowlist does not permit", p)
	}
	for _, filter := range []string{"D", "R"} {
		names, err := gitNames(root, branch, filter)
		if err != nil {
			g.fail("g2b: %v", err)
			continue
		}
		for _, p := range names {
			g.fail("g2b: %s was %sed across the branch; the branch deletes and renames nothing", p, filter)
		}
	}
	fmt.Printf("g2b: mechanical range touches 1 path outside the scan roots and the corpus (%s); "+
		"branch modifies %d files, adds %d, deletes 0\n", i4Path, len(modified), len(addedFiles))
}

// g2cCorpus proves the corpus diff is only the keying transform: nothing
// reflowed a notes: scalar, moved a comment, or touched a given/when line. The
// classification counts are the plan's clauses; the inverse-transform identity
// below is the stronger statement that subsumes them.
func g2cCorpus(g *guard, root, toolRef string, hunks []diffHunk) {
	removed, added := 0, 0
	itemPairs, waiverPairs, unclassified := 0, 0, 0
	for _, h := range hunks {
		if !strings.HasPrefix(h.Path, "spec/") {
			continue
		}
		if !inCorpus(h.Path) {
			g.fail("g2c: %s changed under spec/ but is not a corpus yaml file", h.Path)
			continue
		}
		removed += len(h.Removed)
		added += len(h.Added)
		ri, ai := 0, 0
		for ri < len(h.Removed) {
			r := h.Removed[ri]
			switch {
			case waiverIndexLine.MatchString(r):
				if ai < len(h.Added) && waiverKeyLine.MatchString(h.Added[ai]) {
					ri, ai = ri+1, ai+1
					waiverPairs++
					continue
				}
			case strings.HasPrefix(r, "      - "):
				text := r[len("      - "):]
				if ai+1 < len(h.Added) && keyedItemLine.MatchString(h.Added[ai]) &&
					h.Added[ai+1] == "        text: "+text {
					ri, ai = ri+1, ai+2
					itemPairs++
					continue
				}
			}
			g.fail("g2c %s@%d: unclassified removed line %q", h.Path, h.OldStart, r)
			unclassified++
			ri++
		}
		for ; ai < len(h.Added); ai++ {
			g.fail("g2c %s@%d: unclassified added line %q", h.Path, h.NewStart, h.Added[ai])
			unclassified++
		}
	}
	if removed != expectG2cRemoved || added != expectG2cAdded {
		g.fail("g2c: %d removed / %d added corpus lines, expected %d / %d",
			removed, added, expectG2cRemoved, expectG2cAdded)
	}
	if itemPairs != expectG2cItemPairs {
		g.fail("g2c: %d then-item conversions, expected %d", itemPairs, expectG2cItemPairs)
	}
	if waiverPairs != expectG2cWaivers {
		g.fail("g2c: %d waiver conversions, expected %d", waiverPairs, expectG2cWaivers)
	}
	g2cInverse(g, root, toolRef)
	fmt.Printf("g2c: %d removed / %d added corpus lines; %d then-item + %d waiver conversions; %d unclassified\n",
		removed, added, itemPairs, waiverPairs, unclassified)
}

// g2cInverse reconstructs each corpus file's pre-migration bytes by undoing the
// keying transform on the FINAL file, and asserts byte identity against the
// tool commit's blob. Anything the migration touched that is not a key or a
// waiver target shows up here as a byte difference.
//
// It relies on the base corpus carrying zero keyed items and zero key-form
// waivers, which is true at this PR's base and is what makes "undo every key"
// the correct inverse.
func g2cInverse(g *guard, root, toolRef string) {
	files, err := loadCorpus(root)
	if err != nil {
		g.fail("g2c: %v", err)
		return
	}
	idxOf := indexOfKey(files)
	for _, f := range files {
		rel, err := filepath.Rel(root, f.Path)
		if err != nil {
			g.fail("g2c: %v", err)
			continue
		}
		lines, err := readLines(f.Path)
		if err != nil {
			g.fail("g2c: %v", err)
			continue
		}
		repl := map[int]string{}
		drop := map[int]bool{}
		for bi := range f.Behaviors {
			b := &f.Behaviors[bi]
			for _, it := range b.Then {
				if it.Key == "" {
					continue
				}
				if it.Line+1 > len(lines) {
					g.fail("g2c %s:%d: keyed item has no text line", rel, it.Line)
					continue
				}
				if lines[it.Line-1] != "      - key: "+it.Key || lines[it.Line] != "        text: "+it.Text {
					g.fail("g2c %s:%d: keyed item is not the canonical two-line form", rel, it.Line)
					continue
				}
				repl[it.Line] = "      - " + it.Text
				drop[it.Line+1] = true
			}
		}
		wl, err := waiverThenLines(f.Path)
		if err != nil {
			g.fail("g2c: %v", err)
			continue
		}
		for bi := range f.Behaviors {
			b := &f.Behaviors[bi]
			for wi, w := range b.Waivers {
				if !w.Keyed || wi >= len(wl[b.ID]) {
					continue
				}
				n, ok := idxOf[b.ID][w.Key]
				if !ok {
					g.fail("g2c %s: waiver key %q of %s matches no then item", rel, w.Key, b.ID)
					continue
				}
				repl[wl[b.ID][wi]] = fmt.Sprintf("      - then: %d", n)
			}
		}
		out := make([]string, 0, len(lines))
		for i, l := range lines {
			if drop[i+1] {
				continue
			}
			if r, ok := repl[i+1]; ok {
				out = append(out, r)
				continue
			}
			out = append(out, l)
		}
		want, err := gitShow(root, toolRef+":"+rel)
		if err != nil {
			g.fail("g2c: %v", err)
			continue
		}
		if got := strings.Join(out, "\n"); got != want {
			g.fail("g2c %s: undoing the keying transform does not reproduce %s — the diff carries more than the transform (%s)",
				rel, toolRef, firstDiff(want, got))
		}
	}
}

// firstDiff names the first differing line so a G2c failure points somewhere.
func firstDiff(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("first difference at line %d: want %q, got %q", i+1, a[i], b[i])
		}
	}
	return fmt.Sprintf("line counts differ: %d vs %d", len(a), len(b))
}

// g2Disjoint proves no file appears in more than one of the branch's three
// commit ranges, closing the "hide a bad edit in the tool commit or the
// carry-forward commit" hole in both directions.
func g2Disjoint(g *guard, root, base, tool, passB, head string) {
	ranges := []struct {
		name string
		rng  string
	}{
		{"base..tool", base + ".." + tool},
		{"tool..passB", tool + ".." + passB},
		{"passB..head", passB + ".." + head},
	}
	sets := make([]map[string]bool, len(ranges))
	for i, r := range ranges {
		names, err := gitNames(root, r.rng, "")
		if err != nil {
			g.fail("g2 disjointness: %v", err)
			return
		}
		sets[i] = map[string]bool{}
		for _, p := range names {
			sets[i][p] = true
		}
	}
	live := 0
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			if len(sets[i]) == 0 || len(sets[j]) == 0 {
				continue
			}
			live++
			for p := range sets[i] {
				if sets[j][p] {
					g.fail("g2 disjointness: %s appears in both %s and %s", p, ranges[i].name, ranges[j].name)
				}
			}
		}
	}
	if len(sets[2]) == 0 {
		fmt.Printf("g2 disjointness: third range empty, branch head == mechanical head; %d of 3 pairs live\n", live)
		return
	}
	fmt.Printf("g2 disjointness: %d of 3 pairs live\n", live)
}

// ---------------------------------------------------------------- G3

// cmdG3 proves the keying pass changed no assertion: every behavior's then
// TEXTS, its given/when/statement, its metadata, and its waiver reasons are
// identical before and after.
func cmdG3(args []string) int {
	if len(args) != 2 {
		usage()
		return 2
	}
	g := &guard{name: "g3"}
	root, err := absRoot(args[1])
	if err != nil {
		return g.opFail("%v", err)
	}
	baseDir, cleanup, err := materialize(root, args[0])
	defer cleanup()
	if err != nil {
		return g.opFail("%v", err)
	}
	baseFiles, err := loadCorpus(baseDir)
	if err != nil {
		return g.opFail("base corpus: %v", err)
	}
	newFiles, err := loadCorpus(root)
	if err != nil {
		return g.opFail("%v", err)
	}
	oldByID, newByID := behaviorsByID(baseFiles), behaviorsByID(newFiles)
	for id := range oldByID {
		if _, ok := newByID[id]; !ok {
			g.fail("%s: behavior disappeared", id)
		}
	}
	for id := range newByID {
		if _, ok := oldByID[id]; !ok {
			g.fail("%s: behavior appeared", id)
		}
	}
	ids := make([]string, 0, len(oldByID))
	for id := range oldByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := 0
	for _, id := range ids {
		o, n := oldByID[id], newByID[id]
		if n == nil {
			continue
		}
		cmpStr := func(field, a, b string) {
			if a != b {
				g.fail("%s: %s changed (%q -> %q)", id, field, a, b)
			}
		}
		cmpStr("title", o.Title, n.Title)
		cmpStr("type", o.Type, n.Type)
		cmpStr("status", o.Status, n.Status)
		cmpStr("surface", o.Surface, n.Surface)
		cmpStr("when", o.When, n.When)
		cmpStr("statement", o.Statement, n.Statement)
		cmpStr("notes", o.Notes, n.Notes)
		cmpList(g, id, "given", o.Given, n.Given)
		cmpList(g, id, "serves", o.Serves, n.Serves)
		cmpList(g, id, "then", o.ThenTexts(), n.ThenTexts())
		items += len(o.Then)
		var or, nr []string
		for _, w := range o.Waivers {
			or = append(or, w.Reason)
		}
		for _, w := range n.Waivers {
			nr = append(nr, w.Reason)
		}
		cmpList(g, id, "waiver reasons", or, nr)
	}
	fmt.Printf("g3: %d behaviors, %d then items compared\n", len(ids), items)
	return g.report()
}

func cmpList(g *guard, id, field string, a, b []string) {
	if len(a) != len(b) {
		g.fail("%s: %s length changed (%d -> %d)", id, field, len(a), len(b))
		return
	}
	for i := range a {
		if a[i] != b[i] {
			g.fail("%s: %s[%d] changed (%q -> %q)", id, field, i, a[i], b[i])
		}
	}
}

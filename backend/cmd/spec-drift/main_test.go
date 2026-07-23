package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"personal-crm/backend/internal/spec"
)

// ---------------------------------------------------------------------------
// extractTar — reject-not-skip policy (permit-and-skip only the global header)
// ---------------------------------------------------------------------------

func TestExtractTar_ValidControl(t *testing.T) {
	// Real-git-shaped: a pax_global_header first (every `git archive <commit>`
	// emits it), then a trailing-slash dir and two regular files.
	stream := tarWith(t, func(w *tar.Writer) {
		_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
			PAXRecords: map[string]string{"comment": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}})
		_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "spec/", Mode: 0o755})
		writeReg(t, w, "spec/a.yaml", "aaa")
		writeReg(t, w, "spec/b.yaml", "bbb")
	})
	dest := t.TempDir()
	if err := extractTar(bytes.NewReader(stream), dest); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "spec", "a.yaml")); got != "aaa" {
		t.Fatalf("a.yaml = %q, want aaa", got)
	}
	if got := readFile(t, filepath.Join(dest, "spec", "b.yaml")); got != "bbb" {
		t.Fatalf("b.yaml = %q, want bbb", got)
	}
	// The global header must never be materialized to disk.
	if _, err := os.Stat(filepath.Join(dest, "pax_global_header")); !os.IsNotExist(err) {
		t.Fatalf("pax_global_header must not be materialized (stat err = %v)", err)
	}
}

func TestExtractTar_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		build func(*tar.Writer)
	}{
		{"dotdot traversal", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "../evil", Size: 0})
		}},
		{"exact dotdot dir", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "..", Mode: 0o755})
		}},
		{"absolute path", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "/etc/x", Size: 0})
		}},
		{"symlink", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "t"})
		}},
		{"hardlink", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "spec/x.yaml"})
		}},
		{"char device", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeChar, Name: "c", Devmajor: 1, Devminor: 1})
		}},
		{"block device", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeBlock, Name: "b", Devmajor: 1, Devminor: 1})
		}},
		{"fifo", func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeFifo, Name: "f"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := tarWith(t, tc.build)
			if err := extractTar(bytes.NewReader(stream), t.TempDir()); err == nil {
				t.Fatalf("expected extractTar to reject %s, got nil error", tc.name)
			}
		})
	}
}

func TestExtractTar_Truncated(t *testing.T) {
	full := tarWith(t, func(w *tar.Writer) { writeReg(t, w, "a.txt", strings.Repeat("x", 4096)) })
	trunc := full[:600] // 512-byte header + partial body → unexpected EOF
	if err := extractTar(bytes.NewReader(trunc), t.TempDir()); err == nil {
		t.Fatalf("expected error for truncated stream")
	}
}

// ---------------------------------------------------------------------------
// GitHub workflow-command escaping
// ---------------------------------------------------------------------------

func TestEscapeData(t *testing.T) {
	tests := []struct{ in, want string }{
		{"%", "%25"},
		{"\r", "%0D"},
		{"\n", "%0A"},
		{"a,b:cé", "a,b:cé"}, // comma, colon, non-ASCII pass through in data
		{"%0D", "%250D"},     // % escapes first — no double-decode
	}
	for _, tc := range tests {
		if got := escapeData(tc.in); got != tc.want {
			t.Errorf("escapeData(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeProp(t *testing.T) {
	tests := []struct{ in, want string }{
		{"%", "%25"},
		{"\r", "%0D"},
		{"\n", "%0A"},
		{",", "%2C"},
		{":", "%3A"},
		{"é", "é"},       // non-ASCII passes through
		{"%0D", "%250D"}, // % escapes first
	}
	for _, tc := range tests {
		if got := escapeProp(tc.in); got != tc.want {
			t.Errorf("escapeProp(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWriteGitHubWarning_Golden feeds a Violation whose Path (property side) and
// Msg (data side) each carry all five special characters, and asserts full-line
// equality — proving the assembled annotation, not just the two helpers.
func TestWriteGitHubWarning_Golden(t *testing.T) {
	v := spec.Violation{
		Path: "spec/we,ird:100%.yaml",
		Line: 7,
		Ref:  "X-001",
		Msg:  "50% of\r\ncases, see: notes",
	}
	var buf bytes.Buffer
	if err := writeGitHubWarning(&buf, v); err != nil {
		t.Fatalf("writeGitHubWarning: %v", err)
	}
	want := "::warning file=spec/we%2Cird%3A100%25.yaml,line=7::X-001: 50%25 of%0D%0Acases, see: notes\n"
	if buf.String() != want {
		t.Fatalf("annotation\n got: %q\nwant: %q", buf.String(), want)
	}
}

func TestWriteGitHubWarning_OmitsZeroLine(t *testing.T) {
	var buf bytes.Buffer
	if err := writeGitHubWarning(&buf, spec.Violation{Path: "spec/x.yaml", Ref: "X-001", Msg: "m"}); err != nil {
		t.Fatalf("writeGitHubWarning: %v", err)
	}
	if strings.Contains(buf.String(), "line=") {
		t.Fatalf("line property should be omitted for line 0, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// run — argument contract
// ---------------------------------------------------------------------------

func TestRun_ArgCount(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	for _, args := range [][]string{{}, {"only-one"}, {"a", "b", "c"}} {
		var out, errb bytes.Buffer
		if code := run(args, failingGit("rev-parse"), &out, &errb); code != 2 {
			t.Fatalf("run(%v) = %d, want 2", args, code)
		}
	}
}

// ---------------------------------------------------------------------------
// run — real-git end-to-end (exit codes + drift semantics)
// ---------------------------------------------------------------------------

func TestRun_CleanNoDrift(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("outcome"))
	c1 := commit(t, root, "c1")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	if out.String() != "" {
		t.Fatalf("expected no output, got %q", out.String())
	}
}

// TestRun_DriftWarns is the mandatory path-normalization fixture: a behavior's
// then changes without touching its citing test → warn. Exercises filepath.Rel
// on the absolute tmpHead citation path vs the repo-relative diff path.
func TestRun_DriftWarns(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("original outcome"))
	c1 := commit(t, root, "c1")
	writeFile(t, root, "spec/x.yaml", validSpec("changed outcome"))
	commit(t, root, "c2 edit then only")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), "X-001") {
		t.Fatalf("expected drift warning naming X-001, got %q", out.String())
	}
}

// Null variant: editing the citing test alongside the behavior silences the
// warning — proving the changed-set intersection fires (not a fail-open no-op).
func TestRun_DriftNullVariant(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("original outcome"))
	c1 := commit(t, root, "c1")
	writeFile(t, root, "spec/x.yaml", validSpec("changed outcome"))
	writeFile(t, root, "backend/x_test.go", citingTest()+"// touched\n")
	commit(t, root, "c2 edit then + test")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	if strings.Contains(out.String(), "WARN") {
		t.Fatalf("expected silence (citing file touched), got %q", out.String())
	}
}

// TestRun_ReadsCommittedTreeNotWorktree proves the HEAD corpus AND its citations
// are read from the committed tree (git archive), never the working tree: after
// committing the drift, the worktree reverts the spec to the base text and
// deletes the citation — both uncommitted. If run read disk it would see no
// drift (spec reverted) or a zero-citation behavior (marker gone) and stay
// silent; the committed warning must still appear.
func TestRun_ReadsCommittedTreeNotWorktree(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("original outcome"))
	c1 := commit(t, root, "c1")
	writeFile(t, root, "spec/x.yaml", validSpec("changed outcome"))
	commit(t, root, "c2 drift")

	// Uncommitted worktree edits that would hide the drift if disk were read.
	writeFile(t, root, "spec/x.yaml", validSpec("original outcome")) // revert to base text
	writeFile(t, root, "backend/x_test.go", "package x\n")           // delete the citation
	// Deliberately NOT committed — HEAD still points at c2's drifted, cited tree.

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "X-001") {
		t.Fatalf("committed drift must warn from the git tree despite a reverted worktree, got %q", out.String())
	}
}

// TestRun_DriftAnnotation asserts the GitHub annotation carries the behavior ID
// and the repo-relative path, with a comma-bearing filename escaped to %2C.
func TestRun_DriftAnnotation(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	root := initRepo(t)
	writeFile(t, root, "spec/we,ird.yaml", validSpec("original outcome"))
	writeFile(t, root, "backend/x_test.go", citingTest())
	writeFile(t, root, "frontend/tests/e2e/.gitkeep", "")
	c1 := commit(t, root, "c1")
	writeFile(t, root, "spec/we,ird.yaml", validSpec("changed outcome"))
	commit(t, root, "c2")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "::warning ") {
		t.Fatalf("expected ::warning annotation, got %q", got)
	}
	if !strings.Contains(got, "X-001") {
		t.Fatalf("expected behavior id in annotation, got %q", got)
	}
	if !strings.Contains(got, "file=spec/we%2Cird.yaml") {
		t.Fatalf("expected %%2C-escaped repo-relative path, got %q", got)
	}
	// The annotation's line must be the behavior's REAL parsed source line, not a
	// hand-set constant — so removing the parser's Behavior.Line assignment (which
	// would drop ,line= from every CI annotation) fails this test.
	probe := filepath.Join(t.TempDir(), "probe.yaml")
	if err := os.WriteFile(probe, []byte(validSpec("changed outcome")), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, _, perr := spec.ParseFile(probe)
	if perr != nil || pf == nil || len(pf.Behaviors) != 1 {
		t.Fatalf("probe parse: %v (file = %+v)", perr, pf)
	}
	if pf.Behaviors[0].Line == 0 {
		t.Fatalf("parser did not set Behavior.Line; the annotation would omit ,line=")
	}
	if want := fmt.Sprintf(",line=%d::", pf.Behaviors[0].Line); !strings.Contains(got, want) {
		t.Fatalf("annotation must carry the parsed behavior line %q, got %q", want, got)
	}
}

func TestRun_UnresolvableBaseRef(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("o"))
	commit(t, root, "c1")

	if _, rc := runGitRC(root, "rev-parse", "--verify", "bogus-ref-name"); rc == 0 {
		t.Fatalf("precondition: bogus ref should not resolve")
	}
	var out, errb bytes.Buffer
	if code := run([]string{root, "bogus-ref-name"}, execGit, &out, &errb); code != 2 {
		t.Fatalf("bogus-ref exit = %d, want 2; stderr = %s", code, errb.String())
	}
	// New-branch case: assert origin/develop is genuinely ABSENT before run, so
	// the exit 2 proves the fail-closed new-branch path (not an incidental miss).
	if _, rc := runGitRC(root, "rev-parse", "--verify", "origin/develop"); rc == 0 {
		t.Fatalf("precondition: origin/develop must be absent in a fresh repo")
	}
	if code := run([]string{root, "origin/develop"}, execGit, &out, &errb); code != 2 {
		t.Fatalf("origin/develop-absent exit = %d, want 2; stderr = %s", code, errb.String())
	}
}

func TestRun_UnrelatedHistory(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("o"))
	mainTip := commit(t, root, "c1")

	// Orphan branch = a second root with no common ancestor.
	runGit(t, root, "checkout", "-q", "--orphan", "other")
	writeFile(t, root, "orphan.txt", "x")
	otherTip := commit(t, root, "orphan")
	runGit(t, root, "checkout", "-q", "main") // restore HEAD — load-bearing

	// Precondition: both tips resolve, and git reports "no merge-base" as exactly
	// rc 1 (not a generic nonzero error) — so run's exit 2 is the real
	// unrelated-history path, not an incidental git failure on a bad ref.
	_ = runGitTrim(t, root, "rev-parse", mainTip)
	_ = runGitTrim(t, root, "rev-parse", otherTip)
	if _, rc := runGitRC(root, "merge-base", "HEAD", otherTip); rc != 1 {
		t.Fatalf("precondition: want merge-base rc == 1 (no common ancestor), got %d", rc)
	}

	var out, errb bytes.Buffer
	if code := run([]string{root, otherTip}, execGit, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
}

func TestRun_ShallowClone(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	origin := initRepo(t)
	seedStdFixture(t, origin, validSpec("o"))
	base := commit(t, origin, "base")
	writeFile(t, origin, "main2.txt", "m")
	commit(t, origin, "main2")
	runGit(t, origin, "checkout", "-q", "-b", "feature", base)
	writeFile(t, origin, "feat2.txt", "f")
	commit(t, origin, "feat2")
	runGit(t, origin, "checkout", "-q", "main")

	parent := t.TempDir()
	clone := filepath.Join(parent, "clone")
	// file:// is required: a plain local-path --depth is silently ignored.
	runGit(t, parent, "clone", "-q", "--depth", "1", "--no-single-branch", "file://"+origin, clone)

	if s := runGitTrim(t, clone, "rev-parse", "--is-shallow-repository"); s != "true" {
		t.Fatalf("precondition: clone must be shallow, got %q", s)
	}
	_ = runGitTrim(t, clone, "rev-parse", "refs/remotes/origin/feature")
	if _, rc := runGitRC(clone, "merge-base", "HEAD", "origin/feature"); rc != 1 {
		t.Fatalf("precondition: want shallow merge-base rc == 1 (truncated history), got %d", rc)
	}

	var out, errb bytes.Buffer
	if code := run([]string{clone, "origin/feature"}, execGit, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
}

func TestRun_HeadLintFailure(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, specMissingWhen)
	c1 := commit(t, root, "c1")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "lint violations") {
		t.Fatalf("stderr should report the dirty HEAD corpus, got %q", errb.String())
	}
}

// TestRun_CorruptBaseObjectDB proves a corrupt/unreadable base tree exits 2 via
// the ls-tree probe — never conflated with confirmed absence (exit 0).
func TestRun_CorruptBaseObjectDB(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("original"))
	baseSHA := commit(t, root, "c1")
	writeFile(t, root, "spec/x.yaml", validSpec("changed")) // distinct root tree
	headSHA := commit(t, root, "c2")

	// (1) Capture BOTH root trees before any deletion; assert they differ.
	baseTree := runGitTrim(t, root, "rev-parse", baseSHA+"^{tree}")
	headTree := runGitTrim(t, root, "rev-parse", headSHA+"^{tree}")
	if baseTree == headTree {
		t.Fatalf("precondition: base and head root trees must differ")
	}
	// (2) The base root tree is a loose object; assert it exists, then delete it.
	obj := filepath.Join(root, ".git", "objects", baseTree[:2], baseTree[2:])
	if _, err := os.Stat(obj); err != nil {
		t.Fatalf("precondition: expected loose base tree object: %v", err)
	}
	if err := os.Remove(obj); err != nil {
		t.Fatalf("remove base tree object: %v", err)
	}
	// (3) HEAD materialization still succeeds; only the base tree is unreadable.
	if _, rc := runGitRC(root, "archive", headSHA, "spec", "backend", "frontend/tests/e2e"); rc != 0 {
		t.Fatalf("precondition: head archive should still succeed (rc=%d)", rc)
	}
	if _, rc := runGitRC(root, "archive", baseSHA, "spec"); rc == 0 {
		t.Fatalf("precondition: base archive should fail after object deletion")
	}

	var out, errb bytes.Buffer
	code := run([]string{root, baseSHA}, execGit, &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "ls-tree") {
		t.Fatalf("exit-2 must be pinned to the ls-tree probe, stderr = %q", errb.String())
	}
}

// TestRun_BaseAbsence exercises decision 6's confirmed-absence leg: a base with
// no spec/ dir → empty ls-tree stdout → empty base → HEAD behaviors read as
// newly-added → exit 0, silent.
func TestRun_BaseAbsence(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	writeFile(t, root, "backend/x_test.go", citingTest())
	writeFile(t, root, "frontend/tests/e2e/.gitkeep", "")
	c1 := commit(t, root, "c1 no spec")
	writeFile(t, root, "spec/x.yaml", validSpec("outcome"))
	commit(t, root, "c2 add spec")

	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
	}
	if strings.Contains(out.String(), "WARN") {
		t.Fatalf("expected silence for newly-added behavior, got %q", out.String())
	}
}

// TestRun_UncleanBaseTaint: a base behavior with a lint violation is demoted to
// absent, so a HEAD fix that also changes its assertions never reads as drift.
// Each variant changes an assertable field so a taint-policy regression would
// surface as a false warn.
func TestRun_UncleanBaseTaint(t *testing.T) {
	// Every base carries a dirty X-001 PLUS a clean, drifted, separately-cited
	// X-002. x002Warns is the discriminator: under ID-taint only X-001 is
	// demoted, so the clean sibling X-002 stays present and warns; under a
	// file-level taint the whole file (including X-002) is demoted, so X-002 is
	// silent. This pins ID-scope vs file-scope demotion — a regression that
	// drops the whole file for an ID-ref violation stops X-002 warning in the
	// id-taint row, and one that never demotes the whole file makes X-002 warn
	// in the file-level rows.
	tests := []struct {
		name      string
		baseSpec  string
		x002Warns bool
	}{
		{"id-taint keeps clean sibling present", taintInvalidSurface2, true},
		{"file-level taint demotes whole file", taintInvalidMaturity2, false},
		{"structural-title taint demotes whole file", taintSeqTitle2, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "false")
			root := initRepo(t)
			writeFile(t, root, "spec/x.yaml", tc.baseSpec)
			writeFile(t, root, "backend/x_test.go", citingTest())   // cites X-001
			writeFile(t, root, "backend/x2_test.go", citingTest2()) // cites X-002
			writeFile(t, root, "frontend/tests/e2e/.gitkeep", "")
			c1 := commit(t, root, "c1 dirty base")
			writeFile(t, root, "spec/x.yaml", headSpec2)
			commit(t, root, "c2 repair + drift both")

			var out, errb bytes.Buffer
			if code := run([]string{root, c1}, execGit, &out, &errb); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr = %s", code, errb.String())
			}
			// X-001 is always demoted (tainted by ID, or with the whole file) → silent.
			if strings.Contains(out.String(), "X-001") {
				t.Fatalf("X-001 must be suppressed (tainted → absent), got %q", out.String())
			}
			if got := strings.Contains(out.String(), "X-002"); got != tc.x002Warns {
				t.Fatalf("X-002 warned = %v, want %v; out = %q", got, tc.x002Warns, out.String())
			}
			if !strings.Contains(errb.String(), "base corpus has") {
				t.Fatalf("expected base-violation notice on stderr, got %q", errb.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// run — exit 2 via the injectable gitRunner seam
// ---------------------------------------------------------------------------

func TestRun_FakeGitFailures(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("o"))
	c1 := commit(t, root, "c1")

	for _, sub := range []string{"rev-parse", "archive", "ls-tree", "diff"} {
		t.Run(sub+" failure", func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{root, c1}, failingGit(sub), &out, &errb); code != 2 {
				t.Fatalf("forced %s failure: exit = %d, want 2; stderr = %s", sub, code, errb.String())
			}
		})
	}
}

// TestRun_BaseArchiveFailure drives the BASE archive call site specifically: a
// gitRunner that succeeds on the first archive (HEAD) and fails the second
// (base). failingGit("archive") only ever fails the FIRST archive, so a
// regression that swallowed only the base-archive failure would slip past it.
func TestRun_BaseArchiveFailure(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("o"))
	c1 := commit(t, root, "c1")

	archiveCalls := 0
	g := gitRunner(func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "archive" {
			archiveCalls++
			if archiveCalls >= 2 {
				return nil, fmt.Errorf("forced base archive failure")
			}
		}
		return execGit(dir, args...)
	})
	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, g, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
	if archiveCalls < 2 {
		t.Fatalf("base archive was never reached (archiveCalls = %d)", archiveCalls)
	}
	// Pin the exit 2 to the archive branch. materialize RETURNS its error and the
	// caller prints it, so a regression that dropped the base-archive return would
	// emit NO "git archive" message — the base spec dir would simply never
	// materialize and exit 2 would come from a later, differently worded Lint IO
	// error ("read spec directory ..."). Requiring "git archive" here therefore
	// fails on that swallow, distinguishing the archive branch from downstream.
	if !strings.Contains(errb.String(), "git archive") {
		t.Fatalf("exit 2 must name the failing base archive, stderr = %q", errb.String())
	}
}

func TestRun_MalformedArchive(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("o"))
	c1 := commit(t, root, "c1")

	bad := gitRunner(func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "archive" {
			return []byte("not a tar stream"), nil
		}
		return execGit(dir, args...)
	})
	var out, errb bytes.Buffer
	if code := run([]string{root, c1}, bad, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %s", code, errb.String())
	}
}

func TestRun_StdoutWriteFailure(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "false")
	root := initRepo(t)
	seedStdFixture(t, root, validSpec("original outcome"))
	c1 := commit(t, root, "c1")
	writeFile(t, root, "spec/x.yaml", validSpec("changed outcome"))
	commit(t, root, "c2")

	var errb bytes.Buffer
	if code := run([]string{root, c1}, execGit, failWriter{}, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// ---------------------------------------------------------------------------
// helpers + fixtures
// ---------------------------------------------------------------------------

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("forced write failure") }

func tarWith(t *testing.T, build func(*tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	build(w)
	if err := w.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func writeReg(t *testing.T, w *tar.Writer, name, content string) {
	t.Helper()
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatalf("write header %s: %v", name, err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatalf("write body %s: %v", name, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// failingGit delegates to execGit for every subcommand except fail, which it
// forces to error — reaching an otherwise-unreachable exit-2 branch of run.
func failingGit(fail string) gitRunner {
	return func(dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == fail {
			return nil, fmt.Errorf("forced %s failure", fail)
		}
		return execGit(dir, args...)
	}
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v\nstderr: %s", args, dir, err, stderr.String())
	}
	return stdout.String()
}

func runGitTrim(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, dir, args...))
}

func runGitRC(dir string, args ...string) (string, int) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err == nil {
		return stdout.String(), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), ee.ExitCode()
	}
	return stdout.String(), -1
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	return runGitTrim(t, dir, "rev-parse", "HEAD")
}

// seedStdFixture writes the standard drift fixture: a spec file, a Go test
// citing X-001, and a placeholder so frontend/tests/e2e exists in the tree.
func seedStdFixture(t *testing.T, root, specBody string) {
	t.Helper()
	writeFile(t, root, "spec/x.yaml", specBody)
	writeFile(t, root, "backend/x_test.go", citingTest())
	writeFile(t, root, "frontend/tests/e2e/.gitkeep", "")
}

// citingTest is the fixture Go test that cites X-001. The marker token is
// assembled from split literals so this scanner-facing _test.go file does not
// itself carry a stray citation marker that make spec-coverage would flag.
const citeMarker = "// " + "spec: X-001"

func citingTest() string { return "package x\n" + citeMarker + "\n" }

// citingTest2 is the fixture Go test citing X-002 (the clean sibling in the
// base-taint fixtures). Split-literal marker, same reason as citeMarker.
const citeMarker2 = "// " + "spec: X-002"

func citingTest2() string { return "package x\n" + citeMarker2 + "\n" }

const specTemplate = `domain: x
prefix: X
maturity: draft
settled: [ui]
behaviors:
  - id: X-001
    title: A behavior
    type: business-logic
    status: current
    surface: api
    when: something happens
    then:
      - %s
`

func validSpec(then string) string { return fmt.Sprintf(specTemplate, then) }

// specMissingWhen lints dirty (a GWT behavior with no when).
const specMissingWhen = `domain: x
prefix: X
maturity: draft
settled: [ui]
behaviors:
  - id: X-001
    title: t
    type: business-logic
    status: current
    surface: api
    then:
      - o
`

// headSpec2 — both behaviors clean, both drifted (then → changed). The repaired
// HEAD counterpart of every base-taint fixture.
const headSpec2 = `domain: x
prefix: X
maturity: draft
settled: [ui]
behaviors:
  - id: X-001
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - changed
  - id: X-002
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - changed
`

// taintInvalidSurface2 — X-001 carries a semantic violation attributable to its
// ID (invalid surface enum) → tainted by ID; X-002 is clean and must survive.
const taintInvalidSurface2 = `domain: x
prefix: X
maturity: draft
settled: [ui]
behaviors:
  - id: X-001
    title: t
    type: business-logic
    status: current
    surface: bogus
    when: w
    then:
      - original
  - id: X-002
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - original
`

// taintInvalidMaturity2 — a file-level violation (empty ref) → whole file
// tainted; both X-001 and X-002 are demoted.
const taintInvalidMaturity2 = `domain: x
prefix: X
maturity: bogus
settled: [ui]
behaviors:
  - id: X-001
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - original
  - id: X-002
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - original
`

// taintSeqTitle2 — a pre-ID-promotion structural violation on X-001 (title is a
// sequence, emitted with ref "behaviors[0]") while X-001 still exports a valid
// id → only the non-ID-ref → taint-FILE rule demotes it, taking X-002 with it.
const taintSeqTitle2 = `domain: x
prefix: X
maturity: draft
settled: [ui]
behaviors:
  - id: X-001
    title:
      - not
      - scalar
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - original
  - id: X-002
    title: t
    type: business-logic
    status: current
    surface: api
    when: w
    then:
      - original
`

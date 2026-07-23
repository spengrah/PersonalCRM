// spec-drift is the behavior-changed-without-test-change advisory: it warns
// when a behavior's assertable content (given/when/then/statement) changed
// between a base ref and HEAD but none of its citing test files were touched —
// the classic silent-drift case. The comparison lives in the pure
// spec.SpecDrift core; this CLI only materializes the corpora from git,
// computes the changed-file set, normalizes paths, and prints.
//
// Usage:
//
//	spec-drift <repo-root> <base-ref>
//
// Two positional arguments: the repository root (containing spec/, backend/,
// and frontend/tests/e2e/) and the base ref (e.g. origin/develop). HEAD and the
// base are pinned to immutable SHAs once — headSHA = rev-parse HEAD, baseSHA =
// merge-base(headSHA, base-ref) — and BOTH corpora plus the HEAD test trees are
// materialized from git (never the working tree), so an uncommitted revert
// cannot hide a committed drift and a locally-deleted citation cannot suppress
// a warning.
//
// Exit codes:
//
//	0 — warn-only: emitted whether or not drift warnings were printed (a
//	    reworded-but-unchanged-meaning assertion is legitimate, so drift never
//	    blocks)
//	2 — operational error: bad usage, unresolvable base (missing ref, unrelated
//	    history, shallow clone), unreadable/corrupt tree, a git failure, a HEAD
//	    corpus that fails spec lint (head-side parse/validation failure), or a
//	    write failure
//
// The warn-only contract must NOT swallow git/operational failures into exit 0:
// a gate that silently computes nothing and exits green is one that cannot
// fail. Every git error surfaces as exit 2, never a fail-open empty result.
//
// Under GitHub Actions (GITHUB_ACTIONS=true) drift surfaces as ::warning::
// workflow-command annotations; otherwise a plain WARN: line is printed.
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
	"regexp"
	"strconv"
	"strings"

	"personal-crm/backend/internal/spec"
)

// gitRunner runs git in dir with args, returning captured stdout and an error
// that carries git's stderr text (so run's operational messages can name the
// failing subcommand). An injectable seam so the exit-2 git-failure branches
// are drivable end-to-end from tests.
type gitRunner func(dir string, args ...string) (stdout []byte, err error)

func main() {
	os.Exit(run(os.Args[1:], execGit, os.Stdout, os.Stderr))
}

// gitLocationVars name the environment variables that tell git WHERE the repo
// is. Because they override the -C / working-directory selection, an ambient
// value (e.g. one a pre-push hook exports for the real repo) would silently
// redirect every spec-drift git call away from the <root> argument. execGit
// clears them so `git -C <dir>` is authoritative.
var gitLocationVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_PREFIX":                       true,
	"GIT_NAMESPACE":                    true,
}

// cleanGitLocationEnv returns os.Environ() with the git-location variables
// removed, so `git -C <dir>` (not an ambient GIT_DIR) decides the target repo.
func cleanGitLocationEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, _ := strings.Cut(kv, "=")
		if gitLocationVars[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// execGit runs `git -C <dir> <args...>`, capturing stdout; on failure it wraps
// git's stderr as the error so the caller's message reads naturally. It clears
// the git-location env vars (decision above) so <dir> is authoritative even when
// an ambient GIT_DIR/GIT_WORK_TREE disagrees.
func execGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanGitLocationEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func run(args []string, git gitRunner, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: spec-drift <repo-root> <base-ref>")
		return 2
	}
	root, baseRef := args[0], args[1]

	gitFail := func(err error, subcmd string) int {
		_, _ = fmt.Fprintf(stderr, "spec-drift: git %s: %v\n", subcmd, err)
		return 2
	}
	opFail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "spec-drift: %v\n", err)
		return 2
	}

	// 1. Pin immutable HEAD + base SHAs. merge-base failure (missing ref,
	// unrelated history, shallow clone) is fail-closed exit 2 — never a fallback
	// to HEAD or the ref tip, which would be a silent wrong-base comparison.
	headSHA, err := gitOut(git, root, "rev-parse", "HEAD")
	if err != nil {
		return gitFail(err, "rev-parse HEAD")
	}
	baseSHA, err := gitOut(git, root, "merge-base", headSHA, baseRef)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spec-drift: cannot resolve base (no merge-base of HEAD with %s): %v\n", baseRef, err)
		return 2
	}

	// 2. Materialize the HEAD corpus + test trees from git.
	tmpHead, err := os.MkdirTemp("", "spec-drift-head-")
	if err != nil {
		return opFail(err)
	}
	defer func() { _ = os.RemoveAll(tmpHead) }()
	if err := materialize(git, root, headSHA, tmpHead, []string{"spec", "backend", "frontend/tests/e2e"}); err != nil {
		return opFail(err)
	}
	headFiles, headViol, err := spec.Lint(filepath.Join(tmpHead, "spec"))
	if err != nil {
		return opFail(err)
	}
	if len(headViol) > 0 {
		_, _ = fmt.Fprintf(stderr, "spec-drift: corpus has %d lint violations; run make spec-lint\n", len(headViol))
		return 2
	}
	cites, _, err := spec.CollectCitations(
		filepath.Join(tmpHead, "backend"),
		filepath.Join(tmpHead, "frontend", "tests", "e2e"),
	)
	if err != nil {
		return opFail(err)
	}

	// 3. Materialize the base corpus. Probe first: git archive exits 128 for a
	// missing path, so ls-tree distinguishes confirmed-absence (empty stdout,
	// exit 0) from an unreadable tree (non-zero → exit 2). Absence is never an
	// error; an unreadable tree is never read as absence.
	var baseFiles []*spec.File
	lsOut, err := git(root, "ls-tree", baseSHA, "--", "spec")
	if err != nil {
		return gitFail(err, "ls-tree "+baseSHA+" -- spec")
	}
	if len(bytes.TrimSpace(lsOut)) > 0 {
		tmpBase, err := os.MkdirTemp("", "spec-drift-base-")
		if err != nil {
			return opFail(err)
		}
		defer func() { _ = os.RemoveAll(tmpBase) }()
		if err := materialize(git, root, baseSHA, tmpBase, []string{"spec"}); err != nil {
			return opFail(err)
		}
		bf, baseViol, err := spec.Lint(filepath.Join(tmpBase, "spec"))
		if err != nil {
			return opFail(err)
		}
		baseFiles = filterTaintedBase(bf, baseViol, stderr)
	}

	// 4. Normalize citation + head-file paths to repo-relative slash form so the
	// pure core compares plain strings against the repo-relative diff paths. An
	// un-normalizable path is exit 2, never discarded (silently dropping it from
	// the changed-set intersection is the fail-open trap).
	for i := range cites {
		rel, err := filepath.Rel(tmpHead, cites[i].Path)
		if err != nil {
			return opFail(fmt.Errorf("normalize citation path %s: %w", cites[i].Path, err))
		}
		cites[i].Path = filepath.ToSlash(rel)
	}
	for _, f := range headFiles {
		rel, err := filepath.Rel(tmpHead, f.Path)
		if err != nil {
			return opFail(fmt.Errorf("normalize spec path %s: %w", f.Path, err))
		}
		f.Path = filepath.ToSlash(rel)
	}

	// 5. Changed-file set: NUL-split of git diff --name-only -z (handles quoting,
	// non-ASCII, embedded newlines). Both endpoints are the pinned SHAs; a
	// two-dot diff against the merge-based baseSHA equals a triple-dot against
	// the base ref.
	diffOut, err := git(root, "diff", "--name-only", "-z", baseSHA, headSHA)
	if err != nil {
		return gitFail(err, "diff --name-only -z "+baseSHA+" "+headSHA)
	}
	changed := map[string]bool{}
	for _, p := range strings.Split(string(diffOut), "\x00") {
		if p == "" {
			continue
		}
		changed[filepath.ToSlash(p)] = true
	}

	// 6. Compute + emit. Warnings never change the exit code.
	viol := spec.SpecDrift(baseFiles, headFiles, cites, changed)
	ghActions := os.Getenv("GITHUB_ACTIONS") == "true"
	for _, v := range viol {
		var err error
		if ghActions {
			err = writeGitHubWarning(stdout, v)
		} else {
			_, err = fmt.Fprintf(stdout, "WARN: %s\n", v.String())
		}
		if err != nil {
			return opFail(err)
		}
	}
	return 0
}

// gitOut runs a git subcommand expected to produce a single whitespace-trimmed
// token (a SHA).
func gitOut(git gitRunner, dir string, args ...string) (string, error) {
	out, err := git(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// materialize runs `git archive <sha> <paths...>` from root and extracts the
// tar stream into dest. It RETURNS the operational error (rather than printing
// it and returning a code) so the CALLER owns the exit-2 decision and the
// message: a caller that dropped this return would emit no "git archive"
// message at all — the failure would surface only later, differently worded (a
// missing base tree ⇒ a Lint IO error) — which makes a swallowed archive
// failure detectable instead of masked by materialize's own print.
func materialize(git gitRunner, root, sha, dest string, paths []string) error {
	out, err := git(root, append([]string{"archive", sha}, paths...)...)
	if err != nil {
		return fmt.Errorf("git archive %s: %w", sha, err)
	}
	if err := extractTar(bytes.NewReader(out), dest); err != nil {
		return fmt.Errorf("extract archive %s: %w", sha, err)
	}
	return nil
}

// extractTar extracts a tar stream into dest with a bounded, reject-not-skip
// policy. It PERMITS-and-SKIPS the pax_global_header (TypeXGlobalHeader) that
// every `git archive <commit>` emits — stream metadata carrying the commit SHA,
// not a filesystem entry — and accepts only regular files and directories.
// Every other entry type (symlink, hardlink, device, FIFO), any absolute path,
// and any path escaping dest are rejected with an error (⇒ CLI exit 2).
// Silently skipping a filesystem entry would drop a citation and fail open, so
// the global-header skip is the sole permitted omission.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue // commit-archive metadata; never materialized
		}

		if filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("unsafe archive entry %q: absolute path", hdr.Name)
		}
		target := filepath.Join(dest, hdr.Name)
		rel, err := filepath.Rel(dest, filepath.Clean(target))
		// The exact-".." disjunct is load-bearing: HasPrefix(rel, "../") is false
		// when rel is exactly "..", so an entry literally named ".." would slip a
		// prefix-only check.
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe archive entry %q: escapes destination", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeRegular(target, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %q (type %c): only regular files and directories are allowed", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeRegular(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// behaviorIDRef matches the behavior-ID grammar; a base violation whose Ref
// matches is cleanly attributable to that behavior (taint just the ID), while
// any other ref (empty, "behaviors[i]", or non-ID) taints the whole file.
var behaviorIDRef = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)

// filterTaintedBase applies the conservative taint policy: a base file with any
// violation not cleanly attributable to a well-formed behavior ID is demoted
// whole (its behaviors read as absent — their HEAD counterparts then look
// newly-added, no false drift); a behavior-ID-ref violation taints only that
// ID. The returned base contains only present behaviors. A one-line notice is
// printed whenever the base carries any violation. Never exit 2 for a dirty
// base — stacked-PR safety.
func filterTaintedBase(files []*spec.File, viol []spec.Violation, stderr io.Writer) []*spec.File {
	if len(viol) == 0 {
		return files
	}
	_, _ = fmt.Fprintf(stderr, "spec-drift: base corpus has %d lint violation(s); affected behaviors treated as absent\n", len(viol))

	taintedFiles := map[string]bool{}
	taintedIDs := map[string]bool{}
	for _, v := range viol {
		if behaviorIDRef.MatchString(v.Ref) {
			taintedIDs[v.Ref] = true
		} else {
			taintedFiles[v.Path] = true
		}
	}

	var out []*spec.File
	for _, f := range files {
		if taintedFiles[f.Path] {
			continue
		}
		kept := *f
		kept.Behaviors = nil
		for _, b := range f.Behaviors {
			if taintedIDs[b.ID] {
				continue
			}
			kept.Behaviors = append(kept.Behaviors, b)
		}
		out = append(out, &kept)
	}
	return out
}

// writeGitHubWarning emits one GitHub Actions ::warning:: workflow command for a
// drift violation, escaping the file/line properties (escapeProp) and the
// message (escapeData) so a path or message containing %, CR, LF, comma, or
// colon cannot corrupt the annotation. ,line= is omitted when the line is
// unknown (0).
func writeGitHubWarning(w io.Writer, v spec.Violation) error {
	var b strings.Builder
	b.WriteString("::warning file=")
	b.WriteString(escapeProp(v.Path))
	if v.Line > 0 {
		b.WriteString(",line=")
		b.WriteString(strconv.Itoa(v.Line))
	}
	b.WriteString("::")
	b.WriteString(escapeData(v.Ref + ": " + v.Msg))
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// escapeData escapes a workflow-command message: % first (so an input that
// already looks escaped is not double-decoded), then CR and LF. Comma, colon,
// and non-ASCII pass through unchanged in the data segment.
func escapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	s = strings.ReplaceAll(s, "\n", "%0A")
	return s
}

// escapeProp escapes a workflow-command property value: everything escapeData
// does PLUS comma and colon, which delimit properties.
func escapeProp(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	s = strings.ReplaceAll(s, "\n", "%0A")
	s = strings.ReplaceAll(s, ",", "%2C")
	s = strings.ReplaceAll(s, ":", "%3A")
	return s
}

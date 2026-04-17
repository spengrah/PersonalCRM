// Package tests — PR 8 cutover sole-writer AST guard (plan Step 13).
//
// This test enforces the acceptance criterion that CadenceUpdater is the
// sole post-create writer of contact.last_contacted, contact.last_outreach_at,
// contact.last_response_at, and contact.contact_by. It walks every .go file
// under backend/internal and backend/cmd/crm-api, flags any call to one of
// the cadence-writing sqlc-generated queries or repository tx wrappers, and
// compares the hit set against an explicit allowlist.
//
// The allowlist mirrors plan Decision 9:
//   - backend/internal/consumer/cadence_updater.go — the authoritative writer
//   - backend/internal/repository/contact.go — definitions + CreateContact
//     (initial row seed allowed by spec Design Decision 9)
//   - backend/internal/todoist/provider.go — the two documented Todoist
//     carve-outs (cadence-task deadline edit + skip trigger); the Todoist
//     reconcile branch at :1140 no longer writes contact_by post-cutover.
//
// Any NEW carve-out must add its file to the allowlist here with a code
// comment justifying why it is not the sole writer. A diff that adds a
// cadence-writing call outside the allowlist will fail CI loudly.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cadenceWritingSymbols enumerates the method + query names that mutate
// one or more of the four cadence columns (plus last_interaction_at, which
// piggybacks on last_contacted writes). Matching is by call-expression
// selector name only, so both *ContactRepository method calls and
// *db.Queries generated-method calls reaching these names are caught.
//
// Kept in sync with backend/internal/db/queries/contact.sql. When adding
// a new cadence-writing query, add its name here.
var cadenceWritingSymbols = map[string]struct{}{
	"UpdateContactLastContacted":        {},
	"UpdateContactLastContactedIfLater": {},
	"UpdateContactOutreachAt":           {},
	"UpdateContactOutreachAtTx":         {},
	"UpdateContactResponseFields":       {},
	"UpdateContactResponseFieldsTx":     {},
	"UpdateContactMutualFields":         {},
	"UpdateContactMutualFieldsTx":       {},
	"UpdateContactCadenceForward":       {},
	"UpdateContactCadenceUnconditional": {},
	"UpdateContactBy":                   {},
}

// allowedFiles maps file paths (relative to the backend module root) to a
// human-readable justification. Every call site reported by the AST walk
// MUST live in a file that appears here — otherwise the test fails and
// the reviewer has to decide whether to expand the allowlist or route
// the write through CadenceUpdater.
var allowedFiles = map[string]string{
	// The authoritative consumer-owned cadence writer. All cadence write
	// SQL lives here post-cutover.
	"internal/consumer/cadence_updater.go": "sole writer (plan acceptance criterion 5)",

	// Repository method bodies that wrap the cadence-writing queries
	// themselves. They are definitions, not write call sites — any
	// downstream caller outside the allowlist still trips the guard.
	"internal/repository/contact.go": "defines the repository wrappers (bodies, not exogenous writes)",

	// Todoist carve-outs (plan Design Decision 9 + Step 11). The two
	// surviving UpdateContactBy writes at provider.go:503 and :691 are
	// due-date / skip-trigger task-state reconciliation, NOT interaction-
	// driven cadence updates. The former :1140 write was deleted in
	// Step 11 because the upstream interaction path already updates
	// contact_by via CadenceUpdater.
	"internal/todoist/provider.go": "two explicit Todoist carve-outs (cadence-task deadline edit + skip trigger)",
}

// TestCadenceSoleWriter_OnlyAllowedFilesCallCadenceSQL walks the Go AST of
// backend/internal + backend/cmd/crm-api and asserts every call to a
// cadence-writing symbol lives in an allowedFiles entry. Generated sqlc
// files (contact.sql.go etc.) and test files are skipped — the guard is
// about production code paths.
func TestCadenceSoleWriter_OnlyAllowedFilesCallCadenceSQL(t *testing.T) {
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}

	roots := []string{
		filepath.Join(moduleRoot, "internal"),
		filepath.Join(moduleRoot, "cmd", "crm-api"),
	}

	type violation struct {
		file string
		line int
		call string
	}
	var violations []violation

	fset := token.NewFileSet()
	for _, root := range roots {
		if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// Skip generated sqlc files: their UPDATE SQL is the target
			// of the guard, not a source of uncontrolled writes.
			if strings.HasPrefix(info.Name(), "contact.sql.go") ||
				info.Name() == "querier.go" ||
				info.Name() == "models.go" ||
				info.Name() == "db.go" {
				return nil
			}
			rel, err := filepath.Rel(moduleRoot, path)
			if err != nil {
				return err
			}

			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if _, hit := cadenceWritingSymbols[sel.Sel.Name]; !hit {
					return true
				}
				if _, allowed := allowedFiles[filepath.ToSlash(rel)]; allowed {
					return true
				}
				pos := fset.Position(call.Pos())
				violations = append(violations, violation{
					file: filepath.ToSlash(rel),
					line: pos.Line,
					call: sel.Sel.Name,
				})
				return true
			})
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(violations) > 0 {
		var msg strings.Builder
		msg.WriteString("cadence sole-writer guard: found calls to cadence-writing queries in non-allowlisted files.\n")
		msg.WriteString("Either route the write through CadenceUpdater, or add a justified entry to allowedFiles in this test.\n\n")
		for _, v := range violations {
			msg.WriteString("  ")
			msg.WriteString(v.file)
			msg.WriteString(":")
			msg.WriteString(itoa(v.line))
			msg.WriteString(" calls ")
			msg.WriteString(v.call)
			msg.WriteString("\n")
		}
		t.Fatal(msg.String())
	}
}

// backendModuleRoot returns the absolute path to the backend module's root
// by walking up from cwd until it finds go.mod. The tests binary runs
// with cwd set to the package's directory (backend/tests), so one hop up
// normally suffices — but the loop keeps the helper robust if that
// convention ever changes.
func backendModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// itoa is a tiny strconv.Itoa clone — avoids pulling strconv into an
// otherwise tiny dependency surface for the error-message build.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

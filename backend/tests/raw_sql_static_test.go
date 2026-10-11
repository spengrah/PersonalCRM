// Package tests — raw-SQL-in-Go AST guard.
//
// Repo rule: all SQL lives in sqlc queries (backend/internal/db/queries/*.sql).
// This test walks every non-generated .go file under backend/ (tests included)
// and flags any Exec/Query/QueryRow call (plus their Context variants and the
// pgx batch Queue) whose SQL argument is a string literal, or an identifier
// bound in the same file to a string literal, that begins with a SQL verb.
// Fixtures and assertions in tests need a test-only sqlc query plus a
// repository wrapper, not an inline statement.
//
// Scope limits: the guard resolves literals, "lit"+"lit" concatenations, and
// same-file const/var/:= bindings by name. It does not follow SQL built across
// files or assembled by fmt.Sprintf.
package tests

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// rawSQLCallNames are the method names that take a SQL string. Matching is by
// selector name only.
var rawSQLCallNames = map[string]struct{}{
	"Exec": {}, "ExecContext": {},
	"Query": {}, "QueryContext": {},
	"QueryRow": {}, "QueryRowContext": {},
	"Queue": {},
}

// rawSQLStartRe matches a string that begins with a SQL verb or a SQL comment.
var rawSQLStartRe = regexp.MustCompile(`(?is)^\s*(--|/\*|SELECT\b|INSERT\b|UPDATE\b|DELETE\b|WITH\b|CREATE\b|ALTER\b|DROP\b|BEGIN\b|COMMIT\b|ROLLBACK\b|SET\b|TRUNCATE\b|VACUUM\b|ANALYZE\b|DO\b|GRANT\b|REVOKE\b|COPY\b|LOCK\b|EXPLAIN\b|TABLE\b|VALUES\b)`)

// rawSQLExcludedDirs are skipped wholesale (paths relative to the backend
// module root, forward slashes). internal/testdb is the test-database harness:
// it creates, drops, and reaps databases and cannot go through sqlc.
var rawSQLExcludedDirs = map[string]string{
	"internal/testdb": "test-database harness: CREATE/DROP DATABASE, extensions, advisory locks",
}

// rawSQLAllowedSites maps "file:function" (relative to the backend module
// root) to the reason raw SQL is acceptable there.
var rawSQLAllowedSites = map[string]string{
	"internal/db/migration.go:runRiverMigrations":           "session-scoped advisory lock on one pooled connection; sqlc cannot pin a connection",
	"internal/db/migration.go:SeedDatabase":                 "seed placeholder executes an inline marker statement",
	"internal/db/migration_status.go:migrationsTableExists": "pre-migration probe: runs before schema_migrations exists, so no sqlc-visible schema",
}

// rawSQLBaseline records raw-SQL calls that predate the rule, keyed
// "file:function" with the number of calls in that function. The guard is
// forward-only: a function over its count fails, and an entry whose count is
// no longer reached also fails, so the baseline only shrinks. Migrate a site
// to a sqlc query, then lower or delete its entry.
var rawSQLBaseline = map[string]int{
	"internal/repository/sync.go:EnqueueAccountSyncIfNotInFlight":                                                    1,
	"tests/consumer_interaction_recorder_integration_test.go:TestIntegration_CalendarAttended_Replay":                1,
	"tests/consumer_interaction_recorder_integration_test.go:TestIntegration_ManualHandler_SingleTxFlow":             2,
	"tests/followup_manager_cutover_integration_test.go:TestIntegration_FollowUpManager_UniqueLiveCollisionRecovers": 1,
	"tests/integration_test.go:TestMigration020_UnifyEmailDedup":                                                     6,
	"tests/migration_status_integration_test.go:schemaMigrationsTableExists":                                         1,
	"tests/rematch_integration_test.go:TestRematch_ConcurrentJobs_PerContactMutex":                                   1,
	"tests/rematch_integration_test.go:TestRematch_TelegramPhoneMatch":                                               3,
	"tests/rematch_integration_test.go:TestRematch_TelegramRematchPlusPostImportHook_NoDuplicateInteraction":         4,
	"tests/rematch_integration_test.go:TestRematch_TelegramUsernameMatch":                                            3,
	"tests/river_integration_test.go:TestRunMigrations_River_Integration":                                            1,
	"tests/river/sync_worker_leased_retry_test.go:TestSyncWorker_AbandonedLogOnStartupRecovery":                      6,
	"tests/river/sync_worker_leased_retry_test.go:TestSyncWorker_ContextCancelledRetry":                              9,
	"tests/river/sync_worker_leased_retry_test.go:TestSyncWorker_RescueOnCrash":                                      8,
	"tests/river/sync_worker_leased_retry_test.go:waitForSyncLogStatus":                                              1,
	"tests/scheduler/river_periodic_scheduler_tick_test.go:TestPeriodicTick_EndToEndEnqueueWithAtomicClaim":          5,
	"tests/sync_repo_enqueue_test.go:newEnqueueTestEnv":                                                              1,
	"tests/sync_repo_enqueue_test.go:seedRiverJob":                                                                   1,
	"tests/sync_repo_enqueue_test.go:TestEnqueueAccountSyncIfNotInFlight_AtomicUnderConcurrency":                     1,
	"tests/sync_repo_enqueue_test.go:TestEnqueueAccountSyncIfNotInFlight_CrossWindowOverlapPrevented":                1,
	"tests/sync_repo_enqueue_test.go:TestEnqueueAccountSyncIfNotInFlight_InsertsWhenClear":                           1,
	"tests/sync_repo_enqueue_test.go:TestEnqueueAccountSyncIfNotInFlight_SkipsWhenInFlight":                          1,
	"tests/test_event_bus_harness_test.go:waitForTelegramInteractionCount":                                           1,
	"tests/test_event_bus_harness_test.go:waitForTelegramMessagesProcessed":                                          1,
}

type rawSQLHit struct {
	site    string // file:function
	pos     string // file:line
	call    string
	snippet string
}

func TestNoRawSQLInGo(t *testing.T) {
	t.Parallel()
	root, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}

	var hits []rawSQLHit
	fset := token.NewFileSet()
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			if _, skip := rawSQLExcludedDirs[rel]; skip || info.Name() == "node_modules" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src[:min(len(src), 400)]), "Code generated") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		hits = append(hits, rawSQLHitsInFile(fset, f, rel)...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk backend: %v", walkErr)
	}

	counts := map[string]int{}
	byPos := map[string][]string{}
	for _, h := range hits {
		if _, ok := rawSQLAllowedSites[h.site]; ok {
			continue
		}
		counts[h.site]++
		byPos[h.site] = append(byPos[h.site], h.pos+": "+h.call+" with inline SQL "+strconv.Quote(h.snippet))
	}

	var violations, stale []string
	for site, n := range counts {
		if n > rawSQLBaseline[site] {
			violations = append(violations, byPos[site]...)
		}
	}
	for site, allowed := range rawSQLBaseline {
		if n := counts[site]; n < allowed {
			stale = append(stale, fmt.Sprintf("%s: baseline allows %d, found %d", site, allowed, n))
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)
	if len(violations) > 0 {
		t.Errorf("new raw SQL in Go (%d call sites in functions over their baseline). Add a sqlc query (and a repository wrapper, test-only if needed) instead, or allowlist the site in rawSQLAllowedSites with a reason:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("stale rawSQLBaseline entries (%d). Lower the count or delete the entry; the baseline only shrinks:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// rawSQLHitsInFile returns every raw-SQL call in f. rel is the file path
// relative to the backend module root.
func rawSQLHitsInFile(fset *token.FileSet, f *ast.File, rel string) []rawSQLHit {
	// Same-file string bindings by identifier name.
	bound := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) {
					if s, ok := rawSQLStringLiteral(v.Values[i]); ok {
						bound[name.Name] = s
					}
				}
			}
		case *ast.AssignStmt:
			for i, l := range v.Lhs {
				if id, ok := l.(*ast.Ident); ok && i < len(v.Rhs) {
					if s, ok := rawSQLStringLiteral(v.Rhs[i]); ok {
						bound[id.Name] = s
					}
				}
			}
		}
		return true
	})

	var hits []rawSQLHit
	for _, decl := range f.Decls {
		funcName := "<package>"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			funcName = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, ok := rawSQLCallNames[sel.Sel.Name]; !ok {
				return true
			}
			for _, arg := range call.Args {
				s, found := rawSQLStringLiteral(arg)
				if id, isIdent := arg.(*ast.Ident); !found && isIdent {
					s, found = bound[id.Name]
				}
				if found && rawSQLStartRe.MatchString(s) {
					snippet := strings.Join(strings.Fields(s), " ")
					if len(snippet) > 70 {
						snippet = snippet[:70]
					}
					hits = append(hits, rawSQLHit{
						site:    rel + ":" + funcName,
						pos:     rel + ":" + strconv.Itoa(fset.Position(call.Pos()).Line),
						call:    sel.Sel.Name,
						snippet: snippet,
					})
					break
				}
			}
			return true
		})
	}
	return hits
}

// rawSQLStringLiteral resolves a string literal or a "+" concatenation with
// at least one literal operand.
func rawSQLStringLiteral(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s, true
			}
		}
	case *ast.ParenExpr:
		return rawSQLStringLiteral(v.X)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			a, okA := rawSQLStringLiteral(v.X)
			b, okB := rawSQLStringLiteral(v.Y)
			if okA || okB {
				return a + b, true
			}
		}
	}
	return "", false
}

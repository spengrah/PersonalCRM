// Package tests — silently dropped error AST guard.
//
// Flags, in non-test backend Go, two shapes where an error is checked and
// then discarded without trace:
//
//   - `if err != nil { ... }` whose body never mentions an error-named
//     identifier and makes no call (no log, no HTTP error, no wrap).
//   - `if err := f(); err == nil { ... }` with no else, where the failure
//     is never examined afterwards (nothing later in the block mentions err).
//
// A site that drops the error on purpose carries the marker
// `// best-effort: <reason>` on the line directly above the if. The reason
// is mandatory: a bare `// best-effort:` does not count.
//
// The rule is forward-only. droppedErrorBaseline records how many sites each
// function has today, keyed "path:Func" (methods as "path:Recv.Func"). A
// function with more sites than its baseline fails, and a baseline entry
// with more sites than exist fails, so the baseline only shrinks.
package tests

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const bestEffortMarker = "best-effort:"

func isErrName(s string) bool {
	return s == "err" || strings.HasSuffix(s, "Err") || strings.HasSuffix(s, "err")
}

// isErrCmpNil reports whether e is `<errName> <op> nil`.
func isErrCmpNil(e ast.Expr, op token.Token) bool {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != op {
		return false
	}
	id, ok := b.X.(*ast.Ident)
	if !ok || !isErrName(id.Name) {
		return false
	}
	n, ok := b.Y.(*ast.Ident)
	return ok && n.Name == "nil"
}

// condHasErrNotNil reports whether the condition is `err != nil`, possibly
// parenthesized or one operand of `||`.
func condHasErrNotNil(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.LOR {
			return condHasErrNotNil(x.X) || condHasErrNotNil(x.Y)
		}
		return isErrCmpNil(x, token.NEQ)
	case *ast.ParenExpr:
		return condHasErrNotNil(x.X)
	}
	return false
}

func mentionsErr(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if id, ok := m.(*ast.Ident); ok && isErrName(id.Name) {
			found = true
		}
		return !found
	})
	return found
}

// makesCall reports whether n contains a call that can report or wrap an
// error. Type conversions and pure builtins do not count.
func makesCall(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		c, ok := m.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		switch f := c.Fun.(type) {
		case *ast.ArrayType, *ast.MapType:
			return true
		case *ast.Ident:
			switch f.Name {
			case "string", "int", "int64", "int32", "uint", "bool", "float64", "byte",
				"len", "append", "delete", "copy", "make", "new":
				return true
			}
		}
		found = true
		return false
	})
	return found
}

// assignsErr reports whether n assigns an error-named variable (the
// rollback-guard shape `if err == nil { err = tx.Rollback() }`).
func assignsErr(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if a, ok := m.(*ast.AssignStmt); ok {
			for _, l := range a.Lhs {
				if id, ok := l.(*ast.Ident); ok && isErrName(id.Name) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// bestEffortLines returns the line numbers directly below a comment group
// that has a line `best-effort: <non-empty reason>`.
func bestEffortLines(fset *token.FileSet, f *ast.File) map[int]bool {
	lines := map[int]bool{}
	for _, cg := range f.Comments {
		for _, l := range strings.Split(cg.Text(), "\n") {
			reason, ok := strings.CutPrefix(strings.TrimSpace(l), bestEffortMarker)
			if ok && strings.TrimSpace(reason) != "" {
				lines[fset.Position(cg.End()).Line+1] = true
				break
			}
		}
	}
	return lines
}

// funcKey names the declaration that holds a site: "Func", "Recv.Func", or
// "-" for package-level initializers.
func funcKey(d ast.Decl) string {
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return "-"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// droppedErrorHits returns the line of each site, keyed by enclosing function.
func droppedErrorHits(fset *token.FileSet, f *ast.File) map[string][]int {
	hits := map[string][]int{}
	marked := bestEffortLines(fset, f)
	for _, d := range f.Decls {
		for _, line := range droppedErrorHitsIn(fset, marked, d) {
			hits[funcKey(d)] = append(hits[funcKey(d)], line)
		}
	}
	return hits
}

func droppedErrorHitsIn(fset *token.FileSet, marked map[int]bool, decl ast.Decl) []int {
	var hits []int
	ast.Inspect(decl, func(n ast.Node) bool {
		var list []ast.Stmt
		switch x := n.(type) {
		case *ast.BlockStmt:
			list = x.List
		case *ast.CaseClause:
			list = x.Body
		case *ast.CommClause:
			list = x.Body
		default:
			return true
		}
		for i, st := range list {
			ifs, ok := st.(*ast.IfStmt)
			if !ok {
				continue
			}
			line := fset.Position(ifs.Pos()).Line
			if marked[line] {
				continue
			}
			if condHasErrNotNil(ifs.Cond) && !mentionsErr(ifs.Body) && !makesCall(ifs.Body) {
				hits = append(hits, line)
				continue
			}
			if isErrCmpNil(ifs.Cond, token.EQL) && ifs.Else == nil && !assignsErr(ifs.Body) {
				// An err bound by the if's own init is out of scope after it.
				examinedLater := false
				if ifs.Init == nil {
					for _, s := range list[i+1:] {
						examinedLater = examinedLater || mentionsErr(s)
					}
				}
				if !examinedLater {
					hits = append(hits, line)
				}
			}
		}
		return true
	})
	return hits
}

// droppedErrorBaseline is the count of silent drops per "path:Func" that
// exist today. Fix a site (handle the error) or mark it
// `// best-effort: <reason>`, then lower or delete the entry.
var droppedErrorBaseline = map[string]int{
	"cmd/crm-api/main.go:run":                1,
	"cmd/spec-coverage/main.go:run":          1,
	"cmd/spec-lint/main.go:run":              2,
	"internal/accelerated/time.go:parseBase": 2,
	"internal/api/handlers/calendar.go:CalendarHandler.ListUpcomingEventsForContact":                                      1,
	"internal/api/handlers/calendar.go:parsePagination":                                                                   2,
	"internal/api/handlers/contact.go:DateOnly.UnmarshalJSON":                                                             2,
	"internal/api/handlers/mac_host.go:remoteIP":                                                                          1,
	"internal/config/config.go:getEnvAsBool":                                                                              1,
	"internal/config/config.go:getEnvAsDuration":                                                                          1,
	"internal/config/config.go:getEnvAsInt":                                                                               1,
	"internal/consumer/aggregator_reenqueuer.go:parseTelegramPeerRef":                                                     3,
	"internal/consumer/cadence_updater.go:CadenceUpdater.buildInteractionWrite":                                           1,
	"internal/consumer/comms_aggregator_reenqueuer.go:parseCommsPeerRef":                                                  2,
	"internal/consumer/interaction_recorder.go:InteractionRecorder.resolveVenue":                                          1,
	"internal/consumer/messages_aggregator_reenqueuer.go:parseMessagesPeerRef":                                            2,
	"internal/consumer/rematch_dispatcher.go:riverSnoozeCount":                                                            1,
	"internal/contacttask/marker.go:parseMarker":                                                                          1,
	"internal/google/contacts.go:ContactsProvider.attemptMatch":                                                           2,
	"internal/google/contacts.go:ContactsProvider.checkDuplicates":                                                        1,
	"internal/google/correspondence_rederive.go:resolveRefetchTarget":                                                     1,
	"internal/google/gchat.go:editLookbackFloor":                                                                          1,
	"internal/google/gchat_helpers.go:buildContentMetadata":                                                               1,
	"internal/google/gchat_support.go:cachedEmailExpired":                                                                 1,
	"internal/google/gchat_support.go:cachedUserIDExpired":                                                                1,
	"internal/google/gchat_support.go:decodeMetaKey":                                                                      1,
	"internal/google/gchat_support.go:memberNegativeExpired":                                                              1,
	"internal/google/gchat_support.go:membershipNeedsRefresh":                                                             1,
	"internal/google/gchat_support.go:reapStaleCursors":                                                                   1,
	"internal/google/gmail.go:buildMetadataJSON":                                                                          1,
	"internal/google/gmail.go:decodePartBody":                                                                             1,
	"internal/google/gmail.go:parseAddressList":                                                                           2,
	"internal/google/gmail.go:parseSingleAddress":                                                                         1,
	"internal/matching/handle_normalize.go:NormalizeForExactHandleMatch":                                                  1,
	"internal/matching/handle_normalize.go:NormalizeHandleForNameMatch":                                                   1,
	"internal/messaging/aggregation/engine.go:Engine.tryExplicitReplyBridge":                                              2,
	"internal/messaging/commsadapter/store.go:parseReplyTargetID":                                                         2,
	"internal/repository/calendar.go:convertDbCalendarEvent":                                                              1,
	"internal/repository/comms_message.go:CommsMessageRepository.BackfillParticipantNames":                                1,
	"internal/repository/contact.go:ContactRepository.CreateContact":                                                      1,
	"internal/repository/contact.go:decideContactByAfterDelete":                                                           1,
	"internal/repository/contact_task.go:convertDbContactTask":                                                            1,
	"internal/repository/contact_task.go:convertDbContactTaskWithContact":                                                 1,
	"internal/repository/external_contact.go:ExternalContactRepository.FindAnarlogTitleSiblingsByToken":                   1,
	"internal/repository/external_contact.go:ExternalContactRepository.FindByNormalizedEmail":                             1,
	"internal/repository/external_contact.go:ExternalContactRepository.FindBySourceAndSourceID":                           1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListAllUnmatched":                                  1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListForCRMContact":                                 1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListLinkedAddressBookExternalContactsForReconcile": 1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListPendingMethodSuggestionRows":                   1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListUnmatched":                                     1,
	"internal/repository/external_contact.go:ExternalContactRepository.ListUnmatchedBySources":                            1,
	"internal/repository/external_contact.go:convertDbExternalContact":                                                    4,
	"internal/repository/external_contact.go:parseMethodSuggestions":                                                      1,
	"internal/repository/sync.go:convertDbSyncState":                                                                      1,
	"internal/repository/sync.go:extractBackfillComplete":                                                                 1,
	"internal/service/assert.go:AssertService.AssertTx":                                                                   1,
	"internal/service/contact.go:ContactService.UpdateContact":                                                            1,
	"internal/service/contact.go:ContactService.listOverdueContactsInMemory":                                              1,
	"internal/service/contact.go:buildMergeCadenceFields":                                                                 1,
	"internal/service/enrichment.go:deriveContactByFromCadence":                                                           1,
	"internal/service/ingest.go:parseExternalContactBirthday":                                                             1,
	"internal/service/interaction_content.go:parseSessionUUID":                                                            3,
	"internal/service/staleness.go:parseSourceHealth":                                                                     1,
	"internal/synthetic/declare/cleanup.go:runLadder":                                                                     1,
	"internal/synthetic/replay/batch.go:Harness.trackBatchInteractions":                                                   1,
	"internal/synthetic/replay/gchat.go:laterRFC3339":                                                                     1,
	"internal/synthetic/replay/helpers.go:Harness.trackContactInteractions":                                               1,
	"internal/synthetic/replay/replay.go:Harness.gateBClear":                                                              2,
	"internal/synthetic/replay/todoist.go:finalizeTempID":                                                                 1,
	"internal/telegram/manager.go:TelegramManager.Status":                                                                 1,
	"internal/telegram/manager.go:TelegramManager.enrichStatusFromSyncState":                                              1,
	"internal/telegram/manager.go:TelegramManager.runWithReconnect":                                                       1,
	"internal/telegram/manager.go:TelegramManager.selfUserID":                                                             1,
	"internal/telegram/matcher.go:PeerMatcher.markExternalContactMatched":                                                 1,
	"internal/todoist/provider.go:CadenceSyncProvider.Sync":                                                               1,
	"internal/todoist/provider.go:CadenceSyncProvider.handleTaskCompletion":                                               1,
	"internal/todoist/provider.go:CadenceSyncProvider.tryRecoverPendingTempID":                                            1,
	"internal/whatsapp/actor.go:Manager.SelfJID":                                                                          1,
	"internal/whatsapp/history.go:expectedStoredLIDMappings":                                                              2,
	"internal/whatsapp/manager.go:Manager.ensureSyncState":                                                                2,
	"internal/whatsapp/manager.go:Manager.markTerminal":                                                                   1,
	"internal/whatsapp/manager.go:Manager.writeSelectorMetadata":                                                          1,
}

func TestNoSilentlyDroppedErrors(t *testing.T) {
	t.Parallel()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}

	counts := map[string]int{}
	lines := map[string][]string{}
	fset := token.NewFileSet()
	walkErr := filepath.Walk(moduleRoot, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(src, []byte("Code generated")) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(moduleRoot, path)
		for fn, ls := range droppedErrorHits(fset, f) {
			key := filepath.ToSlash(rel) + ":" + fn
			counts[key] += len(ls)
			for _, l := range ls {
				lines[key] = append(lines[key], fmt.Sprintf("line %d", l))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", moduleRoot, walkErr)
	}

	var added, stale []string
	for key, n := range counts {
		if n > droppedErrorBaseline[key] {
			added = append(added, fmt.Sprintf("%s: %d site(s), baseline %d (%s)",
				key, n, droppedErrorBaseline[key], strings.Join(lines[key], ", ")))
		}
	}
	for key, n := range droppedErrorBaseline {
		if counts[key] < n {
			stale = append(stale, fmt.Sprintf("%s: baseline %d, found %d", key, n, counts[key]))
		}
	}
	sort.Strings(added)
	sort.Strings(stale)
	if len(added) > 0 {
		t.Errorf("new silently dropped error(s). Handle the error (log, wrap, return) or, if dropping it is "+
			"intended, put `// %s <reason>` on the line directly above the if:\n  %s",
			bestEffortMarker, strings.Join(added, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("droppedErrorBaseline entries above the sites that remain (lower or delete them):\n  %s",
			strings.Join(stale, "\n  "))
	}
}

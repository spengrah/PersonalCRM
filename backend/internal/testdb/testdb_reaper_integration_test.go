//go:build integration_testdb

package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/tests/testsupport"

	"github.com/jackc/pgx/v5"
)

// These DB-backed tests exercise the hash-named template + reaper machinery
// directly against personal_crm_test. They never call SetupPackage (no clone /
// DATABASE_URL rewrite is wanted); they connect admin and operate on synthetic,
// test-unique templates so concurrent worktrees and sibling packages stay
// untouchable.
//
// Heavy cases that run a full migration build (TestTestdbTemplateReuseSameSet,
// TestTestdbDistinctSetsNoContention, TestTestdbMissingMarkerRebuilds) are gated
// by testsupport.RequireLongTests and named TestTestdb* so the slow suite's
// -run '...|TestTestdb' allow-list runs them. The cheap reaper/skip cases stay
// un-gated and run in the fast suite; per the "Critical" design seam they call
// reapTemplates with their OWN synthetic candidate slice only — never the global
// CleanStaleDatabases sweep — so they are safe under the parallel suite.

// requireDB skips the test unless a base DB is configured (and not short mode),
// and returns the base URL plus an admin connection bound to t.Cleanup.
func requireDB(t *testing.T) (baseURL string, admin *pgx.Conn) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping DB-backed testdb test in short mode")
	}
	baseURL = envBaseURL()
	if baseURL == "" {
		t.Skip("skipping DB-backed testdb test; DATABASE_URL/TEST_DATABASE_URL not set")
	}
	baseName, err := dbNameFromURL(baseURL)
	if err != nil {
		t.Fatalf("dbNameFromURL: %v", err)
	}
	if err := assertBaseDBName(baseName); err != nil {
		t.Fatalf("base DB guard: %v", err)
	}
	ctx := context.Background()
	admin, err = connectAdmin(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	return baseURL, admin
}

// uniqueToken returns 16 lowercase-hex chars unique per call, used to make each
// test's synthetic template name unique-per-run (so a deterministically-named
// synthetic DB never collides with the same test on a -shuffle/-count iteration
// or a concurrent worktree).
func uniqueToken(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// syntheticMigDir writes a minimal valid golang-migrate migration set into a
// fresh t.TempDir, with a table name unique to this test run. Its content hash
// (and therefore its derived template name) is unique-per-run AND
// deterministic-within-the-run.
func syntheticMigDir(t *testing.T) (migPath, hash, templateName string) {
	t.Helper()
	dir := t.TempDir()
	tbl := "synthetic_" + uniqueToken(t)
	up := "CREATE TABLE " + tbl + " (id int);\n"
	down := "DROP TABLE " + tbl + ";\n"
	if err := os.WriteFile(filepath.Join(dir, "001_"+tbl+".up.sql"), []byte(up), 0o600); err != nil {
		t.Fatalf("write up: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "001_"+tbl+".down.sql"), []byte(down), 0o600); err != nil {
		t.Fatalf("write down: %v", err)
	}
	hash, err := templateHashFromInputs(dir)
	if err != nil {
		t.Fatalf("templateHashFromInputs: %v", err)
	}
	return dir, hash, templateNameFromHash(hash)
}

// createSyntheticTemplate creates an empty hash-named template DB and writes the
// given marker hash into it (no migration run). It registers a name-guarded,
// non-FORCE drop in t.Cleanup. The drop is registered FIRST so that, by LIFO
// ordering, any later-registered connection-close cleanup runs BEFORE the drop
// (a non-FORCE drop refuses while a session is open).
func createSyntheticTemplate(t *testing.T, ctx context.Context, admin *pgx.Conn, baseURL, templateName, markerHash string) {
	t.Helper()
	if err := createDatabaseConn(ctx, admin, templateName); err != nil {
		t.Fatalf("create synthetic template %q: %v", templateName, err)
	}
	t.Cleanup(func() {
		if err := dropDatabaseNoForceConn(context.Background(), admin, templateName); err != nil {
			t.Logf("cleanup drop %q (non-fatal): %v", templateName, err)
		}
	})
	if markerHash != "" {
		templateURL, err := withDatabase(baseURL, templateName)
		if err != nil {
			t.Fatalf("template url: %v", err)
		}
		if err := writeTemplateMarker(ctx, templateURL, markerHash); err != nil {
			t.Fatalf("write synthetic marker: %v", err)
		}
	}
}

func mustOID(t *testing.T, ctx context.Context, admin *pgx.Conn, name string) uint32 {
	t.Helper()
	oid, ok, err := databaseOID(ctx, admin, name)
	if err != nil {
		t.Fatalf("databaseOID(%q): %v", name, err)
	}
	if !ok {
		t.Fatalf("expected database %q to exist", name)
	}
	return oid
}

// TestTestdbTemplateReuseSameSet [heavy]: building a template for a migration
// set, then calling ensureTemplate again for the SAME set, reuses it — the
// template name is unchanged, the OID is unchanged (no drop+rebuild), and the
// marker still equals the set's hash.
func TestTestdbTemplateReuseSameSet(t *testing.T) {
	testsupport.RequireLongTests(t)
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	migPath, wantHash, templateName := syntheticMigDir(t)
	// Drop the synthetic template at the end regardless of which path built it.
	t.Cleanup(func() {
		if err := dropDatabaseNoForceConn(context.Background(), admin, templateName); err != nil {
			t.Logf("cleanup drop %q (non-fatal): %v", templateName, err)
		}
	})

	if err := ensureTemplate(ctx, baseURL, migPath); err != nil {
		t.Fatalf("first ensureTemplate: %v", err)
	}
	firstOID := mustOID(t, ctx, admin, templateName)

	if err := ensureTemplate(ctx, baseURL, migPath); err != nil {
		t.Fatalf("second ensureTemplate: %v", err)
	}
	secondOID := mustOID(t, ctx, admin, templateName)
	if firstOID != secondOID {
		t.Errorf("template was rebuilt on reuse: OID %d → %d", firstOID, secondOID)
	}

	gotHash, ok, err := readTemplateMarker(ctx, baseURL, templateName)
	if err != nil {
		t.Fatalf("readTemplateMarker: %v", err)
	}
	if !ok || gotHash != wantHash {
		t.Errorf("marker after reuse: got (%q, present=%t), want %q", gotHash, ok, wantHash)
	}
}

// TestTestdbDistinctSetsNoContention [heavy]: two distinct migration sets build
// two distinct templates that coexist; building the second leaves the first's
// OID and marker untouched. The core anti-contention proof.
func TestTestdbDistinctSetsNoContention(t *testing.T) {
	testsupport.RequireLongTests(t)
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	migA, hashA, nameA := syntheticMigDir(t)
	migB, hashB, nameB := syntheticMigDir(t)
	if nameA == nameB {
		t.Fatalf("synthetic sets collided on a template name: %q", nameA)
	}
	for _, n := range []string{nameA, nameB} {
		n := n
		t.Cleanup(func() {
			if err := dropDatabaseNoForceConn(context.Background(), admin, n); err != nil {
				t.Logf("cleanup drop %q (non-fatal): %v", n, err)
			}
		})
	}

	if err := ensureTemplate(ctx, baseURL, migA); err != nil {
		t.Fatalf("ensureTemplate A: %v", err)
	}
	oidA := mustOID(t, ctx, admin, nameA)

	if err := ensureTemplate(ctx, baseURL, migB); err != nil {
		t.Fatalf("ensureTemplate B: %v", err)
	}

	// Both templates exist concurrently.
	for _, n := range []string{nameA, nameB} {
		exists, err := databaseExists(ctx, admin, n)
		if err != nil {
			t.Fatalf("databaseExists(%q): %v", n, err)
		}
		if !exists {
			t.Errorf("expected template %q to exist", n)
		}
	}

	// Building B did not disturb A.
	if got := mustOID(t, ctx, admin, nameA); got != oidA {
		t.Errorf("building set B rebuilt template A: OID %d → %d", oidA, got)
	}
	if gotHash, ok, err := readTemplateMarker(ctx, baseURL, nameA); err != nil {
		t.Fatalf("readTemplateMarker A: %v", err)
	} else if !ok || gotHash != hashA {
		t.Errorf("template A marker disturbed: got (%q, present=%t), want %q", gotHash, ok, hashA)
	}
	if gotHash, ok, err := readTemplateMarker(ctx, baseURL, nameB); err != nil {
		t.Fatalf("readTemplateMarker B: %v", err)
	} else if !ok || gotHash != hashB {
		t.Errorf("template B marker wrong: got (%q, present=%t), want %q", gotHash, ok, hashB)
	}
}

// TestTestdbMissingMarkerRebuilds [heavy]: a template DB that exists but has NO
// marker (a crashed/partial build) is dropped and rebuilt by ensureTemplate, and
// the rebuilt template's marker equals the set's hash. Pins D3's surviving
// recovery branch.
func TestTestdbMissingMarkerRebuilds(t *testing.T) {
	testsupport.RequireLongTests(t)
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	migPath, wantHash, templateName := syntheticMigDir(t)
	// Create the matching-named template WITHOUT a marker (empty DB).
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, "")
	partialOID := mustOID(t, ctx, admin, templateName)

	if err := ensureTemplate(ctx, baseURL, migPath); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	rebuiltOID := mustOID(t, ctx, admin, templateName)
	if rebuiltOID == partialOID {
		t.Errorf("partial template was not rebuilt: OID unchanged (%d)", partialOID)
	}
	gotHash, ok, err := readTemplateMarker(ctx, baseURL, templateName)
	if err != nil {
		t.Fatalf("readTemplateMarker: %v", err)
	}
	if !ok || gotHash != wantHash {
		t.Errorf("marker after rebuild: got (%q, present=%t), want %q", gotHash, ok, wantHash)
	}
}

// TestTestdbWrongMarkerFailsLoud [cheap]: a template DB whose marker hash differs
// from the hash its name implies makes ensureTemplate FAIL LOUD without silently
// dropping and rebuilding (OID unchanged). Pins the D3 removal. No migration run
// happens (the wrong-marker branch errors before building).
func TestTestdbWrongMarkerFailsLoud(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	migPath, _, templateName := syntheticMigDir(t)
	// Write a marker whose hash differs from the synthetic set's hash.
	wrongHash := strings.Repeat("0", 64)
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, wrongHash)
	beforeOID := mustOID(t, ctx, admin, templateName)

	err := ensureTemplate(ctx, baseURL, migPath)
	if err == nil {
		t.Fatal("expected ensureTemplate to fail loud on a present-but-wrong marker")
	}
	if !strings.Contains(err.Error(), "corrupted/incompatible template") {
		t.Errorf("error %q does not mention corrupted/incompatible template", err)
	}
	// No silent drop+rebuild.
	afterOID := mustOID(t, ctx, admin, templateName)
	if afterOID != beforeOID {
		t.Errorf("wrong-marker template was silently rebuilt: OID %d → %d", beforeOID, afterOID)
	}
}

// TestTestdbReaperSweepsStaleTemplate [cheap]: reapTemplates, handed a synthetic
// stale template's name as its candidate slice, drops it. Uses the scoped seam
// (NOT the global CleanStaleDatabases sweep), so it can never touch a template it
// did not create.
func TestTestdbReaperSweepsStaleTemplate(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	_, hash, templateName := syntheticMigDir(t)
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, hash)

	dropped, err := reapTemplates(ctx, admin, []string{templateName}, "")
	if err != nil {
		t.Fatalf("reapTemplates: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	exists, err := databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists: %v", err)
	}
	if exists {
		t.Errorf("stale template %q still exists after reap", templateName)
	}
}

// TestTestdbReaperBlocksOnAdvisoryLock [load-bearing]: the reaper cannot drop a
// template while a build/clone critical section holds the advisory lock. Hold
// the lock on a dedicated admin conn, run reapTemplates in a goroutine, assert it
// BLOCKS (the synthetic template still exists) while the lock is held, then
// release and assert it proceeds and drops. This proves the round-1 clone-copy
// race is closed by serialization, not by a connection count.
func TestTestdbReaperBlocksOnAdvisoryLock(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	_, hash, templateName := syntheticMigDir(t)
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, hash)

	// Three distinct connections: `holder` holds the lock, `reaperConn` runs the
	// blocked reapTemplates in a goroutine, and `admin` stays free for the main
	// goroutine's databaseExists checks. A single *pgx.Conn is NOT safe for
	// concurrent use, so the reaper must not share `admin`.
	holder, err := connectAdmin(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect lock holder: %v", err)
	}
	defer func() { _ = holder.Close(context.Background()) }()

	reaperConn, err := connectAdmin(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect reaper conn: %v", err)
	}
	defer func() { _ = reaperConn.Close(context.Background()) }()

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", templateBuildAdvisoryLockID); err != nil {
		t.Fatalf("acquire lock on holder: %v", err)
	}
	lockReleased := false
	releaseLock := func() {
		if lockReleased {
			return
		}
		lockReleased = true
		if _, err := holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", templateBuildAdvisoryLockID); err != nil {
			t.Logf("release lock (non-fatal): %v", err)
		}
	}
	defer releaseLock()

	var wg sync.WaitGroup
	var reapErr error
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, reapErr = reapTemplates(ctx, reaperConn, []string{templateName}, "")
		close(done)
	}()

	// While the lock is held, the reaper must block: the synthetic template
	// stays present. Give the goroutine time to reach the lock acquire.
	select {
	case <-done:
		t.Fatal("reapTemplates returned while the advisory lock was held; it must block")
	case <-time.After(500 * time.Millisecond):
	}
	exists, err := databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists while blocked: %v", err)
	}
	if !exists {
		t.Fatal("synthetic template dropped while the advisory lock was held")
	}

	// Release the lock; the reaper should now proceed and drop.
	releaseLock()
	wg.Wait()
	if reapErr != nil {
		t.Fatalf("reapTemplates after lock release: %v", reapErr)
	}
	exists, err = databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists after release: %v", err)
	}
	if exists {
		t.Errorf("synthetic template %q still exists after the reaper proceeded", templateName)
	}
}

// TestTestdbReaperSkipsOpenSession [cheap]: a synthetic template with a live
// session (numbackends >= 1) is skipped — not dropped, not terminated — and
// reapTemplates returns nil (a skip is not an error). After the session closes a
// re-run drops it.
func TestTestdbReaperSkipsOpenSession(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	_, hash, templateName := syntheticMigDir(t)
	// Register the synthetic-template drop FIRST so it runs LAST (LIFO); the
	// session-close cleanup registered next runs BEFORE the drop, so the
	// non-FORCE cleanup drop never refuses on an open session.
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, hash)

	templateURL, err := withDatabase(baseURL, templateName)
	if err != nil {
		t.Fatalf("template url: %v", err)
	}
	sess, err := pgx.Connect(ctx, templateURL)
	if err != nil {
		t.Fatalf("open session on template: %v", err)
	}
	sessClosed := false
	closeSess := func() {
		if sessClosed {
			return
		}
		sessClosed = true
		_ = sess.Close(context.Background())
	}
	t.Cleanup(closeSess)

	dropped, err := reapTemplates(ctx, admin, []string{templateName}, "")
	if err != nil {
		t.Fatalf("reapTemplates with open session returned error (a skip must not error): %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 (open session must be skipped)", dropped)
	}
	exists, err := databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists: %v", err)
	}
	if !exists {
		t.Fatal("synthetic template with an open session was dropped")
	}

	// Close the session and re-run: now it drops, still returning nil.
	closeSess()
	// pg_stat_database can lag a beat after a close; retry briefly.
	var dropped2 int
	deadline := time.Now().Add(3 * time.Second)
	for {
		dropped2, err = reapTemplates(ctx, admin, []string{templateName}, "")
		if err != nil {
			t.Fatalf("reapTemplates after close: %v", err)
		}
		if dropped2 == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("template not dropped after session close within deadline (dropped=%d)", dropped2)
		}
		time.Sleep(100 * time.Millisecond)
	}
	exists, err = databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists after close: %v", err)
	}
	if exists {
		t.Errorf("template not dropped after session closed")
	}
}

// TestTestdbReaperKeepsCurrentTemplate [cheap]: passing a candidate as the
// currentTemplateName excludes it from the drop set — the current-hash exclusion
// independent of the connection check.
func TestTestdbReaperKeepsCurrentTemplate(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	_, hash, templateName := syntheticMigDir(t)
	createSyntheticTemplate(t, ctx, admin, baseURL, templateName, hash)

	dropped, err := reapTemplates(ctx, admin, []string{templateName}, templateName)
	if err != nil {
		t.Fatalf("reapTemplates: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 (current template must be kept)", dropped)
	}
	exists, err := databaseExists(ctx, admin, templateName)
	if err != nil {
		t.Fatalf("databaseExists: %v", err)
	}
	if !exists {
		t.Errorf("current template %q was dropped", templateName)
	}
}

// TestTestdbListStaleTemplateCandidates [lists only, never drops]: the global
// LIKE-listing + current-hash exclusion, asserted with SUBSET semantics (the
// shared instance may hold other worktrees' templates). Drops nothing, so it is
// safe under the parallel suite.
func TestTestdbListStaleTemplateCandidates(t *testing.T) {
	baseURL, admin := requireDB(t)
	ctx := context.Background()

	_, hashA, nameA := syntheticMigDir(t)
	_, hashB, nameB := syntheticMigDir(t)
	createSyntheticTemplate(t, ctx, admin, baseURL, nameA, hashA)
	createSyntheticTemplate(t, ctx, admin, baseURL, nameB, hashB)

	// A non-matching name (a clone-shaped DB) must never appear in the template
	// candidate list. Create + clean it up.
	cloneName := clonePrefix + uniqueToken(t)
	if err := createDatabaseConn(ctx, admin, cloneName); err != nil {
		t.Fatalf("create decoy clone: %v", err)
	}
	t.Cleanup(func() {
		if err := dropDatabaseConn(context.Background(), admin, cloneName); err != nil {
			t.Logf("cleanup decoy clone %q (non-fatal): %v", cloneName, err)
		}
	})

	// Exclude nameA as the current template; nameB should remain a candidate.
	candidates, err := listStaleTemplateCandidates(ctx, admin, nameA)
	if err != nil {
		t.Fatalf("listStaleTemplateCandidates: %v", err)
	}
	set := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		set[c] = true
	}
	if set[nameA] {
		t.Errorf("current template %q should be excluded from candidates", nameA)
	}
	if !set[nameB] {
		t.Errorf("seeded template %q should be a candidate", nameB)
	}
	if set[cloneName] {
		t.Errorf("clone-shaped name %q must not appear in template candidates", cloneName)
	}
}

// errorsJoinNil is a tiny guard documenting that errors.Join over only-nils is
// nil — relied on by CleanStaleDatabases when nothing needs dropping.
func TestErrorsJoinNilIsNil(t *testing.T) {
	if errors.Join(nil, nil) != nil {
		t.Error("errors.Join(nil, nil) should be nil")
	}
}

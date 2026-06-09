//go:build integration_testdb

package testdb

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNameGuards(t *testing.T) {
	cases := []struct {
		name      string
		creatable bool
		droppable bool
		base      bool
	}{
		// Hash-named templates (exactly 32 lowercase hex): creatable + droppable.
		{"personal_crm_test_template_deadbeefdeadbeefdeadbeefdeadbeef", true, true, false},
		{"personal_crm_test_template_0123456789abcdef0123456789abcdef", true, true, false},
		// Legacy bare template name: NOT creatable/droppable under hash-naming.
		{"personal_crm_test_template", false, false, false},
		{"personal_crm_test_template_", false, false, false},    // empty hex
		{"personal_crm_test_template_XYZ", false, false, false}, // non-hex / uppercase
		// 31 hex (one short) and 33 hex (one over): both rejected by the {32} pin.
		{"personal_crm_test_template_deadbeefdeadbeefdeadbeefdeadbee", false, false, false},
		{"personal_crm_test_template_deadbeefdeadbeefdeadbeefdeadbeef0", false, false, false},
		{"personal_crm_test_template_a b", false, false, false}, // whitespace
		{"personal_crm_test_template_a;DROP DATABASE x", false, false, false},
		{"personal_crm_test_clone_deadbeef", true, true, false},
		{"personal_crm_test_clone_0123456789abcdef", true, true, false},
		{"personal_crm_test", false, false, true}, // base: never create/drop, only base-accept
		{"personal_crm", false, false, false},     // dev DB: never anything
		{"postgres", false, false, false},
		{"template0", false, false, false},
		{"template1", false, false, false},
		{"personal_crm_test_clone_", false, false, false},    // empty hex
		{"personal_crm_test_clone_XYZ", false, false, false}, // non-hex / uppercase
		{"personal_crm_test_clone_a b", false, false, false}, // whitespace
		{"personal_crm_test_clone_a;DROP DATABASE x", false, false, false},
		{"personal_crm_test; DROP DATABASE personal_crm", false, false, false},
		{"", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCreatable := assertCreatableTestDBName(tc.name) == nil
			if gotCreatable != tc.creatable {
				t.Errorf("assertCreatableTestDBName(%q): got creatable=%t, want %t", tc.name, gotCreatable, tc.creatable)
			}
			gotDroppable := assertDroppableTestDBName(tc.name) == nil
			if gotDroppable != tc.droppable {
				t.Errorf("assertDroppableTestDBName(%q): got droppable=%t, want %t", tc.name, gotDroppable, tc.droppable)
			}
			gotBase := assertBaseDBName(tc.name) == nil
			if gotBase != tc.base {
				t.Errorf("assertBaseDBName(%q): got base=%t, want %t", tc.name, gotBase, tc.base)
			}
		})
	}
}

func TestWithDatabasePreservesEverythingButPath(t *testing.T) {
	const base = "postgres://crm_user_test:secret@db.example.com:5433/personal_crm_test?sslmode=disable&pool_max_conns=5"

	out, err := withDatabase(base, "personal_crm_test_clone_deadbeef")
	if err != nil {
		t.Fatalf("withDatabase: %v", err)
	}

	bu, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	ou, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse out: %v", err)
	}

	if ou.Path != "/personal_crm_test_clone_deadbeef" {
		t.Errorf("path: got %q, want %q", ou.Path, "/personal_crm_test_clone_deadbeef")
	}
	if ou.User.String() != bu.User.String() {
		t.Errorf("userinfo changed: got %q, want %q", ou.User.String(), bu.User.String())
	}
	if ou.Host != bu.Host {
		t.Errorf("host changed: got %q, want %q", ou.Host, bu.Host)
	}
	if ou.RawQuery != bu.RawQuery {
		t.Errorf("query changed: got %q, want %q", ou.RawQuery, bu.RawQuery)
	}
	if ou.Scheme != bu.Scheme {
		t.Errorf("scheme changed: got %q, want %q", ou.Scheme, bu.Scheme)
	}
}

func TestAdminAndCloneURLDerivation(t *testing.T) {
	const base = "postgres://crm_user:pw@localhost:5432/personal_crm_test?sslmode=disable"

	adminURL, err := withDatabase(base, maintenanceDBName)
	if err != nil {
		t.Fatalf("admin url: %v", err)
	}
	au, _ := url.Parse(adminURL)
	if au.Path != "/postgres" {
		t.Errorf("admin path: got %q, want /postgres", au.Path)
	}

	cloneURL, err := withDatabase(base, "personal_crm_test_clone_abc123")
	if err != nil {
		t.Fatalf("clone url: %v", err)
	}
	cu, _ := url.Parse(cloneURL)
	if cu.Path != "/personal_crm_test_clone_abc123" {
		t.Errorf("clone path: got %q", cu.Path)
	}
}

func TestDBNameFromURL(t *testing.T) {
	name, err := dbNameFromURL("postgres://u:p@h:5432/personal_crm_test?sslmode=disable")
	if err != nil {
		t.Fatalf("dbNameFromURL: %v", err)
	}
	if name != "personal_crm_test" {
		t.Errorf("got %q, want personal_crm_test", name)
	}
}

func TestTemplateHashDeterministicAndSensitive(t *testing.T) {
	filesA := [][]byte{[]byte("001_initial.up.sql\x00CREATE TABLE a (id int);"), []byte("001_initial.down.sql\x00DROP TABLE a;")}
	riverA := []riverMigrationFingerprint{{version: 1, sqlUp: "CREATE TABLE river_job ();", sqlDown: "DROP TABLE river_job;"}}

	h1 := templateHash(filesA, riverA)
	h2 := templateHash(filesA, riverA)
	if h1 != h2 {
		t.Errorf("templateHash not deterministic: %q vs %q", h1, h2)
	}

	// Different migration file content → different hash.
	filesB := [][]byte{[]byte("001_initial.up.sql\x00CREATE TABLE a (id bigint);"), []byte("001_initial.down.sql\x00DROP TABLE a;")}
	if templateHash(filesB, riverA) == h1 {
		t.Error("templateHash insensitive to migration content change")
	}

	// Different River migration SQL → different hash.
	riverB := []riverMigrationFingerprint{{version: 1, sqlUp: "CREATE TABLE river_job (renamed int);", sqlDown: "DROP TABLE river_job;"}}
	if templateHash(filesA, riverB) == h1 {
		t.Error("templateHash insensitive to River migration change")
	}

	// Different River version → different hash.
	riverC := []riverMigrationFingerprint{{version: 2, sqlUp: "CREATE TABLE river_job ();", sqlDown: "DROP TABLE river_job;"}}
	if templateHash(filesA, riverC) == h1 {
		t.Error("templateHash insensitive to River version change")
	}
}

// TestRiverMigrationFingerprintsNoDB confirms the River fingerprint set is
// derivable without a live database (riverpgxv5.New(nil)).
func TestRiverMigrationFingerprintsNoDB(t *testing.T) {
	fps, err := riverMigrationFingerprints()
	if err != nil {
		t.Fatalf("riverMigrationFingerprints: %v", err)
	}
	if len(fps) == 0 {
		t.Fatal("expected at least one River migration fingerprint")
	}
	for _, fp := range fps {
		if fp.version <= 0 {
			t.Errorf("unexpected non-positive River migration version: %d", fp.version)
		}
	}
}

const hex64 = "deadbeefdeadbeefdeadbeefdeadbeefcafebabecafebabecafebabecafebabe"

// TestTemplateNameArithmetic pins the identifier-length math and ties the regex
// {32} quantifier to templateHashPrefixLen: the produced name is the prefix plus
// exactly the first 32 hex chars, is 59 chars (≤ Postgres's 63-char limit), and
// matches dbNamePattern. A one-sided change to either the constant or the regex
// breaks this.
func TestTemplateNameArithmetic(t *testing.T) {
	if templateHashPrefixLen != 32 {
		t.Fatalf("templateHashPrefixLen = %d, want 32 (dbNamePattern pins {32})", templateHashPrefixLen)
	}

	name := templateNameFromHash(hex64)
	wantName := templatePrefix + hex64[:32]
	if name != wantName {
		t.Errorf("templateNameFromHash: got %q, want %q", name, wantName)
	}

	const wantLen = 59 // len("personal_crm_test_template_") == 27, + 32 == 59
	if got := len(name); got != wantLen {
		t.Errorf("template name length: got %d, want %d", got, wantLen)
	}
	if len(templatePrefix)+templateHashPrefixLen != wantLen {
		t.Errorf("len(templatePrefix)+templateHashPrefixLen = %d, want %d", len(templatePrefix)+templateHashPrefixLen, wantLen)
	}
	if wantLen > 63 {
		t.Errorf("template name length %d exceeds Postgres identifier limit 63", wantLen)
	}

	// The produced name must be accepted by the create/drop guard — this is the
	// assertion that couples the regex {32} to templateHashPrefixLen.
	if !dbNamePattern.MatchString(name) {
		t.Errorf("templateNameFromHash(%q) = %q does not match dbNamePattern", hex64, name)
	}
}

// TestTemplateNameFromHashTruncationAndPanic documents that only the first 32
// hex chars name the template (so two hashes differing only past char 32 map to
// the SAME name — infeasible at 128 bits, but the explicit semantics), that
// differing within the first 32 yields different names, and that a too-short
// hash panics (the internal invariant guard).
func TestTemplateNameFromHashTruncationAndPanic(t *testing.T) {
	// Differ within the first 32 chars ⇒ different names.
	a := templateNameFromHash("0000000000000000000000000000000a" + strings.Repeat("f", 32))
	b := templateNameFromHash("0000000000000000000000000000000b" + strings.Repeat("f", 32))
	if a == b {
		t.Errorf("hashes differing within first 32 chars produced the same name: %q", a)
	}

	// Share the first 32 chars, differ only past char 32 ⇒ same name.
	shared := strings.Repeat("a", 32)
	c := templateNameFromHash(shared + strings.Repeat("0", 32))
	d := templateNameFromHash(shared + strings.Repeat("1", 32))
	if c != d {
		t.Errorf("hashes sharing first 32 chars produced different names: %q vs %q", c, d)
	}

	// A hash shorter than the prefix length must panic (internal invariant).
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("templateNameFromHash with short hash did not panic")
			}
		}()
		_ = templateNameFromHash(strings.Repeat("a", templateHashPrefixLen-1))
	}()
}

// TestTemplateNameFromInputHashMatchesGuard derives a name from the real
// migration-input hash and asserts it passes the create/drop guard — proving the
// full pipeline (templateHashFromInputs → templateNameFromHash → dbNamePattern)
// agrees end to end without a database.
func TestTemplateNameFromInputHashMatchesGuard(t *testing.T) {
	migPath := testMigrationsPath(t)
	hash, err := templateHashFromInputs(migPath)
	if err != nil {
		t.Fatalf("templateHashFromInputs: %v", err)
	}
	name := templateNameFromHash(hash)
	if err := assertCreatableTestDBName(name); err != nil {
		t.Errorf("derived template name %q not creatable: %v", name, err)
	}
	if err := assertDroppableTestDBName(name); err != nil {
		t.Errorf("derived template name %q not droppable: %v", name, err)
	}
}

// TestCleanclonesMigrationsPathArithmetic pins the four-".."-hop arithmetic the
// cmd/cleanclones command uses: the path resolved relative to a file at
// internal/testdb/<x>/<y>/ must reach backend/migrations and contain *.up.sql.
// A wrong hop count fails here instead of silently mis-naming the current
// template at runtime. (This test lives at internal/testdb/, ONE level below the
// command's internal/testdb/cmd/cleanclones/, so it uses two ".." hops to reach
// the same backend/migrations target — the relative depth, not the literal hop
// count, is what's pinned; the command's own four hops are asserted reachable by
// resolving from this file's known location.)
func TestCleanclonesMigrationsPathArithmetic(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// This file is at backend/internal/testdb/testdb_internal_test.go.
	// backend/migrations is two hops up (testdb → internal → backend) + migrations.
	fromHere := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	assertDirHasUpMigrations(t, fromHere)

	// The command file is two levels deeper (cmd/cleanclones), so its own
	// resolution adds two more hops (four total). Simulate that target by
	// descending two synthetic levels from this file's dir and climbing four.
	cmdDir := filepath.Join(filepath.Dir(thisFile), "cmd", "cleanclones")
	fromCmd := filepath.Join(cmdDir, "..", "..", "..", "..", "migrations")
	assertDirHasUpMigrations(t, fromCmd)
}

func assertDirHasUpMigrations(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob %q: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Errorf("migrations dir %q resolved to no *.up.sql files (wrong hop count?)", dir)
	}
}

// testMigrationsPath resolves backend/migrations from this test file's location
// (two hops up from internal/testdb/).
func testMigrationsPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

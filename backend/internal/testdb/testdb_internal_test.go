//go:build integration_testdb

package testdb

import (
	"net/url"
	"testing"
)

func TestNameGuards(t *testing.T) {
	cases := []struct {
		name      string
		creatable bool
		droppable bool
		base      bool
	}{
		{"personal_crm_test_template", true, true, false},
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

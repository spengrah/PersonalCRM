package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

// TestMethodOps_UniquenessKeyMatchesTrigger is the C6 parity test.
//
// value_normalized is written by a BEFORE INSERT OR UPDATE trigger, so
// idx_contact_method_unique_value is always enforced over the SQL function's
// output — never over anything Go computes. The operations endpoint folds its
// uniqueness key in Go to reject duplicates before issuing any statement, so if
// the Go mirror and the SQL function disagree, a conflict the fold judged safe
// reaches the index and surfaces as a 500 instead of a deterministic 400.
//
// This drives the LIVE function through the test-only sqlc query and asserts
// the mirror agrees on every row. A hand-written expected-value table would
// just re-encode whatever misunderstanding produced the divergence in the first
// place; only the real function settles it.
//
// Mutation that must turn this red: change the mirror's handle branch to strip
// one '@' instead of all of them.
func TestMethodOps_UniquenessKeyMatchesTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactMethodRepository(database.Queries)

	// The corpus targets every branch of the SQL function, plus the specific
	// inputs where Go's obvious implementation diverges from it.
	corpus := []struct {
		name       string
		methodType string
		value      string
	}{
		// Handle branch — '^@+' strips ALL leading '@'. identity.Normalize
		// strips exactly one, which is the verified divergence this mirror
		// exists to avoid inheriting.
		{"handle double at", "discord", "@@foo"},
		{"handle triple at", "telegram", "@@@foo"},
		{"handle single at", "telegram", "@foo"},
		{"handle bare", "twitter", "foo"},
		{"handle mixed case", "telegram", "@FooBar"},
		{"handle at only", "discord", "@@@"},

		// Email branch — lower(btrim(...)).
		{"email mixed case", "email", "Case@Example.Test"},
		{"email padded", "email", "  case@example.test  "},
		{"gchat mixed case", "gchat", "Case@Example.Test"},

		// Phone branch. The leading-plus test is '^\\+', which matches literal
		// BACKSLASHES, so a real '+' never reaches that branch and falls
		// through to the US-country-code rule instead.
		{"phone plus ten digits", "phone", "+1234567890"},
		{"phone bare ten digits", "phone", "1234567890"},
		{"phone eleven leading one", "phone", "11234567890"},
		{"phone plus eleven", "phone", "+11234567890"},
		{"phone formatted", "phone", "(555) 010-1234"},
		{"phone international", "phone", "+44 20 7946 0958"},
		{"phone thirteen digits", "phone", "1234567890123"},
		// The ONLY input that reaches the trigger's plus branch. Without it a
		// mirror that models that branch as permanently unreachable passes the
		// entire corpus while still being wrong.
		{"phone backslash prefix", "phone", `\1234567890`},
		{"phone double backslash", "phone", `\\1234567890`},
		{"phone backslash eleven", "signal", `\11234567890`},
		{"phone no digits", "phone", "abc"},
		{"phone blank", "phone", ""},
		{"phone spaces only", "phone", "   "},
		{"whatsapp ten digits", "whatsapp", "5550101234"},
		{"signal formatted", "signal", "555-010-1234"},

		// btrim() removes SPACES only. A tab-padded value is NOT trimmed by the
		// trigger, so strings.TrimSpace would silently disagree here.
		{"email tab padded", "email", "\tcase@example.test\t"},
		{"handle tab padded", "telegram", "\t@foo\t"},
		{"phone tab padded", "phone", "\t1234567890\t"},

		// Blank and whitespace across the other branches.
		{"email blank", "email", ""},
		{"email spaces only", "email", "   "},
		{"handle blank", "telegram", ""},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			fromTrigger, err := repo.NormalizeContactMethodValueViaTrigger(ctx, tc.methodType, tc.value)
			require.NoError(t, err, "live normalize_contact_method_value should not error")

			fromMirror := repository.NormalizeContactMethodValueForUniqueness(tc.methodType, tc.value)

			require.Equal(t, fromTrigger, fromMirror,
				"mirror disagrees with the live trigger for type=%q value=%q — the fold would compute a "+
					"uniqueness key the unique index does not enforce, turning a deterministic 400 into a 500",
				tc.methodType, tc.value)
		})
	}
}

// Package tests — telegram_message all-fields-populated regression test.
//
// This is a live tripwire for the "forgot to add the column to one SELECT"
// failure mode. A full-row read goes through sqlc's generated row.Scan, which
// assigns each column to a struct field positionally; if a future refactor
// replaces SELECT * with an explicit column list that drops a column, the
// dropped field reads back at its Go zero value even though everything still
// compiles. This test seeds a row with EVERY column non-zero and reflects over
// the read-back db.TelegramMessage, failing with the field name if any field
// is zero — naming the dropped column directly.
//
// The assertion target is the generated db.TelegramMessage (not the
// hand-mapped repository.TelegramMessage), because the scan layer is where the
// dropped-column regression manifests.
package tests

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

func TestTelegramMessage_AllFieldsPopulatedOnRead(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	// A real contact + interaction so matched_contact_id / interaction_id FKs
	// resolve. The interaction is hung off the contact.
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "All Fields Probe Contact",
	})
	require.NoError(t, err)

	occurredAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	direction := "inbound"
	sourceRef := "telegram:all-fields-probe"
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     "telegram",
		SourceRef:  &sourceRef,
		OccurredAt: occurredAt,
		Direction:  direction,
	})
	require.NoError(t, err)

	// Per-run chat-ID range for clean cleanup. The range is a single chat ID
	// here; HardDeleteTelegramMessagesByChatIDRange purges [lo, hi] inclusive.
	const chatID int64 = 920001

	t.Cleanup(func() {
		// Hard-delete the seeded message first (it FK-references the
		// interaction and contact via ON DELETE SET NULL, so order is not
		// strictly required, but deleting the child first keeps cleanup
		// independent of FK rules).
		_ = database.Queries.HardDeleteTelegramMessagesByChatIDRange(ctx, db.HardDeleteTelegramMessagesByChatIDRangeParams{
			Lo: chatID,
			Hi: chatID,
		})
		_ = interactionRepo.SoftDeleteInteraction(ctx, interaction.ID)
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	// A non-zero time distinct from createdAt's default, used for every
	// nullable timestamp column.
	ts := pgtype.Timestamptz{Time: occurredAt, Valid: true}

	inserted, err := database.Queries.InsertFullTelegramMessageForTest(ctx, db.InsertFullTelegramMessageForTestParams{
		TelegramMessageID:  920001,
		TelegramChatID:     chatID,
		ChatType:           "private",
		ChatTitle:          pgtype.Text{String: "Probe Chat", Valid: true},
		MessageText:        pgtype.Text{String: "probe message text", Valid: true},
		MessageType:        "text",
		SentAt:             ts,
		EditedAt:           ts,
		IsOutgoing:         true,
		ReplyToMsgID:       pgtype.Int4{Int32: 919999, Valid: true},
		PeerUserID:         pgtype.Int8{Int64: 778899, Valid: true},
		PeerUsername:       pgtype.Text{String: "probe_user", Valid: true},
		PeerFirstName:      pgtype.Text{String: "Probe", Valid: true},
		PeerLastName:       pgtype.Text{String: "User", Valid: true},
		PeerPhone:          pgtype.Text{String: "15555550100", Valid: true},
		MatchedContactID:   pgtype.UUID{Bytes: contact.ID, Valid: true},
		InteractionID:      pgtype.UUID{Bytes: interaction.ID, Valid: true},
		ProcessedAt:        ts,
		DeletedAt:          ts,
		PeerEntityResolved: true,
		ClaimedAt:          ts,
		ClaimedSessionRef:  pgtype.Text{String: "probe-session-ref", Valid: true},
	})
	require.NoError(t, err)

	// Read back via the test-only SELECT * (no deleted_at filter, so the
	// soft-deleted row is still returned). This is the exact scan path a
	// production full-row read uses.
	row, err := database.Queries.GetTelegramMessageByIDForTest(ctx, inserted.ID)
	require.NoError(t, err)

	// Reflect over every exported field of the returned db.TelegramMessage and
	// assert none is left at its Go zero value. A dropped column in a future
	// explicit SELECT list surfaces here as a zero field, named explicitly.
	rv := reflect.ValueOf(*row)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		assertFieldPopulated(t, field.Name, rv.Field(i))
	}
}

// assertFieldPopulated fails the test if v is at its zero value. pgtype.*
// wrapper types (which all carry a Valid bool plus an inner value field) are
// checked by asserting Valid is true AND at least one non-Valid field is
// non-zero; plain scalars are checked directly. The field name is included in
// the failure so a dropped SELECT column is named.
func assertFieldPopulated(t *testing.T, name string, v reflect.Value) {
	t.Helper()

	// pgtype wrappers are structs with a bool field named "Valid". Detect that
	// shape generically rather than enumerating every pgtype.* type.
	if v.Kind() == reflect.Struct {
		if valid := v.FieldByName("Valid"); valid.IsValid() && valid.Kind() == reflect.Bool {
			if !valid.Bool() {
				t.Errorf("field %s: pgtype value is not Valid (column likely missing from the SELECT list)", name)
				return
			}
			// At least one non-Valid field must be non-zero, otherwise the
			// inner value was never scanned.
			vt := v.Type()
			innerPopulated := false
			for i := 0; i < vt.NumField(); i++ {
				if vt.Field(i).Name == "Valid" {
					continue
				}
				if !v.Field(i).IsZero() {
					innerPopulated = true
					break
				}
			}
			if !innerPopulated {
				t.Errorf("field %s: pgtype value is Valid but its inner value is zero (column likely missing from the SELECT list)", name)
			}
			return
		}
	}

	if v.IsZero() {
		t.Errorf("field %s read back as the zero value — a SELECT list is likely missing this column", name)
	}
}

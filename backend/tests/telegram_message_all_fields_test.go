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

	t.Parallel()
	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)

	// A real contact + interaction so matched_contact_id / interaction_id FKs
	// resolve. The interaction is hung off the contact.
	gen, _ := migrationGenerator(t)
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)

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

	// Dedicated chat-ID for this test. HardDeleteTelegramMessagesByChatIDRange
	// purges [lo, hi] inclusive.
	const chatID int64 = 920001

	purgeRow := func() {
		_ = database.Queries.HardDeleteTelegramMessagesByChatIDRange(ctx, db.HardDeleteTelegramMessagesByChatIDRangeParams{
			Lo: chatID,
			Hi: chatID,
		})
	}
	// Pre-clean: an interrupted prior run could have left this row behind, and
	// InsertFullTelegramMessageForTest would then hit the
	// (telegram_chat_id, telegram_message_id) unique before this run gets to
	// prove anything. Purge first.
	purgeRow()

	t.Cleanup(func() {
		// Hard-delete the seeded message first (it FK-references the
		// interaction and contact via ON DELETE SET NULL, so order is not
		// strictly required, but deleting the child first keeps cleanup
		// independent of FK rules).
		purgeRow()
		_ = interactionRepo.SoftDeleteInteraction(ctx, interaction.ID)
		contactCleanup()
	})

	// A non-zero time distinct from createdAt's default, used for every
	// nullable timestamp column.
	ts := occurredAt
	chatTitle := "Probe Chat"
	messageText := "probe message text"
	replyToMsgID := int32(919999)
	peerUserID := int64(778899)
	peerUsername := "probe_user"
	peerFirstName := "Probe"
	peerLastName := "User"
	peerPhone := "15555550100"
	claimedSessionRef := "probe-session-ref"

	inserted, err := database.Queries.InsertFullTelegramMessageForTest(ctx, db.InsertFullTelegramMessageForTestParams{
		TelegramMessageID:  920001,
		TelegramChatID:     chatID,
		ChatType:           "private",
		ChatTitle:          &chatTitle,
		MessageText:        &messageText,
		MessageType:        "text",
		SentAt:             ts,
		EditedAt:           &ts,
		IsOutgoing:         true,
		ReplyToMsgID:       &replyToMsgID,
		PeerUserID:         &peerUserID,
		PeerUsername:       &peerUsername,
		PeerFirstName:      &peerFirstName,
		PeerLastName:       &peerLastName,
		PeerPhone:          &peerPhone,
		MatchedContactID:   &contact.ID,
		InteractionID:      &interaction.ID,
		ProcessedAt:        &ts,
		DeletedAt:          &ts,
		PeerEntityResolved: true,
		ClaimedAt:          &ts,
		ClaimedSessionRef:  &claimedSessionRef,
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

// assertFieldPopulated fails the test if v is at its zero value. Nullable
// columns are plain Go pointers post-flip: a nil pointer means the column
// scanned as SQL NULL (the dropped-column failure mode this test exists to
// catch), and a non-nil pointer must point to a genuinely non-zero value —
// otherwise the fixture above seeded a zero, not a populated one, and the
// test would pass vacuously. Non-nullable columns are checked directly. The
// field name is included in the failure so a dropped SELECT column is named.
func assertFieldPopulated(t *testing.T, name string, v reflect.Value) {
	t.Helper()

	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			t.Errorf("field %s: nil pointer (column likely missing from the SELECT list)", name)
			return
		}
		if v.Elem().IsZero() {
			t.Errorf("field %s: pointer is non-nil but points to a zero value (fixture likely seeded a zero, not a populated value)", name)
		}
		return
	}

	if v.IsZero() {
		t.Errorf("field %s read back as the zero value — a SELECT list is likely missing this column", name)
	}
}

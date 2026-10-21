//go:build integration_testdb

// Coverage for the one-time quote trim (crm-admin --trim-email-quotes) against
// the test's own PostgreSQL clone: rows stored before ingest applied ING-047
// are rewritten by the same rule, reading the gmail id from the per-account
// provenance the upsert writes.
package google_test

import (
	"encoding/json"
	"testing"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEmailQuoteTrim_StoredBodies(t *testing.T) {
	ctx := t.Context()
	h := synthetic.NewHarnessForNamespace(t, ctx, "qt"+uuid.NewString()[:8], factory.DefaultSeed)
	contact, err := h.SeedContact(ctx, h.Generator().Contact(factory.WithEmail()))
	require.NoError(t, err)
	comms := repository.NewCommsMessageRepository(h.Database().Queries)
	account := "me@example.com"
	sentAt := h.Generator().Anchor()

	// store writes a row the way the pre-ING-047 ingest did.
	store := func(gmailID, threadID, body, html string, accountID *string) uuid.UUID {
		t.Helper()
		metadata, err := json.Marshal(map[string]string{"html": html})
		require.NoError(t, err)
		msg, err := comms.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source:           repository.InteractionSourceEmail,
			ExternalID:       "<" + uuid.NewString() + "@example.com>",
			ThreadID:         &threadID,
			Body:             &body,
			Direction:        "inbound",
			SentAt:           sentAt,
			AccountID:        accountID,
			SourceMetadata:   metadata,
			MatchedContactID: contact.ID,
			GmailMessageID:   &gmailID,
		})
		require.NoError(t, err)
		return msg.ID
	}
	bodyOf := func(id uuid.UUID) string {
		t.Helper()
		msg, err := comms.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, msg.Body)
		return *msg.Body
	}

	const quoted = "See you then.\n\nOn Mon, Contact A wrote:\n> Meet Tuesday?\n"
	reply := store("g2", "g1", quoted, "", &account)
	starter := store("g1", "g1", quoted, "", &account)
	noAccountReply := store("g3", "g1", quoted, "", nil)
	html := `<div>See you then.</div><div class="gmail_quote">On Mon, Contact A wrote:<blockquote>Meet Tuesday?</blockquote></div>`
	htmlReply := store("g4", "g1", "See you then. On Mon, Contact A wrote: Meet Tuesday?", html, &account)

	svc := google.NewEmailQuoteTrimService(comms)
	first, err := svc.TrimStoredBodies(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, first.Trimmed, 3)

	require.Equal(t, "See you then.", bodyOf(reply))
	require.Equal(t, quoted, bodyOf(starter))
	require.Equal(t, "See you then.", bodyOf(noAccountReply))
	require.Equal(t, "See you then.", bodyOf(htmlReply))

	second, err := svc.TrimStoredBodies(ctx)
	require.NoError(t, err)
	require.Zero(t, second.Trimmed, "a second run changes nothing")
}

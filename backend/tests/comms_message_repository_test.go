package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCommsMessageTest provisions a per-test repo set against the shared test
// DB. Cleanup is the caller's responsibility (hard-delete content by contact +
// soft-delete the contact), because the upsert does not clear deleted_at on
// conflict and soft-deleted content rows would otherwise resurrect across runs
// (gotcha table).
func setupCommsMessageTest(t *testing.T) (context.Context, *repository.CommsMessageRepository, *repository.ContactRepository, *repository.ContactMethodRepository, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	repo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	cleanup := func() { database.Close() }
	return ctx, repo, contactRepo, methodRepo, cleanup
}

// newEmailContact creates a contact and registers a cleanup that hard-deletes
// its comms_message rows then soft-deletes the contact.
func newEmailContact(t *testing.T, ctx context.Context, repo *repository.CommsMessageRepository, contactRepo *repository.ContactRepository, name string) *repository.Contact {
	t.Helper()
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.HardDeleteByContact(ctx, contact.ID)
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})
	return contact
}

// decodeMetadata unmarshals a comms_message.source_metadata blob.
func decodeMetadata(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// observedAccounts extracts source_metadata.observed_accounts as a []string.
func observedAccounts(t *testing.T, raw []byte) []string {
	t.Helper()
	m := decodeMetadata(t, raw)
	arr, ok := m["observed_accounts"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, v.(string))
	}
	return out
}

// accountGmailIDs extracts source_metadata.account_gmail_ids as a map.
func accountGmailIDs(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	m := decodeMetadata(t, raw)
	obj, ok := m["account_gmail_ids"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		out[k] = v.(string)
	}
	return out
}

// baseUpsertParams builds a fully-populated upsert input for one (account,
// gmail id) observation of a message. The caller overrides per-account fields.
func baseUpsertParams(externalID string, contactID uuid.UUID, sentAt time.Time, accountID, gmailID string, metadata []byte) repository.UpsertCommsMessageParams {
	return repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       externalID,
		ThreadID:         strPtr("thread-1"),
		Subject:          strPtr("Hello"),
		Body:             strPtr("body content"),
		Snippet:          strPtr("body con..."),
		PeerHandle:       strPtr("peer@example.test"),
		PeerNormalized:   strPtr("peer@example.test"),
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           sentAt,
		AccountID:        strPtr(accountID),
		SourceMetadata:   metadata,
		MatchedContactID: contactID,
		GmailMessageID:   strPtr(gmailID),
	}
}

// metadataFor builds an initial source_metadata blob a provider would pass for
// a single-account observation (observed_accounts + account_gmail_ids seeded).
func metadataFor(t *testing.T, accountID, gmailID string, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"observed_accounts": []string{accountID},
		"account_gmail_ids": map[string]string{accountID: gmailID},
	}
	for k, v := range extra {
		m[k] = v
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return raw
}

func TestCommsMessageRepository_UpsertAndGet(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Upsert "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-upsert-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", map[string]any{"html": "<b>hi</b>"})

	msg, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourceEmail, msg.Source)
	assert.Equal(t, externalID, msg.ExternalID)
	assert.Equal(t, contact.ID, msg.MatchedContactID)
	require.NotNil(t, msg.Body)
	assert.Equal(t, "body content", *msg.Body)
	require.NotNil(t, msg.Subject)
	assert.Equal(t, "Hello", *msg.Subject)
	assert.Equal(t, repository.InteractionDirectionInbound, msg.Direction)

	got, err := repo.GetMessage(ctx, repository.InteractionSourceEmail, externalID, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, got.ID)
	assert.Equal(t, []string{"accA"}, observedAccounts(t, got.SourceMetadata))
	assert.Equal(t, map[string]string{"accA": "gidA"}, accountGmailIDs(t, got.SourceMetadata))

	// GetByID round-trip.
	byID, err := repo.GetByID(ctx, msg.ID)
	require.NoError(t, err)
	assert.Equal(t, externalID, byID.ExternalID)
}

func TestCommsMessageRepository_GetMessage_NotFound(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms NotFound "+suffix)

	_, err := repo.GetMessage(ctx, repository.InteractionSourceEmail, "<missing-"+suffix+">", contact.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
}

func TestCommsMessageRepository_UpsertIdempotentSameAccount(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Idem "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-idem-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", nil)

	first, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)

	// Replay with identical account (cursor-overlap simulation). observed_accounts
	// must not grow; the single gmail-id key is unchanged.
	second, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "ID stable across conflict")
	accounts := observedAccounts(t, second.SourceMetadata)
	assert.Equal(t, []string{"accA"}, accounts, "observed_accounts must not grow on same-account replay")
	assert.Equal(t, map[string]string{"accA": "gidA"}, accountGmailIDs(t, second.SourceMetadata))
}

func TestCommsMessageRepository_CrossAccountProvenanceMerge(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms XAcct "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-xacct-" + suffix + ">"

	// Account A writes first (content from A).
	mdA := metadataFor(t, "accA", "gidA", map[string]any{"html": "<b>A</b>"})
	_, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", mdA))
	require.NoError(t, err)

	// Account B observes the same Message-ID with a different body/subject.
	mdB := metadataFor(t, "accB", "gidB", map[string]any{"html": "<b>B</b>"})
	paramsB := baseUpsertParams(externalID, contact.ID, sentAt, "accB", "gidB", mdB)
	paramsB.Body = strPtr("DIFFERENT body")
	paramsB.Subject = strPtr("DIFFERENT subject")
	paramsB.Direction = repository.InteractionDirectionOutbound
	merged, err := repo.UpsertMessage(ctx, paramsB)
	require.NoError(t, err)

	// observed_accounts is the set-union of both; both gmail ids recorded.
	assert.ElementsMatch(t, []string{"accA", "accB"}, observedAccounts(t, merged.SourceMetadata))
	assert.Equal(t, map[string]string{"accA": "gidA", "accB": "gidB"}, accountGmailIDs(t, merged.SourceMetadata))

	// Content fields are immutable on conflict (first writer wins).
	require.NotNil(t, merged.Body)
	assert.Equal(t, "body content", *merged.Body, "body must remain first writer's")
	require.NotNil(t, merged.Subject)
	assert.Equal(t, "Hello", *merged.Subject, "subject must remain first writer's")
	assert.Equal(t, repository.InteractionDirectionInbound, merged.Direction, "direction must remain first writer's")
	// Pre-existing html key from A survives untouched.
	assert.Equal(t, "<b>A</b>", decodeMetadata(t, merged.SourceMetadata)["html"])
}

func TestCommsMessageRepository_SameAccountReplayAfterMerge(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms ReplayMerge "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-replaymerge-" + suffix + ">"

	mdA := metadataFor(t, "accA", "gidA", nil)
	mdB := metadataFor(t, "accB", "gidB", nil)

	// A, then B, then A again (cursor overlap). observed_accounts stays [A, B].
	_, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", mdA))
	require.NoError(t, err)
	_, err = repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accB", "gidB", mdB))
	require.NoError(t, err)
	final, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", mdA))
	require.NoError(t, err)

	accounts := observedAccounts(t, final.SourceMetadata)
	assert.Len(t, accounts, 2, "observed_accounts must not grow on same-account replay after merge")
	assert.ElementsMatch(t, []string{"accA", "accB"}, accounts)
}

func TestCommsMessageRepository_ContentImmutableOnConflict(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Immutable "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-immutable-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", nil)

	_, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)

	// Second upsert (same key, same account) with different content.
	params2 := baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md)
	params2.Body = strPtr("OVERWRITE attempt")
	params2.Subject = strPtr("OVERWRITE subject")
	params2.Snippet = strPtr("OVERWRITE snippet")
	second, err := repo.UpsertMessage(ctx, params2)
	require.NoError(t, err)

	require.NotNil(t, second.Body)
	assert.Equal(t, "body content", *second.Body)
	require.NotNil(t, second.Subject)
	assert.Equal(t, "Hello", *second.Subject)
	require.NotNil(t, second.Snippet)
	assert.Equal(t, "body con...", *second.Snippet)
}

func TestCommsMessageRepository_PerParticipantRowsDistinct(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contactA := newEmailContact(t, ctx, repo, contactRepo, "Test Comms PartA "+suffix)
	contactB := newEmailContact(t, ctx, repo, contactRepo, "Test Comms PartB "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-part-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", nil)

	// Same external_id, different matched_contact_id → two distinct rows.
	msgA, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contactA.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)
	msgB, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contactB.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)

	assert.NotEqual(t, msgA.ID, msgB.ID, "per-participant rows are distinct")

	gotA, err := repo.GetMessage(ctx, repository.InteractionSourceEmail, externalID, contactA.ID)
	require.NoError(t, err)
	gotB, err := repo.GetMessage(ctx, repository.InteractionSourceEmail, externalID, contactB.ID)
	require.NoError(t, err)
	assert.Equal(t, contactA.ID, gotA.MatchedContactID)
	assert.Equal(t, contactB.ID, gotB.MatchedContactID)
}

func TestCommsMessageRepository_MarkProcessedTx(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	databaseURL := os.Getenv("DATABASE_URL")
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms MarkProc "+suffix)
	t.Cleanup(func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceEmail, "email-markproc-%")
	})

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-markproc-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", nil)
	msg, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", md))
	require.NoError(t, err)

	ref := "email-markproc-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceEmail,
		SourceRef:  &ref,
		OccurredAt: sentAt,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	// Mark processed within a tx; sessionRef is arbitrary and must be ignored.
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	affected, err := repo.MarkProcessedTx(ctx, tx, []uuid.UUID{msg.ID}, interaction.ID, "arbitrary-session-ref")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	assert.Equal(t, int64(1), affected)

	got, err := repo.GetByID(ctx, msg.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ProcessedAt)
	require.NotNil(t, got.InteractionID)
	assert.Equal(t, interaction.ID, *got.InteractionID)

	// Replay: already processed → affects 0 rows (idempotent).
	tx2, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	affected2, err := repo.MarkProcessedTx(ctx, tx2, []uuid.UUID{msg.ID}, interaction.ID, "different-session-ref")
	require.NoError(t, err)
	require.NoError(t, tx2.Commit(ctx))
	assert.Equal(t, int64(0), affected2, "replay finds row already processed")

	// Empty input short-circuits.
	tx3, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	affected3, err := repo.MarkProcessedTx(ctx, tx3, nil, interaction.ID, "x")
	require.NoError(t, err)
	require.NoError(t, tx3.Rollback(ctx))
	assert.Equal(t, int64(0), affected3)
}

func TestCommsMessageRepository_ListByContactNewestFirst(t *testing.T) {
	ctx, repo, contactRepo, _, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms List "+suffix)

	now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	older := now.Add(-time.Hour)
	newer := now

	mdOld := metadataFor(t, "accA", "gid-old", nil)
	mdNew := metadataFor(t, "accA", "gid-new", nil)
	_, err := repo.UpsertMessage(ctx, baseUpsertParams("<msgid-old-"+suffix+">", contact.ID, older, "accA", "gid-old", mdOld))
	require.NoError(t, err)
	_, err = repo.UpsertMessage(ctx, baseUpsertParams("<msgid-new-"+suffix+">", contact.ID, newer, "accA", "gid-new", mdNew))
	require.NoError(t, err)

	list, err := repo.ListByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.True(t, list[0].SentAt.After(list[1].SentAt) || list[0].SentAt.Equal(list[1].SentAt),
		"results ordered sent_at DESC")
	assert.Equal(t, "<msgid-new-"+suffix+">", list[0].ExternalID)
	assert.Equal(t, "<msgid-old-"+suffix+">", list[1].ExternalID)
}

func TestCommsMessageRepository_ListEmailIdentitiesForSync(t *testing.T) {
	ctx, repo, contactRepo, methodRepo, cleanup := setupCommsMessageTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	sharedEmail := "shared-" + suffix + "@example.test"
	uniqueEmail := "unique-" + suffix + "@example.test"
	deletedEmail := "deleted-" + suffix + "@example.test"

	// Two contacts sharing one address (many-to-one).
	contact1 := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Ident1 "+suffix)
	contact2 := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Ident2 "+suffix)
	// Third contact with a unique address.
	contact3 := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Ident3 "+suffix)
	// Soft-deleted contact whose email must be excluded.
	deletedContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Test Comms IdentDeleted " + suffix})
	require.NoError(t, err)

	for _, cm := range []repository.CreateContactMethodRequest{
		{ContactID: contact1.ID, Type: string(repository.ContactMethodEmail), Value: sharedEmail},
		{ContactID: contact2.ID, Type: string(repository.ContactMethodEmail), Value: sharedEmail},
		{ContactID: contact3.ID, Type: string(repository.ContactMethodEmail), Value: uniqueEmail},
		{ContactID: deletedContact.ID, Type: string(repository.ContactMethodEmail), Value: deletedEmail},
	} {
		_, err := methodRepo.CreateContactMethod(ctx, cm)
		require.NoError(t, err)
	}
	// Soft-delete the fourth contact so its email is excluded.
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deletedContact.ID))

	identities, err := repo.ListEmailIdentitiesForSync(ctx)
	require.NoError(t, err)

	// Collect this test's identities (the shared DB holds other rows too).
	type pair struct {
		email     string
		contactID uuid.UUID
	}
	got := make([]pair, 0)
	for _, id := range identities {
		switch id.ValueNormalized {
		case sharedEmail, uniqueEmail, deletedEmail:
			got = append(got, pair{id.ValueNormalized, id.ContactID})
		}
	}

	// The shared address appears once per owning contact; the unique address
	// once; the deleted contact's address never.
	assert.ElementsMatch(t, []pair{
		{sharedEmail, contact1.ID},
		{sharedEmail, contact2.ID},
		{uniqueEmail, contact3.ID},
	}, got)
	for _, p := range got {
		assert.NotEqual(t, deletedEmail, p.email, "soft-deleted contact's email must be excluded")
	}
}

// TestInteractionSourceCheck_AcceptsEmail is a thin acceptance smoke-insert for
// the email source, mirroring TestInteractionSourceCheck_AcceptsPhoneCalls:
// HARD-delete the seeded interaction before closing (the 059 down-migration
// data-loss guard counts rows regardless of deleted_at).
func TestInteractionSourceCheck_AcceptsEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := randomSuffix(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test Email Source CHECK " + suffix,
	})
	require.NoError(t, err)
	defer func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceEmail, "email-source-test-%")
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	}()

	ref := "email-source-test-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceEmail,
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourceEmail, interaction.Source)
}

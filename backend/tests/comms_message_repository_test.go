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
func setupCommsMessageTest(t *testing.T) (context.Context, *repository.CommsMessageRepository, *repository.ContactRepository, *repository.ContactMethodRepository) {
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

	// Register the pool close via t.Cleanup BEFORE any row-cleanup callbacks
	// are registered in the test body. t.Cleanup runs LIFO, so this close runs
	// LAST — after newEmailContact's hard-delete/soft-delete callbacks — keeping
	// the pool open through cleanup. (A returned cleanup func that callers
	// `defer` would close the pool before t.Cleanup callbacks run, since Go runs
	// test defers before t.Cleanup, silently leaking rows into the shared DB.)
	t.Cleanup(database.Close)

	repo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)

	return ctx, repo, contactRepo, methodRepo
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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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

// TestCommsMessageRepository_UpsertSynthesizesProvenanceOnInsert proves the
// upsert records the observing account's provenance from the AccountID +
// GmailMessageID params on the INSERT path, even when the caller passes no
// provenance keys (nil metadata, or a metadata blob carrying only content
// keys). The repository must not depend on the caller pre-seeding
// observed_accounts[] / account_gmail_ids.
func TestCommsMessageRepository_UpsertSynthesizesProvenanceOnInsert(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms Synth "+suffix)
	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)

	// (a) nil metadata → provenance synthesized from params alone.
	idNil := "<msgid-synth-nil-" + suffix + ">"
	pNil := baseUpsertParams(idNil, contact.ID, sentAt, "accA", "gidA", nil)
	msgNil, err := repo.UpsertMessage(ctx, pNil)
	require.NoError(t, err)
	assert.Equal(t, []string{"accA"}, observedAccounts(t, msgNil.SourceMetadata))
	assert.Equal(t, map[string]string{"accA": "gidA"}, accountGmailIDs(t, msgNil.SourceMetadata))

	// (b) caller metadata carries only a content key → provenance added, content key preserved.
	idContent := "<msgid-synth-content-" + suffix + ">"
	contentOnly, err := json.Marshal(map[string]any{"html": "<b>x</b>"})
	require.NoError(t, err)
	pContent := baseUpsertParams(idContent, contact.ID, sentAt, "accA", "gidA", contentOnly)
	msgContent, err := repo.UpsertMessage(ctx, pContent)
	require.NoError(t, err)
	assert.Equal(t, []string{"accA"}, observedAccounts(t, msgContent.SourceMetadata))
	assert.Equal(t, map[string]string{"accA": "gidA"}, accountGmailIDs(t, msgContent.SourceMetadata))
	assert.Equal(t, "<b>x</b>", decodeMetadata(t, msgContent.SourceMetadata)["html"])

	// (c) nil account_id → gmail id filed under '__unknown__', observed_accounts empty.
	idNoAcct := "<msgid-synth-noacct-" + suffix + ">"
	pNoAcct := repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       idNoAcct,
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           sentAt,
		AccountID:        nil,
		GmailMessageID:   strPtr("gidZ"),
		MatchedContactID: contact.ID,
	}
	msgNoAcct, err := repo.UpsertMessage(ctx, pNoAcct)
	require.NoError(t, err)
	assert.Empty(t, observedAccounts(t, msgNoAcct.SourceMetadata))
	assert.Equal(t, map[string]string{"__unknown__": "gidZ"}, accountGmailIDs(t, msgNoAcct.SourceMetadata))
}

func TestCommsMessageRepository_GetMessage_NotFound(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms NotFound "+suffix)

	_, err := repo.GetMessage(ctx, repository.InteractionSourceEmail, "<missing-"+suffix+">", contact.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
}

func TestCommsMessageRepository_UpsertIdempotentSameAccount(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Comms XAcct "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "<msgid-xacct-" + suffix + ">"

	// Account A writes first (content from A).
	mdA := metadataFor(t, "accA", "gidA", map[string]any{"html": "<b>A</b>"})
	_, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, sentAt, "accA", "gidA", mdA))
	require.NoError(t, err)

	// Account B observes the same Message-ID with a different body/subject and
	// NO pre-seeded provenance metadata — the conflict-path merge must union B
	// in from the AccountID/GmailMessageID params, not from B's metadata blob.
	paramsB := baseUpsertParams(externalID, contact.ID, sentAt, "accB", "gidB", nil)
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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	databaseURL := os.Getenv("DATABASE_URL")
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// Close this second pool via t.Cleanup (LIFO) so it outlives the
	// interaction-cleanup callback registered below, which uses interactionRepo.
	t.Cleanup(database.Close)
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
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

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
	ctx, repo, contactRepo, methodRepo := setupCommsMessageTest(t)

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

// gchatUpsertParams builds an upsert input for a gchat row (the source the
// edit/delete reconciliation queries serve in production). external_id is the
// Chat message resource name; thread_id is the space resource name.
func gchatUpsertParams(externalID string, contactID uuid.UUID, sentAt time.Time, body string) repository.UpsertCommsMessageParams {
	return repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceGChat,
		ExternalID:       externalID,
		ThreadID:         strPtr("spaces/AAAA-" + externalID),
		Body:             strPtr(body),
		Snippet:          strPtr(body),
		PeerHandle:       strPtr("peer@example.test"),
		PeerNormalized:   strPtr("peer@example.test"),
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           sentAt,
		AccountID:        strPtr("accA"),
		MatchedContactID: contactID,
	}
}

// previousBodies extracts source_metadata.previous_bodies as a []string.
func previousBodies(t *testing.T, raw []byte) []string {
	t.Helper()
	m := decodeMetadata(t, raw)
	arr, ok := m["previous_bodies"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, v.(string))
	}
	return out
}

// TestApplyEditByExternalID_PreviousBodiesCapAndRecencyGuard pins the edit
// query's two SQL-level contracts: (1) previous_bodies[] holds the last 3 prior
// bodies newest-first across repeated edits; (2) the ::timestamptz recency guard
// rejects a stale (or equal) last_update_time so concurrent same-edit
// observation pushes previous_bodies[] exactly once. body/snippet/edited_at/
// last_update_time are updated; processed_at/interaction_id/deleted_at untouched.
func TestApplyEditByExternalID_PreviousBodiesCapAndRecencyGuard(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test GChat Edit "+suffix)

	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	externalID := "spaces/AAAA/messages/edit-" + suffix
	seeded, err := repo.UpsertMessage(ctx, gchatUpsertParams(externalID, contact.ID, sentAt, "body v0"))
	require.NoError(t, err)

	src := repository.InteractionSourceGChat

	// Edit 1 (prior body "body v0") at t1.
	n, err := repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("body v1"), strPtr("snip v1"), "2026-06-04T10:00:00Z", "2026-06-04T10:00:01Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Edit 2 (prior body "body v1") at t2.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("body v2"), strPtr("snip v2"), "2026-06-04T10:00:02Z", "2026-06-04T10:00:02Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Edit 3 (prior body "body v2") at t3.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("body v3"), strPtr("snip v3"), "2026-06-04T10:00:03Z", "2026-06-04T10:00:03Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Edit 4 (prior body "body v3") at t4 — should drop "body v0" from the cap.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("body v4"), strPtr("snip v4"), "2026-06-04T10:00:04Z", "2026-06-04T10:00:04Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := repo.GetMessage(ctx, src, externalID, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Body)
	assert.Equal(t, "body v4", *got.Body)
	require.NotNil(t, got.Snippet)
	assert.Equal(t, "snip v4", *got.Snippet)
	// Newest-first, exactly the last 3 prior bodies (v0 dropped).
	assert.Equal(t, []string{"body v3", "body v2", "body v1"}, previousBodies(t, got.SourceMetadata))
	md := decodeMetadata(t, got.SourceMetadata)
	assert.Equal(t, "2026-06-04T10:00:04Z", md["last_update_time"])
	assert.Equal(t, "2026-06-04T10:00:04Z", md["edited_at"])
	// Content-derived fields untouched by the edit path.
	assert.Nil(t, got.ProcessedAt)
	assert.Nil(t, got.InteractionID)
	assert.Nil(t, got.DeletedAt)
	assert.Equal(t, seeded.ID, got.ID)

	// Recency guard: re-applying the SAME edit (equal last_update_time) is a
	// no-op — 0 rows, previous_bodies unchanged (not double-pushed).
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("body v4 dup"), strPtr("snip dup"), "2026-06-04T10:00:99Z", "2026-06-04T10:00:04Z")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "equal last_update_time must not re-apply")

	// Stale guard: an OLDER last_update_time is rejected too.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("stale body"), strPtr("stale"), "2026-06-04T09:00:00Z", "2026-06-04T10:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "older last_update_time must be rejected")

	got2, err := repo.GetMessage(ctx, src, externalID, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.Body)
	assert.Equal(t, "body v4", *got2.Body, "no-op edits left body unchanged")
	assert.Equal(t, []string{"body v3", "body v2", "body v1"}, previousBodies(t, got2.SourceMetadata), "previous_bodies not double-pushed")
}

// TestApplyEditByExternalID_FractionalSecondOrdering proves the ::timestamptz
// guard orders by real instant, not lexical RFC-3339: a higher-fractional-
// precision newer edit applies, and a genuinely-older edit that sorts lexically
// LATER is rejected. The "...10:00:00Z" vs "...10:00:00.001Z" pair is the exact
// case a string compare would invert (the 'Z' byte sorts after '.').
func TestApplyEditByExternalID_FractionalSecondOrdering(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test GChat Frac "+suffix)
	src := repository.InteractionSourceGChat
	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)

	externalID := "spaces/AAAA/messages/frac-" + suffix
	_, err := repo.UpsertMessage(ctx, gchatUpsertParams(externalID, contact.ID, sentAt, "frac v0"))
	require.NoError(t, err)

	// Seed last_update_time = "...10:00:00.001Z".
	n, err := repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("frac v1"), strPtr("frac v1"), "2026-06-04T10:00:00Z", "2026-06-04T10:00:00.001Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Genuinely OLDER ("...10:00:00Z") but sorts lexically LATER than the stored
	// "...10:00:00.001Z" → must be REJECTED by the timestamptz guard.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("frac older"), strPtr("frac older"), "2026-06-04T10:00:00Z", "2026-06-04T10:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "lexically-later but chronologically-earlier edit must be rejected")

	got, err := repo.GetMessage(ctx, src, externalID, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Body)
	assert.Equal(t, "frac v1", *got.Body)

	// Genuinely NEWER ("...10:00:00.002Z") → applies.
	n, err = repo.ApplyEditByExternalID(ctx, src, externalID, strPtr("frac v2"), strPtr("frac v2"), "2026-06-04T10:00:01Z", "2026-06-04T10:00:00.002Z")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	got, err = repo.GetMessage(ctx, src, externalID, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Body)
	assert.Equal(t, "frac v2", *got.Body)
}

// TestSoftDeleteByExternalID_AllFannedRows proves the production delete path
// soft-deletes EVERY fanned-out row for (source, external_id) and is idempotent.
func TestSoftDeleteByExternalID_AllFannedRows(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contactA := newEmailContact(t, ctx, repo, contactRepo, "Test GChat DelA "+suffix)
	contactB := newEmailContact(t, ctx, repo, contactRepo, "Test GChat DelB "+suffix)
	src := repository.InteractionSourceGChat
	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)

	externalID := "spaces/AAAA/messages/del-" + suffix
	_, err := repo.UpsertMessage(ctx, gchatUpsertParams(externalID, contactA.ID, sentAt, "del body"))
	require.NoError(t, err)
	_, err = repo.UpsertMessage(ctx, gchatUpsertParams(externalID, contactB.ID, sentAt, "del body"))
	require.NoError(t, err)

	now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	n, err := repo.SoftDeleteByExternalID(ctx, src, externalID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "both fanned-out rows soft-deleted")

	// Both rows now invisible to the deleted_at-filtered GetMessage.
	_, err = repo.GetMessage(ctx, src, externalID, contactA.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))
	_, err = repo.GetMessage(ctx, src, externalID, contactB.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound))

	// Idempotent: re-deleting affects 0 rows.
	n, err = repo.SoftDeleteByExternalID(ctx, src, externalID, now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

// TestGetLatestByExternalID_NewestFirstAndNotFound pins the body-pre-check read:
// it returns the newest row by (sent_at DESC, id) and ErrNotFound on miss.
func TestGetLatestByExternalID_NewestFirstAndNotFound(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test GChat Latest "+suffix)
	src := repository.InteractionSourceGChat

	externalID := "spaces/AAAA/messages/latest-" + suffix
	sentAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	_, err := repo.UpsertMessage(ctx, gchatUpsertParams(externalID, contact.ID, sentAt, "latest body"))
	require.NoError(t, err)

	got, err := repo.GetLatestByExternalID(ctx, src, externalID)
	require.NoError(t, err)
	require.NotNil(t, got.Body)
	assert.Equal(t, "latest body", *got.Body)
	assert.Equal(t, externalID, got.ExternalID)

	_, err = repo.GetLatestByExternalID(ctx, src, "spaces/AAAA/messages/missing-"+suffix)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

// TestBackfillParticipantNames_AdditiveMergeAndIdempotent pins the additive
// merge contract directly at the SQL level: BackfillParticipantNames ADDS the
// four display-name keys onto a name-less row while preserving ALL existing
// content + provenance keys, and a second call is a no-op (the NOT (?
// 'from_name') guard → 0 rows).
func TestBackfillParticipantNames_AdditiveMergeAndIdempotent(t *testing.T) {
	ctx, repo, contactRepo, _ := setupCommsMessageTest(t)

	suffix := randomSuffix(t)
	contact := newEmailContact(t, ctx, repo, contactRepo, "Test Backfill Names "+suffix)

	// Seed a name-less row carrying full content + provenance keys.
	externalID := "<msgid-backfill-" + suffix + ">"
	md := metadataFor(t, "accA", "gidA", map[string]any{
		"from":    "unknown@example.test",
		"to":      []string{"me@example.test"},
		"subject": "Original Subject",
		"html":    "<p>original</p>",
	})
	seeded, err := repo.UpsertMessage(ctx, baseUpsertParams(externalID, contact.ID, accelerated.GetCurrentTime().Truncate(time.Microsecond), "accA", "gidA", md))
	require.NoError(t, err)

	// First backfill ADDS the name keys.
	affected, err := repo.BackfillParticipantNames(ctx, seeded.ID, repository.ParticipantNames{
		FromName: "Unknown Person",
		ToNames:  []string{"Me"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	got, err := repo.GetByID(ctx, seeded.ID)
	require.NoError(t, err)
	merged := decodeMetadata(t, got.SourceMetadata)
	// Name keys added.
	require.Equal(t, "Unknown Person", merged["from_name"])
	require.Equal(t, []any{"Me"}, merged["to_names"])
	require.Contains(t, merged, "cc_names")
	require.Contains(t, merged, "bcc_names")
	// All pre-existing content + provenance keys preserved.
	require.Equal(t, "unknown@example.test", merged["from"])
	require.Equal(t, "Original Subject", merged["subject"])
	require.Equal(t, "<p>original</p>", merged["html"])
	require.Contains(t, merged, "observed_accounts")
	require.Contains(t, merged, "account_gmail_ids")

	// Second backfill is a no-op (idempotent — row already has from_name).
	affected2, err := repo.BackfillParticipantNames(ctx, seeded.ID, repository.ParticipantNames{
		FromName: "DIFFERENT Name",
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), affected2, "guard: a row that already has from_name is a no-op")

	got2, err := repo.GetByID(ctx, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, "Unknown Person", decodeMetadata(t, got2.SourceMetadata)["from_name"], "name not overwritten")
}

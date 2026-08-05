package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- harness ----------------------------------------------------------------

// stubGroupInfo is the group-metadata seam. Production reaches a live client;
// no test may.
type stubGroupInfo struct {
	info    *whatsapp.ChatGroupInfo
	err     error
	calls   int
	account string
}

func (s *stubGroupInfo) GroupInfo(context.Context, string) (*whatsapp.ChatGroupInfo, error) {
	s.calls++
	return s.info, s.err
}

// AccountJID reports which linked account this fetcher's client belongs to. The
// harness always answers with the account its projected messages carry, so the
// gate's re-pair guard never fires on an unrelated test.
func (s *stubGroupInfo) AccountJID() string { return s.account }

// injectedFailure wraps the real repository so ONE call can be made to fail
// without faking the rest of the write path.
type injectedFailure struct {
	*repository.CommsMessageRepository
	probeErr     error
	tombstoneErr error
	upsertErr    error
}

func (f *injectedFailure) HasMatchedChatMessage(ctx context.Context, source, externalID string) (bool, error) {
	if f.probeErr != nil {
		return false, f.probeErr
	}
	return f.CommsMessageRepository.HasMatchedChatMessage(ctx, source, externalID)
}

func (f *injectedFailure) SoftDeleteUnmatchedTwin(ctx context.Context, source, externalID string) (int64, error) {
	if f.tombstoneErr != nil {
		return 0, f.tombstoneErr
	}
	return f.CommsMessageRepository.SoftDeleteUnmatchedTwin(ctx, source, externalID)
}

func (f *injectedFailure) UpsertChatMessage(ctx context.Context, params repository.UpsertChatMessageParams) (*repository.CommsMessage, error) {
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	return f.CommsMessageRepository.UpsertChatMessage(ctx, params)
}

// commsChatStore mirrors the ingestor's consumer interface, which is
// package-private to whatsapp. Go interfaces are structural, so naming it here
// is only for the harness's own readability.
type commsChatStore interface {
	UpsertChatMessage(context.Context, repository.UpsertChatMessageParams) (*repository.CommsMessage, error)
	SoftDeleteUnmatchedTwin(context.Context, string, string) (int64, error)
	HasMatchedChatMessage(context.Context, string, string) (bool, error)
}

// ingestEnv is one test's view of the ingest path over the shared package DB.
// Every identifier it mints is namespace-scoped, so assertions only ever see
// this test's own rows.
type ingestEnv struct {
	ctx         context.Context
	comms       *repository.CommsMessageRepository
	contactRepo *repository.ContactRepository
	methodRepo  *repository.ContactMethodRepository
	externals   *repository.ExternalContactRepository
	waRepo      *repository.WhatsAppRepository
	identity    *service.IdentityService
	group       *stubGroupInfo
	matcher     *whatsapp.PeerMatcher
	ingestor    *whatsapp.Ingestor
	ns          string
}

func setupIngestTest(t *testing.T) *ingestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	// Registered first so it runs LAST (t.Cleanup is LIFO) — the row sweeps
	// below need the pool open.
	t.Cleanup(database.Close)

	env := &ingestEnv{
		ctx:         ctx,
		comms:       repository.NewCommsMessageRepository(database.Queries),
		contactRepo: repository.NewContactRepository(database.Queries),
		methodRepo:  repository.NewContactMethodRepository(database.Queries),
		externals:   repository.NewExternalContactRepository(database.Queries),
		waRepo:      repository.NewWhatsAppRepository(database.Queries),
		identity:    service.NewIdentityService(repository.NewIdentityRepository(database.Queries)),
		group:       &stubGroupInfo{info: &whatsapp.ChatGroupInfo{Title: "Book Club", MemberCount: 3}},
		ns:          syntheticNS(t),
	}
	env.group.account = env.accountJID()
	env.comms.SetPool(database.Pool)

	fixtures := repository.NewTestJSONBFixturesRepository(database.Queries)
	t.Cleanup(func() {
		// An unmatched row has no contact for the usual by-contact sweep to
		// reach, and 076's down migration refuses to revert while any
		// NULL-contact row survives.
		_ = env.comms.HardDeleteBySourceAndExternalIDPrefix(ctx,
			repository.InteractionSourceWhatsApp, env.extIDPrefix()+"%")
		_ = fixtures.DeleteExternalContactsBySourceIDPrefix(ctx, env.ns)
	})

	env.rebuild(env.comms)
	return env
}

// rebuild reconstructs the ingest path over a (possibly wrapped) comms store.
func (e *ingestEnv) rebuild(store commsChatStore) {
	gate := whatsapp.NewChatGate(e.waRepo, 10)
	gate.BindGroupInfoSource(func() whatsapp.GroupInfoFetcher { return e.group })
	e.matcher = whatsapp.NewPeerMatcher(e.identity, e.comms, e.externals, nil, nil, 2)
	e.ingestor = whatsapp.NewIngestor(store, gate, e.matcher)
}

func (e *ingestEnv) extIDPrefix() string { return "wa-" + e.ns + "-" }

func (e *ingestEnv) extID(n int) string { return fmt.Sprintf("%s%d", e.extIDPrefix(), n) }

func (e *ingestEnv) peerJID(name string) string { return e.ns + name + "@s.whatsapp.net" }

func (e *ingestEnv) lidJID(name string) string { return e.ns + name + "@lid" }

func (e *ingestEnv) groupJID() string { return e.ns + "group@g.us" }

// accountJID is the linked account every projected message in this test carries.
func (e *ingestEnv) accountJID() string { return e.ns + "own@s.whatsapp.net" }

// uniquePhone mints an E.164 no other test shares, so identity matching cannot
// collide with another test's contact across the shared package DB.
func (e *ingestEnv) uniquePhone(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("+1555%07d", uuid.New().ID()%10000000)
}

func (e *ingestEnv) newContact(t *testing.T, name string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name + " " + e.ns})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.comms.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.methodRepo.DeleteContactMethodsByContact(e.ctx, contact.ID)
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

func (e *ingestEnv) newContactWithPhone(t *testing.T, name, phone string) *repository.Contact {
	t.Helper()
	contact := e.newContact(t, name)
	_, err := e.methodRepo.CreateContactMethod(e.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID, Type: "phone", Value: phone,
	})
	require.NoError(t, err)
	return contact
}

func strPtr2(s string) *string { return &s }

// msg builds a projected message with inbound private-DM defaults.
func (e *ingestEnv) msg(externalID, peerJID string, opts ...func(*whatsapp.IngestedMessage)) whatsapp.IngestedMessage {
	account := e.accountJID()
	m := whatsapp.IngestedMessage{
		MessageID:   externalID,
		ChatJID:     peerJID,
		ChatType:    whatsapp.ChatTypePrivate,
		SentAt:      accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond),
		Body:        strPtr2("hello there"),
		MessageType: whatsapp.MessageTypeText,
		PeerJID:     &peerJID,
		AccountJID:  &account,
	}
	for _, o := range opts {
		o(&m)
	}
	return m
}

func withPhone(e164 string) func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) { m.PeerPhoneE164 = &e164 }
}
func withPush(name string) func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) { m.PushName = &name }
}
func withOutgoing() func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) { m.IsOutgoing = true }
}
func withGroupChat(chatJID string) func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) {
		m.ChatType = whatsapp.ChatTypeGroup
		m.ChatJID = chatJID
	}
}
func withNilPeer() func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) { m.PeerJID = nil; m.PeerPhoneE164 = nil }
}
func withReplyTo(id string) func(*whatsapp.IngestedMessage) {
	return func(m *whatsapp.IngestedMessage) { m.ReplyTargetID = &id }
}

// hasMatched reports whether the message is staged against a contact.
func (e *ingestEnv) hasMatched(t *testing.T, externalID string) bool {
	t.Helper()
	got, err := e.comms.HasMatchedChatMessage(e.ctx, repository.InteractionSourceWhatsApp, externalID)
	require.NoError(t, err)
	return got
}

// liveUnmatched counts a peer's LIVE unmatched staging rows. Every test's peer
// handle is namespace-scoped, so this is a count over that test's rows alone.
func (e *ingestEnv) liveUnmatched(t *testing.T, peerJID string) int64 {
	t.Helper()
	counts, err := e.comms.ListUnmatchedPeerCounts(e.ctx, repository.InteractionSourceWhatsApp, &peerJID, 1)
	require.NoError(t, err)
	if len(counts) == 0 {
		return 0
	}
	return counts[0].TotalCount
}

// storedRow reads the staged row for one external id.
func (e *ingestEnv) storedRow(t *testing.T, externalID string) *repository.CommsMessage {
	t.Helper()
	row, err := e.comms.GetLatestByExternalID(e.ctx, repository.InteractionSourceWhatsApp, externalID)
	require.NoError(t, err)
	return row
}

func (e *ingestEnv) noStoredRow(t *testing.T, externalID string) {
	t.Helper()
	_, err := e.comms.GetLatestByExternalID(e.ctx, repository.InteractionSourceWhatsApp, externalID)
	assert.ErrorIs(t, err, db.ErrNotFound, "nothing may be staged")
}

// --- staging ----------------------------------------------------------------

func TestIngest_DirectInboundMatchesKnownPhone(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	phone := env.uniquePhone(t)
	contact := env.newContactWithPhone(t, "Known Peer", phone)
	peer := env.peerJID("known")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPhone(phone))))

	row := env.storedRow(t, env.extID(1))
	require.NotNil(t, row.MatchedContactID)
	assert.Equal(t, contact.ID, *row.MatchedContactID,
		"a phone the CRM already knows binds the message to its contact at staging time")
	assert.Zero(t, env.liveUnmatched(t, peer))
}

func TestIngest_DirectInboundUnknownPeerStagesUnmatched(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.lidJID("unknown")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))

	row := env.storedRow(t, env.extID(1))
	assert.Nil(t, row.MatchedContactID, "an unknown peer is staged, not dropped")
	require.NotNil(t, row.PeerHandle)
	assert.Equal(t, peer, *row.PeerHandle)
	assert.EqualValues(t, 1, env.liveUnmatched(t, peer))
}

func TestIngest_DirectOutboundAttributedToPeer(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.peerJID("outbound")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withOutgoing())))

	row := env.storedRow(t, env.extID(1))
	assert.Equal(t, repository.InteractionDirectionOutbound, row.Direction)
	require.NotNil(t, row.PeerHandle)
	assert.Equal(t, peer, *row.PeerHandle, "an outbound DM is attributed to the recipient, not to me")
}

func TestIngest_GroupInboundAttributedToSender(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.peerJID("member")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), peer, withGroupChat(env.groupJID()))))

	row := env.storedRow(t, env.extID(1))
	require.NotNil(t, row.ThreadID)
	assert.Equal(t, env.groupJID(), *row.ThreadID, "the chat scope is the group")
	require.NotNil(t, row.PeerHandle)
	assert.Equal(t, peer, *row.PeerHandle, "the counterpart is the sender, not the group")
}

func TestIngest_GroupOutboundStagedWithNullContact(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)

	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("unused"), withGroupChat(env.groupJID()), withOutgoing(), withNilPeer())))

	row := env.storedRow(t, env.extID(1))
	assert.Nil(t, row.MatchedContactID, "there is no single counterpart to attribute it to")
	assert.Nil(t, row.PeerHandle)
}

func TestIngest_GroupAboveThresholdStagesNothing(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	env.group.info = &whatsapp.ChatGroupInfo{Title: "Big Group", MemberCount: 42}

	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("member"), withGroupChat(env.groupJID()))),
		"a resolved count above the threshold is a real decision, so the message is handled")

	env.noStoredRow(t, env.extID(1))

	cfg, err := env.waRepo.GetChatConfig(env.ctx, env.groupJID())
	require.NoError(t, err)
	require.NotNil(t, cfg.MemberCount)
	assert.EqualValues(t, 42, *cfg.MemberCount, "the resolution persists, so no further lookup is needed")
}

// TestIngest_UnresolvedGroupCountWithholdsAck is the P0 regression guard: an
// undecidable gate must return an ERROR — which withholds the ack — rather than
// nil, and must stage nothing.
func TestIngest_UnresolvedGroupCountWithholdsAck(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	env.group.info = nil
	env.group.err = context.DeadlineExceeded

	err := env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("member"), withGroupChat(env.groupJID())))
	require.Error(t, err, "acking here would drop the message irrecoverably")
	assert.ErrorIs(t, err, whatsapp.ErrChatGateUndecided)
	env.noStoredRow(t, env.extID(1))

	_, cfgErr := env.waRepo.GetChatConfig(env.ctx, env.groupJID())
	assert.ErrorIs(t, cfgErr, db.ErrNotFound, "a failed lookup records no false resolution")
}

func TestIngest_IgnoredGroupAcksWithoutStaging(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	_, err := env.waRepo.UpsertChatConfig(env.ctx, repository.WhatsAppChatConfig{
		ChatJID: env.groupJID(), ChatType: whatsapp.ChatTypeGroup, Status: whatsapp.ChatStatusIgnored,
	})
	require.NoError(t, err)

	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("member"), withGroupChat(env.groupJID()))),
		"an explicit ignore is a decision, so the message is handled")
	env.noStoredRow(t, env.extID(1))
	assert.Zero(t, env.group.calls, "an override needs no member count")
}

func TestIngest_SourceMetadataCarriesReplyTarget(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.peerJID("replier")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), peer, withReplyTo("ORIG-9"), withPush("Their Name"))))

	row := env.storedRow(t, env.extID(1))
	var meta map[string]any
	require.NoError(t, json.Unmarshal(row.SourceMetadata, &meta))
	assert.Equal(t, "ORIG-9", meta["reply_target_external_id"],
		"this is the key the aggregation adapter already reads")
	assert.Equal(t, "Their Name", meta["push_name"])
	assert.Equal(t, whatsapp.MessageTypeText, meta["message_type"])
	assert.Equal(t, whatsapp.ChatTypePrivate, meta["chat_type"])
	assert.Nil(t, row.Snippet, "the snippet stays nil: nothing on this route reads it")
	require.NotNil(t, row.AccountID)
	assert.Equal(t, env.accountJID(), *row.AccountID)
}

func TestIngest_RedeliveryIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.peerJID("redeliver")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))

	assert.EqualValues(t, 1, env.liveUnmatched(t, peer),
		"a redelivery must not double-count a conversation turn")
}

// TestIngest_RedeliveryAfterPeerBecomesMatchedStagesOneRow is the matched-wins
// rule forwards. The two upserts have DISJOINT conflict targets, so without the
// tombstone the flip strands a permanent unmatched duplicate.
func TestIngest_RedeliveryAfterPeerBecomesMatchedStagesOneRow(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	phone := env.uniquePhone(t)
	contact := env.newContactWithPhone(t, "Flips To Matched", phone)
	peer := env.lidJID("flip")

	// Staged while the peer's phone was still unresolvable.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))
	require.EqualValues(t, 1, env.liveUnmatched(t, peer))

	// The same message redelivered once the phone resolved.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPhone(phone))))

	assert.True(t, env.hasMatched(t, env.extID(1)), "the matched row is the survivor")
	assert.Zero(t, env.liveUnmatched(t, peer), "the unmatched twin must be tombstoned, not left forever")

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "exactly one live row for one conversation turn")
}

// TestIngest_RedeliveryAfterMatchFailureDoesNotStageAnUnmatchedTwin is the same
// rule in REVERSE: matching is best-effort, so a redelivery of an
// already-matched message can take the unmatched path and mint the duplicate
// pair with nothing to tombstone it.
func TestIngest_RedeliveryAfterMatchFailureDoesNotStageAnUnmatchedTwin(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	phone := env.uniquePhone(t)
	contact := env.newContactWithPhone(t, "Match Then Fail", phone)
	peer := env.lidJID("reverse")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPhone(phone))))

	// Redelivered with no phone: MatchPeer yields nil, so the unmatched path
	// would run — and the probe is what declines it.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))

	assert.True(t, env.hasMatched(t, env.extID(1)))
	assert.Zero(t, env.liveUnmatched(t, peer), "no duplicate pair may be minted")

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	candidate, err := env.externals.GetBySource(env.ctx, repository.InteractionSourceWhatsApp, peer, nil)
	require.NoError(t, err)
	assert.Nil(t, candidate, "and discovery must not fire for a peer that already has a contact")
}

func TestIngest_TwinTombstoneFailureStillAcks(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	phone := env.uniquePhone(t)
	env.newContactWithPhone(t, "Tombstone Fails", phone)
	peer := env.peerJID("tombstonefail")

	env.rebuild(&injectedFailure{CommsMessageRepository: env.comms, tombstoneErr: errors.New("database down")})

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPhone(phone))),
		"failing to tombstone a duplicate must not withhold an ack for a message that IS stored")
	assert.True(t, env.hasMatched(t, env.extID(1)))
}

func TestIngest_MatchedProbeFailureStillStages(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.lidJID("probefail")

	env.rebuild(&injectedFailure{CommsMessageRepository: env.comms, probeErr: errors.New("database down")})

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)),
		"the worst case is a reconcilable duplicate, which is better than losing the message")
	assert.EqualValues(t, 1, env.liveUnmatched(t, peer))
}

func TestIngest_StagingErrorPropagates(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	env.rebuild(&injectedFailure{CommsMessageRepository: env.comms, upsertErr: errors.New("database down")})

	err := env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), env.peerJID("stagefail")))
	require.Error(t, err, "a no-op that succeeds is the shape that loses data")
	env.noStoredRow(t, env.extID(1))
}

func TestIngest_EmptyChatScopeIsRefused(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	msg := env.msg(env.extID(1), env.peerJID("nothread"))
	msg.ChatJID = ""

	err := env.ingestor.IngestMessage(env.ctx, msg)
	require.Error(t, err, "a row with no chat scope is staged, attachable, and permanently unaggregatable")
	assert.ErrorIs(t, err, repository.ErrChatMessageMissingThread)
}

// --- discovery + link -------------------------------------------------------

func TestWhatsAppDiscovery_CandidateAppearsAtThreshold(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.lidJID("discover")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPush("Their Name"))))
	candidate, err := env.externals.GetBySource(env.ctx, repository.InteractionSourceWhatsApp, peer, nil)
	require.NoError(t, err)
	assert.Nil(t, candidate, "one message is below the threshold of two")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(2), peer, withPush("Their Name"))))
	candidate, err = env.externals.GetBySource(env.ctx, repository.InteractionSourceWhatsApp, peer, nil)
	require.NoError(t, err)
	require.NotNil(t, candidate, "a frequent unknown peer surfaces as an import candidate")
	require.NotNil(t, candidate.DisplayName)
	assert.Equal(t, "Their Name", *candidate.DisplayName)
	assert.Equal(t, peer, candidate.SourceID, "keyed by the raw peer JID, like the staging rows")
	assert.EqualValues(t, 2, candidate.Metadata["message_count"])
}

// TestWhatsAppDiscovery_LIDOnlyPeerIsStillLabelled is what makes the
// always-visible choice safe: the peers most at risk of being hidden — LID-only,
// push-name-less — are exactly the ones the user cannot reach any other way, so
// their candidate must never be contentless.
func TestWhatsAppDiscovery_LIDOnlyPeerIsStillLabelled(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	peer := env.lidJID("nameless")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(2), peer)))

	candidate, err := env.externals.GetBySource(env.ctx, repository.InteractionSourceWhatsApp, peer, nil)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	require.NotNil(t, candidate.DisplayName, "never contentless")
	assert.Contains(t, *candidate.DisplayName, "WhatsApp ")
}

func TestWhatsAppOnPeerLinked_AttachesEveryUnmatchedRow(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	contact := env.newContact(t, "Linked Later")
	peer := env.lidJID("linklater")

	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer)))
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(2), peer)))

	require.NoError(t, env.matcher.OnPeerLinked(env.ctx, peer, nil, contact.ID))

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "linking a peer retroactively binds their stored messages")
	assert.Zero(t, env.liveUnmatched(t, peer))
}

// TestWhatsAppOnPeerLinked_ReconcilesDuplicateUnmatchedRow covers the case the
// attach's own dedup statement exists for: a matched row for the same message
// already exists, so the unmatched twin has to be tombstoned rather than
// attached — attaching it would violate the dedup index.
func TestWhatsAppOnPeerLinked_ReconcilesDuplicateUnmatchedRow(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	contact := env.newContact(t, "Dup Reconcile")
	peer := env.lidJID("dupe")
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	base := repository.UpsertChatMessageParams{
		Source: repository.InteractionSourceWhatsApp, ExternalID: env.extID(1), ThreadID: peer,
		PeerHandle: &peer, Direction: repository.InteractionDirectionInbound, SentAt: sentAt,
	}
	// One message staged BOTH ways — the coexistence the two disjoint conflict
	// targets make representable.
	_, err := env.comms.UpsertChatMessage(env.ctx, base)
	require.NoError(t, err)
	matched := base
	matched.MatchedContactID = &contact.ID
	_, err = env.comms.UpsertChatMessage(env.ctx, matched)
	require.NoError(t, err)
	require.EqualValues(t, 1, env.liveUnmatched(t, peer), "the unmatched twin is live")

	require.NoError(t, env.matcher.OnPeerLinked(env.ctx, peer, nil, contact.ID))

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "the duplicate is reconciled, not attached")
	assert.Zero(t, env.liveUnmatched(t, peer))
}

func TestWhatsAppOnPeerLinked_AttachesByPhoneWhenStagedUnderALID(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	contact := env.newContact(t, "Phone Attach")
	peer := env.lidJID("phoneattach")
	phone := env.uniquePhone(t)

	// Staged under the LID handle, but with the phone resolved on the row.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx, env.msg(env.extID(1), peer, withPhone(phone))))

	// Linking a candidate whose HANDLE is a different string still finds it,
	// because the attach matches peer_handle OR peer_normalized. This is what
	// the extra phone parameter exists for.
	require.NoError(t, env.matcher.OnPeerLinked(env.ctx, env.lidJID("other"), &phone, contact.ID))

	rows, err := env.comms.ListByContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// --- the gate's durability --------------------------------------------------

// TestGroupGate_PersistsAcrossRestart is why the gate is a TABLE rather than an
// in-memory map: a resolved size survives a process restart, so a restarted
// backend does not re-ask the server for every group on every message.
func TestGroupGate_PersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)

	// First process: resolves and persists.
	require.NoError(t, env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("member"), withGroupChat(env.groupJID()))))
	require.Equal(t, 1, env.group.calls)
	require.True(t, env.hasMatched(t, env.extID(1)) || env.liveUnmatched(t, env.peerJID("member")) == 1,
		"the message was stored")

	// Second process: a brand-new gate over the same table, whose lookup would
	// FAIL if it were reached at all.
	restarted := whatsapp.NewChatGate(env.waRepo, 10)
	restarted.BindGroupInfoSource(func() whatsapp.GroupInfoFetcher {
		return &stubGroupInfo{account: env.accountJID(), err: errors.New("must not be reached after a restart")}
	})

	tracked, err := restarted.ShouldTrack(env.ctx, env.groupJID(), whatsapp.ChatTypeGroup, env.accountJID())
	require.NoError(t, err, "the persisted count answers without a lookup")
	assert.True(t, tracked)
}

// TestIngest_MissingMessageIDIsRefused is the symmetric twin of the staging
// layer's missing-thread refusal. The live parser always supplies an id, but the
// history drainer builds IngestedMessage by hand, and a row with no id is
// staged and then permanently unreachable — dedup, the twin reconciliation and
// the reply bridge all key on it.
func TestIngest_MissingMessageIDIsRefused(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	msg := env.msg(env.extID(1), env.peerJID("noid"))
	msg.MessageID = ""

	err := env.ingestor.IngestMessage(env.ctx, msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, whatsapp.ErrIngestMissingMessageID)
	assert.Zero(t, env.liveUnmatched(t, env.peerJID("noid")), "and nothing is staged")
}

// TestIngest_GroupMessageFromAnotherAccountWithholdsAck is the re-pair guard at
// the ingest boundary: the connected client belongs to a different account than
// the one that observed the message, so its "not in that group" answer is not
// this account's answer and must not be consumed.
func TestIngest_GroupMessageFromAnotherAccountWithholdsAck(t *testing.T) {
	t.Parallel()
	env := setupIngestTest(t)
	env.group.account = "15559999999@s.whatsapp.net"
	env.group.info = nil
	env.group.err = errors.New("status 403")

	err := env.ingestor.IngestMessage(env.ctx,
		env.msg(env.extID(1), env.peerJID("member"), withGroupChat(env.groupJID())))
	require.Error(t, err, "acking a wrong-account answer would drop the message for good")
	assert.ErrorIs(t, err, whatsapp.ErrChatGateUndecided)
	assert.Zero(t, env.group.calls, "and the wrong client is never asked")
	env.noStoredRow(t, env.extID(1))
}

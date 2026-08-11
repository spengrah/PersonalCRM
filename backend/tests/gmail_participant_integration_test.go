// Integration coverage for the trust-anchored gmail_participant discovery
// gate on BOTH discovery seams (the in-sync Sync sweep and the
// ScanIdentifier rematch backfill scan). Drives the REAL GmailSyncProvider
// against a real *events.Bus + database.Pool + a FAKE gmailFetcher (no OAuth/
// HTTP), reusing the discoveryEnv harness from
// gmail_correspondence_integration_test.go (same package). Proves:
//   - the Sync seam produces a live gmail_participant candidate with the
//     §5.2 metadata, and re-syncing the same mail is idempotent (the message
//     is provably re-evaluated, but the candidate row count/id/match_status
//     stay stable);
//   - the ScanIdentifier (rematch) seam produces an identical candidate shape
//     to the Sync seam;
//   - the own-domain config never leaks into the storage gate's meSet: a
//     message from an own-domain sender (a third party from the storage
//     gate's point of view — own-domain addresses are never registered
//     contact_methods) still yields zero comms_message rows for an unrelated
//     known co-recipient, proving the own-domain flag never reaches
//     processMessage's meSet (a leak would manufacture a phantom OUTBOUND
//     row for that co-recipient, since fromIsMe would wrongly become true);
//   - a participant-upsert failure is non-fatal to Sync (logged, not
//     returned), mirroring TestDiscovery_ErrorNonFatalToSync for the link
//     path.
package tests

import (
	"testing"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// cleanupExternalParticipant hard-deletes a produced gmail_participant
// candidate at test end. Belt-and-braces on the per-test DB clone (the DB is
// dropped on t.Cleanup either way).
func cleanupExternalParticipant(t *testing.T, e *discoveryEnv, sourceID string) {
	t.Helper()
	t.Cleanup(func() {
		row, _ := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, sourceID, nil)
		if row != nil {
			_ = e.externalRepo.Delete(e.ctx, row.ID)
		}
	})
}

// 1. Sync seam + idempotency.
func TestParticipantDiscovery_SyncSeamAndIdempotent(t *testing.T) {
	// spec: IMP-042
	t.Parallel()
	e := newDiscoveryEnv(t)
	e.wireDiscoverer()
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newKnownContact(t) // trusted sender
	guestAddr := prefix + "guest@synthetic.example"
	guestNorm := matching.NormalizeEmail(guestAddr)
	cleanupExternalParticipant(t, e, guestNorm)
	e.cleanupEvents(t, prefix+"partidem")

	msg := gmailMsg("g-partidem-"+prefix, "thr-"+prefix, addrA,
		[]string{guestAddr}, nil, nil,
		"Kickoff", "body", "<"+prefix+"partidem@synthetic.example>", 1700000100000)
	store := newFakeMessageStore([]*gmailapi.Message{msg})
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(store.fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, discoverySyncState(me), nil)
	require.NoError(t, err)

	row, err := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, guestNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the Sync seam must produce a gmail_participant candidate")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	// §5.2 metadata (post round-trip through JSONB, so numbers are float64).
	require.Equal(t, float64(1), row.Metadata["message_count"])
	trustedSender, ok := row.Metadata["trusted_sender"].(map[string]any)
	require.True(t, ok, "trusted_sender present")
	require.Equal(t, addrA, trustedSender["address"])
	require.Equal(t, contactA.FullName, trustedSender["name"])
	require.Equal(t, "Kickoff", row.Metadata["anchor_subject"])

	firstID := row.ID
	firstGetCalls := store.getCalls[msg.Id]
	require.GreaterOrEqual(t, firstGetCalls, 1, "the message was actually fetched")

	// Idempotency leg: re-run Sync with a FRESH state carrying the SAME
	// backfill_since floor (no cursor forwarded) — this resets the scan to
	// the original window, so the same message must be re-listed and
	// re-fetched (proven via the fetcher call count increasing), yet the
	// candidate must not duplicate.
	_, err = e.provider.Sync(e.ctx, discoverySyncState(me), nil)
	require.NoError(t, err)
	require.Greater(t, store.getCalls[msg.Id], firstGetCalls,
		"the second Sync must re-fetch (and so re-evaluate) the same message")

	row2, err := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, guestNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row2)
	require.Equal(t, firstID, row2.ID, "stable row id across re-syncs")
	require.Equal(t, repository.MatchStatusUnmatched, row2.MatchStatus, "match_status untouched by re-sync")
}

// 2. ScanIdentifier (rematch backfill) seam produces an identical candidate
// shape to the Sync seam.
func TestParticipantDiscovery_RematchSeamIdentical(t *testing.T) {
	// spec: IMP-042
	t.Parallel()
	e := newDiscoveryEnv(t)
	e.wireDiscoverer()
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newKnownContact(t)
	addrANorm := matching.NormalizeEmail(addrA)
	guestAddr := prefix + "guest@synthetic.example"
	guestNorm := matching.NormalizeEmail(guestAddr)
	cleanupExternalParticipant(t, e, guestNorm)

	msg := gmailMsg("g-partrematch-"+prefix, "thr-"+prefix, addrA,
		[]string{guestAddr}, nil, nil,
		"Kickoff", "body", "<"+prefix+"partrematch@synthetic.example>", 1700000100000)
	store := newFakeMessageStore([]*gmailapi.Message{msg})
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(store.fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	knownMap := map[string][]uuid.UUID{addrANorm: {contactA.ID}}
	matched, err := e.provider.ScanIdentifier(e.ctx, me, guestNorm, knownMap,
		map[string]struct{}{me: {}}, map[string]struct{}{}, 1700000000)
	require.NoError(t, err)
	require.Equal(t, 0, matched, "storage gate: guest is not a known contact, no row qualifies")

	row, err := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, guestNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the rematch seam must produce identically to the Sync seam")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	require.Equal(t, float64(1), row.Metadata["message_count"])
	trustedSender, ok := row.Metadata["trusted_sender"].(map[string]any)
	require.True(t, ok, "trusted_sender present")
	require.Equal(t, addrANorm, trustedSender["address"])
	require.Equal(t, contactA.FullName, trustedSender["name"])
	require.Equal(t, "Kickoff", row.Metadata["anchor_subject"])
}

// 3. The own-domain config never reaches the storage gate's meSet.
//
// From is an own-domain address that is NOT itself a registered
// contact_method of anyone (a "wildcard" alias — own-domain addresses anchor
// discovery trust but are never contacts). A KNOWN contact is a mere
// CO-RECIPIENT of this message, unrelated to the sender. Under correct
// behavior contact A is a bystander for THIS message (zero comms_message
// rows): the sender isn't contact A's own address (fails the inbound check),
// and the sender is not a real connected account (fails the outbound check —
// this is exactly what "own-domain must never reach meSet" guarantees). If
// own-domain leaked into the meSet passed to processMessage, the sender would
// wrongly satisfy fromIsMe, and contact A (present in the recipient set)
// would get a PHANTOM outbound row — the regression this test pins.
func TestParticipantDiscovery_OwnDomainConfigAndStorageGateUnaffected(t *testing.T) {
	// spec: IMP-049
	t.Parallel()
	e := newDiscoveryEnv(t)
	e.wireDiscoverer()
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	ownDomain := prefix + "own-domain.example"
	aliasAddr := "alias@" + ownDomain
	contactA, addrA := e.newKnownContact(t) // an UNRELATED known contact, mere co-recipient
	guestAddr := prefix + "guest@synthetic.example"
	guestNorm := matching.NormalizeEmail(guestAddr)
	cleanupExternalParticipant(t, e, guestNorm)
	e.cleanupEvents(t, prefix+"partdom")

	msg := gmailMsg("g-partdom-"+prefix, "thr-"+prefix, aliasAddr,
		[]string{addrA, guestAddr}, nil, nil,
		"Own-domain subj", "body", "<"+prefix+"partdom@synthetic.example>", 1700000100000)
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(
		newFakeMessageStore([]*gmailapi.Message{msg}).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	state := discoverySyncState(me)
	state.Metadata["discovery_own_domains"] = []any{ownDomain}

	result, err := e.provider.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// (a) the own-domain-anchored participant candidate exists.
	row, err := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, guestNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "own-domain sender anchors trust → participant candidate created")
	trustedSender, ok := row.Metadata["trusted_sender"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, trustedSender["self"], "own-domain sender is trusted_sender.self")

	// (b) the storage gate is UNCHANGED: contact A (co-recipient, unrelated to
	// the own-domain sender) gets ZERO comms_message rows — no phantom
	// outbound row from an own-domain-to-meSet leak.
	require.Equal(t, 0, result.ItemsMatched, "no storage-gate row for this message at all")
	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows, "own-domain must never reach meSet/processMessage — no phantom row for the co-recipient")
}

// 4. A participant-upsert failure is non-fatal to Sync: logged, never
// returned, cursor still advances, and the storage-gate-passing message is
// still stored — mirroring TestDiscovery_ErrorNonFatalToSync for the link
// path but exercising the participant upsert specifically.
func TestParticipantDiscovery_ErrorNonFatalToSync(t *testing.T) {
	// spec: IMP-042
	t.Parallel()
	e := newDiscoveryEnv(t)
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	contactA, addrA := e.newKnownContact(t)
	guestAddr := prefix + "guest@synthetic.example"
	guestNorm := matching.NormalizeEmail(guestAddr)
	cleanupExternalParticipant(t, e, guestNorm)
	e.cleanupEvents(t, prefix+"partfail")
	e.cleanupEvents(t, prefix+"partstore")

	failExt := &failingUpsertExternal{
		inner:  e.externalRepo,
		failOn: map[string]struct{}{guestNorm: {}},
	}
	e.provider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.contactRepo, failExt))

	// Message 1: trust-anchored (From = known contact A), To = unknown guest.
	// Storage gate misses (guest isn't "me"); discovery attempts a
	// participant upsert that fails.
	discMsg := gmailMsg("g-partfail-"+prefix, "thr-f-"+prefix, addrA,
		[]string{guestAddr}, nil, nil,
		"Subj", "body", "<"+prefix+"partfail@synthetic.example>", 1700000100000)
	// Message 2: a clean you↔contact message that DOES pass the storage gate.
	storeMsg := gmailMsg("g-partstore-"+prefix, "thr-s-"+prefix, addrA,
		[]string{me}, nil, nil,
		"Stored", "stored body", "<"+prefix+"partstore@synthetic.example>", 1700000200000)

	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(
		newFakeMessageStore([]*gmailapi.Message{discMsg, storeMsg}).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, discoverySyncState(me), nil)

	require.NoError(t, err, "a participant discovery error must be logged, not returned from Sync")
	require.NotEmpty(t, result.NewCursor, "cursor must advance — the discovery failure must not rewind it")
	require.GreaterOrEqual(t, result.ItemsMatched, 1, "the clean you↔contact message must still be stored")

	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.NotEmpty(t, aRows, "the clean inbound message is stored despite the discovery failure")

	row, err := e.externalRepo.GetBySource(e.ctx, google.ParticipantSource, guestNorm, nil)
	require.NoError(t, err)
	require.Nil(t, row, "the failing discovery upsert produced no participant candidate")
}

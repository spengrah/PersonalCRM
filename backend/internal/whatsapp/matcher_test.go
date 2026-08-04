package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ------------------------------------------------------------------

type fakeIdentityMatcher struct {
	contactID *uuid.UUID
	err       error
	requests  []service.MatchRequest
}

func (f *fakeIdentityMatcher) MatchOrCreate(_ context.Context, req service.MatchRequest) (*service.MatchResult, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return &service.MatchResult{ContactID: f.contactID, MatchType: repository.MatchTypeExact}, nil
}

type fakeCommsPeerStore struct {
	counts       []repository.UnmatchedPeerCount
	countsErr    error
	attachErr    error
	attachCalls  []attachCall
	countsCalls  []countsCall
	attached     int64
	dedupedCount int64
}

type attachCall struct {
	source         string
	peerNormalized *string
	peerHandle     *string
	contactID      uuid.UUID
}

type countsCall struct {
	source      string
	peerHandle  *string
	minMessages int
}

func (f *fakeCommsPeerStore) AttachUnmatchedByPeer(_ context.Context, source string, peerNormalized, peerHandle *string, contactID uuid.UUID) (int64, int64, error) {
	f.attachCalls = append(f.attachCalls, attachCall{source, peerNormalized, peerHandle, contactID})
	if f.attachErr != nil {
		return 0, 0, f.attachErr
	}
	return f.attached, f.dedupedCount, nil
}

func (f *fakeCommsPeerStore) ListUnmatchedPeerCounts(_ context.Context, source string, peerHandle *string, minMessages int) ([]repository.UnmatchedPeerCount, error) {
	f.countsCalls = append(f.countsCalls, countsCall{source, peerHandle, minMessages})
	return f.counts, f.countsErr
}

type fakeExternalContactUpserter struct {
	upserts      []repository.UpsertDiscoveryCandidateRequest
	getResult    *repository.ExternalContact
	getErr       error
	updateCalls  int
	upsertErrOne error
}

func (f *fakeExternalContactUpserter) UpsertDiscoveryCandidate(_ context.Context, req repository.UpsertDiscoveryCandidateRequest) (*repository.ExternalContact, error) {
	f.upserts = append(f.upserts, req)
	if f.upsertErrOne != nil {
		return nil, f.upsertErrOne
	}
	return &repository.ExternalContact{ID: uuid.New(), Source: req.Source, SourceID: req.SourceID}, nil
}

func (f *fakeExternalContactUpserter) GetBySource(_ context.Context, _, _ string, _ *string) (*repository.ExternalContact, error) {
	return f.getResult, f.getErr
}

func (f *fakeExternalContactUpserter) UpdateMatch(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ repository.MatchStatus) (*repository.ExternalContact, error) {
	f.updateCalls++
	return nil, nil
}

type fakeEnricher struct {
	calls    int
	external *repository.ExternalContact
	err      error
}

func (f *fakeEnricher) SyncMethodsFromExternal(_ context.Context, _ uuid.UUID, external *repository.ExternalContact) error {
	f.calls++
	f.external = external
	return f.err
}

func strp(s string) *string { return &s }

// --- MatchPeer --------------------------------------------------------------

func TestMatchPeer_ResolvedPhoneMatchesContact(t *testing.T) {
	want := uuid.New()
	ids := &fakeIdentityMatcher{contactID: &want}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(ids, &fakeCommsPeerStore{}, ext, nil, 3)

	got, err := m.MatchPeer(context.Background(), "15559876543@s.whatsapp.net", strp("+15559876543"), strp("Their Name"))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)

	require.Len(t, ids.requests, 1, "one call covers both whatsapp and phone contact methods")
	assert.Equal(t, identity.IdentifierTypeWhatsApp, ids.requests[0].Type)
	assert.Equal(t, "+15559876543", ids.requests[0].RawIdentifier)
	assert.Equal(t, "whatsapp", ids.requests[0].Source)
	require.NotNil(t, ids.requests[0].SourceID)
	assert.Equal(t, "15559876543@s.whatsapp.net", *ids.requests[0].SourceID, "keyed by the raw peer JID")
	require.NotNil(t, ids.requests[0].DisplayName)
	assert.Equal(t, "Their Name", *ids.requests[0].DisplayName)
}

func TestMatchPeer_NilPhoneIsUnmatchedWithoutIdentityCall(t *testing.T) {
	ids := &fakeIdentityMatcher{}
	m := NewPeerMatcher(ids, &fakeCommsPeerStore{}, &fakeExternalContactUpserter{}, nil, 3)

	got, err := m.MatchPeer(context.Background(), "88800000002@lid", nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Empty(t, ids.requests, "a LID-only peer has no identifier worth minting")
}

func TestMatchPeer_IdentityErrorIsNonFatal(t *testing.T) {
	ids := &fakeIdentityMatcher{err: errors.New("identity service down")}
	m := NewPeerMatcher(ids, &fakeCommsPeerStore{}, &fakeExternalContactUpserter{}, nil, 3)

	got, err := m.MatchPeer(context.Background(), "15559876543@s.whatsapp.net", strp("+15559876543"), nil)
	require.NoError(t, err, "matching is best-effort; the message still stages")
	assert.Nil(t, got)
}

func TestMatchPeer_FiresEnricherOnMatch(t *testing.T) {
	want := uuid.New()
	ids := &fakeIdentityMatcher{contactID: &want}
	ext := &fakeExternalContactUpserter{getResult: &repository.ExternalContact{
		ID: uuid.New(), Source: "whatsapp", SourceID: "15559876543@s.whatsapp.net",
		MatchStatus: repository.MatchStatusUnmatched, Metadata: map[string]any{},
	}}
	enricher := &fakeEnricher{}
	m := NewPeerMatcher(ids, &fakeCommsPeerStore{}, ext, enricher, 3)

	_, err := m.MatchPeer(context.Background(), "15559876543@s.whatsapp.net", strp("+15559876543"), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, enricher.calls)
	require.NotNil(t, enricher.external)
	assert.Equal(t, "+15559876543", enricher.external.Metadata["phone_e164"],
		"the current message is the freshest source of the peer's number")
	assert.Equal(t, 1, ext.updateCalls, "an existing unmatched candidate is marked matched")
}

func TestMatchPeer_NilEnricherNoPanic(t *testing.T) {
	want := uuid.New()
	m := NewPeerMatcher(&fakeIdentityMatcher{contactID: &want}, &fakeCommsPeerStore{}, &fakeExternalContactUpserter{}, nil, 3)

	got, err := m.MatchPeer(context.Background(), "15559876543@s.whatsapp.net", strp("+15559876543"), nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- OnPeerLinked -----------------------------------------------------------

func TestOnPeerLinked_PassesBothSelectors(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCommsPeerStore{attached: 4, dedupedCount: 1}
	ids := &fakeIdentityMatcher{contactID: &contactID}
	m := NewPeerMatcher(ids, comms, &fakeExternalContactUpserter{}, nil, 3)

	require.NoError(t, m.OnPeerLinked(context.Background(), "88800000002@lid", strp("+15559876543"), contactID))

	require.Len(t, comms.attachCalls, 1)
	assert.Equal(t, "whatsapp", comms.attachCalls[0].source)
	require.NotNil(t, comms.attachCalls[0].peerHandle)
	assert.Equal(t, "88800000002@lid", *comms.attachCalls[0].peerHandle)
	require.NotNil(t, comms.attachCalls[0].peerNormalized)
	assert.Equal(t, "+15559876543", *comms.attachCalls[0].peerNormalized,
		"a peer staged under a LID before its phone resolved is attachable only by the phone")

	require.Len(t, ids.requests, 1)
	require.NotNil(t, ids.requests[0].KnownContactID)
	assert.Equal(t, contactID, *ids.requests[0].KnownContactID)
}

func TestOnPeerLinked_NilPhoneReproducesTheContractedShape(t *testing.T) {
	contactID := uuid.New()
	comms := &fakeCommsPeerStore{}
	ids := &fakeIdentityMatcher{}
	m := NewPeerMatcher(ids, comms, &fakeExternalContactUpserter{}, nil, 3)

	require.NoError(t, m.OnPeerLinked(context.Background(), "88800000002@lid", nil, contactID))
	assert.Empty(t, ids.requests, "with no phone there is nothing to link the identity by")
	require.Len(t, comms.attachCalls, 1)
	assert.Nil(t, comms.attachCalls[0].peerNormalized)
}

func TestOnPeerLinked_AttachErrorIsFatal(t *testing.T) {
	comms := &fakeCommsPeerStore{attachErr: errors.New("database down")}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, &fakeExternalContactUpserter{}, nil, 3)

	err := m.OnPeerLinked(context.Background(), "88800000002@lid", nil, uuid.New())
	require.Error(t, err, "the attach IS the user-visible effect of linking; a silent failure loses it")
}

func TestOnPeerLinked_IdentityLinkFailureIsNonFatal(t *testing.T) {
	comms := &fakeCommsPeerStore{attached: 2}
	ids := &fakeIdentityMatcher{err: errors.New("identity service down")}
	m := NewPeerMatcher(ids, comms, &fakeExternalContactUpserter{}, nil, 3)

	require.NoError(t, m.OnPeerLinked(context.Background(), "88800000002@lid", strp("+15559876543"), uuid.New()),
		"the identity link is a convenience for future matches; the attach still has to run")
	assert.Len(t, comms.attachCalls, 1)
}

// --- discovery --------------------------------------------------------------

func peerCount(handle string, total int64, normalized, pushName *string) repository.UnmatchedPeerCount {
	return repository.UnmatchedPeerCount{
		PeerHandle:     handle,
		PeerNormalized: normalized,
		TotalCount:     total,
		InboundCount:   total,
		LastMessageAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		LastPushName:   pushName,
	}
}

func TestDiscovery_ThresholdIsAppliedByTheQuery(t *testing.T) {
	comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{
		peerCount("15559876543@s.whatsapp.net", 3, strp("+15559876543"), strp("Their Name")),
	}}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), strp("15559876543@s.whatsapp.net")))

	require.Len(t, comms.countsCalls, 1)
	assert.Equal(t, 3, comms.countsCalls[0].minMessages,
		"the HAVING clause applies the threshold, so there is no client-side filter to keep in step")
	assert.Equal(t, "whatsapp", comms.countsCalls[0].source)
	require.Len(t, ext.upserts, 1)
}

func TestDiscovery_BelowThresholdNoCandidate(t *testing.T) {
	// The query returns nothing below the threshold; the matcher must not
	// invent a candidate from an empty result.
	comms := &fakeCommsPeerStore{}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))
	assert.Empty(t, ext.upserts)
}

func TestDiscovery_MetadataCarriesCountsAndPhone(t *testing.T) {
	row := peerCount("15559876543@s.whatsapp.net", 5, strp("+15559876543"), strp("Their Name"))
	row.OutboundCount = 2
	row.InboundCount = 3
	comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{row}}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))
	require.Len(t, ext.upserts, 1)
	meta := ext.upserts[0].Metadata
	assert.EqualValues(t, 5, meta["message_count"])
	assert.EqualValues(t, 2, meta["outbound_count"])
	assert.EqualValues(t, 3, meta["inbound_count"])
	assert.Equal(t, "+15559876543", meta["phone_e164"],
		"the resolved phone rides in metadata, which is what the import method builder reads")
	assert.Equal(t, "Their Name", meta["push_name"])
	assert.Equal(t, "15559876543@s.whatsapp.net", meta["peer_jid"])
	assert.Equal(t, "2026-05-01T12:00:00Z", meta["last_message_at"])
	assert.Equal(t, "whatsapp", ext.upserts[0].Source)
	assert.Equal(t, "15559876543@s.whatsapp.net", ext.upserts[0].SourceID,
		"the candidate is keyed by the same value the staging rows and the attach use")
	assert.Nil(t, ext.upserts[0].FirstName, "WhatsApp gives a push name, not a split name")
	assert.Nil(t, ext.upserts[0].LastName)
}

// TestDiscovery_AlwaysWritesADisplayName is the guarantee that makes
// always-visible WhatsApp candidates safe: the imports queue's unresolved-hiding
// predicates are Telegram-scoped, so a contentless WhatsApp row would reach the
// user as a bare JID with nothing to act on.
func TestDiscovery_AlwaysWritesADisplayName(t *testing.T) {
	tests := []struct {
		name string
		row  repository.UnmatchedPeerCount
		want string
	}{
		{"push name wins", peerCount("15559876543@s.whatsapp.net", 3, strp("+15559876543"), strp("Their Name")), "Their Name"},
		{"phone next", peerCount("15559876543@s.whatsapp.net", 3, strp("+15559876543"), nil), "+15559876543"},
		{"jid label last", peerCount("88800000002@lid", 3, nil, nil), "WhatsApp 88800000002"},
		{"an empty push name is not a name", peerCount("88800000002@lid", 3, nil, strp("")), "WhatsApp 88800000002"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{tt.row}}
			ext := &fakeExternalContactUpserter{}
			m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

			require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))
			require.Len(t, ext.upserts, 1)
			require.NotNil(t, ext.upserts[0].DisplayName, "a WhatsApp candidate is never contentless")
			assert.Equal(t, tt.want, *ext.upserts[0].DisplayName)
		})
	}
}

// TestDiscovery_LaterPushNameUpgradesJIDLabel pins the ladder's monotonicity:
// the upsert's COALESCE preserve means a non-nil incoming value always wins, so
// a peer first labelled by JID is relabelled once a push name is seen.
func TestDiscovery_LaterPushNameUpgradesJIDLabel(t *testing.T) {
	comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{
		peerCount("88800000002@lid", 3, nil, nil),
	}}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))
	comms.counts = []repository.UnmatchedPeerCount{peerCount("88800000002@lid", 4, nil, strp("Their Name"))}
	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))

	require.Len(t, ext.upserts, 2)
	assert.Equal(t, "WhatsApp 88800000002", *ext.upserts[0].DisplayName)
	assert.Equal(t, "Their Name", *ext.upserts[1].DisplayName)
}

// TestDiscovery_EmptyPushNameDoesNotClobberStoredName is the reason the
// nil-if-empty normalization exists: an empty string is a POPULATED value to
// COALESCE(EXCLUDED.x, …) and would overwrite a stored name with nothing.
func TestDiscovery_EmptyPushNameDoesNotClobberStoredName(t *testing.T) {
	comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{
		peerCount("15559876543@s.whatsapp.net", 3, strp("+15559876543"), strp("")),
	}}
	ext := &fakeExternalContactUpserter{}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil))
	require.Len(t, ext.upserts, 1)
	assert.NotContains(t, ext.upserts[0].Metadata, "push_name")
	assert.Equal(t, "+15559876543", *ext.upserts[0].DisplayName)
}

func TestDiscovery_UpsertErrorIsNonFatal(t *testing.T) {
	comms := &fakeCommsPeerStore{counts: []repository.UnmatchedPeerCount{
		peerCount("15559876543@s.whatsapp.net", 3, strp("+15559876543"), nil),
	}}
	ext := &fakeExternalContactUpserter{upsertErrOne: errors.New("database down")}
	m := NewPeerMatcher(&fakeIdentityMatcher{}, comms, ext, nil, 3)

	require.NoError(t, m.UpdateDiscoveryCandidates(context.Background(), nil),
		"discovery is best-effort; it must not withhold an ack for a message already staged")
}

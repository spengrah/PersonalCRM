package google

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// --- fakes (reused from the prior stored-row suite; only the comms/stored-row
// plumbing was dropped — the contact + external fakes stay) ---

type fakeCorrespondenceContacts struct {
	// matches keyed by exact search name → matches returned (already sorted).
	matches map[string][]repository.ContactMatch
	names   map[uuid.UUID]string
	// lastThreshold records the threshold the discoverer passed on the most
	// recent FindSimilarContacts call so a test can pin the floor-minus-epsilon
	// SQL workaround. calls counts FindSimilarContacts invocations so the token
	// gate's "no call below 2 tokens" invariant can be asserted.
	lastThreshold float64
	calls         int
}

func (f *fakeCorrespondenceContacts) FindSimilarContacts(_ context.Context, name string, threshold float64, _ int32) ([]repository.ContactMatch, error) {
	f.calls++
	f.lastThreshold = threshold
	return f.matches[name], nil
}

func (f *fakeCorrespondenceContacts) GetContact(_ context.Context, id uuid.UUID) (*repository.Contact, error) {
	if n, ok := f.names[id]; ok {
		return &repository.Contact{ID: id, FullName: n}, nil
	}
	return nil, nil
}

// fakeExternalKey is the (source, sourceID) key fakeCorrespondenceExternal
// indexes by. Re-keyed from a sourceID-only map (contract review finding): a
// sourceID-only fake would make every cross-source assertion vacuous — a test
// could pass even if production checked the WRONG source, since both sources'
// candidates share the same normalized-address sourceID space.
type fakeExternalKey struct {
	source   string
	sourceID string
}

// fakeExternalLookup records one GetBySource call so a test can assert the
// opposite-source cross-check actually happened (not just that its outcome
// looked right).
type fakeExternalLookup struct {
	source    string
	sourceID  string
	accountID *string
}

// fakeCorrespondenceExternal records upserts and serves seeded rows, keyed by
// (source, sourceID) — see fakeExternalKey. upsertErr maps a source_id to an
// error the Upsert should return (to exercise the continue-on-error /
// aggregate-error path).
type fakeCorrespondenceExternal struct {
	existing  map[fakeExternalKey]*repository.ExternalContact
	upserts   []repository.UpsertExternalContactRequest
	upsertErr map[string]error
	lookups   []fakeExternalLookup
}

func newFakeExternal() *fakeCorrespondenceExternal {
	return &fakeCorrespondenceExternal{
		existing:  map[fakeExternalKey]*repository.ExternalContact{},
		upsertErr: map[string]error{},
	}
}

// seedExisting seeds a live row for (source, sourceID) — the source-qualified
// replacement for direct map assignment.
func (f *fakeCorrespondenceExternal) seedExisting(source, sourceID string, row *repository.ExternalContact) {
	f.existing[fakeExternalKey{source: source, sourceID: sourceID}] = row
}

func (f *fakeCorrespondenceExternal) GetBySource(_ context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error) {
	f.lookups = append(f.lookups, fakeExternalLookup{source: source, sourceID: sourceID, accountID: accountID})
	return f.existing[fakeExternalKey{source: source, sourceID: sourceID}], nil
}

func (f *fakeCorrespondenceExternal) Upsert(_ context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	if err := f.upsertErr[req.SourceID]; err != nil {
		return nil, err
	}
	f.upserts = append(f.upserts, req)
	key := fakeExternalKey{source: req.Source, sourceID: req.SourceID}
	row := &repository.ExternalContact{
		Source:      req.Source,
		SourceID:    req.SourceID,
		MatchStatus: repository.MatchStatusUnmatched,
	}
	// Preserve a pre-existing status (mirrors the real upsert's DO UPDATE SET
	// which never touches match_status).
	if prior := f.existing[key]; prior != nil {
		row.MatchStatus = prior.MatchStatus
	}
	f.existing[key] = row
	return row, nil
}

// lookedUp reports whether GetBySource was ever called for (source, sourceID).
func (f *fakeCorrespondenceExternal) lookedUp(source, sourceID string) bool {
	for _, l := range f.lookups {
		if l.source == source && l.sourceID == sourceID {
			return true
		}
	}
	return false
}

// --- helpers ---

func contactMatch(id uuid.UUID, name string, sim float64) repository.ContactMatch {
	return repository.ContactMatch{Contact: repository.Contact{ID: id, FullName: name}, Similarity: sim}
}

// runDiscovery folds one message's participants into a fresh aggregate then
// evaluates it, returning the upserted count + any aggregated error — the exact
// per-pass shape the provider hook drives. msgCtx defaults to the zero value
// (not trust-anchored) for the link-gate-only tests in this file; the
// participant-gate test suite (gmail_participant_test.go) drives real
// foldDiscovery output instead of calling this helper.
func runDiscovery(
	t *testing.T,
	d *CorrespondenceDiscoverer,
	parts []participant,
	known, own map[string]struct{},
	coOccurIDs []uuid.UUID,
) (int, error) {
	t.Helper()
	agg := map[string]*correspondenceAggregate{}
	aggregateParticipants(parts, known, own, emptySet(), coOccurIDs, participantMessageContext{}, agg)
	return d.EvaluateAddresses(context.Background(), sortedAggregates(agg))
}

func emptySet() map[string]struct{} { return map[string]struct{}{} }

// --- §6.1 unit tests ---

// 1. Multi-party message → candidate. From=known A, To=own, Cc=unknown whose
// ≥2-token name matches a DIFFERENT contact B at sim ≥ 0.60. The candidate's
// suggested match is B; its co-occurring-contact evidence is A (the KNOWN
// contact on the message, NOT the suggested match).
func TestDiscovery_MultiPartyMessageYieldsCandidate(t *testing.T) {
	contactA := uuid.New() // known co-occurring contact (on From)
	contactB := uuid.New() // trigram-matched suggested contact (different)

	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(contactB, "Pat Carter", 0.60)},
		},
		names: map[uuid.UUID]string{contactA: "Known Alpha", contactB: "Pat Carter"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{
		{name: "Known Alpha", address: "a@example.com"},  // From: known A
		{name: "Me", address: "me@example.com"},          // To: own
		{name: "Pat Carter", address: "pat@example.com"}, // Cc: unknown → candidate
	}
	known := map[string]struct{}{"a@example.com": {}}
	own := map[string]struct{}{"me@example.com": {}}
	// Co-occurrence ids = known ids on From/To/Cc = {A}.
	coOccurIDs := []uuid.UUID{contactA}

	n, err := runDiscovery(t, d, parts, known, own, coOccurIDs)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "pat@example.com", ext.upserts[0].SourceID)
	require.Equal(t, CorrespondenceSource, ext.upserts[0].Source)
	require.NotNil(t, ext.upserts[0].DisplayName)
	require.Equal(t, "Pat Carter", *ext.upserts[0].DisplayName)

	// Co-occurring evidence is A (the known contact present on the message), NOT
	// the suggested match B.
	var meta struct {
		CoOccurring struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"co_occurring_contact"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, contactA.String(), meta.CoOccurring.ID, "evidence is the known co-occurring contact A")
	require.Equal(t, "Known Alpha", meta.CoOccurring.Name)
	require.NotEqual(t, contactB.String(), meta.CoOccurring.ID, "evidence must NOT be the suggested match B")
}

// Boundary case: an EXACT 0.60 match must qualify (the floor-minus-epsilon +
// Go `>=` re-check honors `>= 0.60`, not the SQL's strict `>`).
func TestDiscovery_GateExactBoundary(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Exact Match": {contactMatch(contactID, "Exact Match", 0.60)},
		},
		names: map[uuid.UUID]string{contactID: "Exact Match"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Exact Match", address: "unknown@example.com"}}
	n, err := runDiscovery(t, d, parts, emptySet(), emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, n, "exact-0.60 similarity must qualify")
	// Pin the floor-minus-epsilon SQL workaround: a regression to passing 0.60
	// would silently drop exact-0.60 matches.
	require.Less(t, contacts.lastThreshold, correspondenceSimThreshold,
		"FindSimilarContacts must be called with a floor strictly below 0.60")
	require.Equal(t, correspondenceSimFloor, contacts.lastThreshold)
}

// 2. Reject sub-0.60.
func TestDiscovery_RejectsBelowThreshold(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Weak Match": {contactMatch(contactID, "Weak Match", 0.59)},
		},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Weak Match", address: "unknown@example.com"}}
	n, err := runDiscovery(t, d, parts, emptySet(), emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "sub-0.60 must be rejected")
	require.Empty(t, ext.upserts)
}

// 3. Reject single-token name — the token gate drops it BEFORE FindSimilar
// Contacts is called (assert the fake was not called).
func TestDiscovery_RejectsSingleTokenNameBeforeQuery(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		// Even a perfect similarity must not save a single-token name.
		matches: map[string][]repository.ContactMatch{
			"Jane": {contactMatch(contactID, "Jane", 0.99)},
		},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Jane", address: "unknown@example.com"}}
	n, err := runDiscovery(t, d, parts, emptySet(), emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "bare first name must be rejected by the ≥2-token gate")
	require.Empty(t, ext.upserts)
	require.Equal(t, 0, contacts.calls, "token gate must short-circuit before FindSimilarContacts")
}

// 4. Dedup by address — same unknown address in To of msg1 and Cc of msg2 of one
// pass → one aggregate, one evaluateAndUpsert, message_count == 2.
func TestDiscovery_DedupByAddressAcrossMessages(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(contactID, "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{contactID: "Pat Carter"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	agg := map[string]*correspondenceAggregate{}
	// msg1: To = the unknown address.
	aggregateParticipants([]participant{{name: "Pat Carter", address: "pat@example.com"}}, emptySet(), emptySet(), emptySet(), nil, participantMessageContext{}, agg)
	// msg2: Cc = the same unknown address.
	aggregateParticipants([]participant{{name: "Pat Carter", address: "pat@example.com"}}, emptySet(), emptySet(), emptySet(), nil, participantMessageContext{}, agg)

	n, err := d.EvaluateAddresses(context.Background(), sortedAggregates(agg))
	require.NoError(t, err)
	require.Equal(t, 1, n, "two messages for one address → one candidate")
	require.Len(t, ext.upserts, 1)
	require.Equal(t, 1, contacts.calls, "one FindSimilarContacts call for the deduped address")

	var meta struct {
		MessageCount int `json:"message_count"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, 2, meta.MessageCount, "message_count sums across the two messages")
}

// Per-message dedup: the same unknown address in BOTH To and Cc of ONE message
// counts as one message, not two.
func TestDiscovery_MessageCountDedupsWithinMessage(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(contactID, "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{contactID: "Pat Carter"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	// Same address listed twice in one message (To + Cc).
	parts := []participant{
		{name: "Pat Carter", address: "pat@example.com"},
		{name: "Pat Carter", address: "pat@example.com"},
	}
	n, err := runDiscovery(t, d, parts, emptySet(), emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)

	var meta struct {
		MessageCount int `json:"message_count"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, 1, meta.MessageCount, "same address in To+Cc of one message counts once")
}

// 5. Skip known — a Cc address in the known-set is never aggregated.
func TestDiscovery_SkipKnown(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Known Person": {contactMatch(contactID, "Known Person", 0.9)},
		},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Known Person", address: "known@example.com"}}
	known := map[string]struct{}{"known@example.com": {}}
	n, err := runDiscovery(t, d, parts, known, emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "known addresses are never emitted")
	require.Empty(t, ext.upserts)
}

// 6. Skip own-account — a Cc address in meSet is never aggregated.
func TestDiscovery_SkipOwnAccount(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Own Two": {contactMatch(contactID, "Own Two", 0.9)},
		},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Own Two", address: "own2@example.com"}}
	own := map[string]struct{}{"own2@example.com": {}}
	n, err := runDiscovery(t, d, parts, emptySet(), own, nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "own-account addresses are never emitted")
	require.Empty(t, ext.upserts)
}

// 7. Sticky-ignored — a live ignored row → no upsert (write-avoidance guard
// preserved, no clobber to unmatched).
func TestDiscovery_StickyIgnoreNoClobber(t *testing.T) {
	contactID := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Ignored Person": {contactMatch(contactID, "Ignored Person", 0.9)},
		},
	}
	ext := newFakeExternal()
	ext.seedExisting(CorrespondenceSource, "ignored@example.com", &repository.ExternalContact{
		Source:      CorrespondenceSource,
		SourceID:    "ignored@example.com",
		MatchStatus: repository.MatchStatusIgnored,
	})
	d := NewCorrespondenceDiscoverer(contacts, ext)

	parts := []participant{{name: "Ignored Person", address: "ignored@example.com"}}
	n, err := runDiscovery(t, d, parts, emptySet(), emptySet(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ext.upserts, "ignored row → no redundant write")
	require.Equal(t, repository.MatchStatusIgnored, ext.existing[fakeExternalKey{source: CorrespondenceSource, sourceID: "ignored@example.com"}].MatchStatus)
}

// 8a. BCC excluded — candidate. collectDiscoveryParticipants must NOT return a
// Bcc address even though the parser can read it, so an unknown qualifying name
// in Bcc-only yields no candidate.
func TestDiscovery_BccExcludedFromCollection(t *testing.T) {
	p := newProviderForResolution()
	msg := &gmailapi.Message{
		Payload: &gmailapi.MessagePart{
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: "Known Alpha <a@example.com>"},
				{Name: "To", Value: "Me <me@example.com>"},
				{Name: "Cc", Value: "Cc Person <cc@example.com>"},
				{Name: "Bcc", Value: "Pat Carter <pat@example.com>"},
			},
		},
	}

	parts := p.RunDiscoverParticipantsForTest(msg)
	addrs := map[string]bool{}
	for _, part := range parts {
		addrs[part.Address] = true
	}
	require.True(t, addrs["a@example.com"], "From collected")
	require.True(t, addrs["me@example.com"], "To collected")
	require.True(t, addrs["cc@example.com"], "Cc collected")
	require.False(t, addrs["pat@example.com"], "Bcc must NOT be collected for discovery")
}

// 8b. BCC excluded — evidence. A KNOWN contact in Bcc-only must NOT appear as
// co-occurring evidence (the hook's coOccurIDs is computed over From/To/Cc only,
// not the Bcc-inclusive candidateContacts set). The candidate is produced from
// the Cc unknown address, with evidence drawn only from the known From contact.
func TestDiscovery_BccKnownContactNotEvidence(t *testing.T) {
	p := newProviderForResolution()
	contactA := uuid.New()   // known, on From → legitimate evidence
	contactBcc := uuid.New() // known, on Bcc → must NOT be evidence

	msg := &gmailapi.Message{
		Payload: &gmailapi.MessagePart{
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: "Known Alpha <a@example.com>"},
				{Name: "Cc", Value: "Pat Carter <pat@example.com>"},
				{Name: "Bcc", Value: "Known Bcc <bcc@example.com>"},
			},
		},
	}
	parts := make([]participant, 0)
	for _, dp := range p.RunDiscoverParticipantsForTest(msg) {
		parts = append(parts, participant{name: dp.Name, address: dp.Address})
	}

	knownMap := map[string][]uuid.UUID{
		"a@example.com":   {contactA},
		"bcc@example.com": {contactBcc},
	}
	coOccurIDs := discoveryCoOccurIDs(parts, knownMap)
	require.Equal(t, []uuid.UUID{contactA}, coOccurIDs,
		"co-occurrence ids are From/To/Cc only; the Bcc'd known contact must not appear")

	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(uuid.New(), "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{contactA: "Known Alpha"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)

	known := map[string]struct{}{"a@example.com": {}, "bcc@example.com": {}}
	agg := map[string]*correspondenceAggregate{}
	aggregateParticipants(parts, known, emptySet(), emptySet(), coOccurIDs, participantMessageContext{}, agg)
	n, err := d.EvaluateAddresses(context.Background(), sortedAggregates(agg))
	require.NoError(t, err)
	require.Equal(t, 1, n, "the Cc unknown address still produces a candidate")
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "pat@example.com", ext.upserts[0].SourceID)

	var meta struct {
		CoOccurring struct {
			ID string `json:"id"`
		} `json:"co_occurring_contact"`
	}
	b, err := json.Marshal(ext.upserts[0].Metadata)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, contactA.String(), meta.CoOccurring.ID, "evidence is the From known contact")
	require.NotEqual(t, contactBcc.String(), meta.CoOccurring.ID, "the Bcc'd known contact must NOT be evidence")
}

// Empty input → no error, no upsert.
func TestDiscovery_EmptyInputNoError(t *testing.T) {
	d := NewCorrespondenceDiscoverer(&fakeCorrespondenceContacts{}, newFakeExternal())
	n, err := d.EvaluateAddresses(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// A per-address upsert failure must NOT abort the pass (the other address still
// upserts) but EvaluateAddresses must return the successful count AND a non-nil
// aggregated error, so the provider can log it. (The provider then suppresses
// the error so it never fails the sync — covered in the integration suite.)
func TestDiscovery_PerAddressErrorAggregated(t *testing.T) {
	goodContact := uuid.New()
	badContact := uuid.New()
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Good Person": {contactMatch(goodContact, "Good Person", 0.8)},
			"Bad Person":  {contactMatch(badContact, "Bad Person", 0.8)},
		},
		names: map[uuid.UUID]string{goodContact: "Good Person", badContact: "Bad Person"},
	}
	ext := newFakeExternal()
	ext.upsertErr["bad@example.com"] = errors.New("db down")
	d := NewCorrespondenceDiscoverer(contacts, ext)

	agg := map[string]*correspondenceAggregate{}
	aggregateParticipants([]participant{{name: "Good Person", address: "good@example.com"}}, emptySet(), emptySet(), emptySet(), nil, participantMessageContext{}, agg)
	aggregateParticipants([]participant{{name: "Bad Person", address: "bad@example.com"}}, emptySet(), emptySet(), emptySet(), nil, participantMessageContext{}, agg)

	n, err := d.EvaluateAddresses(context.Background(), sortedAggregates(agg))
	require.Error(t, err, "an upsert failure must surface as a non-nil aggregated error")
	require.Contains(t, err.Error(), "failed")
	require.Equal(t, 1, n, "the surviving address still upserts and is counted")
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "good@example.com", ext.upserts[0].SourceID)
}

// Participant collection pairs each address with its index-aligned display name
// (From/To/Cc), tolerating headers without a display part.
func TestDiscovery_CollectParticipantsNamesAndAddresses(t *testing.T) {
	p := newProviderForResolution()
	msg := &gmailapi.Message{
		Payload: &gmailapi.MessagePart{
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: "Pat Carter <pat@example.com>"},
				{Name: "To", Value: "first@example.com, Named Two <two@example.com>"},
				{Name: "Cc", Value: "Cc Person <cc@example.com>"},
			},
		},
	}
	got := p.RunDiscoverParticipantsForTest(msg)
	byAddr := map[string]string{}
	for _, dp := range got {
		byAddr[dp.Address] = dp.Name
	}
	require.Equal(t, "Pat Carter", byAddr["pat@example.com"])
	require.Equal(t, "", byAddr["first@example.com"], "no display part → empty name")
	require.Equal(t, "Named Two", byAddr["two@example.com"])
	require.Equal(t, "Cc Person", byAddr["cc@example.com"])
}

func TestBestDisplayName(t *testing.T) {
	require.Equal(t, "", bestDisplayName(nil))
	require.Equal(t, "Two Tokens", bestDisplayName([]string{"One", "Two Tokens"}))
	require.Equal(t, "Longer Full Name", bestDisplayName([]string{"Full Name", "Longer Full Name"}))
}

func TestTokenCount(t *testing.T) {
	require.Equal(t, 0, tokenCount(""))
	require.Equal(t, 1, tokenCount("Jane"))
	require.Equal(t, 2, tokenCount("Jane Doe"))
	require.Equal(t, 2, tokenCount("  Jane   Doe  "))
}

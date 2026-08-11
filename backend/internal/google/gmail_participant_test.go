// Unit coverage for the trust-anchored gmail_participant discovery gate
// (spec `.ai/spec/2026-08-11-gmail-participant-discovery.md`, IMP-042..045,
// IMP-049). Drives the REAL fold: builds real *gmail.Message values (with
// Gmail-faithful headers — address-only participants have NO name part, and
// the no-Subject case OMITS the header entirely), calls the real
// GmailSyncProvider.foldDiscovery for each message into a shared aggregate,
// then evaluates once via CorrespondenceDiscoverer.EvaluateAddresses — the
// exact per-pass shape the provider hook drives. Reuses the
// fakeCorrespondenceContacts/fakeCorrespondenceExternal fakes from
// gmail_correspondence_test.go (source-keyed, so cross-source checks are
// falsifiable) and the buildMessage helper from gmail_test.go (Gmail-faithful
// header construction).
package google

import (
	"context"
	"fmt"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/gmail/v1"
)

// newParticipantProvider builds a provider with d wired as its discoverer —
// the production seam (SetCorrespondenceDiscoverer), so foldDiscovery is live
// rather than a no-op.
func newParticipantProvider(d *CorrespondenceDiscoverer) *GmailSyncProvider {
	p := NewGmailSyncProvider(nil, nil, nil, nil)
	p.SetCorrespondenceDiscoverer(d)
	return p
}

// foldAndEvaluate folds each message into a shared per-pass aggregate via the
// real foldDiscovery, then evaluates once — mirroring Sync/ScanIdentifier's
// fold-then-evaluate-once shape.
func foldAndEvaluate(
	t *testing.T,
	p *GmailSyncProvider,
	d *CorrespondenceDiscoverer,
	knownMap map[string][]uuid.UUID,
	knownSet, meSet, ownDomains map[string]struct{},
	msgs ...*gmail.Message,
) (int, error) {
	t.Helper()
	agg := map[string]*correspondenceAggregate{}
	for _, msg := range msgs {
		p.foldDiscovery(msg, knownMap, knownSet, meSet, ownDomains, agg)
	}
	return d.EvaluateAddresses(context.Background(), sortedAggregates(agg))
}

// 1. First sighting from a trusted (known-contact) sender proposes a create
// candidate with the full §5.2 metadata literal.
func TestParticipant_TrustedContactSenderCreatesCandidate(t *testing.T) {
	// spec: IMP-042
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{
		"sender@example.com": {senderID},
		"known2@example.com": {uuid.New()},
	}
	knownSet := map[string]struct{}{
		"sender@example.com": {},
		"known2@example.com": {},
	}
	contacts := &fakeCorrespondenceContacts{
		names: map[uuid.UUID]string{senderID: "Trusted Sender"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"Alex Guest <alex@example.net>", "known2@example.com"}, nil, nil,
		"Project kickoff", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)

	up := ext.upserts[0]
	require.Equal(t, ParticipantSource, up.Source)
	require.Equal(t, "alex@example.net", up.SourceID)
	require.NotNil(t, up.DisplayName)
	require.Equal(t, "Alex Guest", *up.DisplayName)
	require.Equal(t, []repository.EmailEntry{{Value: "alex@example.net"}}, up.Emails)

	expected := map[string]any{
		"message_count":      1,
		"last_message_at":    formatEvidenceTime(1700000000000 / 1000),
		"display_names_seen": []string{"Alex Guest"},
		"trusted_sender": map[string]any{
			"address": "sender@example.com",
			"name":    "Trusted Sender",
		},
		"anchor_subject": "Project kickoff",
	}
	require.Equal(t, expected, up.Metadata, "full §5.2 metadata literal — every key present, no others")
}

// 2. An address-only (nameless) To participant qualifies. No display_names_seen
// key, nil DisplayName.
func TestParticipant_NamelessAddressQualifies(t *testing.T) {
	// spec: IMP-042
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{names: map[uuid.UUID]string{senderID: "Trusted Sender"}}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	// Header is address-only (no display-name part) — real-Gmail fidelity.
	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"nameless@example.net"}, nil, nil, "Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)

	up := ext.upserts[0]
	require.Equal(t, ParticipantSource, up.Source)
	require.Equal(t, "nameless@example.net", up.SourceID)
	require.Nil(t, up.DisplayName)
	require.Equal(t, []repository.EmailEntry{{Value: "nameless@example.net"}}, up.Emails)
	_, hasNames := up.Metadata["display_names_seen"]
	require.False(t, hasNames, "no display_names_seen key for a nameless sighting")
}

// 3. From ∈ meSet anchors trust; the anchor's trusted_sender carries self=true
// and NO name key.
func TestParticipant_SelfSenderAnchorsTrust(t *testing.T) {
	// spec: IMP-042
	meSet := map[string]struct{}{"me@example.com": {}}
	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "me@example.com",
		nil, []string{"cc-guest@example.net"}, nil, "Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, nil, emptySet(), meSet, emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)

	up := ext.upserts[0]
	require.Equal(t, "cc-guest@example.net", up.SourceID)
	trustedSender, ok := up.Metadata["trusted_sender"].(map[string]any)
	require.True(t, ok, "trusted_sender present")
	require.Equal(t, "me@example.com", trustedSender["address"])
	require.Equal(t, true, trustedSender["self"])
	_, hasName := trustedSender["name"]
	require.False(t, hasName, "self anchor carries no name key")
}

// 4. An untrusted sender never proposes a create candidate — not for the
// unknown participant, and not for the sender itself, even though a known
// contact co-occurs on the same message (the nearly-free signal that must not
// qualify).
func TestParticipant_UntrustedSenderNeverCreates(t *testing.T) {
	// spec: IMP-043
	knownID := uuid.New()
	knownMap := map[string][]uuid.UUID{"known@example.com": {knownID}}
	knownSet := map[string]struct{}{"known@example.com": {}}
	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "untrusted@example.net",
		[]string{"known@example.com", "bystander@example.net"}, nil, nil,
		"Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, ext.upserts)
}

// 5. Known, own(meSet), own-domain, and sticky-ignored addresses are never
// proposed even on a trust-anchored message; a fifth clean address IS
// upserted (proves the test could fail).
func TestParticipant_ExclusionsNeverProposed(t *testing.T) {
	// spec: IMP-042, IMP-049
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{
		"sender@example.com":          {senderID},
		"known-recipient@example.com": {uuid.New()},
	}
	knownSet := map[string]struct{}{
		"sender@example.com":          {},
		"known-recipient@example.com": {},
	}
	meSet := map[string]struct{}{"me-alt@example.com": {}}
	ownDomains := map[string]struct{}{"own-domain.example": {}}

	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	ext.seedExisting(ParticipantSource, "ignored@example.net", &repository.ExternalContact{
		Source:      ParticipantSource,
		SourceID:    "ignored@example.net",
		MatchStatus: repository.MatchStatusIgnored,
	})
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{
			"known-recipient@example.com",
			"me-alt@example.com",
			"guest@own-domain.example",
			"ignored@example.net",
			"clean@example.net",
		}, nil, nil, "Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, meSet, ownDomains, msg)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the clean address qualifies (proves the test could fail)")
	require.Len(t, ext.upserts, 1)
	require.Equal(t, "clean@example.net", ext.upserts[0].SourceID)
}

// 6. The recipient cap (>20 To∪Cc) suppresses the CREATE path only, per
// message; link discovery is unaffected. A duplicate address across To+Cc
// counts once toward the cap.
func TestParticipant_RecipientCapBoundary(t *testing.T) {
	// spec: IMP-044
	senderID := uuid.New()
	matchedID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(matchedID, "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{matchedID: "Pat Carter"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	// Exactly 20 unique To∪Cc recipients (18 fillers + a To/Cc duplicate +
	// cap-a): the duplicate must count once, so this message stays cap-OK.
	var toBoundary []string
	for i := 0; i < 18; i++ {
		toBoundary = append(toBoundary, fmt.Sprintf("filler%d@example.net", i))
	}
	toBoundary = append(toBoundary, "dup@example.net")
	msgBoundary := buildMessage(t, "g-boundary", "t1", "sender@example.com",
		toBoundary, []string{"dup@example.net", "cap-a@example.net"}, nil,
		"Subj", "body", "<m-boundary@example.com>", 1700000000000)

	// 21 unique recipients: over the cap. cap-c carries a strong name match
	// (link discovery must still fire despite the cap).
	var toOver []string
	for i := 0; i < 19; i++ {
		toOver = append(toOver, fmt.Sprintf("filler-b%d@example.net", i))
	}
	toOver = append(toOver, "cap-b@example.net")
	msgOver := buildMessage(t, "g-over", "t1", "sender@example.com",
		toOver, []string{"Pat Carter <cap-c@example.net>"}, nil,
		"Subj", "body", "<m-over@example.com>", 1700000100000)

	_, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msgBoundary, msgOver)
	require.NoError(t, err)

	bySourceID := map[string]string{}
	for _, up := range ext.upserts {
		bySourceID[up.SourceID] = up.Source
	}
	require.Equal(t, ParticipantSource, bySourceID["cap-a@example.net"], "exactly-20 message: cap-a qualifies")
	_, capBPresent := bySourceID["cap-b@example.net"]
	require.False(t, capBPresent, "21-recipient message: cap-b never proposed")
	require.Equal(t, CorrespondenceSource, bySourceID["cap-c@example.net"], "link path unaffected by the cap")
}

// 7. Link precedence: a strong name match wins over the trust anchor —
// exactly one upsert, as gmail_correspondence, zero participant upserts.
func TestParticipant_LinkPrecedenceWins(t *testing.T) {
	// spec: IMP-045
	senderID := uuid.New()
	matchedID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(matchedID, "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{matchedID: "Pat Carter"},
	}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"Pat Carter <pat@example.net>"}, nil, nil, "Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)
	require.Equal(t, CorrespondenceSource, ext.upserts[0].Source)
	require.Equal(t, "pat@example.net", ext.upserts[0].SourceID)
}

// 8. One-card-per-address mutual exclusion, both directions, with proof the
// opposite-source lookup actually happened.
func TestParticipant_CrossSourceStickyBothDirections(t *testing.T) {
	// spec: IMP-045
	senderID := uuid.New()
	matchedID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{
		matches: map[string][]repository.ContactMatch{
			"Pat Carter": {contactMatch(matchedID, "Pat Carter", 0.8)},
		},
		names: map[uuid.UUID]string{matchedID: "Pat Carter"},
	}
	ext := newFakeExternal()
	// (a) a live gmail_participant row already exists for this address.
	ext.seedExisting(ParticipantSource, "sticky-a@example.net", &repository.ExternalContact{
		Source:      ParticipantSource,
		SourceID:    "sticky-a@example.net",
		MatchStatus: repository.MatchStatusUnmatched,
	})
	// (b) a live (unmatched) gmail_correspondence row already exists.
	ext.seedExisting(CorrespondenceSource, "sticky-b@example.net", &repository.ExternalContact{
		Source:      CorrespondenceSource,
		SourceID:    "sticky-b@example.net",
		MatchStatus: repository.MatchStatusUnmatched,
	})
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	// sticky-a: now has a strong name match → link WOULD fire.
	// sticky-b: address-only, trust-anchored → participant WOULD fire.
	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"Pat Carter <sticky-a@example.net>", "sticky-b@example.net"}, nil, nil,
		"Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 0, n, "both addresses are cross-source sticky — first classification wins")
	require.Empty(t, ext.upserts)
	require.True(t, ext.lookedUp(ParticipantSource, "sticky-a@example.net"),
		"the opposite-source (participant) lookup must have occurred for the link-gate address")
	require.True(t, ext.lookedUp(CorrespondenceSource, "sticky-b@example.net"),
		"the opposite-source (correspondence) lookup must have occurred for the trust-anchor address")
}

// 9. An own-domain sender (not in meSet) anchors trust; own-domain
// participants on the same message are never proposed.
func TestParticipant_OwnDomainSenderAnchors(t *testing.T) {
	// spec: IMP-049
	ownDomains := map[string]struct{}{"own-domain.example": {}}
	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "alias@own-domain.example",
		[]string{"guest@example.net", "other@own-domain.example"}, nil, nil,
		"Subj", "body", "<m1@example.com>", 1700000000000)

	n, err := foldAndEvaluate(t, p, d, nil, emptySet(), emptySet(), ownDomains, msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)

	up := ext.upserts[0]
	require.Equal(t, "guest@example.net", up.SourceID)
	trustedSender, ok := up.Metadata["trusted_sender"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "alias@own-domain.example", trustedSender["address"])
	require.Equal(t, true, trustedSender["self"])

	for _, u := range ext.upserts {
		require.NotEqual(t, "other@own-domain.example", u.SourceID, "own-domain participant must never be proposed")
	}
}

// 10. A message with no Subject header at all (never an empty-string Subject)
// still qualifies; the metadata carries no anchor_subject key.
func TestParticipant_NoSubjectHeaderOmitsAnchor(t *testing.T) {
	// spec: IMP-042
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{names: map[uuid.UUID]string{senderID: "Trusted Sender"}}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	msg := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"guest@example.net"}, nil, nil, "", "body", "<m1@example.com>", 1700000000000)

	for _, h := range msg.Payload.Headers {
		require.NotEqual(t, "Subject", h.Name, "no-Subject case must OMIT the header, never send an empty string")
	}

	n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msg)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, ext.upserts, 1)
	_, hasSubject := ext.upserts[0].Metadata["anchor_subject"]
	require.False(t, hasSubject)
}

// 11. The anchor is the most-recent trust-anchored message's subject,
// regardless of fold order (tie-break is a non-issue here: epochs differ).
func TestParticipant_AnchorSubjectMostRecent(t *testing.T) {
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}

	const t1Epoch = int64(1700000000)
	const t2Epoch = int64(1700000500)

	msg1 := buildMessage(t, "g1", "t1", "sender@example.com",
		[]string{"guest@example.net"}, nil, nil, "Earlier subj", "body", "<m1@example.com>", t1Epoch*1000)
	msg2 := buildMessage(t, "g2", "t2", "sender@example.com",
		[]string{"guest@example.net"}, nil, nil, "Later subj", "body", "<m2@example.com>", t2Epoch*1000)

	orders := [][2]*gmail.Message{{msg1, msg2}, {msg2, msg1}}
	for _, order := range orders {
		contacts := &fakeCorrespondenceContacts{names: map[uuid.UUID]string{senderID: "Trusted Sender"}}
		ext := newFakeExternal()
		d := NewCorrespondenceDiscoverer(contacts, ext)
		p := newParticipantProvider(d)

		n, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), order[0], order[1])
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Len(t, ext.upserts, 1)

		up := ext.upserts[0]
		require.Equal(t, "Later subj", up.Metadata["anchor_subject"], "most-recent trust-anchored subject wins regardless of fold order")
		require.Equal(t, 2, up.Metadata["message_count"])
		require.Equal(t, formatEvidenceTime(t2Epoch), up.Metadata["last_message_at"])
	}
}

// 12. The cap does not stick across messages: a qualifying sighting on ANY
// message is enough, even if the same address also appeared on a capped one.
func TestParticipant_CapDoesNotStickAcrossMessages(t *testing.T) {
	// spec: IMP-044
	senderID := uuid.New()
	knownMap := map[string][]uuid.UUID{"sender@example.com": {senderID}}
	knownSet := map[string]struct{}{"sender@example.com": {}}
	contacts := &fakeCorrespondenceContacts{}
	ext := newFakeExternal()
	d := NewCorrespondenceDiscoverer(contacts, ext)
	p := newParticipantProvider(d)

	// Over-cap message (21 recipients): both addresses appear here, capped.
	var over []string
	for i := 0; i < 19; i++ {
		over = append(over, fmt.Sprintf("filler-o%d@example.net", i))
	}
	over = append(over, "both@example.net", "capped-only@example.net")
	msgOver := buildMessage(t, "g-over", "t1", "sender@example.com", over, nil, nil,
		"Subj", "body", "<m-over@example.com>", 1700000000000)

	// Cap-OK message: only "both@example.net" appears here again.
	msgUnder := buildMessage(t, "g-under", "t2", "sender@example.com",
		[]string{"both@example.net"}, nil, nil, "Subj2", "body", "<m-under@example.com>", 1700000100000)

	_, err := foldAndEvaluate(t, p, d, knownMap, knownSet, emptySet(), emptySet(), msgOver, msgUnder)
	require.NoError(t, err)

	bySourceID := map[string]bool{}
	for _, up := range ext.upserts {
		bySourceID[up.SourceID] = true
	}
	require.True(t, bySourceID["both@example.net"], "a qualifying sighting on the cap-OK message is enough")
	require.False(t, bySourceID["capped-only@example.net"], "seen only on the capped message → never trust-anchored")
}

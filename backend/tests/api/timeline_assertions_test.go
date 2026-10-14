package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests observe GET /api/v1/timeline-items/:kind/:id/assertions: the
// assertions whose provenance cites a content item the drill-down for that
// identity displays.

type timelineAssertion struct {
	ContentRef  string    `json:"content_ref"`
	AssertionID uuid.UUID `json:"assertion_id"`
	Predicate   string    `json:"predicate"`
	Subject     string    `json:"subject"`
	ValueText   *string   `json:"value_text"`
	Status      string    `json:"status"`
}

// assertionsPath is the assertions read of the typed identity in the context
// of a contact.
func assertionsPath(kind string, id, contactID uuid.UUID) string {
	return "/api/v1/timeline-items/" + kind + "/" + id.String() + "/assertions?contact_id=" + contactID.String()
}

// timelineAssertions reads a timeline item's assertions and requires 200.
func (e *correctionEnv) timelineAssertions(t *testing.T, kind string, id, contactID uuid.UUID) []timelineAssertion {
	t.Helper()
	w := e.get(t, assertionsPath(kind, id, contactID))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data struct {
			Items []timelineAssertion `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data.Items, "an empty list, not null")
	return envelope.Data.Items
}

// claim asserts a preference about subject whose extractor provenance cites one
// content row. A proposed claim awaits review; otherwise it lands accepted.
func (e *correctionEnv) claim(t *testing.T, subject uuid.UUID, sourceKind string, rowID uuid.UUID, proposed bool) repository.Assertion {
	t.Helper()
	value := "Synthetic preference " + uuid.NewString()
	a, err := e.asserts.Assert(e.ctx, service.AssertRequest{
		SubjectNodeID: subject,
		PredicateKey:  "preference",
		ValueText:     &value,
		Confidence:    100,
		ForceConfirm:  proposed,
		Locators: []service.ProvenanceLocator{{
			SourceKind:   sourceKind,
			SourceID:     rowID.String(),
			ProducerKind: repository.ProducerKindExtractor,
		}},
	})
	require.NoError(t, err)
	return *a
}

// spec: IXN-039.cites-displayed-content
func TestTimelineAssertions_CitingADisplayedMessage(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	a := e.contact(t, "monthly")
	thread := e.ref("thread")
	observation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, "2026-01-02"), repository.InteractionDirectionInbound, e.at(0))
	cited := e.linkEmail(t, observation, thread, e.ref("message"))
	e.linkEmail(t, observation, thread, e.ref("message"))
	proposed := e.claim(t, a, repository.SourceKindCommsMessage, cited.ID, true)
	accepted := e.claim(t, a, repository.SourceKindCommsMessage, cited.ID, false)
	node := *observation.InteractionNodeID
	var ref string
	for _, message := range e.content(t, "interaction", node, a).Members[0].Messages {
		if message.ID == cited.ID {
			ref = message.ContentRef
		}
	}
	require.NotEmpty(t, ref, "the cited message is displayed")

	got := e.timelineAssertions(t, "interaction", node, a)

	require.Len(t, got, 2)
	byID := map[uuid.UUID]timelineAssertion{}
	for _, entry := range got {
		require.Equal(t, ref, entry.ContentRef)
		require.Equal(t, "preference", entry.Predicate)
		byID[entry.AssertionID] = entry
	}
	require.Equal(t, repository.AssertionStatusProposed, byID[proposed.ID].Status)
	require.Equal(t, proposed.ValueText, byID[proposed.ID].ValueText)
	require.Equal(t, repository.AssertionStatusAccepted, byID[accepted.ID].Status)
}

// spec: IXN-039.undisplayed-content-omitted
func TestTimelineAssertions_CitingUndisplayedContentIsOmitted(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// The contact's message in another thread is content of another
	// interaction, which this drill-down does not display.
	a := e.contact(t, "monthly")
	thread, other := e.ref("thread"), e.ref("thread")
	shown := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, "2026-01-02"), repository.InteractionDirectionInbound, e.at(0))
	elsewhere := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, other, "2026-01-02"), repository.InteractionDirectionInbound, e.at(1))
	displayed := e.linkEmail(t, shown, thread, e.ref("message"))
	undisplayed := e.linkEmail(t, elsewhere, other, e.ref("message"))
	kept := e.claim(t, a, repository.SourceKindCommsMessage, displayed.ID, true)
	e.claim(t, a, repository.SourceKindCommsMessage, undisplayed.ID, true)

	got := e.timelineAssertions(t, "interaction", *shown.InteractionNodeID, a)

	require.Len(t, got, 1)
	require.Equal(t, kept.ID, got[0].AssertionID)
}

// spec: IXN-039.merged-any-member
func TestTimelineAssertions_MergedInteractionReturnsEveryMembersCitations(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// A meeting with a linked note merged with an email thread-day: a claim
	// citing the note and one citing the message are both beside the merged
	// interaction's content, each keyed by its own item.
	a := e.contact(t, "monthly")
	ref := e.event(t)
	meeting := e.meeting(t, a, ref, e.at(0))
	sessionID := uuid.New()
	e.linkNote(t, sessionID, ptr(uuid.MustParse(ref)), "Synthetic note")
	notes, err := repository.NewMeetingNoteRepository(e.database.Queries).ListBySessionIDs(e.ctx, []uuid.UUID{sessionID})
	require.NoError(t, err)
	require.Len(t, notes, 1)
	thread := e.ref("thread")
	email := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, "2026-01-02"), repository.InteractionDirectionInbound, e.at(1))
	message := e.linkEmail(t, email, thread, e.ref("message"))
	e.merge(t, a, meeting, *email.InteractionNodeID)
	noteClaim := e.claim(t, a, repository.SourceKindMeetingNote, notes[0].ID, true)
	messageClaim := e.claim(t, a, repository.SourceKindCommsMessage, message.ID, true)
	refs := map[uuid.UUID]string{}
	for _, member := range e.content(t, "interaction", meeting, a).Members {
		for _, note := range member.MeetingNotes {
			refs[noteClaim.ID] = note.ContentRef
		}
		for _, m := range member.Messages {
			refs[messageClaim.ID] = m.ContentRef
		}
	}
	require.Len(t, refs, 2, "both members display their content")

	got := e.timelineAssertions(t, "interaction", *email.InteractionNodeID, a)

	require.Len(t, got, 2)
	for _, entry := range got {
		require.Equal(t, refs[entry.AssertionID], entry.ContentRef)
	}
}

// spec: IXN-039.corrections-never-returned
func TestTimelineAssertions_CorrectionIsNeverReturned(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// B is removed from the thread-day A and B share. The removal's assertion
	// is given a locator citing A's displayed message, so only the catalog's
	// correction marker keeps it out of the read.
	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	thread, day := e.ref("thread"), "2026-01-02"
	aObservation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, day), repository.InteractionDirectionInbound, e.at(0))
	e.record(t, b, repository.InteractionSourceEmail, emailRef(b, thread, day), repository.InteractionDirectionInbound, e.at(0))
	node := *aObservation.InteractionNodeID
	message := e.linkEmail(t, aObservation, thread, e.ref("message"))
	e.remove(t, node, removalBody(service.RemovalReasonDidNotAttend, b))
	corrections := e.correctionAssertions(t, node, "not_participant_in")
	require.Len(t, corrections, 1)
	inserted, err := e.assertions.InsertProvenance(e.ctx, repository.InsertProvenanceParams{
		AssertionID: corrections[0].ID, LocatorHash: "synthetic-" + uuid.NewString(), SourceKind: repository.SourceKindCommsMessage,
		SourceID: message.ID.String(), ProducerKind: repository.ProducerKindUser,
	})
	require.NoError(t, err)
	require.True(t, inserted)
	claim := e.claim(t, a, repository.SourceKindCommsMessage, message.ID, true)

	got := e.timelineAssertions(t, "interaction", node, a)

	require.Len(t, got, 1)
	require.Equal(t, claim.ID, got[0].AssertionID)
}

// spec: IXN-039.no-cited-content-empty
func TestTimelineAssertions_NoCitedContentIsAnEmptyList(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// A thread-day whose message no assertion cites, and a manual entry with
	// no content at all.
	a := e.contact(t, "monthly")
	thread := e.ref("thread")
	email := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, "2026-01-02"), repository.InteractionDirectionInbound, e.at(0))
	e.linkEmail(t, email, thread, e.ref("message"))
	manual := e.record(t, a, repository.InteractionSourceManual, "", repository.InteractionDirectionOutbound, e.at(1))

	require.Empty(t, e.timelineAssertions(t, "interaction", *email.InteractionNodeID, a))
	require.Empty(t, e.timelineAssertions(t, "interaction", *manual.InteractionNodeID, a))
}

// spec: IXN-039.cites-displayed-content
func TestTimelineAssertions_CitingAnotherContactsCopy(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// A and B each hold a copy of one email message. A claim citing B's copy
	// cites the message A's drill-down displays from A's own copy.
	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	thread, day, external := e.ref("thread"), "2026-01-02", e.ref("message")
	aObservation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, day), repository.InteractionDirectionInbound, e.at(0))
	bObservation := e.record(t, b, repository.InteractionSourceEmail, emailRef(b, thread, day), repository.InteractionDirectionInbound, e.at(0))
	aCopy := e.linkEmail(t, aObservation, thread, external)
	bCopy := e.linkEmail(t, bObservation, thread, external)
	claim := e.claim(t, b, repository.SourceKindCommsMessage, bCopy.ID, true)
	shown := e.content(t, "interaction", *aObservation.InteractionNodeID, a).Members[0].Messages
	require.Len(t, shown, 1)
	require.Equal(t, aCopy.ID, shown[0].ID, "A's drill-down displays A's copy")

	got := e.timelineAssertions(t, "interaction", *aObservation.InteractionNodeID, a)

	require.Len(t, got, 1)
	require.Equal(t, claim.ID, got[0].AssertionID)
	require.Equal(t, shown[0].ContentRef, got[0].ContentRef)
}

// spec: IXN-039.cites-displayed-content
func TestTimelineAssertions_CitationSurvivesContactMergeDedup(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()
	e := newCorrectionEnv(t)

	// A and B each hold a copy of one email message, and a claim cites A's
	// copy. Merging A into B tombstones A's duplicate copy and keeps B's, and
	// leaves B two observations of the thread-day: A's, the later, holding only
	// the tombstoned copy, and B's own, holding the surviving one. B's
	// drill-down displays the surviving copy: the claim still cites that message.
	a, b := e.contact(t, "monthly"), e.contact(t, "monthly")
	thread, day, external := e.ref("thread"), "2026-01-02", e.ref("message")
	aObservation := e.record(t, a, repository.InteractionSourceEmail, emailRef(a, thread, day), repository.InteractionDirectionInbound, e.at(1))
	bObservation := e.record(t, b, repository.InteractionSourceEmail, emailRef(b, thread, day), repository.InteractionDirectionInbound, e.at(0))
	aCopy := e.linkEmailSentAt(t, aObservation, thread, external, bObservation.OccurredAt)
	bCopy := e.linkEmail(t, bObservation, thread, external)
	claim := e.claim(t, a, repository.SourceKindCommsMessage, aCopy.ID, true)
	_, err := e.contacts.MergeContacts(e.ctx, service.MergeContactsRequest{SourceContactID: a, TargetContactID: b})
	require.NoError(t, err)
	node := *bObservation.InteractionNodeID
	var shown []contentMessage
	for _, member := range e.content(t, "interaction", node, b).Members {
		shown = append(shown, member.Messages...)
	}
	require.Len(t, shown, 1)
	require.Equal(t, bCopy.ID, shown[0].ID, "B's drill-down displays B's surviving copy")

	got := e.timelineAssertions(t, "interaction", node, b)

	require.Len(t, got, 1)
	require.Equal(t, claim.ID, got[0].AssertionID)
	require.Equal(t, shown[0].ContentRef, got[0].ContentRef)
}

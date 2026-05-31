package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestSnapshotContains covers the membership check used by
// ResolveLink's snapshot validation path.
func TestSnapshotContains(t *testing.T) {
	eventID := uuid.New()
	callID := uuid.New()
	snap := []repository.ConflictCandidateSummary{
		{Kind: repository.LinkedKindEvent, ID: eventID, OverlapCount: 2},
		{Kind: repository.LinkedKindPhoneCall, ID: callID, OverlapCount: 1},
	}

	require.True(t, snapshotContains(snap, repository.LinkedKindEvent, eventID))
	require.True(t, snapshotContains(snap, repository.LinkedKindPhoneCall, callID))
	require.False(t, snapshotContains(snap, repository.LinkedKindEvent, callID),
		"id alone is not enough — kind must match")
	require.False(t, snapshotContains(snap, repository.LinkedKindEvent, uuid.New()),
		"random id rejected")
	require.False(t, snapshotContains(nil, repository.LinkedKindEvent, eventID),
		"nil snapshot returns false")
}

// TestTruncateRunes covers rune-aware truncation including UTF-8 edges.
func TestTruncateRunes(t *testing.T) {
	require.Equal(t, "", truncateRunes("anything", 0))
	require.Equal(t, "abc", truncateRunes("abc", 10), "shorter than max returns original")
	require.Equal(t, "abc", truncateRunes("abcdef", 3))
	// Multi-byte runes: "héllo" is 5 runes, 6 bytes. Cutting at 3 runes
	// returns "hél" — must not split mid-byte.
	got := truncateRunes("héllo", 3)
	require.Equal(t, "hél", got)
}

// TestResolveLinkInputFromKind is a sanity test for the convenience
// constructor used by the handler.
func TestResolveLinkInputFromKind(t *testing.T) {
	id := uuid.New()
	got := ResolveLinkInputFromKind(repository.LinkedKindEvent, id)
	require.Equal(t, repository.LinkedKindEvent, got.Kind)
	require.Equal(t, id, got.ID)
}

// --- needs-attention attendee enrichment ---

// stubLinkageTargets is a minimal LinkageTargetReader for the preview
// projection. Only the non-tx event/phone reads are exercised here.
type stubLinkageTargets struct {
	event *repository.CalendarEvent
	call  *repository.PhoneCall
}

func (s *stubLinkageTargets) GetEventByID(ctx context.Context, id uuid.UUID) (*repository.CalendarEvent, error) {
	return s.event, nil
}
func (s *stubLinkageTargets) GetPhoneCallByID(ctx context.Context, id uuid.UUID) (*repository.PhoneCall, error) {
	return s.call, nil
}
func (s *stubLinkageTargets) GetEventByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.CalendarEvent, error) {
	return s.event, nil
}
func (s *stubLinkageTargets) GetPhoneCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.PhoneCall, error) {
	return s.call, nil
}

// stubContactNameReader maps contact ids to full names for the matched-
// name resolution.
type stubContactNameReader struct {
	names map[uuid.UUID]string
}

func (s *stubContactNameReader) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	name, ok := s.names[id]
	if !ok {
		return nil, db.ErrNotFound
	}
	return &repository.Contact{ID: id, FullName: name}, nil
}

func TestCandidatePreview_EventAttendeesMatchedFlag(t *testing.T) {
	matchedID := uuid.New()
	unmatchedID := uuid.New()
	title := "Design sync"
	evt := &repository.CalendarEvent{
		ID:    uuid.New(),
		Title: &title,
		Attendees: []repository.Attendee{
			{DisplayName: "Alice Matched", Email: "alice@example.com"},
			{DisplayName: "Bob Unmatched", Email: "bob@example.com"},
			{DisplayName: "", Email: "carol@example.com"}, // email-only → local-part label
		},
		MatchedContactIDs: []uuid.UUID{matchedID, unmatchedID},
	}
	svc := &MeetingNoteService{
		linkageTargets: &stubLinkageTargets{event: evt},
		contactNames: &stubContactNameReader{names: map[uuid.UUID]string{
			matchedID:   "Alice Matched",
			unmatchedID: "Someone Else",
		}},
	}

	// Implied set contains only the matched contact (the unmatched
	// MatchedContactID is NOT in the implied set, so it must not emphasize
	// any attendee).
	impliedSet := map[uuid.UUID]struct{}{matchedID: {}}

	preview, missing, err := svc.candidatePreview(context.Background(), repository.LinkedKindEvent, evt.ID, impliedSet)
	require.NoError(t, err)
	require.False(t, missing)
	require.NotNil(t, preview)
	require.Len(t, preview.Attendees, 3)

	require.Equal(t, "Alice Matched", preview.Attendees[0].Name)
	require.True(t, preview.Attendees[0].Matched, "Alice is in the implied set and matched")
	require.Equal(t, "Bob Unmatched", preview.Attendees[1].Name)
	require.False(t, preview.Attendees[1].Matched, "Bob is not in the implied set")
	require.Equal(t, "carol", preview.Attendees[2].Name, "email-only attendee renders its local-part")
	require.False(t, preview.Attendees[2].Matched)

	// Every matched:true name is in the matched-name set (no false
	// positives) — matched flags are a subset signal, not a count.
	for _, a := range preview.Attendees {
		if a.Matched {
			require.Equal(t, "Alice Matched", a.Name)
		}
	}
}

func TestCandidatePreview_PhoneCallSyntheticAttendee(t *testing.T) {
	peerID := uuid.New()
	call := &repository.PhoneCall{
		ID:               uuid.New(),
		PeerHandle:       "+15551234567",
		MatchedContactID: &peerID,
	}
	svc := &MeetingNoteService{linkageTargets: &stubLinkageTargets{call: call}}

	// Matched when the peer contact is in the implied set.
	preview, missing, err := svc.candidatePreview(context.Background(), repository.LinkedKindPhoneCall, call.ID, map[uuid.UUID]struct{}{peerID: {}})
	require.NoError(t, err)
	require.False(t, missing)
	require.Len(t, preview.Attendees, 1)
	require.Equal(t, "+15551234567", preview.Attendees[0].Name)
	require.True(t, preview.Attendees[0].Matched)

	// Not matched when the peer is absent from the implied set.
	preview2, _, err := svc.candidatePreview(context.Background(), repository.LinkedKindPhoneCall, call.ID, map[uuid.UUID]struct{}{})
	require.NoError(t, err)
	require.Len(t, preview2.Attendees, 1)
	require.False(t, preview2.Attendees[0].Matched)
}

func TestAttendeeLabel(t *testing.T) {
	require.Equal(t, "Alice", attendeeLabel(repository.Attendee{DisplayName: "Alice", Email: "a@x.com"}))
	require.Equal(t, "bob", attendeeLabel(repository.Attendee{Email: "bob@x.com"}))
	require.Equal(t, "plainstring", attendeeLabel(repository.Attendee{Email: "plainstring"}))
}

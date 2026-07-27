package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// seedCalendarEventWithAttendees seeds an event whose Attendees carry
// display-name labels (so the needs-attention projection has names to
// flag) alongside the matched-contact id set.
func (e *meetingNoteIngestEnv) seedCalendarEventWithAttendees(t *testing.T, startTime time.Time, attendees []repository.Attendee, matchedContactIDs []uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	upserted, err := e.calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       e.sourceIDPrefix + "cal-att-" + suffix,
		GcalCalendarID:    "primary",
		GoogleAccountID:   "test-account",
		Title:             stringPtr(e.sourceIDPrefix + "Attendee Event"),
		StartTime:         startTime,
		EndTime:           startTime.Add(time.Hour),
		Status:            "confirmed",
		Attendees:         attendees,
		MatchedContactIDs: matchedContactIDs,
		SyncedAt:          accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	return upserted.ID
}

// needsAttentionListResponse mirrors the wire shape of the
// needs-attention list for decoding the enriched attendee payload.
type needsAttentionListResponse struct {
	Data []struct {
		ID         uuid.UUID `json:"id"`
		Candidates []struct {
			Kind         string `json:"kind"`
			OverlapCount int    `json:"overlap_count"`
			Preview      *struct {
				Title     *string `json:"title"`
				Attendees []struct {
					Name    string `json:"name"`
					Matched bool   `json:"matched"`
				} `json:"attendees"`
			} `json:"preview"`
		} `json:"candidates"`
	} `json:"data"`
}

// TestNeedsAttention_EnrichedAttendees seeds a conflict scenario where a
// tagged participant resolves to contactA, then asserts the
// needs-attention projection flags the attendee whose name matches
// contactA while leaving the other attendee unflagged. The meter's
// OverlapCount stays authoritative.
func TestNeedsAttention_EnrichedAttendees(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	ctx := context.Background()

	contactA, err := env.contactRepo.GetContact(ctx, env.contactA)
	require.NoError(t, err)
	contactB, err := env.contactRepo.GetContact(ctx, env.contactB)
	require.NoError(t, err)

	// Resolve the session's tagged participant to contactA via the email
	// path (same recipe the projection's implied-set recompute uses).
	emailA := contactAEmail(t, env)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	meetingAt := time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	// Two candidate events BOTH matching contactA so the overlap against
	// the implied set ({contactA}) is tied at 1 → no strict winner → row
	// lands conflict_pending. The first event lists contactA (matched + in
	// implied set) and contactB (a matched CONTACT but NOT in the implied
	// set) as named attendees, so the projection must emphasize ONLY
	// contactA.
	env.seedCalendarEventWithAttendees(t, meetingAt,
		[]repository.Attendee{
			{DisplayName: contactA.FullName, Email: "att-a@example.invalid"},
			{DisplayName: contactB.FullName, Email: "att-b@example.invalid"},
		},
		[]uuid.UUID{env.contactA, env.contactB},
	)
	env.seedCalendarEventWithAttendees(t, meetingAt.Add(5*time.Minute),
		[]repository.Attendee{{DisplayName: contactA.FullName, Email: "att-a2@example.invalid"}},
		[]uuid.UUID{env.contactA},
	)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Enriched Attendees Session", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = getNeedsAttention(t, env, "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp needsAttentionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var found bool
	for _, item := range resp.Data {
		if item.ID != row.ID {
			continue
		}
		found = true
		require.NotEmpty(t, item.Candidates)
		// Locate the candidate that carries contactA + contactB attendees.
		var target *struct {
			Title     *string `json:"title"`
			Attendees []struct {
				Name    string `json:"name"`
				Matched bool   `json:"matched"`
			} `json:"attendees"`
		}
		for ci := range item.Candidates {
			p := item.Candidates[ci].Preview
			if p == nil || len(p.Attendees) < 2 {
				continue
			}
			target = p
		}
		require.NotNil(t, target, "candidate with named attendees must be present")

		matchedNames := map[string]bool{}
		for _, a := range target.Attendees {
			if a.Matched {
				matchedNames[a.Name] = true
			}
		}
		// contactA is tagged → in the implied set → emphasized.
		require.True(t, matchedNames[contactA.FullName], "contactA attendee must be matched")
		// contactB is a matched CONTACT on the event but NOT in the implied
		// set (not tagged, not title-matched) → must NOT be emphasized.
		require.False(t, matchedNames[contactB.FullName], "contactB is not in the implied set")
	}
	require.True(t, found, "the conflict row must appear in needs-attention")
}

// TestNeedsAttention_OverlapCountIndependentOfMatchedFlags — a
// candidate's overlap_count meter is carried straight from the stored
// snapshot value and does NOT track the per-attendee matched flags
// computed at read time from attendee display-name matching. Seeds a
// candidate whose overlap_count (2, from two matched CONTACT ids in the
// implied set) and whose count of attendee matched=true flags (1,
// because one attendee's display label doesn't resolve to either
// contact's name) diverge — proving the two are carried independently.
// spec: NTS-025.candidate-overlap-count-independent
func TestNeedsAttention_OverlapCountIndependentOfMatchedFlags(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	ctx := context.Background()

	contactA, err := env.contactRepo.GetContact(ctx, env.contactA)
	require.NoError(t, err)
	contactB, err := env.contactRepo.GetContact(ctx, env.contactB)
	require.NoError(t, err)

	// Tag both contactA and contactB so the implied set is {contactA,
	// contactB} — the same pair both candidate events match on, giving
	// both an overlap_count of 2 (kept tied → conflict_pending).
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	emailB := fmt.Sprintf("mn-test-1-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)
	anarlogB := env.seedAnarlogHumanResolvingTo(t, emailB)

	meetingAt := time.Date(2026, 5, 21, 9, 0, 0, 0, time.UTC)
	// First candidate: matches {contactA, contactB} (overlap_count=2)
	// but names an attendee whose display label resolves to NEITHER
	// contact's full_name, so only 1 of its 2 attendees is flagged
	// matched=true.
	env.seedCalendarEventWithAttendees(t, meetingAt,
		[]repository.Attendee{
			{DisplayName: contactA.FullName, Email: "att-a@example.invalid"},
			{DisplayName: "Unresolvable Attendee Label", Email: "att-x@example.invalid"},
		},
		[]uuid.UUID{env.contactA, env.contactB},
	)
	// Second candidate: same overlap_count=2 (ties the top so the row
	// stays conflict_pending), both attendees resolve.
	env.seedCalendarEventWithAttendees(t, meetingAt.Add(5*time.Minute),
		[]repository.Attendee{
			{DisplayName: contactA.FullName, Email: "att-a2@example.invalid"},
			{DisplayName: contactB.FullName, Email: "att-b2@example.invalid"},
		},
		[]uuid.UUID{env.contactA, env.contactB},
	)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Overlap Independence Session", []string{anarlogA, anarlogB})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = getNeedsAttention(t, env, env.pairedHostID.String())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp needsAttentionListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var foundCandidate bool
	var overlapCount, matchedTrue int
	for i := range resp.Data {
		if resp.Data[i].ID != row.ID {
			continue
		}
		require.Len(t, resp.Data[i].Candidates, 2, "both tied candidates projected")
		for ci := range resp.Data[i].Candidates {
			c := resp.Data[i].Candidates[ci]
			if c.Preview == nil {
				continue
			}
			// Identify the candidate carrying the unresolvable-label
			// attendee (as opposed to the other tied candidate, whose
			// attendees both resolve).
			var hasUnresolvable bool
			for _, a := range c.Preview.Attendees {
				if a.Name == "Unresolvable Attendee Label" {
					hasUnresolvable = true
				}
			}
			if !hasUnresolvable {
				continue
			}
			foundCandidate = true
			overlapCount = c.OverlapCount
			for _, a := range c.Preview.Attendees {
				if a.Matched {
					matchedTrue++
				}
			}
		}
	}
	require.True(t, foundCandidate, "candidate with the mismatched-label attendee must be present")
	require.Equal(t, 2, overlapCount, "overlap_count is the stored meter value")
	require.Equal(t, 1, matchedTrue, "only the resolvable-label attendee is flagged matched")
	require.NotEqual(t, overlapCount, matchedTrue,
		"overlap_count must be carried independently of the per-attendee matched-flag count")
}

// contactAEmail recomputes contactA's seeded synthetic email
// (mn-test-0-<suffix>@example.invalid) from the env's source prefix
// (mn-ingest-<suffix>-), mirroring the setup helper.
func contactAEmail(t *testing.T, env *meetingNoteIngestEnv) string {
	t.Helper()
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	return "mn-test-0-" + suffix + "@example.invalid"
}

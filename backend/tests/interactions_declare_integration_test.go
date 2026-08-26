//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireInteractionsDeclareIntegration(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
}

func interactionContentService(database *db.Database) *service.InteractionContentService {
	return service.NewInteractionContentService(
		repository.NewInteractionRepository(database.Queries),
		repository.NewCommsMessageRepository(database.Queries),
		repository.NewTelegramMessageRepository(database.Queries),
		repository.NewMessagesMessageRepository(database.Queries),
		repository.NewMeetingNoteRepository(database.Queries),
		repository.NewCalendarEventRepository(database.Queries),
		repository.NewPhoneCallRepository(database.Queries),
		repository.NewContactRepository(database.Queries),
	)
}

func TestInteractionsDeclare_RowWorld(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	database, ctx := newSyntheticDB(t)
	seeded, err := declare.Run(ctx, database, "IXN-002", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	require.Len(t, seeded.Entities, 11)
	expectedKinds := map[string]string{
		"subject": "contact", "email-row": "interaction", "gchat-row": "interaction",
		"whatsapp-row": "interaction", "telegram-row": "interaction", "messages-row": "interaction",
		"gcal-row": "calendar_event", "call-row": "interaction", "manual-row": "interaction",
		"todoist-row": "interaction", "anarlog-row": "interaction",
	}
	for handle, kind := range expectedKinds {
		entity, ok := seeded.Entities[handle]
		require.True(t, ok, "manifest missing %s", handle)
		assert.Equal(t, kind, entity.Kind, handle)
		_, err := uuid.Parse(entity.ID)
		require.NoError(t, err, handle)
	}

	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	rows, err := repository.NewInteractionRepository(database.Queries).ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 10)
	sources := map[string]bool{}
	for _, row := range rows {
		sources[row.Source] = true
	}
	assert.Equal(t, map[string]bool{
		"email": true, "gchat": true, "whatsapp": true, "telegram": true,
		"messages": true, "gcal": true, "phone_calls": true, "manual": true,
		"todoist": true, "anarlog_sessions": true,
	}, sources)
	for handle, days := range map[string]int{"email-row": 1, "gchat-row": 2, "whatsapp-row": 3, "telegram-row": 4, "messages-row": 5, "call-row": 7, "manual-row": 8, "todoist-row": 9, "anarlog-row": 10} {
		row := findDeclaredRow(t, rows, seeded.Entities[handle].ID)
		assert.Equal(t, seeded.Anchor.Truncate(time.Second).Add(-time.Duration(days)*24*time.Hour).UTC(), row.OccurredAt)
	}
	enriched, err := interactionContentService(database).EnrichPage(ctx, rows)
	require.NoError(t, err)

	messageTarget := func(handle string, days int) time.Time {
		return seeded.Anchor.Truncate(time.Second).Add(-time.Duration(days) * 24 * time.Hour).UTC()
	}
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	for _, tc := range []struct {
		handle string
		days   int
	}{
		{"email-row", 1}, {"gchat-row", 2}, {"whatsapp-row", 3},
	} {
		interactionID := uuid.MustParse(seeded.Entities[tc.handle].ID)
		messages, err := commsRepo.ListByInteractionIDs(ctx, []uuid.UUID{interactionID})
		require.NoError(t, err)
		require.Len(t, messages, 1, tc.handle)
		assert.Equal(t, messageTarget(tc.handle, tc.days), messages[0].SentAt.UTC(), tc.handle)
	}
	telegramMessages, err := repository.NewTelegramMessageRepository(database.Queries).ListByInteractionIDs(ctx, []uuid.UUID{uuid.MustParse(seeded.Entities["telegram-row"].ID)})
	require.NoError(t, err)
	require.Len(t, telegramMessages, 1)
	assert.Equal(t, messageTarget("telegram-row", 4), telegramMessages[0].SentAt.UTC())
	messagesMessages, err := repository.NewMessagesMessageRepository(database.Queries).ListByInteractionIDs(ctx, []uuid.UUID{uuid.MustParse(seeded.Entities["messages-row"].ID)})
	require.NoError(t, err)
	require.Len(t, messagesMessages, 3)
	newestMessage := messagesMessages[0]
	for _, message := range messagesMessages[1:] {
		if message.SentAt.After(newestMessage.SentAt) {
			newestMessage = message
		}
	}
	assert.Equal(t, messageTarget("messages-row", 5), newestMessage.SentAt.UTC())

	messagesRow := enriched[uuid.MustParse(seeded.Entities["messages-row"].ID)]
	assert.Equal(t, 3, messagesRow.MessageCount)
	gchat := enriched[uuid.MustParse(seeded.Entities["gchat-row"].ID)]
	require.NotEmpty(t, gchat.VenueTags)
	assert.Equal(t, "group_chat", gchat.VenueTags[0].Kind)
	assert.True(t, gchat.VenueTags[0].IsGroup)
	whatsapp := enriched[uuid.MustParse(seeded.Entities["whatsapp-row"].ID)]
	require.NotEmpty(t, whatsapp.VenueTags)
	assert.Equal(t, "dm", whatsapp.VenueTags[0].Kind)
	assert.Equal(t, "DM", whatsapp.VenueTags[0].Label)
	assert.False(t, whatsapp.VenueTags[0].IsGroup)
	email := enriched[uuid.MustParse(seeded.Entities["email-row"].ID)]
	require.NotEmpty(t, email.VenueTags)
	assert.Equal(t, "email_thread", email.VenueTags[0].Kind)
	assert.NotEmpty(t, email.VenueTags[0].Label)
	commsMessages, err := commsRepo.ListByInteractionIDs(ctx, []uuid.UUID{uuid.MustParse(seeded.Entities["email-row"].ID)})
	require.NoError(t, err)
	require.Len(t, commsMessages, 1)
	require.NotNil(t, commsMessages[0].Subject)
	assert.NotEmpty(t, *commsMessages[0].Subject)
	assert.Equal(t, *commsMessages[0].Subject, email.VenueTags[0].Label)
	assert.Equal(t, *commsMessages[0].Subject, email.Label)

	var gcalRow repository.Interaction
	for _, row := range rows {
		if row.Source == repository.InteractionSourceGCal {
			gcalRow = row
			break
		}
	}
	require.NotEqual(t, uuid.Nil, gcalRow.ID)
	gcal := enriched[gcalRow.ID]
	require.NotNil(t, gcal.Event)
	assert.NotNil(t, gcal.Event.Location)
	assert.NotEmpty(t, *gcal.Event.Location)
	assert.NotNil(t, gcal.Event.HTMLLink)
	assert.NotEmpty(t, *gcal.Event.HTMLLink)

	callRow := findDeclaredRow(t, rows, seeded.Entities["call-row"].ID)
	assert.Equal(t, repository.InteractionDirectionInbound, callRow.Direction)
	for _, handle := range []string{"email-row", "gchat-row", "whatsapp-row", "telegram-row", "messages-row"} {
		assert.Equal(t, repository.InteractionDirectionInbound, findDeclaredRow(t, rows, seeded.Entities[handle].ID).Direction, handle)
	}
	for _, handle := range []string{"manual-row", "todoist-row", "anarlog-row"} {
		assert.Equal(t, repository.InteractionDirectionOutbound, findDeclaredRow(t, rows, seeded.Entities[handle].ID).Direction, handle)
	}
	assert.Equal(t, repository.InteractionDirectionMutual, gcalRow.Direction)
	call := enriched[callRow.ID].Call
	require.NotNil(t, call)
	assert.Equal(t, repository.PhoneCallServiceVoice, call.Service)
	require.NotNil(t, call.Answered)
	assert.True(t, *call.Answered)
	assert.Equal(t, 372, call.DurationSeconds)
	for _, handle := range []string{"manual-row", "todoist-row"} {
		row := enriched[uuid.MustParse(seeded.Entities[handle].ID)]
		assert.Equal(t, "none", row.ContentKind, handle)
		assert.Empty(t, row.VenueTags, handle)
	}
	assert.Equal(t, "meeting_note", enriched[uuid.MustParse(seeded.Entities["anarlog-row"].ID)].ContentKind)
}

func TestInteractionsDeclare_PagingWorld(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	database, ctx := newSyntheticDB(t)
	seeded, err := declare.Run(ctx, database, "IXN-001", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	rows, err := repository.NewInteractionRepository(database.Queries).ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 25)
	tieA := findDeclaredRow(t, rows, seeded.Entities["tie-a"].ID)
	tieB := findDeclaredRow(t, rows, seeded.Entities["tie-b"].ID)
	assert.Equal(t, tieA.OccurredAt, tieB.OccurredAt)
	tieIDs := []string{seeded.Entities["tie-a"].ID, seeded.Entities["tie-b"].ID}
	sort.Sort(sort.Reverse(sort.StringSlice(tieIDs)))
	expected := make([]string, 0, 25)
	for i := 1; i <= 19; i++ {
		expected = append(expected, seeded.Entities[fmt.Sprintf("recent-%02d", i)].ID)
	}
	expected = append(expected, tieIDs...)
	for i := 1; i <= 4; i++ {
		expected = append(expected, seeded.Entities[fmt.Sprintf("old-%d", i)].ID)
	}
	actual := make([]string, len(rows))
	for i, row := range rows {
		actual[i] = row.ID.String()
	}
	assert.Equal(t, expected, actual)
}

func TestInteractionsDeclare_UpcomingWorld(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	database, ctx := newSyntheticDB(t)
	seeded, err := declare.Run(ctx, database, "IXN-006", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	events, err := repository.NewCalendarEventRepository(database.Queries).ListUpcomingEventsForContact(ctx, subject, accelerated.GetCurrentTime(), 250)
	require.NoError(t, err)
	assert.Len(t, events, 13)
	expectedEventIDs := make([]string, 0, 13)
	for _, handle := range []string{"underway", "future-01", "future-02", "future-03", "future-04", "future-05", "future-06", "future-07", "future-08", "future-09", "future-10", "future-11", "future-12"} {
		expectedEventIDs = append(expectedEventIDs, seeded.Entities[handle].ID)
	}
	actualEventIDs := make([]string, len(events))
	upcomingIDs := make(map[string]struct{}, len(events))
	for i, event := range events {
		actualEventIDs[i] = event.ID.String()
		upcomingIDs[event.ID.String()] = struct{}{}
	}
	assert.Equal(t, expectedEventIDs, actualEventIDs)
	rows, err := repository.NewInteractionRepository(database.Queries).ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, seeded.Entities["past-row"].ID, rows[0].ID.String())
	for _, row := range rows {
		if row.SourceRef == nil {
			continue
		}
		_, isUpcomingEvent := upcomingIDs[*row.SourceRef]
		assert.False(t, isUpcomingEvent, "interaction source_ref must not reference an upcoming event")
	}
}

func TestInteractionsDeclare_HonestWorld(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	database, ctx := newSyntheticDB(t)
	seeded, err := declare.Run(ctx, database, "IXN-009", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	ir := repository.NewInteractionRepository(database.Queries)
	silent := uuid.MustParse(seeded.Entities["silent"].ID)
	rows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: silent, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows)
	count, err := ir.CountContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: silent})
	require.NoError(t, err)
	assert.Zero(t, count)
	events, err := repository.NewCalendarEventRepository(database.Queries).ListUpcomingEventsForContact(ctx, silent, accelerated.GetCurrentTime(), 250)
	require.NoError(t, err)
	assert.Empty(t, events)
	dup := uuid.MustParse(seeded.Entities["dup-host"].ID)
	rows, err = ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: dup, Limit: 100})
	require.NoError(t, err)
	assert.Len(t, rows, 4)
	count, err = ir.CountContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: dup})
	require.NoError(t, err)
	assert.Equal(t, int64(4), count)
	events, err = repository.NewCalendarEventRepository(database.Queries).ListUpcomingEventsForContact(ctx, dup, accelerated.GetCurrentTime(), 250)
	require.NoError(t, err)
	assert.Empty(t, events)
	findGCalRow := func(eventHandle string) repository.Interaction {
		for _, row := range rows {
			if row.Source == repository.InteractionSourceGCal && row.SourceRef != nil && *row.SourceRef == seeded.Entities[eventHandle].ID {
				return row
			}
		}
		require.FailNow(t, "declared calendar interaction missing", eventHandle)
		return repository.Interaction{}
	}
	gcalA := findGCalRow("gcal-dup-a")
	gcalB := findGCalRow("gcal-dup-b")
	assert.Equal(t, gcalA.OccurredAt, gcalB.OccurredAt)
	assert.NotEqual(t, gcalA.ID, gcalB.ID)
	manualA := findDeclaredRow(t, rows, seeded.Entities["manual-dup-a"].ID)
	manualB := findDeclaredRow(t, rows, seeded.Entities["manual-dup-b"].ID)
	assert.Equal(t, manualA.OccurredAt, manualB.OccurredAt)
	assert.NotEqual(t, manualA.ID, manualB.ID)
	enriched, err := interactionContentService(database).EnrichPage(ctx, []repository.Interaction{manualA, manualB})
	require.NoError(t, err)
	assert.Equal(t, "Logged interaction", enriched[manualA.ID].Label)
	assert.Equal(t, "Logged interaction", enriched[manualB.ID].Label)
	future := uuid.MustParse(seeded.Entities["future-only"].ID)
	rows, err = ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: future, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows)
	count, err = ir.CountContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: future})
	require.NoError(t, err)
	assert.Zero(t, count)
	events, err = repository.NewCalendarEventRepository(database.Queries).ListUpcomingEventsForContact(ctx, future, accelerated.GetCurrentTime(), 250)
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

func TestInteractionsDeclare_CleanupEmptiesNamespace(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	database, ctx := newSyntheticDB(t)
	seeded, err := declare.Run(ctx, database, "IXN-002", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	res, err := declare.CleanupNamespaces(ctx, database, []string{seeded.Namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	require.Equal(t, declare.StatusCleaned, res.Results[seeded.Namespace].Status)
	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	rows, err := repository.NewInteractionRepository(database.Queries).ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, rows)
	callUniqueID := factory.SyntheticSourcePrefix + seeded.Namespace + "-call-call-row"
	call, err := repository.NewPhoneCallRepository(database.Queries).GetCallByUniqueID(ctx, callUniqueID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.Nil(t, call)
}

func findDeclaredRow(t *testing.T, rows []repository.Interaction, id string) repository.Interaction {
	t.Helper()
	wanted := uuid.MustParse(id)
	for _, row := range rows {
		if row.ID == wanted {
			return row
		}
	}
	require.FailNow(t, "declared interaction missing", id)
	return repository.Interaction{}
}

func TestInteractionsDeclare_DrilldownWorld(t *testing.T) {
	t.Parallel()
	requireInteractionsDeclareIntegration(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	seeded, err := declare.Run(ctx, database, "IXN-004", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	speaker := uuid.MustParse(seeded.Entities["speaker"].ID)
	groupID := uuid.MustParse(seeded.Entities["group-thread"].ID)
	ir := repository.NewInteractionRepository(database.Queries)
	rows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	var subjectRows []repository.Interaction
	for _, row := range rows {
		if row.Source == repository.InteractionSourceGChat {
			subjectRows = append(subjectRows, row)
		}
	}
	require.Len(t, subjectRows, 1)
	assert.Equal(t, groupID, subjectRows[0].ID)

	contentSvc := interactionContentService(database)
	enriched, err := contentSvc.EnrichPage(ctx, subjectRows)
	require.NoError(t, err)
	assert.True(t, enriched[groupID].IsGroup)
	assert.Equal(t, "group_chat", enriched[groupID].VenueTags[0].Kind)
	assert.Equal(t, "messages", enriched[groupID].ContentKind)
	assert.Equal(t, 2, enriched[groupID].MessageCount)
	content, err := contentSvc.GetContent(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, "messages", content.Kind)
	require.Len(t, content.Messages, 3)
	wantTimes := []time.Time{seeded.Anchor.Truncate(time.Second).Add(-24*time.Hour - 2*time.Minute).UTC(), seeded.Anchor.Truncate(time.Second).Add(-24*time.Hour - time.Minute).UTC(), seeded.Anchor.Truncate(time.Second).Add(-24 * time.Hour).UTC()}
	senders := []string{content.Messages[0].Sender, content.Messages[1].Sender, content.Messages[2].Sender}
	assert.Equal(t, wantTimes[0], content.Messages[0].SentAt.UTC())
	assert.Equal(t, wantTimes[1], content.Messages[1].SentAt.UTC())
	assert.Equal(t, wantTimes[2], content.Messages[2].SentAt.UTC())
	assert.NotEqual(t, senders[0], senders[1])
	assert.Equal(t, senders[0], senders[2])
	assert.NotEmpty(t, content.Messages[0].Body)
	assert.NotEmpty(t, content.Messages[1].Body)
	assert.NotEmpty(t, content.Messages[2].Body)
	assert.NotEqual(t, content.Messages[0].Body, content.Messages[1].Body)
	assert.NotEqual(t, content.Messages[1].Body, content.Messages[2].Body)
	assert.Contains(t, content.Messages[1].Body, "<b>not-markup</b>")
	assert.Equal(t, content.Messages[0].VenueKey, content.Messages[1].VenueKey)
	assert.Equal(t, content.Messages[1].VenueKey, content.Messages[2].VenueKey)

	speakerRows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: speaker, Limit: 100})
	require.NoError(t, err)
	require.Len(t, speakerRows, 1)
	speakerContent, err := contentSvc.GetContent(ctx, speakerRows[0].ID)
	require.NoError(t, err)
	require.Len(t, speakerContent.Messages, 1)
}

func TestInteractionsDeclare_NotesWorld(t *testing.T) {
	t.Parallel()
	requireInteractionsDeclareIntegration(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	seeded, err := declare.Run(ctx, database, "IXN-005", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	eventID := uuid.MustParse(seeded.Entities["noted-meeting"].ID)
	noteIDs := []uuid.UUID{uuid.MustParse(seeded.Entities["note-a"].ID), uuid.MustParse(seeded.Entities["note-b"].ID)}
	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	ir := repository.NewInteractionRepository(database.Queries)
	rows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, repository.InteractionSourceGCal, rows[0].Source)
	require.NotNil(t, rows[0].SourceRef)
	assert.Equal(t, eventID.String(), *rows[0].SourceRef)
	enriched, err := interactionContentService(database).EnrichPage(ctx, rows)
	require.NoError(t, err)
	assert.Equal(t, "meeting_note", enriched[rows[0].ID].ContentKind)
	content, err := interactionContentService(database).GetContent(ctx, rows[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "meeting_note", content.Kind)
	assert.Empty(t, content.Messages)
	require.Len(t, content.MeetingNotes, 2)
	notes, err := repository.NewMeetingNoteRepository(database.Queries).ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{eventID})
	require.NoError(t, err)
	require.Len(t, notes, 2)
	assert.ElementsMatch(t, noteIDs, []uuid.UUID{notes[0].ID, notes[1].ID})
	prefix := factory.SyntheticSourcePrefix + seeded.Namespace + "-"
	expectedNotes := map[string]struct {
		title   string
		summary string
		memo    string
	}{
		"note-a": {
			title:   strings.ToLower(fmt.Sprintf("%snote-a note", prefix)),
			summary: fmt.Sprintf("Summary for %snote-a", prefix),
			memo:    fmt.Sprintf("Memo for %snote-a <i>not-markup</i>", prefix),
		},
		"note-b": {
			title:   strings.ToLower(fmt.Sprintf("%snote-b note", prefix)),
			summary: fmt.Sprintf("Summary for %snote-b", prefix),
			memo:    fmt.Sprintf("Memo for %snote-b <i>not-markup</i>", prefix),
		},
	}
	notesByID := make(map[uuid.UUID]repository.MeetingNote, len(notes))
	for _, note := range notes {
		notesByID[note.ID] = note
	}
	events, err := repository.NewCalendarEventRepository(database.Queries).ListByIDs(ctx, []uuid.UUID{eventID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	for handle, expected := range expectedNotes {
		noteID := uuid.MustParse(seeded.Entities[handle].ID)
		note, ok := notesByID[noteID]
		require.True(t, ok, handle)
		assert.Equal(t, repository.LinkageStateLinked, note.LinkageState)
		require.NotNil(t, note.LinkedKind)
		assert.Equal(t, repository.LinkedKindEvent, *note.LinkedKind)
		require.NotNil(t, note.LinkedID)
		assert.Equal(t, eventID, *note.LinkedID)
		assert.Regexp(t, "^[0-9a-f]{64}$", note.InputHash)
		assert.Regexp(t, "^[0-9a-f]{64}$", note.ResolvedSetHash)
		assert.NotNil(t, note.LastContentHash)
		require.NotNil(t, note.MacHostID)
		host, hostErr := repository.NewMacHostRepository(database.Queries).GetHost(ctx, *note.MacHostID)
		require.NoError(t, hostErr)
		assert.Equal(t, factory.SyntheticSourcePrefix+seeded.Namespace+"-host", host.Hostname)
		assert.True(t, events[0].StartTime.Equal(note.MeetingAt))
		require.NotNil(t, note.Title)
		require.NotNil(t, note.Summary)
		require.NotNil(t, note.Memo)
		assert.Equal(t, expected.title, *note.Title, handle)
		assert.Equal(t, expected.summary, *note.Summary, handle)
		assert.Equal(t, expected.memo, *note.Memo, handle)
	}
	// The content reader preserves each note's exact field values.
	contentByTitle := make(map[string]struct {
		summary string
		memo    string
	}, len(content.MeetingNotes))
	for _, note := range content.MeetingNotes {
		require.NotNil(t, note.Title)
		require.NotNil(t, note.Summary)
		require.NotNil(t, note.Memo)
		contentByTitle[*note.Title] = struct {
			summary string
			memo    string
		}{summary: *note.Summary, memo: *note.Memo}
	}
	for _, expected := range expectedNotes {
		actual, ok := contentByTitle[expected.title]
		require.True(t, ok, expected.title)
		assert.Equal(t, expected.summary, actual.summary, expected.title)
		assert.Equal(t, expected.memo, actual.memo, expected.title)
	}
	assert.Zero(t, mustCountUnmatchedExternalContacts(t, database, ctx, "anarlog_title"))
}

func mustCountUnmatchedExternalContacts(t *testing.T, database *db.Database, ctx context.Context, source string) int64 {
	t.Helper()
	n, err := repository.NewExternalContactRepository(database.Queries).CountUnmatched(ctx, source, true)
	require.NoError(t, err)
	return n
}

func TestInteractionsDeclare_NotesCleanup(t *testing.T) {
	requireInteractionsDeclareIntegration(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	t0 := accelerated.GetCurrentTime().Truncate(time.Second)
	restore := accelerated.SetNowForTest(func() time.Time { return t0 })
	defer restore()
	first, err := declare.Run(ctx, database, "IXN-005", "notes-one", factory.DefaultSeed)
	require.NoError(t, err)
	restore()
	t1 := t0.Add(time.Hour)
	restore = accelerated.SetNowForTest(func() time.Time { return t1 })
	defer restore()
	second, err := declare.Run(ctx, database, "IXN-005", "notes-two", factory.DefaultSeed)
	require.NoError(t, err)
	restore()
	support := repository.NewSyntheticSupportRepository(database.Queries)
	noteRepo := repository.NewMeetingNoteRepository(database.Queries)
	rootIDs := func(result declare.Result) []uuid.UUID {
		var out []uuid.UUID
		eventID := uuid.MustParse(result.Entities["noted-meeting"].ID)
		notes, e := noteRepo.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{eventID})
		require.NoError(t, e)
		for _, note := range notes {
			ids, e := support.ListEventIdsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceAnarlogSessions, note.AnarlogSessionID.String())
			require.NoError(t, e)
			require.Len(t, ids, 1)
			out = append(out, ids[0])
		}
		return out
	}
	noteIDs := func(result declare.Result) []uuid.UUID {
		eventID := uuid.MustParse(result.Entities["noted-meeting"].ID)
		notes, e := noteRepo.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{eventID})
		require.NoError(t, e)
		ids := make([]uuid.UUID, 0, len(notes))
		for _, note := range notes {
			ids = append(ids, note.ID)
		}
		return ids
	}
	firstRoots, secondRoots := rootIDs(first), rootIDs(second)
	require.Len(t, firstRoots, 2)
	require.Len(t, secondRoots, 2)
	firstNotes, secondNotes := noteIDs(first), noteIDs(second)
	require.Len(t, firstNotes, 2)
	require.Len(t, secondNotes, 2)
	eventRepo := repository.NewEventRepository(database.Queries)
	cleanup, err := declare.CleanupNamespaces(ctx, database, []string{first.Namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	assert.Equal(t, declare.StatusCleaned, cleanup.Results[first.Namespace].Status)
	for _, id := range firstRoots {
		_, e := eventRepo.GetEvent(ctx, id)
		assert.ErrorIs(t, e, db.ErrNotFound)
	}
	for _, id := range secondRoots {
		_, e := eventRepo.GetEvent(ctx, id)
		assert.NoError(t, e)
	}
	firstEventID := uuid.MustParse(first.Entities["noted-meeting"].ID)
	secondEventID := uuid.MustParse(second.Entities["noted-meeting"].ID)
	firstLinkedNotes, err := noteRepo.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{firstEventID})
	require.NoError(t, err)
	assert.Empty(t, firstLinkedNotes)
	secondLinkedNotes, err := noteRepo.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{secondEventID})
	require.NoError(t, err)
	require.Len(t, secondLinkedNotes, 2)
	assert.ElementsMatch(t, secondNotes, []uuid.UUID{secondLinkedNotes[0].ID, secondLinkedNotes[1].ID})
	cleanup, err = declare.CleanupNamespaces(ctx, database, []string{second.Namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	assert.Equal(t, declare.StatusCleaned, cleanup.Results[second.Namespace].Status)
	for _, id := range secondRoots {
		_, e := eventRepo.GetEvent(ctx, id)
		assert.ErrorIs(t, e, db.ErrNotFound)
	}
	secondLinkedNotes, err = noteRepo.ListByLinkedRefs(ctx, repository.LinkedKindEvent, []uuid.UUID{secondEventID})
	require.NoError(t, err)
	assert.Empty(t, secondLinkedNotes)
	count, err := repository.NewEventRepository(database.Queries).CountBySource(ctx, repository.InteractionSourceAnarlogSessions)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestInteractionsDeclare_VenueFilterWorld(t *testing.T) {
	t.Parallel()
	requireInteractionsDeclareIntegration(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	seeded, err := declare.Run(ctx, database, "IXN-007", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)
	for handle, kind := range map[string]string{
		"subject": "contact", "gchat-thread": "interaction", "email-thread": "interaction", "future-noise": "calendar_event",
	} {
		entity, ok := seeded.Entities[handle]
		require.True(t, ok, "manifest missing %s", handle)
		assert.Equal(t, kind, entity.Kind, handle)
		_, err := uuid.Parse(entity.ID)
		require.NoError(t, err, handle)
	}

	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	ir := repository.NewInteractionRepository(database.Queries)
	rows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 2, "the future event must not mint an interaction")

	contentSvc := interactionContentService(database)
	venues, err := contentSvc.ListContactVenues(ctx, subject)
	require.NoError(t, err)
	require.Len(t, venues, 2)
	byKind := make(map[string]service.VenueTag, len(venues))
	for _, venue := range venues {
		byKind[venue.Kind] = venue
	}
	group, ok := byKind["group_chat"]
	require.True(t, ok)
	assert.True(t, group.IsGroup)
	email, ok := byKind["email_thread"]
	require.True(t, ok)
	assert.False(t, email.IsGroup)
	assert.NotEqual(t, group.Key, email.Key)

	for handle, venue := range map[string]service.VenueTag{"gchat-thread": group, "email-thread": email} {
		row := findDeclaredRow(t, rows, seeded.Entities[handle].ID)
		content, err := contentSvc.GetContent(ctx, row.ID)
		require.NoError(t, err, handle)
		require.NotEmpty(t, content.Messages, handle)
		for _, message := range content.Messages {
			assert.Equal(t, venue.Key, message.VenueKey, handle)
		}
	}
	assert.NotEqual(t,
		findDeclaredRow(t, rows, seeded.Entities["gchat-thread"].ID).ID,
		findDeclaredRow(t, rows, seeded.Entities["email-thread"].ID).ID,
	)
}

func TestInteractionsDeclare_DateSpreadWorld(t *testing.T) {
	t.Parallel()
	requireInteractionsDeclareIntegration(t)
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	seeded, err := declare.Run(ctx, database, "IXN-008", syntheticNS(t), factory.DefaultSeed)
	require.NoError(t, err)

	interactionAgeByHandle := make(map[string]int, 24)
	for i := 1; i <= 20; i++ {
		interactionAgeByHandle[fmt.Sprintf("recent-%02d", i)] = i
	}
	for _, n := range []int{29, 31, 89, 91} {
		interactionAgeByHandle[fmt.Sprintf("edge-%d", n)] = n
	}
	for handle := range interactionAgeByHandle {
		entity, ok := seeded.Entities[handle]
		require.True(t, ok, "manifest missing %s", handle)
		assert.Equal(t, "interaction", entity.Kind, handle)
	}
	for _, handle := range []string{"bound-a", "bound-b", "bound-c"} {
		entity, ok := seeded.Entities[handle]
		require.True(t, ok, "manifest missing %s", handle)
		assert.Equal(t, "calendar_event", entity.Kind, handle)
	}

	subject := uuid.MustParse(seeded.Entities["subject"].ID)
	ir := repository.NewInteractionRepository(database.Queries)
	rows, err := ir.ListContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 24, "upcoming events must not mint interactions")
	for handle, age := range interactionAgeByHandle {
		row := findDeclaredRow(t, rows, seeded.Entities[handle].ID)
		want := seeded.Anchor.Truncate(time.Second).Add(-time.Duration(age) * 24 * time.Hour).UTC()
		assert.Equal(t, want, row.OccurredAt.UTC(), handle)
	}
	from30 := seeded.Anchor.Add(-720 * time.Hour)
	count, err := ir.CountContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, From: &from30})
	require.NoError(t, err)
	assert.Equal(t, int64(21), count)
	from90 := seeded.Anchor.Add(-2160 * time.Hour)
	count, err = ir.CountContactInteractionsFiltered(ctx, repository.InteractionListFilterParams{ContactID: subject, From: &from90})
	require.NoError(t, err)
	assert.Equal(t, int64(23), count)

	eventIDs := make([]uuid.UUID, 0, 3)
	for _, handle := range []string{"bound-a", "bound-b", "bound-c"} {
		eventIDs = append(eventIDs, uuid.MustParse(seeded.Entities[handle].ID))
	}
	events, err := repository.NewCalendarEventRepository(database.Queries).ListByIDs(ctx, eventIDs)
	require.NoError(t, err)
	require.Len(t, events, 3)
	byID := make(map[uuid.UUID]repository.CalendarEvent, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	for i, handle := range []string{"bound-a", "bound-b", "bound-c"} {
		event := byID[uuid.MustParse(seeded.Entities[handle].ID)]
		require.NotEqual(t, uuid.Nil, event.ID, handle)
		assert.Equal(t, 0, event.StartTime.UTC().Hour(), handle)
		assert.Equal(t, 0, event.StartTime.UTC().Minute(), handle)
		assert.Equal(t, 0, event.StartTime.UTC().Second(), handle)
		assert.Equal(t, 0, event.StartTime.UTC().Nanosecond(), handle)
		assert.Equal(t, seeded.Anchor.UTC().Truncate(24*time.Hour).AddDate(0, 0, []int{2, 5, 9}[i]), event.StartTime.UTC(), handle)
		assert.True(t, event.StartTime.After(seeded.Anchor), handle)
		assert.Equal(t, event.StartTime.Add(time.Hour), event.EndTime, handle)
	}

	upcomingIDs := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		upcomingIDs[id.String()] = struct{}{}
	}
	for _, row := range rows {
		if row.SourceRef != nil {
			_, isUpcomingEvent := upcomingIDs[*row.SourceRef]
			assert.False(t, isUpcomingEvent, "interaction source_ref must not reference an upcoming event")
		}
	}
}

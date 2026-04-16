package google

import (
	"context"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// capturingBus records every envelope passed to Publish. Used to assert
// SourceID construction without running a full *events.Bus.
type capturingBus struct {
	calls []*events.Envelope
}

func (b *capturingBus) Publish(_ context.Context, env *events.Envelope) error {
	b.calls = append(b.calls, env)
	return nil
}

// TestPublishCalendarAttended_SourceIDPerContact asserts two publishes
// sharing the same calendar event ID but targeting different contacts
// produce distinct SourceID values — prerequisite for N-contact events
// in cutover (plan Decision 11).
func TestPublishCalendarAttended_SourceIDPerContact(t *testing.T) {
	bus := &capturingBus{}
	eventID := uuid.NewString()
	contactA := uuid.New()
	contactB := uuid.New()
	occurredAt := time.Date(2026, 4, 10, 14, 0, 0, 0, time.UTC)

	require.NoError(t, publishCalendarAttended(context.Background(), bus, contactA, eventID, occurredAt, nil))
	require.NoError(t, publishCalendarAttended(context.Background(), bus, contactB, eventID, occurredAt, nil))
	require.Len(t, bus.calls, 2)

	idA := bus.calls[0].SourceID
	idB := bus.calls[1].SourceID
	require.NotEqual(t, idA, idB,
		"two different contacts must produce different SourceIDs for the same event")
	require.True(t, strings.HasPrefix(idA, eventID+":"),
		"SourceID must be <eventID>:<contactID>, got %q", idA)
	require.Equal(t, eventID+":"+contactA.String(), idA)
	require.Equal(t, eventID+":"+contactB.String(), idB)
}

// TestPublishCalendarAttended_RetriesSamePair_SameSourceID asserts the
// publisher-side idempotency primitive: retrying the same (event, contact)
// pair produces the same SourceID, which the event table's partial
// unique index uses to dedupe.
func TestPublishCalendarAttended_RetriesSamePair_SameSourceID(t *testing.T) {
	bus := &capturingBus{}
	eventID := uuid.NewString()
	contact := uuid.New()
	occurredAt := time.Date(2026, 4, 10, 14, 0, 0, 0, time.UTC)

	require.NoError(t, publishCalendarAttended(context.Background(), bus, contact, eventID, occurredAt, nil))
	require.NoError(t, publishCalendarAttended(context.Background(), bus, contact, eventID, occurredAt, nil))
	require.Len(t, bus.calls, 2)
	require.Equal(t, bus.calls[0].SourceID, bus.calls[1].SourceID)
}

// TestPublishCalendarAttended_EnvelopeFields asserts the full envelope
// shape matches the consumer's expectations (Source=gcal, Kind=calendar.attended,
// ObservedAt=occurredAt).
func TestPublishCalendarAttended_EnvelopeFields(t *testing.T) {
	bus := &capturingBus{}
	eventID := uuid.NewString()
	contact := uuid.New()
	occurredAt := time.Date(2026, 4, 10, 14, 30, 0, 0, time.UTC)

	require.NoError(t, publishCalendarAttended(context.Background(), bus, contact, eventID, occurredAt, nil))
	require.Len(t, bus.calls, 1)

	env := bus.calls[0]
	require.Equal(t, repository.InteractionSourceGCal, env.Source)
	require.Equal(t, events.KindCalendarAttended, env.Kind)
	require.True(t, occurredAt.Equal(env.ObservedAt))
	require.Contains(t, string(env.Payload), eventID)
	require.Contains(t, string(env.Payload), contact.String())
}

// TestPublishCalendarAttended_CarriesTitleInPayload asserts that the
// calendar event's title flows through the payload so the consumer can
// populate interaction.description. Fixes Codex PR 6 P2 regression where
// the pre-cutover description-from-title was silently dropped.
func TestPublishCalendarAttended_CarriesTitleInPayload(t *testing.T) {
	bus := &capturingBus{}
	eventID := uuid.NewString()
	contact := uuid.New()
	occurredAt := time.Date(2026, 4, 10, 14, 0, 0, 0, time.UTC)
	title := "Quarterly sync with Alice"

	require.NoError(t, publishCalendarAttended(context.Background(), bus, contact, eventID, occurredAt, &title))
	require.Len(t, bus.calls, 1)

	env := bus.calls[0]
	var payload events.CalendarAttendedPayload
	require.NoError(t, events.Unmarshal(env, &payload))
	require.NotNil(t, payload.Title)
	require.Equal(t, title, *payload.Title)
}

// TestPublishCalendarAttended_NilTitleOmittedFromJSON asserts that
// nil title (calendar event without a summary) produces a payload whose
// JSON elides the `title` field so the consumer sees a nil Title pointer.
func TestPublishCalendarAttended_NilTitleOmittedFromJSON(t *testing.T) {
	bus := &capturingBus{}
	eventID := uuid.NewString()
	contact := uuid.New()
	occurredAt := time.Date(2026, 4, 10, 14, 0, 0, 0, time.UTC)

	require.NoError(t, publishCalendarAttended(context.Background(), bus, contact, eventID, occurredAt, nil))
	require.Len(t, bus.calls, 1)

	env := bus.calls[0]
	require.NotContains(t, string(env.Payload), `"title"`,
		"omitempty should elide nil title from payload JSON")
}

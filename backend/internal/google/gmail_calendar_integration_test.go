//go:build integration_testdb

// Coverage for the calendar-message exclusion (ING-046, IMP-050), driven
// through the REAL GmailSyncProvider.Sync and ScanIdentifier against the
// package's PostgreSQL clone. The synthetic toolkit seeds every contact, builds
// every Gmail payload and wires the provider to its harness (fake fetcher, real
// bus); the Gmail API is the one thing the test cannot run.
package google_test

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type calendarEnv struct {
	ctx       context.Context
	h         *synthetic.Harness
	gen       *factory.Generator
	eventRepo *repository.EventRepository
}

func newCalendarEnv(t *testing.T) *calendarEnv {
	t.Helper()
	database := testsupport.OpenDatabase(t)
	ctx := t.Context()
	h := synthetic.NewHarnessForNamespace(t, ctx, database, "cal"+uuid.NewString()[:8], factory.DefaultSeed)
	return &calendarEnv{
		ctx:       ctx,
		h:         h,
		gen:       h.Generator(),
		eventRepo: repository.NewEventRepository(database.Queries),
	}
}

// seed writes a contact spec through the harness.
func (e *calendarEnv) seed(t *testing.T, spec factory.ContactSpec) *repository.Contact {
	t.Helper()
	contact, err := e.h.SeedContact(e.ctx, spec)
	require.NoError(t, err)
	return contact
}

// knownContact seeds a contact with one email method.
func (e *calendarEnv) knownContact(t *testing.T) (*repository.Contact, factory.ContactSpec) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	return e.seed(t, spec), spec
}

// stranger is an address no seeded contact owns.
func (e *calendarEnv) stranger() factory.GmailAddress {
	return factory.GmailAddress{Email: e.gen.Contact(factory.WithEmail()).Email}
}

// provider serves msgs, as Gmail search would for a window or identifier chunk
// that contains them all.
func (e *calendarEnv) provider(t *testing.T, msgs ...factory.GmailMessageSpec) *google.GmailSyncProvider {
	t.Helper()
	provider, err := e.h.GmailProvider(msgs...)
	require.NoError(t, err)
	return provider
}

// sync runs one Sync pass from a backfill floor before every message, then
// drains the jobs it enqueued.
func (e *calendarEnv) sync(t *testing.T, provider *google.GmailSyncProvider, account string) {
	t.Helper()
	_, err := provider.Sync(e.ctx, &repository.SyncState{
		Source:    google.GmailSourceName,
		AccountID: &account,
		Metadata:  map[string]any{"backfill_since": e.gen.Anchor().AddDate(0, 0, -2).UTC().Format("2006-01-02")},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, e.h.DrainGateB(e.ctx))
}

// scanIdentifier runs the rematch scan for one contact address, then drains
// the jobs it enqueued.
func (e *calendarEnv) scanIdentifier(t *testing.T, provider *google.GmailSyncProvider, account string, contactID uuid.UUID, addr string) {
	t.Helper()
	norm := matching.NormalizeEmail(addr)
	_, err := provider.ScanIdentifier(e.ctx, account, norm, map[string][]uuid.UUID{norm: {contactID}}, map[string]struct{}{account: {}}, nil, 0)
	require.NoError(t, err)
	require.NoError(t, e.h.DrainGateB(e.ctx))
}

// emailEvent reports whether an email.* event was published for (message,
// contact).
func (e *calendarEnv) emailEvent(t *testing.T, externalID string, contactID uuid.UUID) bool {
	t.Helper()
	_, err := e.eventRepo.FindEventBySource(e.ctx, repository.InteractionSourceEmail, externalID+":"+contactID.String())
	if errors.Is(err, db.ErrNotFound) {
		return false
	}
	require.NoError(t, err)
	return true
}

// calendarLayouts are the MIME layouts that carry calendar data.
var calendarLayouts = []struct {
	name   string
	layout factory.GmailCalendarLayout
}{
	{"inline part with no filename", factory.GmailCalendarInline},
	{"part nested inside a multipart part", factory.GmailCalendarNested},
	{"named attachment", factory.GmailCalendarAttachment},
}

// scanPaths runs the two scan paths that store Gmail messages over msgs.
var scanPaths = []struct {
	name string
	scan func(t *testing.T, e *calendarEnv, contactID uuid.UUID, addr string, msgs ...factory.GmailMessageSpec)
}{
	{"Sync", func(t *testing.T, e *calendarEnv, _ uuid.UUID, _ string, msgs ...factory.GmailMessageSpec) {
		e.sync(t, e.provider(t, msgs...), msgs[0].AccountID)
	}},
	{"ScanIdentifier", func(t *testing.T, e *calendarEnv, contactID uuid.UUID, addr string, msgs ...factory.GmailMessageSpec) {
		e.scanIdentifier(t, e.provider(t, msgs...), msgs[0].AccountID, contactID, addr)
	}},
}

func TestGmailCalendarMessage_NotIngested(t *testing.T) {
	for _, path := range scanPaths {
		for _, layout := range calendarLayouts {
			for _, outbound := range []bool{false, true} {
				direction := "inbound"
				var opts []factory.MessageOption
				if outbound {
					direction = "outbound"
					opts = append(opts, factory.WithOutbound())
				}
				t.Run(path.name+"/"+layout.name+"/"+direction, func(t *testing.T) {
					// spec: ING-046.no-email-stored, ING-046.no-interaction-or-cadence
					t.Parallel()
					e := newCalendarEnv(t)
					contact, spec := e.knownContact(t)
					invite := e.gen.GmailMessageWithShape(spec, factory.MatchSeeded, factory.GmailMessageShape{Calendar: layout.layout}, opts...)

					path.scan(t, e, contact.ID, spec.Email, invite)

					rows, err := e.h.CommsRepo().ListByContact(e.ctx, contact.ID)
					require.NoError(t, err)
					require.Empty(t, rows, "a calendar message stores no email row")
					require.False(t, e.emailEvent(t, invite.ExternalID, contact.ID), "a calendar message publishes no email event")
				})
			}
		}
	}
}

func TestGmailCalendarThread_PlainReplyStored(t *testing.T) {
	for _, path := range scanPaths {
		t.Run(path.name, func(t *testing.T) {
			// spec: ING-046.non-calendar-reply-stored
			t.Parallel()
			e := newCalendarEnv(t)
			contact, spec := e.knownContact(t)
			invite := e.gen.GmailMessageWithShape(spec, factory.MatchSeeded, factory.GmailMessageShape{Calendar: factory.GmailCalendarInline}, factory.WithOutbound())
			reply := e.gen.GmailMessageWithShape(spec, factory.MatchSeeded, factory.GmailMessageShape{ThreadID: invite.Message.ThreadId})

			path.scan(t, e, contact.ID, spec.Email, invite, reply)

			rows, err := e.h.CommsRepo().ListByContact(e.ctx, contact.ID)
			require.NoError(t, err)
			require.Len(t, rows, 1, "only the plain reply is stored")
			require.Equal(t, reply.ExternalID, rows[0].ExternalID)
			require.Equal(t, "inbound", rows[0].Direction)
			require.True(t, e.emailEvent(t, reply.ExternalID, contact.ID), "the plain reply publishes its email event")
			require.False(t, e.emailEvent(t, invite.ExternalID, contact.ID))
		})
	}
}

func TestGmailCalendarMessage_NoCreateCandidate(t *testing.T) {
	// spec: IMP-050.no-create-candidate
	t.Parallel()
	e := newCalendarEnv(t)
	_, sender := e.knownContact(t)
	calendarGuest, plainGuest := e.stranger(), e.stranger()
	invite := e.gen.GmailMessageWithShape(sender, factory.MatchSeeded, factory.GmailMessageShape{Calendar: factory.GmailCalendarInline, To: []factory.GmailAddress{calendarGuest}})
	// The control: the same trusted sender's plain message proposes its unknown
	// recipient.
	plain := e.gen.GmailMessageWithShape(sender, factory.MatchSeeded, factory.GmailMessageShape{To: []factory.GmailAddress{plainGuest}})
	provider := e.provider(t, invite, plain)
	provider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.h.ContactRepo(), e.h.ExternalContactRepo()))

	e.sync(t, provider, invite.AccountID)

	row, err := e.h.ExternalContactRepo().GetBySource(e.ctx, google.ParticipantSource, matching.NormalizeEmail(calendarGuest.Email), nil)
	require.NoError(t, err)
	require.Nil(t, row, "a calendar message proposes no create candidate")
	row, err = e.h.ExternalContactRepo().GetBySource(e.ctx, google.ParticipantSource, matching.NormalizeEmail(plainGuest.Email), nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the plain message proposes its unknown recipient")
}

func TestGmailCalendarMessage_NoCorrespondenceEvidence(t *testing.T) {
	// spec: IMP-050.no-correspondence-evidence
	t.Parallel()
	e := newCalendarEnv(t)
	plainSender, plainSpec := e.knownContact(t)
	_, calendarSpec := e.knownContact(t)
	// The unknown participant's display name strong-matches this contact, which
	// makes the participant a link candidate.
	match := e.seed(t, e.gen.Contact(factory.WithNoMethods()))
	participant := factory.GmailAddress{Name: match.FullName, Email: e.stranger().Email}
	cc := []factory.GmailAddress{participant}
	plain := e.gen.GmailMessageWithShape(plainSpec, factory.MatchSeeded, factory.GmailMessageShape{Cc: cc})
	// Two calendar messages from a second contact: counted, they would make it
	// the most frequent co-occurring contact.
	invite1 := e.gen.GmailMessageWithShape(calendarSpec, factory.MatchSeeded, factory.GmailMessageShape{Calendar: factory.GmailCalendarInline, Cc: cc})
	invite2 := e.gen.GmailMessageWithShape(calendarSpec, factory.MatchSeeded, factory.GmailMessageShape{Calendar: factory.GmailCalendarNested, ThreadID: invite1.Message.ThreadId, Cc: cc})
	provider := e.provider(t, plain, invite1, invite2)
	provider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.h.ContactRepo(), e.h.ExternalContactRepo()))

	e.sync(t, provider, plain.AccountID)

	row, err := e.h.ExternalContactRepo().GetBySource(e.ctx, google.CorrespondenceSource, matching.NormalizeEmail(participant.Email), nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the plain message makes the participant a link candidate")
	require.EqualValues(t, 1, row.Metadata["message_count"], "calendar messages add no message count")
	co, ok := row.Metadata["co_occurring_contact"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, plainSender.ID.String(), co["id"], "calendar messages add no co-occurrence evidence")
}

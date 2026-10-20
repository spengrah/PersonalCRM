//go:build integration_testdb

// Coverage for the calendar-message exclusion (ING-046, IMP-050), driven
// through the REAL GmailSyncProvider.Sync and ScanIdentifier against the
// package's PostgreSQL clone, a real *events.Bus on an insert-only River client
// (never started, so no job drains), and a fake gmailFetcher (the Gmail API is
// the one thing the test cannot run).
package google

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/gmail/v1"
)

// calendarEnv bundles a provider wired to the package clone plus the
// repositories its assertions read.
type calendarEnv struct {
	ctx          context.Context
	gen          *factory.Generator
	provider     *GmailSyncProvider
	commsRepo    *repository.CommsMessageRepository
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	eventRepo    *repository.EventRepository
	externalRepo *repository.ExternalContactRepository
	me           string
}

func newCalendarEnv(t *testing.T) *calendarEnv {
	t.Helper()
	database := testsupport.OpenDatabase(t)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	eventRepo := repository.NewEventRepository(database.Queries)

	// Insert-only: the bus enqueues each email.* consumer job in the publish tx,
	// and nothing drains them.
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{TestOnly: true})
	require.NoError(t, err)

	gen := factory.NewGenerator(factory.DefaultSeed, "cal"+uuid.NewString()[:8])
	e := &calendarEnv{
		ctx:          t.Context(),
		gen:          gen,
		provider:     NewGmailSyncProvider(nil, commsRepo, events.NewBus(database.Pool, riverClient, eventRepo), database.Pool),
		commsRepo:    commsRepo,
		contactRepo:  contactRepo,
		methodRepo:   repository.NewContactMethodRepository(database.Queries),
		eventRepo:    eventRepo,
		externalRepo: repository.NewExternalContactRepository(database.Queries),
		me:           gen.Prefix() + "me@synthetic.example",
	}
	e.provider.SetMeSetForTest(map[string]struct{}{e.me: {}})
	return e
}

// seedContact writes a factory ContactSpec and its methods through the
// repositories.
func (e *calendarEnv) seedContact(t *testing.T, spec factory.ContactSpec) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: spec.FullName, Cadence: spec.Cadence})
	require.NoError(t, err)
	for _, m := range spec.Methods {
		_, err := e.methodRepo.CreateContactMethod(e.ctx, repository.CreateContactMethodRequest{
			ContactID: contact.ID, Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary,
		})
		require.NoError(t, err)
	}
	return contact
}

// knownContact seeds a contact with one email method and returns it with that
// address.
func (e *calendarEnv) knownContact(t *testing.T) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	return e.seedContact(t, spec), spec.Email
}

// mailbox serves every message for any query, as Gmail search would for a
// window or identifier chunk that contains them all.
func (e *calendarEnv) mailbox(msgs ...*gmail.Message) {
	e.provider.SetFetcherFactoryForTest(func(context.Context, string) (gmailFetcher, error) {
		return mailboxFetcher(msgs), nil
	})
}

type mailboxFetcher []*gmail.Message

func (m mailboxFetcher) ListMessageIDs(_ context.Context, _, pageToken string) ([]gmailMessageRef, string, error) {
	if pageToken != "" {
		return nil, "", nil
	}
	refs := make([]gmailMessageRef, 0, len(m))
	for _, msg := range m {
		refs = append(refs, gmailMessageRef{ID: msg.Id, ThreadID: msg.ThreadId})
	}
	return refs, "", nil
}

func (m mailboxFetcher) GetMessage(_ context.Context, id string) (*gmail.Message, error) {
	for _, msg := range m {
		if msg.Id == id {
			return msg, nil
		}
	}
	return nil, errors.New("message not found")
}

// sync runs one Sync pass from a backfill floor before every test message.
func (e *calendarEnv) sync(t *testing.T) {
	t.Helper()
	_, err := e.provider.Sync(e.ctx, &repository.SyncState{
		Source:    GmailSourceName,
		AccountID: &e.me,
		Metadata:  map[string]any{"backfill_since": "2023-11-01"},
	}, nil)
	require.NoError(t, err)
}

// scanIdentifier runs the rematch scan for one contact address.
func (e *calendarEnv) scanIdentifier(t *testing.T, contactID uuid.UUID, addr string) {
	t.Helper()
	norm := matching.NormalizeEmail(addr)
	_, err := e.provider.ScanIdentifier(e.ctx, e.me, norm, map[string][]uuid.UUID{norm: {contactID}}, map[string]struct{}{e.me: {}}, nil, 0)
	require.NoError(t, err)
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

// testMessageEpochMillis sits inside the first Sync pass after the backfill
// floor.
const testMessageEpochMillis = 1700000100000

// fakeMail builds a message whose payload is root, carrying buildMessage's
// headers and a Message-ID of "<externalID>".
func fakeMail(t *testing.T, id, threadID, from string, to, cc []string, externalID string, root *gmail.MessagePart) *gmail.Message {
	msg := buildMessage(t, id, threadID, from, to, cc, nil, "Subject", "body", "<"+externalID+">", testMessageEpochMillis)
	root.Headers = msg.Payload.Headers
	msg.Payload = root
	return msg
}

// calendarLeaf is an inline text/calendar part with no filename, the shape
// Outlook and Apple Calendar send.
func calendarLeaf() *gmail.MessagePart {
	ics := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n"
	return &gmail.MessagePart{MimeType: "text/calendar", Body: &gmail.MessagePartBody{Data: b64url(ics), Size: int64(len(ics))}}
}

func multipart(mime string, parts ...*gmail.MessagePart) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: mime, Parts: parts}
}

// calendarShapes are the MIME layouts that carry calendar data.
var calendarShapes = []struct {
	name string
	root func() *gmail.MessagePart
}{
	{"inline part with no filename", func() *gmail.MessagePart {
		return multipart("multipart/alternative", plainTextPart("body"), calendarLeaf())
	}},
	{"part nested inside a multipart part", func() *gmail.MessagePart {
		return multipart("multipart/mixed",
			multipart("multipart/alternative", plainTextPart("body"), htmlPart("<p>body</p>"), calendarLeaf()),
			attachmentPart("invite.ics", "application/ics", 64))
	}},
	{"named attachment", func() *gmail.MessagePart {
		return multipart("multipart/mixed", plainTextPart("body"), attachmentPart("invite.ics", "text/calendar", 64))
	}},
}

// scanPaths runs the two scan paths that store Gmail messages.
var scanPaths = []struct {
	name string
	scan func(t *testing.T, e *calendarEnv, contactID uuid.UUID, addr string)
}{
	{"Sync", func(t *testing.T, e *calendarEnv, _ uuid.UUID, _ string) { e.sync(t) }},
	{"ScanIdentifier", func(t *testing.T, e *calendarEnv, contactID uuid.UUID, addr string) {
		e.scanIdentifier(t, contactID, addr)
	}},
}

func TestGmailCalendarMessage_NotIngested(t *testing.T) {
	for _, path := range scanPaths {
		for _, shape := range calendarShapes {
			for _, outbound := range []bool{false, true} {
				direction := "inbound"
				if outbound {
					direction = "outbound"
				}
				t.Run(path.name+"/"+shape.name+"/"+direction, func(t *testing.T) {
					// spec: ING-046.no-email-stored, ING-046.no-interaction-or-cadence
					t.Parallel()
					e := newCalendarEnv(t)
					contact, addr := e.knownContact(t)
					externalID := e.gen.Prefix() + "invite@synthetic.example"
					from, to := addr, e.me
					if outbound {
						from, to = e.me, addr
					}
					e.mailbox(fakeMail(t, "g-invite", "thr-invite", from, []string{to}, nil, externalID, shape.root()))

					path.scan(t, e, contact.ID, addr)

					rows, err := e.commsRepo.ListByContact(e.ctx, contact.ID)
					require.NoError(t, err)
					require.Empty(t, rows, "a calendar message stores no email row")
					require.False(t, e.emailEvent(t, externalID, contact.ID), "a calendar message publishes no email event")
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
			contact, addr := e.knownContact(t)
			inviteID := e.gen.Prefix() + "invite@synthetic.example"
			replyID := e.gen.Prefix() + "reply@synthetic.example"
			e.mailbox(
				fakeMail(t, "g-invite", "thr-invite", e.me, []string{addr}, nil, inviteID, calendarShapes[0].root()),
				fakeMail(t, "g-reply", "thr-invite", addr, []string{e.me}, nil, replyID,
					multipart("multipart/alternative", plainTextPart("sounds good"), htmlPart("<p>sounds good</p>"))),
			)

			path.scan(t, e, contact.ID, addr)

			rows, err := e.commsRepo.ListByContact(e.ctx, contact.ID)
			require.NoError(t, err)
			require.Len(t, rows, 1, "only the plain reply is stored")
			require.Equal(t, replyID, rows[0].ExternalID)
			require.Equal(t, "inbound", rows[0].Direction)
			require.True(t, e.emailEvent(t, replyID, contact.ID), "the plain reply publishes its email event")
			require.False(t, e.emailEvent(t, inviteID, contact.ID))
		})
	}
}

func TestGmailCalendarMessage_NoCreateCandidate(t *testing.T) {
	// spec: IMP-050.no-create-candidate
	t.Parallel()
	e := newCalendarEnv(t)
	e.provider.SetCorrespondenceDiscoverer(NewCorrespondenceDiscoverer(e.contactRepo, e.externalRepo))
	_, sender := e.knownContact(t)
	calendarGuest := e.gen.Prefix() + "calendar-guest@synthetic.example"
	plainGuest := e.gen.Prefix() + "plain-guest@synthetic.example"
	e.mailbox(
		fakeMail(t, "g-invite", "thr-invite", sender, []string{e.me, calendarGuest}, nil,
			e.gen.Prefix()+"invite@synthetic.example", calendarShapes[0].root()),
		// The control: the same trusted sender's plain message proposes its
		// unknown recipient.
		fakeMail(t, "g-plain", "thr-plain", sender, []string{e.me, plainGuest}, nil,
			e.gen.Prefix()+"plain@synthetic.example", plainTextPart("body")),
	)

	e.sync(t)

	row, err := e.externalRepo.GetBySource(e.ctx, ParticipantSource, matching.NormalizeEmail(calendarGuest), nil)
	require.NoError(t, err)
	require.Nil(t, row, "a calendar message proposes no create candidate")
	row, err = e.externalRepo.GetBySource(e.ctx, ParticipantSource, matching.NormalizeEmail(plainGuest), nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the plain message proposes its unknown recipient")
}

func TestGmailCalendarMessage_NoCorrespondenceEvidence(t *testing.T) {
	// spec: IMP-050.no-correspondence-evidence
	t.Parallel()
	e := newCalendarEnv(t)
	e.provider.SetCorrespondenceDiscoverer(NewCorrespondenceDiscoverer(e.contactRepo, e.externalRepo))
	plainSender, plainAddr := e.knownContact(t)
	_, calendarAddr := e.knownContact(t)
	// The unknown participant's display name strong-matches this contact, which
	// makes the participant a link candidate.
	match := e.seedContact(t, e.gen.Contact(factory.WithNoMethods()))
	participant := match.FullName + " <" + e.gen.Prefix() + "participant@synthetic.example>"
	other := e.gen.Prefix() + "other@synthetic.example"
	e.mailbox(
		fakeMail(t, "g-plain", "thr-plain", plainAddr, []string{other}, []string{participant},
			e.gen.Prefix()+"plain@synthetic.example", plainTextPart("body")),
		// Two calendar messages from a second contact: counted, they would make
		// it the most frequent co-occurring contact.
		fakeMail(t, "g-invite-1", "thr-invite", calendarAddr, []string{other}, []string{participant},
			e.gen.Prefix()+"invite-1@synthetic.example", calendarShapes[0].root()),
		fakeMail(t, "g-invite-2", "thr-invite", calendarAddr, []string{other}, []string{participant},
			e.gen.Prefix()+"invite-2@synthetic.example", calendarShapes[1].root()),
	)

	e.sync(t)

	row, err := e.externalRepo.GetBySource(e.ctx, CorrespondenceSource, matching.NormalizeEmail(e.gen.Prefix()+"participant@synthetic.example"), nil)
	require.NoError(t, err)
	require.NotNil(t, row, "the plain message makes the participant a link candidate")
	require.EqualValues(t, 1, row.Metadata["message_count"], "calendar messages add no message count")
	co, ok := row.Metadata["co_occurring_contact"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, plainSender.ID.String(), co["id"], "calendar messages add no co-occurrence evidence")
}

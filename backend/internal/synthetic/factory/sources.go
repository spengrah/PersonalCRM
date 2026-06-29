package factory

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	calendarapi "google.golang.org/api/calendar/v3"
	chat "google.golang.org/api/chat/v1"
	gmailapi "google.golang.org/api/gmail/v1"
)

// All source-payload factories take a target ContactSpec (the seeded contact the
// payload should match when intent == MatchSeeded) and a MatchIntent. For
// MatchUnknown the factory addresses an unknown identifier the seeded contacts
// do NOT own, exercising the per-source pending / match-only path.

// MessageOption tunes a source-message payload. The only knob today is the
// message's age — how far before the generator anchor it is timestamped — so the
// same contact can be replayed repeatedly with its interactions spread back over
// time (weeks/months) instead of all landing in one ~1h window.
type MessageOption func(*messageConfig)

type messageConfig struct {
	// age is an EXTRA backward offset added on top of each source's small default
	// offset (e.g. Gmail's −2h). Zero leaves the default (the most-recent message);
	// growing it across a replay loop spreads older interactions back over time.
	age time.Duration
}

// WithMessageAge shifts a source message's timestamp further back by age, added
// on top of the source's small default offset. Looping a source replay with
// growing ages spreads a contact's interactions over time. The anchor stays the
// generator's (accelerated.GetCurrentTime() by default — never time.Now()), so
// the result is still anchor-relative/deterministic. age is clamped to ≥ 0 (a
// negative age, which would date a message into the future, is treated as 0).
func WithMessageAge(age time.Duration) MessageOption {
	return func(c *messageConfig) {
		if age < 0 {
			age = 0
		}
		c.age = age
	}
}

// applyMessageOptions folds the variadic options into a config.
func applyMessageOptions(opts []MessageOption) messageConfig {
	var c messageConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// --- Gmail -----------------------------------------------------------------

// GmailMessageSpec bundles a *gmail.Message plus the account ("me") it was
// observed in. The replay adapter wires Message into a FakeGmailFetcherFuncs and
// injects AccountID as the me-set + sync account.
type GmailMessageSpec struct {
	AccountID  string // the connected account ("me")
	Message    *gmailapi.Message
	ExternalID string // RFC822 Message-ID (the comms_message.external_id)
	Intent     MatchIntent
}

// GmailMessage builds an inbound email from the target contact (MatchSeeded) or
// from an unknown correspondent (MatchUnknown → match-only: no contact, no
// interaction). SentAt is anchored before now − safety lag so the Gmail cursor
// window includes it.
func (g *Generator) GmailMessage(target ContactSpec, intent MatchIntent, opts ...MessageOption) GmailMessageSpec {
	mo := applyMessageOptions(opts)
	account := g.accountEmail()
	from := target.Email
	if intent == MatchUnknown {
		from = g.unknownEmail()
	}
	if from == "" {
		// Target has no email (e.g. phone-only contact) — fall back to an
		// addressable synthetic email so the message is well-formed.
		from = g.unknownEmail()
	}
	externalID := fmt.Sprintf("<%sgmail-%d@synthetic.example>", g.Prefix(), g.bumpSourceSeq())
	// 2h before now − safety lag (anchor-relative) so it is inside the scanned,
	// already-closed window; mo.age shifts it further back for the temporal spread.
	sentAt := g.at(-(2 * time.Hour) - mo.age)
	gmailID := g.Prefix() + "gid-" + fmt.Sprint(g.sourceIDSeq)

	msg := &gmailapi.Message{
		Id:           gmailID,
		ThreadId:     g.Prefix() + "thr-" + fmt.Sprint(g.sourceIDSeq),
		InternalDate: sentAt.UnixMilli(),
		Snippet:      "synthetic snippet",
		LabelIds:     []string{"INBOX"},
		Payload: &gmailapi.MessagePart{
			MimeType: "text/plain",
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: from},
				{Name: "To", Value: account},
				{Name: "Subject", Value: "synthetic subject"},
				{Name: "Message-ID", Value: externalID},
			},
			Body: &gmailapi.MessagePartBody{Data: encodeBody("synthetic email body"), Size: 20},
		},
	}
	// The persisted external_id strips the angle brackets.
	return GmailMessageSpec{
		AccountID:  account,
		Message:    msg,
		ExternalID: trimAngle(externalID),
		Intent:     intent,
	}
}

// --- GChat -----------------------------------------------------------------

// GChatMessageSpec bundles everything the GChat replay adapter needs to drive a
// single-message sweep through the real provider: the space, its members, the
// message, the account ("me"), and the sender→email resolution map.
type GChatMessageSpec struct {
	AccountID   string
	SpaceName   string
	Space       *chat.Space
	Members     []*chat.Membership
	Message     *chat.Message
	EmailByUser map[string]string // userName → email (for ResolvePersonEmail)
	ExternalID  string            // the message Name (comms_message.external_id)
	Intent      MatchIntent
}

// GChatMessage builds an inbound chat message from the target contact
// (MatchSeeded) or an unknown sender (MatchUnknown → match-only: no row, no
// interaction, no contact). Anchor-relative create time.
func (g *Generator) GChatMessage(target ContactSpec, intent MatchIntent, opts ...MessageOption) GChatMessageSpec {
	mo := applyMessageOptions(opts)
	account := g.accountEmail()
	seq := g.bumpSourceSeq()
	ns := g.Prefix()
	spaceName := fmt.Sprintf("spaces/%sSP-%d", ns, seq)
	msgName := fmt.Sprintf("%s/messages/%sm-%d", spaceName, ns, seq)
	createTime := g.at(-time.Hour - mo.age)

	senderUser := fmt.Sprintf("users/%ssender-%d", ns, seq)
	meUser := fmt.Sprintf("users/%sme-%d", ns, seq)

	senderEmail := target.Email
	if intent == MatchUnknown || senderEmail == "" {
		senderEmail = g.unknownEmail()
	}

	return GChatMessageSpec{
		AccountID: account,
		SpaceName: spaceName,
		Space:     &chat.Space{Name: spaceName, SpaceType: "SPACE", LastActiveTime: createTime.UTC().Format(time.RFC3339Nano)},
		Members: []*chat.Membership{
			{State: "JOINED", Member: &chat.User{Name: senderUser, Type: "HUMAN"}},
			{State: "JOINED", Member: &chat.User{Name: meUser, Type: "HUMAN"}},
		},
		Message: &chat.Message{
			Name:       msgName,
			Sender:     &chat.User{Name: senderUser, Type: "HUMAN"},
			Text:       "synthetic chat message",
			CreateTime: createTime.UTC().Format(time.RFC3339Nano),
		},
		EmailByUser: map[string]string{
			senderUser: senderEmail,
			meUser:     account,
		},
		ExternalID: msgName,
		Intent:     intent,
	}
}

// --- Google Calendar -------------------------------------------------------

// GCalEventSpec bundles a *calendar.Event plus the account ("me"). The replay
// adapter wires it into a FakeCalendarFetcherFuncs page. A PAST event (end < now)
// produces a calendar.attended interaction for matched attendees; an unmatched
// attendee becomes an external_contact import candidate.
type GCalEventSpec struct {
	AccountID   string
	Event       *calendarapi.Event
	GcalEventID string
	Intent      MatchIntent
}

// GCalEvent builds a past meeting the account attended together with the target
// contact (MatchSeeded → matched attendee + attended interaction) or with an
// unknown attendee (MatchUnknown → unmatched attendee → matched_contact_ids='{}'
// + external_contact candidate).
func (g *Generator) GCalEvent(target ContactSpec, intent MatchIntent, opts ...MessageOption) GCalEventSpec {
	mo := applyMessageOptions(opts)
	account := g.accountEmail()
	seq := g.bumpSourceSeq()
	gcalEventID := fmt.Sprintf("%sgcal-%d", g.Prefix(), seq)

	attendeeEmail := target.Email
	if intent == MatchUnknown || attendeeEmail == "" {
		attendeeEmail = g.unknownEmail()
	}
	// Past meeting: started 2h ago, ended 1h ago (anchor-relative); mo.age shifts
	// the whole meeting further back for the temporal spread.
	start := g.at(-2*time.Hour - mo.age)
	end := g.at(-1*time.Hour - mo.age)

	ev := &calendarapi.Event{
		Id:      gcalEventID,
		Summary: "synthetic meeting " + fmt.Sprint(seq),
		Status:  "confirmed",
		Start:   &calendarapi.EventDateTime{DateTime: start.Format(time.RFC3339)},
		End:     &calendarapi.EventDateTime{DateTime: end.Format(time.RFC3339)},
		Attendees: []*calendarapi.EventAttendee{
			{Email: account, Self: true, ResponseStatus: "accepted"},
			{Email: attendeeEmail, ResponseStatus: "accepted", DisplayName: "Synthetic Attendee"},
		},
	}
	return GCalEventSpec{AccountID: account, Event: ev, GcalEventID: gcalEventID, Intent: intent}
}

// --- Telegram (private chats only in Element 1) ----------------------------

// TelegramMessageSpec bundles a single private inbound tg.Message plus the
// derived peer identifiers. The replay adapter feeds it through
// MessageHandler.HandleNewMessage with a nil api client (safe for the private
// path). Group chats are deferred to a later element.
type TelegramMessageSpec struct {
	PeerUserID        int64
	PeerUsername      string
	TelegramMessageID int32
	TelegramChatID    int64
	Text              string
	SentAt            time.Time
	Intent            MatchIntent
	// MatchHandle is the telegram handle to register as the seeded contact's
	// method (MatchSeeded). Empty for MatchUnknown.
	MatchHandle string
}

// TelegramMessage builds a private inbound message from the target contact
// (MatchSeeded → matched interaction; requires the target has a telegram method)
// or an unknown peer (MatchUnknown → telegram_message.matched_contact_id IS NULL
// + discovery candidate). PeerUserID/TelegramMessageID come from this namespace's
// reserved numeric sub-blocks.
func (g *Generator) TelegramMessage(target ContactSpec, intent MatchIntent, opts ...MessageOption) TelegramMessageSpec {
	mo := applyMessageOptions(opts)
	peerUserID := g.nextPeerUserID()
	msgID := g.nextTelegramMessageID()
	// Private chat id == peer user id (1:1 chat).
	chatID := peerUserID
	sentAt := g.at(-time.Hour - mo.age)

	handle := target.TelegramHandle
	username := handle
	if intent == MatchUnknown {
		handle = ""
		username = g.telegramHandle(int(g.bumpSourceSeq()))
	} else if username == "" {
		username = g.telegramHandle(int(g.bumpSourceSeq()))
	}

	return TelegramMessageSpec{
		PeerUserID:        peerUserID,
		PeerUsername:      username,
		TelegramMessageID: msgID,
		TelegramChatID:    chatID,
		Text:              "synthetic telegram message",
		SentAt:            sentAt,
		Intent:            intent,
		MatchHandle:       handle,
	}
}

// --- Telegram group chats --------------------------------------------------

// TelegramGroupMessageSpec bundles a single inbound GROUP tg.Message plus the
// derived peer + group identifiers. The replay adapter feeds it through
// MessageHandler.HandleNewMessage with a nil api client (the group path, like
// the private path, never dereferences the api client — member count + title
// arrive via tg.Entities). ChatID is drawn from this namespace's reserved
// telegram peer band (disjoint per namespace) because telegram_chat_config keys
// on telegram_chat_id with NO namespace column. SenderUserID is a normal peer id
// (the matcher keys on it). ChatID and SenderUserID occupy disjoint ends of the
// band, and ChatID never enters the matcher, so the two id roles cannot collide.
type TelegramGroupMessageSpec struct {
	ChatID            int64
	SenderUserID      int64
	SenderUsername    string
	TelegramMessageID int32
	ChatTitle         string
	ParticipantsCount int
	Text              string
	SentAt            time.Time
	Intent            MatchIntent
	// MatchHandle is the telegram handle to register as the seeded contact's
	// method (MatchSeeded). Empty for MatchUnknown.
	MatchHandle string
}

// TelegramGroupMessage builds an inbound group message whose sender is the
// target contact (MatchSeeded → the sender's username matches the seeded
// contact's telegram handle → matched interaction) or an unknown sender
// (MatchUnknown → telegram_message.matched_contact_id IS NULL + discovery
// candidate once the per-peer message threshold is crossed). participantsCount
// is caller-controlled so a test can drive both the tracked (≤ groupMaxMembers)
// and untracked-by-size (> groupMaxMembers) cases. A fresh ChatID is allocated;
// to model a multi-message group CONVERSATION (one chat id, many messages) the
// caller threads the returned ChatID back via TelegramGroupMessageInChat.
func (g *Generator) TelegramGroupMessage(target ContactSpec, intent MatchIntent, participantsCount int) TelegramGroupMessageSpec {
	return g.TelegramGroupMessageInChat(target, intent, participantsCount, g.nextGroupChatID())
}

// TelegramGroupMessageInChat is TelegramGroupMessage with a caller-supplied
// chatID, so a sequence of messages can share ONE group chat id (the
// semantically-correct shape for a group conversation, and the way to avoid
// consuming a band slot per message). Reuse a chatID returned by a prior call.
func (g *Generator) TelegramGroupMessageInChat(target ContactSpec, intent MatchIntent, participantsCount int, chatID int64) TelegramGroupMessageSpec {
	senderUserID := g.nextPeerUserID()
	msgID := g.nextTelegramMessageID()
	sentAt := g.at(-time.Hour)

	handle := target.TelegramHandle
	username := handle
	if intent == MatchUnknown {
		handle = ""
		username = g.telegramHandle(int(g.bumpSourceSeq()))
	} else if username == "" {
		username = g.telegramHandle(int(g.bumpSourceSeq()))
	}

	return TelegramGroupMessageSpec{
		ChatID:            chatID,
		SenderUserID:      senderUserID,
		SenderUsername:    username,
		TelegramMessageID: msgID,
		ChatTitle:         g.Prefix() + "group " + fmt.Sprint(g.sourceIDSeq),
		ParticipantsCount: participantsCount,
		Text:              "synthetic telegram group message",
		SentAt:            sentAt,
		Intent:            intent,
		MatchHandle:       handle,
	}
}

// --- iMessage (raw_message.* ingest envelopes) -----------------------------

// IMessageSpec is a raw_message.received ingest envelope (already marshalled)
// plus the stable guid (messages_message.guid). The replay adapter passes the
// envelope to IngestService.IngestBatch with the revoked synthetic host id.
type IMessageSpec struct {
	Envelope *events.Envelope
	Guid     string
	Intent   MatchIntent
}

// IMessage builds a raw_message.received envelope from the target contact's
// phone (MatchSeeded → matched interaction) or an unknown handle (MatchUnknown →
// messages_message.matched_contact_id IS NULL). hostID is the revoked synthetic
// mac host the harness seeded (the payload's HostID field; the host-only kind
// allowlist requires a non-nil host).
func (g *Generator) IMessage(target ContactSpec, intent MatchIntent, hostID uuid.UUID, opts ...MessageOption) (IMessageSpec, error) {
	mo := applyMessageOptions(opts)
	seq := g.bumpSourceSeq()
	guid := fmt.Sprintf("%simsg-%d", g.Prefix(), seq)
	chatGuid := fmt.Sprintf("%schat-%d", g.Prefix(), seq)

	peerHandle := target.Phone
	if intent == MatchUnknown || peerHandle == "" {
		peerHandle = g.unknownPhone()
	}
	sentAt := g.at(-time.Hour - mo.age)

	payload := events.RawMessageReceivedPayload{
		Version:     1,
		HostID:      hostID,
		Source:      "messages",
		Guid:        guid,
		ChatID:      chatGuid,
		PeerHandle:  peerHandle,
		MessageType: "text",
		IsGroup:     false,
		SentAt:      sentAt,
	}
	raw, err := events.Marshal(events.KindRawMessageReceived, payload)
	if err != nil {
		return IMessageSpec{}, fmt.Errorf("marshal raw_message payload: %w", err)
	}
	return IMessageSpec{
		Envelope: &events.Envelope{
			Source:     "messages",
			SourceID:   guid,
			Kind:       events.KindRawMessageReceived,
			Payload:    raw,
			ObservedAt: sentAt,
		},
		Guid:   guid,
		Intent: intent,
	}, nil
}

// --- Mac Contacts (external_contact.upserted ingest envelopes) -------------

// MacContactSpec is an external_contact.upserted ingest envelope plus the stable
// entity id (external_contact.source_id). The replay adapter passes it to
// IngestService.IngestBatch.
type MacContactSpec struct {
	Envelope *events.Envelope
	EntityID string
	Intent   MatchIntent
}

// MacContact builds an external_contact.upserted envelope whose email matches
// the target contact (MatchSeeded → linked to seeded contact) or is unknown
// (MatchUnknown → external_contact.match_status='unmatched'). hostID is the
// revoked synthetic host.
func (g *Generator) MacContact(target ContactSpec, intent MatchIntent, hostID uuid.UUID) (MacContactSpec, error) {
	seq := g.bumpSourceSeq()
	entityID := fmt.Sprintf("%sec-%d", g.Prefix(), seq)

	email := target.Email
	if intent == MatchUnknown || email == "" {
		email = g.unknownEmail()
	}
	displayName := g.Prefix() + "MacContact " + fmt.Sprint(seq)

	payload := events.ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      hostID,
		Source:      "icloud_contacts",
		EntityID:    entityID,
		DisplayName: &displayName,
		Emails:      []events.ExternalContactMethodValue{{Value: email, Primary: true}},
	}
	raw, err := events.Marshal(events.KindExternalContactUpserted, payload)
	if err != nil {
		return MacContactSpec{}, fmt.Errorf("marshal external_contact payload: %w", err)
	}
	return MacContactSpec{
		Envelope: &events.Envelope{
			Source:     "icloud_contacts",
			SourceID:   entityID,
			Kind:       events.KindExternalContactUpserted,
			Payload:    raw,
			ObservedAt: g.at(-time.Hour),
		},
		EntityID: entityID,
		Intent:   intent,
	}, nil
}

// --- Todoist (fake Client SyncItem) ----------------------------------------
// Todoist has no inbound sender / no pending equivalent; MatchIntent is ignored.
// The Todoist payload factory lives in the replay adapter alongside the fake
// Client (it needs todoist.SyncItem, a leaf type, but the reconciliation shape
// is adapter-coupled). See replay/todoist.go.

// --- shared small helpers --------------------------------------------------

// accountEmail is the synthetic connected account ("me") for this namespace.
func (g *Generator) accountEmail() string {
	return fmt.Sprintf("%sme@synthetic.example", g.Prefix())
}

// unknownEmail is an addressable synthetic email no seeded contact owns.
func (g *Generator) unknownEmail() string {
	return fmt.Sprintf("%sunknown-%d@synthetic.example", g.Prefix(), g.bumpSourceSeq())
}

// unknownPhone is a synthetic phone no seeded contact owns. It is drawn from
// THIS namespace's disjoint phone sub-block (same band as phoneFor), so it can
// never collide with another namespace's seeded phone — only with an as-yet
// unissued index in this namespace, which is fine for the unknown/stranded path.
func (g *Generator) unknownPhone() string {
	return g.phoneFor()
}

// bumpSourceSeq advances and returns the shared monotonic source sequence so
// repeated source-payload calls within a run get distinct ids.
func (g *Generator) bumpSourceSeq() int {
	g.sourceIDSeq++
	return g.sourceIDSeq
}

// encodeBody base64url-encodes a Gmail message body (the wire form the provider
// decodes).
func encodeBody(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

// trimAngle strips the surrounding < > from an RFC822 Message-ID so the value
// matches the comms_message.external_id the provider persists.
func trimAngle(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")
}

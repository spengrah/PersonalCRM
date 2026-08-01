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
	// outbound flips the message's direction marker so the real provider derives an
	// OUTBOUND (I-sent-it) interaction instead of the default inbound. Each factory
	// applies it to whichever field the provider reads for direction (Gmail From/
	// LabelIds, GChat Sender, Telegram Out, iMessage kind). Direction is computed
	// provider-side, so this is purely a payload-shape change — no PRNG draw.
	outbound bool
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

// WithOutbound marks a source message as OUTBOUND (sent by the account) rather
// than the default inbound. The provider derives direction from the payload, so
// each factory flips the field the provider reads: Gmail swaps From/To + the
// INBOX→SENT label, GChat sets the sender to the me-user, Telegram sets the Out
// flag, iMessage marshals the raw_message.sent kind. Counter-neutral (draws the
// identical deterministic counters as inbound; no PRNG).
func WithOutbound() MessageOption {
	return func(c *messageConfig) {
		c.outbound = true
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

// GmailMessage builds an email between the account and the target contact
// (MatchSeeded) or an unknown correspondent (MatchUnknown → match-only: no
// contact, no interaction). Inbound by default (From=contact); WithOutbound()
// flips it to a sent message (From=account, SENT label). SentAt is anchored
// before now − safety lag so the Gmail cursor window includes it.
func (g *Generator) GmailMessage(target ContactSpec, intent MatchIntent, opts ...MessageOption) GmailMessageSpec {
	mo := applyMessageOptions(opts)
	account := g.accountEmail()
	peer := target.Email
	if intent == MatchUnknown {
		peer = g.unknownEmail()
	}
	if peer == "" {
		// Target has no email (e.g. phone-only contact) — fall back to an
		// addressable synthetic email so the message is well-formed.
		peer = g.unknownEmail()
	}
	// The provider derives direction from whether From ∈ meSet: inbound is
	// From=peer/To=account with an INBOX label; outbound swaps them and carries a
	// SENT label instead.
	from, to := peer, account
	label := "INBOX"
	if mo.outbound {
		from, to = account, peer
		label = "SENT"
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
		LabelIds:     []string{label},
		Payload: &gmailapi.MessagePart{
			MimeType: "text/plain",
			Headers: []*gmailapi.MessagePartHeader{
				{Name: "From", Value: from},
				{Name: "To", Value: to},
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

// GChatMessage builds a chat message in a space the account and the target
// contact both belong to (MatchSeeded) or with an unknown sender (MatchUnknown →
// match-only: no row, no interaction, no contact). Inbound by default (the
// contact is the sender); WithOutbound() sets the sender to the me-user, so the
// provider derives OUTBOUND and fans the message out to the contact co-member.
// Anchor-relative create time.
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

	// The message sender is the contact (inbound) by default; outbound makes the
	// account the sender while the contact stays a JOINED co-member, so the
	// provider's outbound fan-out matches the contact (see gchat classifyMessage).
	messageSender := senderUser
	if mo.outbound {
		messageSender = meUser
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
			Sender:     &chat.User{Name: messageSender, Type: "HUMAN"},
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
	// Out marks the message as OUTGOING (sent by the account). The replay adapter
	// maps it to tg.Message.Out, from which the provider derives the outbound
	// direction; for outbound it also sets FromID to self while PeerID stays the
	// peer, so peer matching still resolves the contact.
	Out bool
	// MatchHandle is the telegram handle to register as the seeded contact's
	// method (MatchSeeded). Empty for MatchUnknown.
	MatchHandle string
}

// TelegramMessage builds a private message with the target contact (MatchSeeded →
// matched interaction; requires the target has a telegram method) or an unknown
// peer (MatchUnknown → telegram_message.matched_contact_id IS NULL + discovery
// candidate). Inbound by default; WithOutbound() sets Out so the adapter drives an
// outgoing message. PeerUserID/TelegramMessageID come from this namespace's
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
		Out:               mo.outbound,
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

// IMessage builds a raw_message envelope for the target contact's phone
// (MatchSeeded → matched interaction) or an unknown handle (MatchUnknown →
// messages_message.matched_contact_id IS NULL). Received by default; WithOutbound()
// marshals the raw_message.sent kind + payload so the inline ingest handler sets
// IsOutgoing and the derived interaction is outbound. hostID is the revoked
// synthetic mac host the harness seeded (the payload's HostID field; the host-only
// kind allowlist requires a non-nil host).
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
	// events.Marshal enforces the exact payload type per kind, so an outbound
	// message MUST switch to the RawMessageSentPayload alias, not just flip the
	// kind. Both kinds share the same field shape (the alias is a defined type over
	// RawMessageReceivedPayload).
	kind := events.KindRawMessageReceived
	var raw []byte
	var err error
	if mo.outbound {
		kind = events.KindRawMessageSent
		raw, err = events.Marshal(kind, events.RawMessageSentPayload(payload))
	} else {
		raw, err = events.Marshal(kind, payload)
	}
	if err != nil {
		return IMessageSpec{}, fmt.Errorf("marshal raw_message payload: %w", err)
	}
	return IMessageSpec{
		Envelope: &events.Envelope{
			Source:     "messages",
			SourceID:   guid,
			Kind:       kind,
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

// MacContact builds an icloud_contacts external_contact.upserted envelope whose
// email matches the target contact (MatchSeeded → linked to seeded contact) or
// is unknown (MatchUnknown → external_contact.match_status='unmatched'). hostID
// is the revoked synthetic host.
func (g *Generator) MacContact(target ContactSpec, intent MatchIntent, hostID uuid.UUID) (MacContactSpec, error) {
	return g.MacContactForSource(target, intent, hostID, "icloud_contacts")
}

// MacContactForSource is MacContact parameterized by the external_contact ingest
// source. source MUST be an ingest-allowed external_contact source
// (icloud_contacts or anarlog_humans — service.externalContactAllowedSources);
// the ReplayMacContacts adapter feeds the envelope through IngestService, which
// rejects any other source. Used to emit anarlog_humans import candidates (an
// Imports subtab the icloud path does not cover) through the same ingest pipeline.
func (g *Generator) MacContactForSource(target ContactSpec, intent MatchIntent, hostID uuid.UUID, source string) (MacContactSpec, error) {
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
		Source:      source,
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
			Source:     source,
			SourceID:   entityID,
			Kind:       events.KindExternalContactUpserted,
			Payload:    raw,
			ObservedAt: g.at(-time.Hour),
		},
		EntityID: entityID,
		Intent:   intent,
	}, nil
}

// --- Direct-upsert external_contact candidates (gcontacts/gmail_correspondence) -

// ExternalContactCandidateSpec is a direct-upsert (non-ingest) external_contact
// import candidate — the shape the Google sync providers write straight through
// ExternalContactRepository.Upsert (gcontacts via contacts.go, gmail_correspondence
// via gmail_correspondence.go, gcal_attendee via calendar.go). Those sources are NOT
// ingest-allowed, so the seed mirrors the providers' write path rather than the
// ingest envelope path. The replay harness maps this neutral spec into an
// UpsertExternalContactRequest and chooses the source_id keying + extra fields per
// source. All identifiers are ns-prefixed (so the teardown's source_id-prefix sweep
// reclaims the row); every email is an unknown *.example address no seeded contact
// owns, so the upsert lands match_status='unmatched' (the Imports-queue surface).
type ExternalContactCandidateSpec struct {
	Source   string
	EntityID string // ns-prefixed id-shaped key (the source_id for id-keyed sources, e.g. gcontacts)
	// Emails is at least one unknown *.example address. Emails[0] is the primary,
	// and it is the source_id for email-keyed sources (gmail_correspondence keys on
	// the address; gcal_attendee keys on the normalized attendee email).
	Emails      []string
	Phones      []string
	DisplayName string
	FirstName   string
	LastName    string
	AccountID   string // the connected account ("me"); the harness sets it only for account-scoped sources
}

// ExternalContactCandidate builds a neutral UNMATCHED import-candidate spec for a
// direct-upsert source carrying `emails` addresses and `phones` numbers (emails is
// floored at one, because the email-keyed sources use the first address as their
// source_id). It generates deterministic ns-prefixed identifiers plus display/name
// parts; the harness's SeedExternalContactCandidate decides which become the
// external_contact.source_id and which extra fields apply for the given source.
// Deterministic (source seq), no PRNG name draw.
func (g *Generator) ExternalContactCandidate(source string, emails, phones int) ExternalContactCandidateSpec {
	seq := g.bumpSourceSeq()
	ns := g.Prefix()
	if emails < 1 {
		emails = 1
	}
	addresses := make([]string, 0, emails)
	for i := 0; i < emails; i++ {
		addresses = append(addresses, fmt.Sprintf("%scand-%d-%d@synthetic.example", ns, seq, i))
	}
	numbers := make([]string, 0, phones)
	for i := 0; i < phones; i++ {
		numbers = append(numbers, g.phoneFor())
	}
	return ExternalContactCandidateSpec{
		Source:      source,
		EntityID:    fmt.Sprintf("%scand-%d", ns, seq),
		Emails:      addresses,
		Phones:      numbers,
		DisplayName: fmt.Sprintf("%sImport Candidate %d", ns, seq),
		FirstName:   ns + "Import",
		LastName:    fmt.Sprintf("Candidate%d", seq),
		AccountID:   g.accountEmail(),
	}
}

// --- Telegram discovery candidates ------------------------------------------

// TelegramDiscoveryCandidateSpec is a telegram discovery candidate — the shape
// PeerMatcher.UpdateDiscoveryCandidates writes through
// ExternalContactRepository.UpsertTelegramDiscoveryCandidate (telegram/matcher.go).
// Its source_id is the DECIMAL peer user id, so the row carries no ns-prefixed
// string at all and the prefix sweep cannot recover it — the namespace ownership
// record is what makes it cleanable.
//
// A caller that wants an UNRESOLVED peer (no name, no handle, no methods — the
// state the Imports UI hides behind its opt-in toggle) blanks DisplayName,
// FirstName, LastName and Username; the telegram upsert's COALESCE preserves
// nothing on a first insert, so the blanked fields land NULL.
type TelegramDiscoveryCandidateSpec struct {
	// PeerUserID is drawn from this namespace's reserved peer sub-block.
	PeerUserID  int64
	DisplayName string
	FirstName   string
	LastName    string
	// Username is the '@handle' form the matcher stores in metadata.username.
	Username string
	// MessageCount is the observed conversation volume the matcher records. Any
	// row it writes has already passed its discovery threshold, so this is a
	// small plausible count above the default minimum; nothing renders it.
	MessageCount int64
	// LastMessageAt is when the conversation was last seen. The matcher records it
	// for every candidate whose last message has a timestamp — which any peer past
	// the discovery threshold has — so a candidate without it is a row the matcher
	// does not write. Anchor-relative, like every other generated instant.
	LastMessageAt time.Time
}

// TelegramDiscoveryCandidate builds a fully-identified telegram discovery
// candidate. Deterministic (source seq + the namespace peer band), no PRNG name
// draw. The handle uses underscores because a telegram username is
// [A-Za-z0-9_] and the namespace prefix carries hyphens.
func (g *Generator) TelegramDiscoveryCandidate() TelegramDiscoveryCandidateSpec {
	seq := g.bumpSourceSeq()
	ns := g.Prefix()
	first := ns + "Telegram"
	last := fmt.Sprintf("Peer%d", seq)
	return TelegramDiscoveryCandidateSpec{
		PeerUserID:    g.nextPeerUserID(),
		DisplayName:   first + " " + last,
		FirstName:     first,
		LastName:      last,
		Username:      fmt.Sprintf("@%stg%d", sanitizeHandle(ns), seq),
		MessageCount:  telegramDiscoveryMessageCount,
		LastMessageAt: g.at(-telegramDiscoveryLastMessageAge),
	}
}

// telegramDiscoveryLastMessageAge is how long before the anchor the seeded
// conversation was last seen. Recent, because a peer only reaches the discovery
// threshold through an active conversation.
const telegramDiscoveryLastMessageAge = 2 * time.Hour

// telegramDiscoveryMessageCount is the conversation volume a seeded discovery
// candidate reports. The matcher only writes a candidate once the peer is past
// its configured minimum, so any value it could have written is at least that
// minimum; the exact number is not rendered anywhere.
const telegramDiscoveryMessageCount = 5

// --- anarlog_title weak candidates ------------------------------------------

// AnarlogTitleCandidateSpec is ONE (token, session) anarlog_title pair — the
// shape anarlog.DiscoveryWriter.UpsertTitleCandidateTx writes. Its source_id is a
// SHA-256 digest of (normalized token ‖ session uuid), so like the telegram peer id
// it carries no ns-prefixed string and is recovered by the namespace ownership
// record.
//
// The token must be namespace-unique, because the Imports discovery surface
// groups by token_normalized DB-wide with no namespace scoping — two namespaces
// sharing a token would land in one grouped row and each would read the other's
// evidence in its count. NormalizedToken is the lower-cased form of DisplayToken,
// which is the invariant the grouped-row test ids depend on.
type AnarlogTitleCandidateSpec struct {
	NormalizedToken string
	DisplayToken    string
}

// AnarlogTitleCandidate builds the token pair for one anarlog_title row. Callers
// sharing a `group` share a token, which is what makes their rows ONE grouped
// candidate whose evidence count is the number of members.
//
// The token is LETTERS ONLY and uppercase-first, because every production token
// reaching UpsertTitleCandidateTx comes out of anarlog.ExtractNameTokens, whose
// keep-rule admits only 2..30 alphabetic characters starting uppercase. The
// obvious ns-prefixed form ("synth-<ns>-<group>") is a string no discovery pass
// could ever emit — hyphens, digits and a lower-case first letter are all
// excluded — so seeding it would exercise grouping against a token shape
// production never sees. The replay harness re-runs the real extractor over this
// value before writing, so a future group or namespace that leaves the grammar
// fails at seed time rather than silently.
//
// Namespace uniqueness therefore has to survive an alphabetic-only alphabet, and
// the namespace token itself cannot: it is up to 60 characters of [a-z0-9-],
// which no injective letter encoding fits inside 30. The namespace hash is
// encoded instead, in 14 base-26 letters — the full 64 bits, so the collision
// profile is far stronger than the ~800-bucket phone area code and the peer
// bucket this toolkit's isolation already rests on.
func (g *Generator) AnarlogTitleCandidate(group string) AnarlogTitleCandidateSpec {
	// Already in the writer's asciiTitleCase form (upper first, rest lower), so
	// the value seeded here and the value stored on the row are the same string.
	token := "Synth" + hashLetters(seedHash(g.namespace)) + strings.ToLower(group)
	return AnarlogTitleCandidateSpec{
		NormalizedToken: strings.ToLower(token),
		DisplayToken:    token,
	}
}

// nsTokenLetters is how many base-26 letters carry the namespace hash: 26^14 >
// 2^64, so the full hash fits with no truncation.
const nsTokenLetters = 14

// hashLetters renders a hash as fixed-width lower-case base-26, the only
// alphabet the anarlog token grammar admits.
func hashLetters(h uint64) string {
	out := make([]byte, nsTokenLetters)
	for i := nsTokenLetters - 1; i >= 0; i-- {
		out[i] = byte('a' + h%26)
		h /= 26
	}
	return string(out)
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

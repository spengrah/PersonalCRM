package whatsapp

import (
	"context"
	"regexp"
	"time"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Chat types. These are the only two values IngestedMessage.ChatType ever
// carries: every other server is refused by classifyChat rather than typed.
const (
	ChatTypePrivate = "private"
	ChatTypeGroup   = "group"
)

// Message types, derived from the envelope alone — no media byte is ever
// downloaded, so the type is what the message carried, not what it contains.
const (
	MessageTypeText     = "text"
	MessageTypePhoto    = "photo"
	MessageTypeAudio    = "audio"
	MessageTypeVideo    = "video"
	MessageTypeDocument = "document"
	MessageTypeSticker  = "sticker"
	MessageTypeOther    = "other"
)

// Drop reasons, logged when an event is acknowledged without being stored.
const (
	dropReasonProtocol = "protocol"
	dropReasonReaction = "reaction"
	dropReasonPollVote = "poll_vote"
	// dropReasonStub covers an envelope with no message at all: WhatsApp uses
	// the same wrapper for records of things that HAPPENED — a revoke, an
	// undecryptable ciphertext, a missed call, a membership or security-code
	// change — as for things that were SAID.
	dropReasonStub = "stub"
)

// phoneUserPattern is what a subscriber number looks like in a JID's user part.
// A JID whose user is not a bare number is not a phone number, whatever server
// it claims — so it falls through to the unresolved rung rather than being
// normalized into a plausible-looking identifier.
var phoneUserPattern = regexp.MustCompile(`^[0-9]{5,15}$`)

// nullUserJID is the user part WhatsApp addresses protocol and system
// records to — not a subscriber, and not anybody the CRM could ever have a
// conversation with. An envelope that names it (as the chat itself, or as a
// sender inside a group) describes no person, and must never be allowed to
// mint a discovery candidate or be attributed to a real contact.
//
// The match is on the exact user part, never a prefix: a real subscriber
// number can legitimately begin with a zero.
const nullUserJID = "0"

// isNullUserJID reports whether jid is the protocol's null user.
func isNullUserJID(jid types.JID) bool {
	return jid.User == nullUserJID
}

// normalizeServer rewrites device-domain and legacy servers onto the canonical
// user servers, so eligibility is decided on IDENTITY rather than on transport.
//
// hosted and hosted.lid are device domains for the same user identity — the
// library mints them from the hosted domain constants and treats them as
// first-class user servers — so a blind allowlist would drop real
// conversations. c.us is the library's own legacy rewrite, already mirrored in
// VerifyHistoryLIDMappings.
//
// It is applied to the chat JID AND to the peer JID before any comparison, so
// peer_handle stays stable for one human across device domains.
func normalizeServer(jid types.JID) types.JID {
	switch jid.Server {
	case types.HostedServer, types.LegacyUserServer:
		jid.Server = types.DefaultUserServer
	case types.HostedLIDServer:
		jid.Server = types.HiddenUserServer
	}
	return jid
}

// ownIdentity is the emitting session's own JIDs. Both forms are carried
// because a self-chat can be addressed either way and a phone-server own JID is
// what makes the outbound-DM direction decidable.
type ownIdentity struct {
	PN  types.JID
	LID types.JID
}

// ok reports whether at least one own JID is known. Without one the parser can
// neither reject a self-chat nor stamp the account, so the handler withholds
// the ack rather than dropping the message.
func (o ownIdentity) ok() bool {
	return !o.PN.IsEmpty() || !o.LID.IsEmpty()
}

// isSelf reports whether jid addresses the linked account itself, in either
// form and on any device domain.
func (o ownIdentity) isSelf(jid types.JID) bool {
	target := normalizeServer(jid).ToNonAD()
	if !o.PN.IsEmpty() && normalizeServer(o.PN).ToNonAD() == target {
		return true
	}
	if !o.LID.IsEmpty() && normalizeServer(o.LID).ToNonAD() == target {
		return true
	}
	return false
}

// canonicalAccountJID picks the single canonical string form of one account's
// identity: the phone-number JID when known, else the internal id, always
// normalized and non-AD.
//
// It is SHARED by the two places that must agree on that form — the parser,
// which stamps it onto every message, and the group-info seam, which reports it
// so the gate can refuse to ask the wrong account about a group. If those two
// derived it independently and disagreed about which form represents an
// account, every group message would look like it came from a different account
// and the gate would withhold its ack forever.
//
// ToNonAD is load-bearing rather than decoration: the device store's own ID is
// an AD JID and String() re-appends ":<device>" whenever the device number is
// non-zero — and that number is reassigned on every re-link. Storing the AD
// form would fragment account_id across exactly the re-pair this integration
// has to survive.
func canonicalAccountJID(pn, lid types.JID) string {
	source := pn
	if source.IsEmpty() {
		source = lid
	}
	if source.IsEmpty() {
		return ""
	}
	return normalizeServer(source).ToNonAD().String()
}

// accountJID is the value stamped onto comms_message.account_id.
func (o ownIdentity) accountJID() *string {
	out := canonicalAccountJID(o.PN, o.LID)
	if out == "" {
		return nil
	}
	return &out
}

// classifyChat applies the person-to-person allowlist to an ALREADY-NORMALIZED
// chat JID. ok=false means DROP — the event is acknowledged, because an
// ineligible chat is handled, just to no effect.
//
// It deliberately does not branch on events.Message.Info.IsGroup: the library
// sets that true for broadcast lists and the status broadcast as well as for
// g.us, so it would admit exactly the chats this allowlist exists to refuse.
func classifyChat(chat types.JID, own ownIdentity) (string, bool) {
	switch chat.Server {
	case types.GroupServer:
		return ChatTypeGroup, true
	case types.DefaultUserServer, types.HiddenUserServer:
		if own.isSelf(chat) {
			return "", false
		}
		if isNullUserJID(chat) {
			return "", false
		}
		return ChatTypePrivate, true
	default:
		// Broadcast, status broadcast, newsletter, messenger, bot, interop and
		// anything the protocol grows later: fail closed.
		return "", false
	}
}

// classifiedContent is what the envelope says about a message, with no network
// call and no media download.
type classifiedContent struct {
	MessageType   string
	Body          *string
	ReplyTargetID *string
	Drop          bool
	DropReason    string
}

// classifyMessage reads the UNWRAPPED message. whatsmeow unwraps ephemeral,
// view-once, device-sent, document-with-caption and edited envelopes before
// dispatch, so RawMessage would re-introduce every wrapper this reads through.
//
// Protocol messages, reactions and poll votes are dropped with an ack: an edit
// would try to mutate a body the staging upsert holds immutable on conflict (a
// silent no-op wearing the costume of an update), and reactions and poll votes
// are not conversational turns. A poll CREATION is a real message and stages.
func classifyMessage(msg *waE2E.Message) classifiedContent {
	if msg == nil {
		// The safe default is DROP, not "store it as other". The live path
		// never reaches this — the library does not dispatch an *events.Message
		// with no message — but the history envelope carries non-messages under
		// exactly this shape, and a zero-valued Drop would stage every missed
		// call and security notice as a bodiless conversational turn.
		return classifiedContent{Drop: true, DropReason: dropReasonStub}
	}

	switch {
	case msg.GetProtocolMessage() != nil:
		return classifiedContent{Drop: true, DropReason: dropReasonProtocol}
	case msg.GetReactionMessage() != nil || msg.GetEncReactionMessage() != nil:
		return classifiedContent{Drop: true, DropReason: dropReasonReaction}
	case msg.GetPollUpdateMessage() != nil:
		return classifiedContent{Drop: true, DropReason: dropReasonPollVote}
	}

	switch {
	case msg.GetConversation() != "":
		// A plain-text reply always arrives as an ExtendedTextMessage, so a
		// bare conversation needs no context probe.
		body := msg.GetConversation()
		return classifiedContent{MessageType: MessageTypeText, Body: &body}

	case msg.GetExtendedTextMessage() != nil:
		ext := msg.GetExtendedTextMessage()
		return classifiedContent{
			MessageType:   MessageTypeText,
			Body:          nilIfEmptyString(ext.GetText()),
			ReplyTargetID: nilIfEmptyString(ext.GetContextInfo().GetStanzaID()),
		}

	case msg.GetImageMessage() != nil:
		img := msg.GetImageMessage()
		return classifiedContent{
			MessageType:   MessageTypePhoto,
			Body:          nilIfEmptyString(img.GetCaption()),
			ReplyTargetID: nilIfEmptyString(img.GetContextInfo().GetStanzaID()),
		}

	case msg.GetVideoMessage() != nil:
		vid := msg.GetVideoMessage()
		return classifiedContent{
			MessageType:   MessageTypeVideo,
			Body:          nilIfEmptyString(vid.GetCaption()),
			ReplyTargetID: nilIfEmptyString(vid.GetContextInfo().GetStanzaID()),
		}

	case msg.GetPtvMessage() != nil:
		ptv := msg.GetPtvMessage()
		return classifiedContent{
			MessageType:   MessageTypeVideo,
			Body:          nilIfEmptyString(ptv.GetCaption()),
			ReplyTargetID: nilIfEmptyString(ptv.GetContextInfo().GetStanzaID()),
		}

	case msg.GetAudioMessage() != nil:
		return classifiedContent{
			MessageType:   MessageTypeAudio,
			ReplyTargetID: nilIfEmptyString(msg.GetAudioMessage().GetContextInfo().GetStanzaID()),
		}

	case msg.GetDocumentMessage() != nil:
		doc := msg.GetDocumentMessage()
		return classifiedContent{
			MessageType:   MessageTypeDocument,
			Body:          nilIfEmptyString(doc.GetCaption()),
			ReplyTargetID: nilIfEmptyString(doc.GetContextInfo().GetStanzaID()),
		}

	case msg.GetStickerMessage() != nil:
		return classifiedContent{
			MessageType:   MessageTypeSticker,
			ReplyTargetID: nilIfEmptyString(msg.GetStickerMessage().GetContextInfo().GetStanzaID()),
		}

	case msg.GetPollCreationMessage() != nil:
		return classifiedContent{MessageType: MessageTypeOther, Body: nilIfEmptyString(msg.GetPollCreationMessage().GetName())}
	case msg.GetPollCreationMessageV2() != nil:
		return classifiedContent{MessageType: MessageTypeOther, Body: nilIfEmptyString(msg.GetPollCreationMessageV2().GetName())}
	case msg.GetPollCreationMessageV3() != nil:
		return classifiedContent{MessageType: MessageTypeOther, Body: nilIfEmptyString(msg.GetPollCreationMessageV3().GetName())}

	default:
		return classifiedContent{MessageType: MessageTypeOther}
	}
}

// peerAltResolver is the device store's LID<->PN mapping lookup, narrowed to
// the one method the peer ladder uses. Satisfied by *store.Device.
type peerAltResolver interface {
	GetAltJID(ctx context.Context, jid types.JID) (types.JID, error)
}

// parseMessage projects a live event onto IngestedMessage.
//
// eligible=false means DROP with a true ack. It never returns an error: every
// resolution failure degrades to a nil field rather than withholding a message
// that is otherwise perfectly storable.
//
// ChatTitle and MemberCount are deliberately left nil — both would cost a
// GetGroupInfo round trip per message, and the group's title and size live in
// whatsapp_chat_config, written by the gate.
// altTimeout bounds the device-store lookup. It is a parameter rather than the
// package constant read directly so tests can shrink it without a mutable
// global that a parallel test could race.
func parseMessage(ctx context.Context, evt *events.Message, own ownIdentity, resolver peerAltResolver, altTimeout time.Duration) (IngestedMessage, string, bool) {
	chat := normalizeServer(evt.Info.Chat).ToNonAD()
	chatType, ok := classifyChat(chat, own)
	if !ok {
		return IngestedMessage{}, "", false
	}

	content := classifyMessage(evt.Message)
	if content.Drop {
		logger.Debug().
			Str("message_id", evt.Info.ID).
			Str("reason", content.DropReason).
			Msg("whatsapp: message dropped as a non-conversational turn")
		return IngestedMessage{}, "", false
	}

	msg := IngestedMessage{
		MessageID: evt.Info.ID,
		ChatJID:   chat.String(),
		ChatType:  chatType,
		// External data, stored verbatim: the acceleration rule governs clocks
		// we compute, not timestamps we receive.
		SentAt:        evt.Info.Timestamp,
		IsOutgoing:    evt.Info.IsFromMe,
		Body:          content.Body,
		MessageType:   content.MessageType,
		ReplyTargetID: content.ReplyTargetID,
		AccountJID:    own.accountJID(),
	}

	// The push name is the SENDER's, so on an outbound message it is the
	// user's own display name. Filling it there would flow the user's name into
	// the peer's display name, into source_metadata.push_name, and — via the
	// COALESCE-preserve discovery upsert — stickily onto the peer's own import
	// candidate. One outbound DM would relabel an unknown peer as the user.
	if !evt.Info.IsFromMe {
		msg.PushName = nilIfEmptyString(evt.Info.PushName)
	}

	peer, altJID, hasPeer := resolvePeer(evt, chat, chatType)
	if !hasPeer {
		// An outbound group message has no counterpart: it stages with a null
		// contact and never aggregates.
		return msg, "", true
	}

	peerStr := peer.String()
	msg.PeerJID = &peerStr

	if e164, resolved := resolvePeerPhone(ctx, peer, altJID, resolver, altTimeout); resolved {
		msg.PeerPhoneE164 = &e164
		return msg, "", true
	}
	return msg, peerStr, true
}

// resolvePeer attributes the message to its counterpart, on normalized JIDs.
// hasPeer=false is the outbound-group case, which is permanently unmatched by
// design.
func resolvePeer(evt *events.Message, chat types.JID, chatType string) (peer types.JID, altJID types.JID, hasPeer bool) {
	if chatType == ChatTypePrivate {
		// whatsmeow sets Chat to the OTHER party for a DM in both directions.
		if evt.Info.IsFromMe {
			return chat, normalizeServer(evt.Info.RecipientAlt).ToNonAD(), true
		}
		return chat, normalizeServer(evt.Info.SenderAlt).ToNonAD(), true
	}
	if evt.Info.IsFromMe {
		return types.EmptyJID, types.EmptyJID, false
	}
	sender := normalizeServer(evt.Info.Sender).ToNonAD()
	if isNullUserJID(sender) {
		// A system envelope inside a tracked group still stages — the group
		// itself is real — but it has no human counterpart, so it must not
		// carry a peer handle that could mint a discovery candidate.
		return types.EmptyJID, types.EmptyJID, false
	}
	return sender, normalizeServer(evt.Info.SenderAlt).ToNonAD(), true
}

// resolvePeerPhone walks the four-rung ladder. The order is load-bearing: each
// rung is cheaper and more certain than the next, and rung 4 (no phone) is a
// real outcome rather than a failure — a LID-only peer stages unmatched and is
// reachable through the import queue.
func resolvePeerPhone(ctx context.Context, peer, altJID types.JID, resolver peerAltResolver, altTimeout time.Duration) (string, bool) {
	// 1. The peer is already addressed by phone number.
	if candidate, ok := phoneCandidate(peer); ok {
		return candidate, true
	}
	// 2. The stanza carried the peer's alternative address.
	if candidate, ok := phoneCandidate(altJID); ok {
		return candidate, true
	}
	// 3. The device store may already know the mapping. The explicit bound
	//    matters because the dispatcher hands in an unbounded context.
	if resolver != nil && peer.Server == types.HiddenUserServer {
		altCtx, cancel := context.WithTimeout(ctx, altTimeout)
		resolved, err := resolver.GetAltJID(altCtx, peer)
		cancel()
		if err != nil {
			// The ordinary unmapped case is (EmptyJID, nil), so an error here
			// is exceptional rather than routine.
			logger.Debug().Err(err).Msg("whatsapp: LID to phone lookup failed")
		} else if candidate, ok := phoneCandidate(normalizeServer(resolved).ToNonAD()); ok {
			return candidate, true
		}
	}
	// 4. No phone. The caller reports the peer as unresolved.
	return "", false
}

// phoneCandidate accepts a JID only when it is addressed on the phone-number
// server AND its user part really is a subscriber number, then normalizes it to
// E.164. A JID that fails either test degrades to the next rung rather than
// minting a plausible-looking identifier.
func phoneCandidate(jid types.JID) (string, bool) {
	if jid.IsEmpty() || jid.Server != types.DefaultUserServer {
		return "", false
	}
	if !phoneUserPattern.MatchString(jid.User) {
		return "", false
	}
	e164 := identity.Normalize("+"+jid.User, identity.IdentifierTypeWhatsApp)
	if e164 == "" {
		return "", false
	}
	return e164, true
}

// nilIfEmptyString is the whatsapp twin of telegram's nilIfEmpty, needed for
// the same reason: an empty string is a POPULATED value to a
// COALESCE(EXCLUDED.x, …) upsert and would clobber a stored name.
func nilIfEmptyString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nilIfEmptyPtr is nilIfEmptyString for a value that is already a pointer.
func nilIfEmptyPtr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

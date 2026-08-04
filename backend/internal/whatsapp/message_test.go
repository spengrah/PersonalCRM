package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// --- fixtures ---------------------------------------------------------------

var (
	testOwnPN    = types.NewJID("15550000001", types.DefaultUserServer)
	testOwnLID   = types.NewJID("99900000001", types.HiddenUserServer)
	testPeerPN   = types.NewJID("15559876543", types.DefaultUserServer)
	testPeerLID  = types.NewJID("88800000002", types.HiddenUserServer)
	testGroupJID = types.NewJID("120363000000000001", types.GroupServer)
)

func testOwn() ownIdentity { return ownIdentity{PN: testOwnPN, LID: testOwnLID} }

// testAltTimeout keeps the bounded-lookup tests off the production 3s bound —
// the point of those tests is that a bound EXISTS, not how long it is, and
// spending the real one in wall clock buys nothing.
const testAltTimeout = 50 * time.Millisecond

// fakeAltResolver stands in for the device store's LID mapping.
type fakeAltResolver struct {
	result types.JID
	err    error
	// entered is closed on the first call and block, when non-nil, holds the
	// resolver inside it — which is how the time bound is proved without
	// sleeping for it.
	entered chan struct{}
	block   chan struct{}
	calls   int
}

func (f *fakeAltResolver) GetAltJID(ctx context.Context, _ types.JID) (types.JID, error) {
	f.calls++
	if f.entered != nil {
		close(f.entered)
		f.entered = nil
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return types.EmptyJID, ctx.Err()
		}
	}
	return f.result, f.err
}

func textEvent(id string, info types.MessageSource, body string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			ID:            id,
			MessageSource: info,
			Timestamp:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		},
		Message:    &waE2E.Message{Conversation: proto.String(body)},
		RawMessage: &waE2E.Message{Conversation: proto.String(body)},
	}
}

// --- normalizeServer --------------------------------------------------------

func TestNormalizeServer_HostedBecomesDefaultUser(t *testing.T) {
	got := normalizeServer(types.NewJID("15551234567", types.HostedServer))
	assert.Equal(t, types.DefaultUserServer, got.Server)
	assert.Equal(t, "15551234567", got.User, "only the server is rewritten")
}

func TestNormalizeServer_HostedLIDBecomesLID(t *testing.T) {
	got := normalizeServer(types.NewJID("777000111", types.HostedLIDServer))
	assert.Equal(t, types.HiddenUserServer, got.Server)
}

func TestNormalizeServer_LegacyBecomesDefaultUser(t *testing.T) {
	got := normalizeServer(types.NewJID("15551234567", types.LegacyUserServer))
	assert.Equal(t, types.DefaultUserServer, got.Server)
}

func TestNormalizeServer_UnknownServerUntouched(t *testing.T) {
	for _, server := range []string{
		types.GroupServer, types.BroadcastServer, types.NewsletterServer,
		types.MessengerServer, types.BotServer, types.InteropServer, "future.server",
	} {
		got := normalizeServer(types.NewJID("x", server))
		assert.Equal(t, server, got.Server, "%s must be left alone for the allowlist to judge", server)
	}
}

// --- classifyChat -----------------------------------------------------------

func TestChatEligibility_SelfChatRejected(t *testing.T) {
	_, ok := classifyChat(normalizeServer(testOwnPN).ToNonAD(), testOwn())
	assert.False(t, ok, "a note-to-self is not a conversation with anybody")
}

func TestChatEligibility_SelfChatInLIDFormRejected(t *testing.T) {
	_, ok := classifyChat(normalizeServer(testOwnLID).ToNonAD(), testOwn())
	assert.False(t, ok, "the same account addressed by its internal id is still the same account")
}

func TestChatEligibility_SelfChatInHostedFormRejected(t *testing.T) {
	hosted := types.NewJID(testOwnPN.User, types.HostedServer)
	_, ok := classifyChat(normalizeServer(hosted).ToNonAD(), testOwn())
	assert.False(t, ok, "a device domain does not make the account a different person")
}

func TestChatEligibility_StatusBroadcastRejected(t *testing.T) {
	_, ok := classifyChat(types.StatusBroadcastJID, testOwn())
	assert.False(t, ok)
}

func TestChatEligibility_NewsletterRejected(t *testing.T) {
	_, ok := classifyChat(types.NewJID("123", types.NewsletterServer), testOwn())
	assert.False(t, ok, "a channel is a broadcast, not a person-to-person conversation")
}

func TestChatEligibility_UnknownServerRejected(t *testing.T) {
	for _, server := range []string{types.BroadcastServer, types.MessengerServer, types.BotServer, "future.server"} {
		_, ok := classifyChat(types.NewJID("x", server), testOwn())
		assert.False(t, ok, "%s must fail closed", server)
	}
}

func TestChatEligibility_DirectAndGroupAccepted(t *testing.T) {
	chatType, ok := classifyChat(testPeerPN, testOwn())
	require.True(t, ok)
	assert.Equal(t, ChatTypePrivate, chatType)

	chatType, ok = classifyChat(testPeerLID, testOwn())
	require.True(t, ok)
	assert.Equal(t, ChatTypePrivate, chatType, "a LID-addressed peer is still a person")

	chatType, ok = classifyChat(testGroupJID, testOwn())
	require.True(t, ok)
	assert.Equal(t, ChatTypeGroup, chatType)
}

func TestChatEligibility_HostedDirectChatAccepted(t *testing.T) {
	hosted := types.NewJID("15559876543", types.HostedServer)
	chatType, ok := classifyChat(normalizeServer(hosted).ToNonAD(), testOwn())
	require.True(t, ok, "a hosted-device chat is a real conversation, not an unrecognised server")
	assert.Equal(t, ChatTypePrivate, chatType)
}

// --- classifyMessage --------------------------------------------------------

func TestClassifyMessage_TextConversation(t *testing.T) {
	got := classifyMessage(&waE2E.Message{Conversation: proto.String("hello")})
	assert.False(t, got.Drop)
	assert.Equal(t, MessageTypeText, got.MessageType)
	require.NotNil(t, got.Body)
	assert.Equal(t, "hello", *got.Body)
	assert.Nil(t, got.ReplyTargetID)
}

func TestClassifyMessage_ExtendedTextCarriesReplyTarget(t *testing.T) {
	got := classifyMessage(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        proto.String("re: that"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIGINAL-1")},
	}})
	assert.Equal(t, MessageTypeText, got.MessageType)
	require.NotNil(t, got.Body)
	assert.Equal(t, "re: that", *got.Body)
	require.NotNil(t, got.ReplyTargetID)
	assert.Equal(t, "ORIGINAL-1", *got.ReplyTargetID)
}

func TestClassifyMessage_MediaTypesFromEnvelope(t *testing.T) {
	caption := proto.String("look")
	tests := []struct {
		name     string
		msg      *waE2E.Message
		wantType string
		wantBody *string
	}{
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: caption}}, MessageTypePhoto, caption},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: caption}}, MessageTypeVideo, caption},
		{"ptv", &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Caption: caption}}, MessageTypeVideo, caption},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, MessageTypeAudio, nil},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: caption}}, MessageTypeDocument, caption},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, MessageTypeSticker, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMessage(tt.msg)
			assert.False(t, got.Drop)
			assert.Equal(t, tt.wantType, got.MessageType)
			if tt.wantBody == nil {
				assert.Nil(t, got.Body)
			} else {
				require.NotNil(t, got.Body)
				assert.Equal(t, *tt.wantBody, *got.Body)
			}
		})
	}
}

func TestClassifyMessage_ProtocolMessageDropped(t *testing.T) {
	got := classifyMessage(&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
	}})
	assert.True(t, got.Drop, "an edit or a revoke is not a new conversational turn")
	assert.Equal(t, dropReasonProtocol, got.DropReason)
}

func TestClassifyMessage_ReactionDropped(t *testing.T) {
	got := classifyMessage(&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}})
	assert.True(t, got.Drop)
	assert.Equal(t, dropReasonReaction, got.DropReason)
}

func TestClassifyMessage_PollVoteDropped(t *testing.T) {
	got := classifyMessage(&waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}})
	assert.True(t, got.Drop)
	assert.Equal(t, dropReasonPollVote, got.DropReason)
}

func TestClassifyMessage_PollCreationStagesAsOther(t *testing.T) {
	got := classifyMessage(&waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{
		Name: proto.String("Lunch where?"),
	}})
	assert.False(t, got.Drop, "creating a poll IS a conversational turn")
	assert.Equal(t, MessageTypeOther, got.MessageType)
	require.NotNil(t, got.Body)
	assert.Equal(t, "Lunch where?", *got.Body)
}

// TestClassifyMessage_ReadsUnwrappedMessageNotRaw is the guard for the field the
// parser must read: whatsmeow unwraps ephemeral / view-once / device-sent /
// edited envelopes into Message before dispatch, so reading RawMessage would
// classify every wrapped message as "other" with no body.
func TestClassifyMessage_ReadsUnwrappedMessageNotRaw(t *testing.T) {
	inner := &waE2E.Message{Conversation: proto.String("secret")}
	evt := &events.Message{
		Info: types.MessageInfo{
			ID:            "wrapped-1",
			MessageSource: types.MessageSource{Chat: testPeerPN},
			Timestamp:     accelerated.GetCurrentTime(),
		},
		Message: inner,
		// The wire form still carries the wrapper.
		RawMessage: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: inner}},
	}

	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	assert.Equal(t, MessageTypeText, msg.MessageType)
	require.NotNil(t, msg.Body)
	assert.Equal(t, "secret", *msg.Body)
}

// --- peer resolution --------------------------------------------------------

func TestPeerResolution_PhoneServerJID(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN}, "hi")
	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	assert.Empty(t, unresolved)
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164)
}

func TestPeerResolution_SenderAltCarriesPhone(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{
		Chat:      testGroupJID,
		Sender:    testPeerLID,
		SenderAlt: testPeerPN,
	}, "hi")
	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	assert.Empty(t, unresolved)
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerLID.String(), *msg.PeerJID, "the handle stays the raw peer JID")
}

func TestPeerResolution_RecipientAltOnOutboundDM(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{
		Chat:         testPeerLID,
		IsFromMe:     true,
		RecipientAlt: testPeerPN,
	}, "hi")
	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164,
		"an outbound DM carries the peer's alternative address under RecipientAlt, not SenderAlt")
}

func TestPeerResolution_GetAltJIDFallback(t *testing.T) {
	resolver := &fakeAltResolver{result: testPeerPN}
	evt := textEvent("m1", types.MessageSource{Chat: testPeerLID}, "hi")

	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), resolver, testAltTimeout)
	require.True(t, eligible)
	assert.Empty(t, unresolved)
	assert.Equal(t, 1, resolver.calls)
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164)
}

// TestPeerResolution_GetAltJIDIsTimeBounded proves the store lookup cannot hang
// the library's handler goroutine: the dispatcher hands in an UNBOUNDED context,
// so the bound has to be applied here.
func TestPeerResolution_GetAltJIDIsTimeBounded(t *testing.T) {
	resolver := &fakeAltResolver{block: make(chan struct{})}
	t.Cleanup(func() { close(resolver.block) })

	evt := textEvent("m1", types.MessageSource{Chat: testPeerLID}, "hi")

	done := make(chan struct{})
	var unresolved string
	go func() {
		defer close(done)
		_, unresolved, _ = parseMessage(context.Background(), evt, testOwn(), resolver, testAltTimeout)
	}()

	select {
	case <-done:
	case <-time.After(testAltTimeout + 5*time.Second):
		t.Fatal("the LID lookup was not bounded; it would block whatsmeow's handler goroutine")
	}
	assert.NotEmpty(t, unresolved, "a timed-out lookup degrades to unresolved, it does not fail the message")
}

func TestPeerResolution_UnresolvableLIDStagesUnmatched(t *testing.T) {
	resolver := &fakeAltResolver{err: errors.New("store down")}
	evt := textEvent("m1", types.MessageSource{Chat: testPeerLID}, "hi")

	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), resolver, testAltTimeout)
	require.True(t, eligible, "an unresolvable peer is still a real message")
	assert.Nil(t, msg.PeerPhoneE164)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerLID.String(), *msg.PeerJID)
	assert.Equal(t, testPeerLID.String(), unresolved, "the peer is reported so the gap is countable")
}

func TestPeerResolution_NonPhoneUserRejected(t *testing.T) {
	// A phone-server JID whose user part is not a subscriber number: normalizing
	// it would mint a plausible-looking identifier out of nothing.
	weird := types.NewJID("not-a-number", types.DefaultUserServer)
	evt := textEvent("m1", types.MessageSource{Chat: weird}, "hi")

	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	assert.Nil(t, msg.PeerPhoneE164)
	assert.Equal(t, weird.String(), unresolved)
}

func TestPeerResolution_HostedPeerNormalizedToDefaultServer(t *testing.T) {
	hosted := types.NewJID("15559876543", types.HostedServer)
	evt := textEvent("m1", types.MessageSource{Chat: hosted}, "hi")

	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerPN.String(), *msg.PeerJID,
		"one human must produce one peer_handle across device domains")
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164)
}

// --- parseMessage -----------------------------------------------------------

func TestParseMessage_DirectInboundPeerIsChat(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN}, "hi")
	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	assert.Equal(t, ChatTypePrivate, msg.ChatType)
	assert.False(t, msg.IsOutgoing)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerPN.String(), *msg.PeerJID)
	assert.Equal(t, testPeerPN.String(), msg.ChatJID)
}

func TestParseMessage_DirectOutboundPeerIsChat(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN, IsFromMe: true}, "hi")
	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	assert.True(t, msg.IsOutgoing)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerPN.String(), *msg.PeerJID,
		"whatsmeow names the OTHER party in Chat for a DM in both directions")
}

func TestParseMessage_GroupInboundPeerIsSender(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{
		Chat:   testGroupJID,
		Sender: testPeerPN,
	}, "hi")
	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible)
	assert.Equal(t, ChatTypeGroup, msg.ChatType)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerPN.String(), *msg.PeerJID)
	assert.Equal(t, testGroupJID.String(), msg.ChatJID)
}

func TestParseMessage_GroupOutboundHasNilPeer(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{
		Chat:     testGroupJID,
		Sender:   testOwnPN,
		IsFromMe: true,
	}, "hi")
	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)

	require.True(t, eligible, "an outbound group message is still stored")
	assert.Nil(t, msg.PeerJID, "there is no single counterpart in a group I sent to")
	assert.Nil(t, msg.PeerPhoneE164)
	assert.Empty(t, unresolved, "a nil peer is not an unresolved one")
}

// TestParseMessage_OutboundDMCarriesNoPushName is the guard for the user's own
// name leaking onto a peer's record. PushName is the SENDER's, so on an outbound
// message it is the user's — and it would flow into the peer's display name,
// into source_metadata, and stickily onto the peer's own import candidate.
func TestParseMessage_OutboundDMCarriesNoPushName(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN, IsFromMe: true}, "hi")
	evt.Info.PushName = "My Own Name"

	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	assert.Nil(t, msg.PushName, "one outbound DM must not relabel an unknown peer as the user")
}

func TestParseMessage_InboundDMCarriesPushName(t *testing.T) {
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN}, "hi")
	evt.Info.PushName = "Their Name"

	msg, _, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	require.NotNil(t, msg.PushName)
	assert.Equal(t, "Their Name", *msg.PushName)
}

// TestParseMessage_AccountJIDComesFromTheEmittingSession pins both halves: the
// account JID is derived from the identity handed in (the emitting session's),
// not read from anywhere global, and it is stored in NON-AD form so the device
// number a re-link reassigns cannot fragment account_id.
func TestParseMessage_AccountJIDComesFromTheEmittingSession(t *testing.T) {
	adOwn := testOwnPN
	adOwn.Device = 7
	evt := textEvent("m1", types.MessageSource{Chat: testPeerPN}, "hi")

	msg, _, eligible := parseMessage(context.Background(), evt, ownIdentity{PN: adOwn}, nil, testAltTimeout)
	require.True(t, eligible)
	require.NotNil(t, msg.AccountJID)
	assert.Equal(t, testOwnPN.ToNonAD().String(), *msg.AccountJID)
	assert.NotContains(t, *msg.AccountJID, ":", "an AD-form account id fragments across every re-link")

	// A different session yields a different account id — nothing global is read.
	other := types.NewJID("15550000009", types.DefaultUserServer)
	msg2, _, _ := parseMessage(context.Background(), evt, ownIdentity{PN: other}, nil, testAltTimeout)
	require.NotNil(t, msg2.AccountJID)
	assert.Equal(t, other.String(), *msg2.AccountJID)
}

// TestParseMessage_FillsEveryLiveKnowableField is the corrected inverse of the
// handler's doc comment: everything except ChatTitle and MemberCount is filled
// for a fully-populated event, and those two are nil BY DESIGN.
func TestParseMessage_FillsEveryLiveKnowableField(t *testing.T) {
	evt := &events.Message{
		Info: types.MessageInfo{
			ID: "full-1",
			MessageSource: types.MessageSource{
				Chat:      testGroupJID,
				Sender:    testPeerLID,
				SenderAlt: testPeerPN,
			},
			Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			PushName:  "Their Name",
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption:     proto.String("a photo"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIG-9")},
		}},
	}

	msg, unresolved, eligible := parseMessage(context.Background(), evt, testOwn(), nil, testAltTimeout)
	require.True(t, eligible)
	assert.Empty(t, unresolved)

	assert.Equal(t, "full-1", msg.MessageID)
	assert.Equal(t, testGroupJID.String(), msg.ChatJID)
	assert.Equal(t, ChatTypeGroup, msg.ChatType)
	assert.False(t, msg.IsOutgoing)
	assert.Equal(t, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), msg.SentAt)
	assert.Equal(t, MessageTypePhoto, msg.MessageType)
	require.NotNil(t, msg.Body)
	assert.Equal(t, "a photo", *msg.Body)
	require.NotNil(t, msg.ReplyTargetID)
	assert.Equal(t, "ORIG-9", *msg.ReplyTargetID)
	require.NotNil(t, msg.PeerJID)
	assert.Equal(t, testPeerLID.String(), *msg.PeerJID)
	require.NotNil(t, msg.PeerPhoneE164)
	assert.Equal(t, "+15559876543", *msg.PeerPhoneE164)
	require.NotNil(t, msg.PushName)
	assert.Equal(t, "Their Name", *msg.PushName)
	require.NotNil(t, msg.AccountJID)
	assert.Equal(t, testOwnPN.String(), *msg.AccountJID)

	assert.Nil(t, msg.ChatTitle, "the live parser makes no group-metadata call, by design")
	assert.Nil(t, msg.MemberCount, "the size lives in whatsapp_chat_config, written by the gate")
}

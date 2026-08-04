package whatsapp

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChatSettingsStore drives the service without a database.
type fakeChatSettingsStore struct {
	cfgs      []repository.WhatsAppChatConfig
	listErr   error
	setErr    error
	setCalls  []struct{ jid, status string }
	setResult *repository.WhatsAppChatConfig
}

func (f *fakeChatSettingsStore) ListChatConfigs(_ context.Context) ([]repository.WhatsAppChatConfig, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.cfgs, nil
}

func (f *fakeChatSettingsStore) SetChatStatus(_ context.Context, chatJID, status string) (*repository.WhatsAppChatConfig, error) {
	f.setCalls = append(f.setCalls, struct{ jid, status string }{chatJID, status})
	if f.setErr != nil {
		return nil, f.setErr
	}
	if f.setResult != nil {
		return f.setResult, nil
	}
	return &repository.WhatsAppChatConfig{ChatJID: chatJID, ChatType: ChatTypeGroup, Status: status}, nil
}

var _ ChatSettingsStore = (*fakeChatSettingsStore)(nil)

// trackingCase mirrors, case for case, the expectations the gate's own
// TestEffectiveTracked_* functions assert against the same threshold of 10.
// The point of restating them here is that the list and the gate must not be
// able to disagree: a change to the tracking rule that this table did not
// anticipate fails here and in chatconfig_test.go together.
type trackingCase struct {
	name        string
	status      string
	memberCount *int32
	wantTracked bool
}

var trackingCases = []trackingCase{
	{"tracked overrides a large group", ChatStatusTracked, int32p(500), true},
	{"ignored overrides a small group", ChatStatusIgnored, int32p(2), false},
	{"auto tracks a small group", ChatStatusAuto, int32p(4), true},
	{"auto declines a large group", ChatStatusAuto, int32p(11), false},
	{"auto includes the exact threshold", ChatStatusAuto, int32p(10), true},
	{"auto fails closed on an unknown size", ChatStatusAuto, nil, false},
	{"auto treats a zero size as unresolved", ChatStatusAuto, int32p(0), false},
	{"tracked needs no size at all", ChatStatusTracked, nil, true},
}

func TestChatSettingsService_ListComputesEffectiveTrackedFromTheGatePredicate(t *testing.T) {
	for _, tc := range trackingCases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeChatSettingsStore{cfgs: []repository.WhatsAppChatConfig{{
				ChatJID:     "123-456@g.us",
				ChatType:    ChatTypeGroup,
				Status:      tc.status,
				MemberCount: tc.memberCount,
			}}}

			chats, err := NewChatSettingsService(store, 10).ListChats(context.Background())
			require.NoError(t, err)
			require.Len(t, chats, 1)
			assert.Equal(t, tc.wantTracked, chats[0].EffectiveTracked)
			// The row itself is carried through unchanged, so the list can show
			// the stored override next to the decision it produced.
			assert.Equal(t, tc.status, chats[0].Status)
			assert.Equal(t, "123-456@g.us", chats[0].ChatJID)
		})
	}
}

// TestChatSettingsService_UnknownMemberCountIsNotTracked pins the fail-closed
// rule at the surface: a group whose size was never established is presented as
// not tracked, never as tracked-by-default.
func TestChatSettingsService_UnknownMemberCountIsNotTracked(t *testing.T) {
	store := &fakeChatSettingsStore{cfgs: []repository.WhatsAppChatConfig{{
		ChatJID: "nosize@g.us", ChatType: ChatTypeGroup, Status: ChatStatusAuto,
	}}}

	chats, err := NewChatSettingsService(store, 10).ListChats(context.Background())
	require.NoError(t, err)
	require.Len(t, chats, 1)
	assert.False(t, chats[0].EffectiveTracked)
}

func TestChatSettingsService_ExplicitOverrideWinsOverSize(t *testing.T) {
	store := &fakeChatSettingsStore{cfgs: []repository.WhatsAppChatConfig{
		{ChatJID: "big@g.us", ChatType: ChatTypeGroup, Status: ChatStatusTracked, MemberCount: int32p(900)},
		{ChatJID: "small@g.us", ChatType: ChatTypeGroup, Status: ChatStatusIgnored, MemberCount: int32p(3)},
	}}

	chats, err := NewChatSettingsService(store, 10).ListChats(context.Background())
	require.NoError(t, err)
	require.Len(t, chats, 2)
	assert.True(t, chats[0].EffectiveTracked, "an explicit track is the user's decision, whatever the size")
	assert.False(t, chats[1].EffectiveTracked, "an explicit ignore is too")
}

func TestChatSettingsService_ListPropagatesStoreError(t *testing.T) {
	store := &fakeChatSettingsStore{listErr: errors.New("connection refused")}

	_, err := NewChatSettingsService(store, 10).ListChats(context.Background())
	require.Error(t, err)
}

// TestChatSettingsService_SetStatusRecomputesTheDecision proves the echoed row
// carries the decision the NEW status produces, not the one the old status did
// — the client renders the returned row without a second read.
func TestChatSettingsService_SetStatusRecomputesTheDecision(t *testing.T) {
	store := &fakeChatSettingsStore{setResult: &repository.WhatsAppChatConfig{
		ChatJID: "big@g.us", ChatType: ChatTypeGroup, Status: ChatStatusTracked, MemberCount: int32p(900),
	}}

	chat, err := NewChatSettingsService(store, 10).SetChatStatus(context.Background(), "big@g.us", ChatStatusTracked)
	require.NoError(t, err)
	require.NotNil(t, chat)
	assert.True(t, chat.EffectiveTracked)
	require.Len(t, store.setCalls, 1)
	assert.Equal(t, "big@g.us", store.setCalls[0].jid)
	assert.Equal(t, ChatStatusTracked, store.setCalls[0].status)
}

func TestChatSettingsService_SetStatusPropagatesNotFound(t *testing.T) {
	store := &fakeChatSettingsStore{setErr: db.ErrNotFound}

	_, err := NewChatSettingsService(store, 10).SetChatStatus(context.Background(), "never-seen@g.us", ChatStatusIgnored)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound,
		"the handler branches on this to answer 404 rather than 500")
}

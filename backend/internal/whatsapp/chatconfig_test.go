package whatsapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"go.mau.fi/whatsmeow/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ------------------------------------------------------------------

type fakeChatConfigStore struct {
	cfg       *repository.WhatsAppChatConfig
	getErr    error
	upsertErr error
	upserts   []repository.WhatsAppChatConfig
	gets      int
}

func (f *fakeChatConfigStore) GetChatConfig(_ context.Context, _ string) (*repository.WhatsAppChatConfig, error) {
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.cfg == nil {
		return nil, db.ErrNotFound
	}
	out := *f.cfg
	return &out, nil
}

func (f *fakeChatConfigStore) UpsertChatConfig(_ context.Context, cfg repository.WhatsAppChatConfig) (*repository.WhatsAppChatConfig, error) {
	f.upserts = append(f.upserts, cfg)
	if f.upsertErr != nil {
		return nil, f.upsertErr
	}
	return &cfg, nil
}

type fakeGroupInfoFetcher struct {
	info    *ChatGroupInfo
	err     error
	calls   int
	account string
}

func (f *fakeGroupInfoFetcher) GroupInfo(_ context.Context, _ string) (*ChatGroupInfo, error) {
	f.calls++
	return f.info, f.err
}

func (f *fakeGroupInfoFetcher) AccountJID() string { return f.account }

// testLookupTimeout keeps the bounded-lookup test off the production 5s bound.
const testLookupTimeout = 50 * time.Millisecond

func gateWith(store *fakeChatConfigStore, fetcher GroupInfoFetcher, maxMembers int) *ChatGate {
	g := NewChatGate(store, maxMembers)
	g.BindGroupInfoSource(func() GroupInfoFetcher { return fetcher })
	g.lookupTimeout = testLookupTimeout
	return g
}

func int32p(v int32) *int32 { return &v }
func intp(v int) *int       { return &v }

const (
	testGroupChatJID = "120363000000000001@g.us"
	// testAccountJID is the account the fixtures' messages were observed by.
	testAccountJID = "15550000001@s.whatsapp.net"
)

// --- EffectiveTracked -------------------------------------------------------

func TestEffectiveTracked_TrackedOverridesLarge(t *testing.T) {
	assert.True(t, EffectiveTracked(ChatStatusTracked, intp(500), 10),
		"an explicit track is the user's decision, whatever the size")
}

func TestEffectiveTracked_IgnoredOverridesSmall(t *testing.T) {
	assert.False(t, EffectiveTracked(ChatStatusIgnored, intp(2), 10))
}

func TestEffectiveTracked_AutoSmallGroup(t *testing.T) {
	assert.True(t, EffectiveTracked(ChatStatusAuto, intp(4), 10))
}

func TestEffectiveTracked_AutoLargeGroup(t *testing.T) {
	assert.False(t, EffectiveTracked(ChatStatusAuto, intp(11), 10))
}

func TestEffectiveTracked_AutoExactThresholdTracked(t *testing.T) {
	assert.True(t, EffectiveTracked(ChatStatusAuto, intp(10), 10), "the bound is inclusive")
}

// TestEffectiveTracked_AutoNilMemberCountFailsClosed is the deliberate
// divergence from Telegram, which tracks by default when the size is unknown.
func TestEffectiveTracked_AutoNilMemberCountFailsClosed(t *testing.T) {
	assert.False(t, EffectiveTracked(ChatStatusAuto, nil, 10),
		"an unknown size must not be treated as a small group")
}

// TestEffectiveTracked_ZeroMemberCountIsUnresolved guards the wire's optional
// size attribute: an absent size yields 0 with no error, and 0 satisfies any
// "<= threshold" test, so a resolved-looking 0 would fail OPEN forever.
func TestEffectiveTracked_ZeroMemberCountIsUnresolved(t *testing.T) {
	assert.False(t, EffectiveTracked(ChatStatusAuto, intp(0), 10))
	assert.False(t, EffectiveTracked(ChatStatusAuto, intp(-3), 10))
}

// --- ShouldTrack ------------------------------------------------------------

func TestGroupGate_PrivateChatSkipsTheGateEntirely(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{}
	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), "x@s.whatsapp.net", ChatTypePrivate, testAccountJID)

	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Zero(t, store.gets, "the gate is the group-SIZE gate")
	assert.Zero(t, fetcher.calls)
}

func TestGroupGate_ResolvesAndPersistsANewGroup(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Book Club", MemberCount: 4}}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.True(t, tracked)

	require.Len(t, store.upserts, 1)
	assert.Equal(t, testGroupChatJID, store.upserts[0].ChatJID)
	require.NotNil(t, store.upserts[0].MemberCount)
	assert.EqualValues(t, 4, *store.upserts[0].MemberCount)
	require.NotNil(t, store.upserts[0].ChatTitle)
	assert.Equal(t, "Book Club", *store.upserts[0].ChatTitle)
	assert.Empty(t, store.upserts[0].Status, "an automatic write never sets a status")
	require.NotNil(t, store.upserts[0].LastLookupAt)
}

// TestGroupGate_UnknownMemberCountIsUndecided is the P0: a group whose size
// cannot be resolved right now stores nothing AND is not acknowledged.
func TestGroupGate_UnknownMemberCountIsUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: context.DeadlineExceeded}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked, "fail closed on storage")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChatGateUndecided, "a timeout is a transient failure, not a decision")
}

func TestGroupGate_LookupFailureWritesNoConfig(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: errors.New("transport down")}

	_, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.ErrorIs(t, err, ErrChatGateUndecided)
	assert.Empty(t, store.upserts, "nothing may record a false resolution")
}

func TestGroupGate_RetriesLookupOnNextMessage(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: errors.New("transport down")}
	gate := gateWith(store, fetcher, 10)

	_, err := gate.ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.ErrorIs(t, err, ErrChatGateUndecided)

	// Because the failure wrote nothing, the next message re-enters the lookup.
	fetcher.err = nil
	fetcher.info = &ChatGroupInfo{Title: "Book Club", MemberCount: 4}
	tracked, err := gate.ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Equal(t, 2, fetcher.calls)
}

func TestGroupGate_NilFetcherIsUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	gate := NewChatGate(store, 10)
	gate.BindGroupInfoSource(func() GroupInfoFetcher { return nil })

	tracked, err := gate.ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked)
	assert.ErrorIs(t, err, ErrChatGateUndecided, "no connected client is transient, not a decision")
}

func TestGroupGate_UnboundSourceIsUndecided(t *testing.T) {
	tracked, err := NewChatGate(&fakeChatConfigStore{}, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked)
	assert.ErrorIs(t, err, ErrChatGateUndecided)
}

func TestGroupGate_ConfigReadErrorIsUndecided(t *testing.T) {
	store := &fakeChatConfigStore{getErr: errors.New("database down")}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{MemberCount: 4}}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked)
	require.ErrorIs(t, err, ErrChatGateUndecided,
		"a database blip must withhold the ack, exactly as the same blip on the staging write does")
	assert.Zero(t, fetcher.calls)
}

func TestGroupGate_UpsertErrorIsUndecided(t *testing.T) {
	store := &fakeChatConfigStore{upsertErr: errors.New("database down")}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{MemberCount: 4}}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked)
	assert.ErrorIs(t, err, ErrChatGateUndecided,
		"without the write the count would be re-fetched on every message")
}

func TestGroupGate_UserIgnoredOverrideSurvivesLookup(t *testing.T) {
	store := &fakeChatConfigStore{cfg: &repository.WhatsAppChatConfig{
		ChatJID: testGroupChatJID, ChatType: ChatTypeGroup, Status: ChatStatusIgnored, MemberCount: int32p(3),
	}}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{MemberCount: 3}}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.False(t, tracked, "a small group the user ignored stays ignored")
}

// TestGroupGate_ExplicitStatusSkipsLookup is the short-circuit: an override
// needs no member count, so consulting it first is what stops an ignored group
// paying a network round trip per message for a number it discards.
func TestGroupGate_ExplicitStatusSkipsLookup(t *testing.T) {
	for _, status := range []string{ChatStatusTracked, ChatStatusIgnored} {
		t.Run(status, func(t *testing.T) {
			store := &fakeChatConfigStore{cfg: &repository.WhatsAppChatConfig{
				ChatJID: testGroupChatJID, ChatType: ChatTypeGroup, Status: status,
				// Deliberately unresolved: only the short-circuit can answer.
				MemberCount: nil,
			}}
			fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{MemberCount: 4}}

			tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
			require.NoError(t, err)
			assert.Equal(t, status == ChatStatusTracked, tracked)
			assert.Zero(t, fetcher.calls, "an override needs no member count")
			assert.Empty(t, store.upserts, "and it writes nothing")
		})
	}
}

func TestGroupGate_UnchangedRowIsNotRewritten(t *testing.T) {
	title := "Book Club"
	store := &fakeChatConfigStore{cfg: &repository.WhatsAppChatConfig{
		ChatJID: testGroupChatJID, ChatType: ChatTypeGroup, Status: ChatStatusAuto,
		MemberCount: int32p(4), ChatTitle: &title,
	}}
	fetcher := &fakeGroupInfoFetcher{}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Zero(t, fetcher.calls, "a resolved count needs no lookup")
	assert.Empty(t, store.upserts, "re-writing an unchanged row on every message is a pointless write")
}

// TestGroupGate_NotInGroupIsDecidedNotUndecided pins the permanent-vs-transient
// split. Routing a 403 into the undecided bucket turns one poisoned message into
// an unbounded redelivery loop, each iteration paying a config read and a
// network round trip on the library's serialized handler queue.
func TestGroupGate_NotInGroupIsDecidedNotUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: errors.New("wrapped: " + ErrGroupUnavailable.Error())}
	fetcher.err = errors.Join(ErrGroupUnavailable, errors.New("status 403"))

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err, "we are not in the group; that answer does not change on redelivery")
	assert.False(t, tracked)
	assert.Empty(t, store.upserts)
}

func TestGroupGate_GroupNotFoundIsDecidedNotUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: errors.Join(ErrGroupUnavailable, errors.New("status 404"))}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.False(t, tracked)
}

func TestGroupGate_UnknownSizeIsDecidedNotUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: ErrGroupSizeUnknown}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err, "the server answered; retrying re-asks a question already answered")
	assert.False(t, tracked)
}

func TestGroupGate_LargeGroupIsDecidedNotTracked(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Big", MemberCount: 42}}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err, "a resolved count above the threshold is a real decision")
	assert.False(t, tracked)
	require.Len(t, store.upserts, 1, "the resolution is persisted so the next message needs no lookup")
}

// --- the clientGroupInfoFetcher's count derivation --------------------------

func TestManagerGroupInfoFetcher_NilWhenNotConnected(t *testing.T) {
	m := newManagerForTest(t, newFakeSyncStore(), &fakeBackfillReader{})
	assert.Nil(t, m.GroupInfoFetcher(), "no client means the count stays unresolved, which is undecidable")
}

// TestManagerGroupInfo_ZeroSizeIsNotAResolvedCount is the guard for the wire's
// optional size attribute. A resolved-looking 0 would persist, satisfy any
// "<= threshold" test, and make the fail-closed gate fail OPEN forever — the
// retry never re-runs, because the count is no longer NULL.
func TestManagerGroupInfo_ZeroSizeIsNotAResolvedCount(t *testing.T) {
	_, err := projectGroupInfo(&types.GroupInfo{
		GroupName:        types.GroupName{Name: "Silent"},
		ParticipantCount: 0,
	})
	assert.ErrorIs(t, err, ErrGroupSizeUnknown)
}

func TestManagerGroupInfo_FallsBackToParticipantListLength(t *testing.T) {
	got, err := projectGroupInfo(&types.GroupInfo{
		GroupName:        types.GroupName{Name: "Book Club"},
		ParticipantCount: 0,
		Participants:     make([]types.GroupParticipant, 3),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, got.MemberCount, "the list is the answer the wire attribute omitted")
	assert.Equal(t, "Book Club", got.Title)
}

func TestManagerGroupInfo_PrefersTheReportedCount(t *testing.T) {
	got, err := projectGroupInfo(&types.GroupInfo{
		ParticipantCount: 42,
		// A truncated participant list is normal for a large group.
		Participants: make([]types.GroupParticipant, 3),
	})
	require.NoError(t, err)
	assert.Equal(t, 42, got.MemberCount)
}

func TestManagerGroupInfo_NilInfoIsUnknownSize(t *testing.T) {
	_, err := projectGroupInfo(nil)
	assert.ErrorIs(t, err, ErrGroupSizeUnknown)
}

func TestGroupGate_LookupIsTimeBounded(t *testing.T) {
	store := &fakeChatConfigStore{}
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	gate := NewChatGate(store, 10)
	gate.BindGroupInfoSource(func() GroupInfoFetcher { return blockingGroupInfoFetcher{blocked} })
	gate.lookupTimeout = testLookupTimeout

	done := make(chan error, 1)
	go func() {
		_, err := gate.ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
		done <- err
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, ErrChatGateUndecided)
	case <-time.After(testLookupTimeout + 5*time.Second):
		t.Fatal("the group lookup was not bounded; it would block whatsmeow's handler goroutine")
	}
}

type blockingGroupInfoFetcher struct{ block chan struct{} }

func (b blockingGroupInfoFetcher) AccountJID() string { return "" }

func (b blockingGroupInfoFetcher) GroupInfo(ctx context.Context, _ string) (*ChatGroupInfo, error) {
	select {
	case <-b.block:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestGroupGate_DifferentAccountIsUndecided is the re-pair guard. The group
// fetcher is resolved from the PUBLISHED session while the message was parsed
// against its EMITTING one; mid-re-pair those are different accounts, and asking
// the new account about a group only the old one was in answers "not in that
// group" — a PERMANENT answer this gate would otherwise consume and acknowledge,
// losing the message for good.
func TestGroupGate_DifferentAccountIsUndecided(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{
		account: "15559999999@s.whatsapp.net",
		err:     errors.Join(ErrGroupUnavailable, errors.New("status 403")),
	}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	assert.False(t, tracked, "fail closed on storage")
	require.ErrorIs(t, err, ErrChatGateUndecided,
		"a wrong-account answer is transient by construction — the re-pair settles — so it must not be consumed")
	assert.Zero(t, fetcher.calls, "and the wrong client is never asked at all")
	assert.Empty(t, store.upserts)
}

func TestGroupGate_SameAccountProceeds(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{
		account: testAccountJID,
		info:    &ChatGroupInfo{Title: "Book Club", MemberCount: 4},
	}

	tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, testAccountJID)
	require.NoError(t, err)
	assert.True(t, tracked)
	assert.Equal(t, 1, fetcher.calls)
}

// TestGroupGate_UnknownAccountSkipsTheComparison keeps a caller that cannot know
// its own account (a fetcher with no store, a projection built by hand) working
// exactly as before rather than permanently undecided.
func TestGroupGate_UnknownAccountSkipsTheComparison(t *testing.T) {
	for _, tc := range []struct{ name, msgAccount, liveAccount string }{
		{"message account unknown", "", "15559999999@s.whatsapp.net"},
		{"client account unknown", testAccountJID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeChatConfigStore{}
			fetcher := &fakeGroupInfoFetcher{account: tc.liveAccount, info: &ChatGroupInfo{MemberCount: 4}}

			tracked, err := gateWith(store, fetcher, 10).ShouldTrack(context.Background(), testGroupChatJID, ChatTypeGroup, tc.msgAccount)
			require.NoError(t, err)
			assert.True(t, tracked)
			assert.Equal(t, 1, fetcher.calls)
		})
	}
}

// --- ShouldTrackHistoryChat -------------------------------------------------

// TestChatGate_ShouldTrackIsUnchangedByTheHistoryRefactor replays the gate's
// decisive cases through the extracted decide() + the three-valued lookupGroup,
// with the outcomes written out rather than compared against the other entry
// point — a table that only asserted "both paths agree" could not tell a
// correct pair from a pair that broke together.
func TestChatGate_ShouldTrackIsUnchangedByTheHistoryRefactor(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *repository.WhatsAppChatConfig
		fetcher     *fakeGroupInfoFetcher
		chatType    string
		wantTracked bool
		wantErr     bool
		wantUpserts int
	}{
		{
			name:        "a private chat needs no group decision",
			fetcher:     &fakeGroupInfoFetcher{},
			chatType:    ChatTypePrivate,
			wantTracked: true,
		},
		{
			name:        "a small resolved group is tracked and persisted",
			fetcher:     &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Book Club", MemberCount: 4}},
			chatType:    ChatTypeGroup,
			wantTracked: true,
			wantUpserts: 1,
		},
		{
			name:        "an oversized resolved group is not tracked but is still persisted",
			fetcher:     &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Big", MemberCount: 400}},
			chatType:    ChatTypeGroup,
			wantTracked: false,
			wantUpserts: 1,
		},
		{
			name:        "an explicit track override skips the lookup entirely",
			cfg:         &repository.WhatsAppChatConfig{ChatJID: testGroupChatJID, Status: ChatStatusTracked},
			fetcher:     &fakeGroupInfoFetcher{err: errors.New("must not be called")},
			chatType:    ChatTypeGroup,
			wantTracked: true,
		},
		{
			name:        "an unavailable group is decided, not withheld, and writes nothing",
			fetcher:     &fakeGroupInfoFetcher{err: errors.Join(ErrGroupUnavailable, errors.New("status 403"))},
			chatType:    ChatTypeGroup,
			wantTracked: false,
		},
		{
			name:        "an unreported size is decided, not withheld, and writes nothing",
			fetcher:     &fakeGroupInfoFetcher{err: ErrGroupSizeUnknown},
			chatType:    ChatTypeGroup,
			wantTracked: false,
		},
		{
			name:     "a transient lookup failure is undecided",
			fetcher:  &fakeGroupInfoFetcher{err: errors.New("socket closed")},
			chatType: ChatTypeGroup,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeChatConfigStore{cfg: tc.cfg}
			gate := gateWith(store, tc.fetcher, 10)

			tracked, err := gate.ShouldTrack(context.Background(), testGroupChatJID, tc.chatType, testAccountJID)

			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrChatGateUndecided)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTracked, tracked)
			assert.Len(t, store.upserts, tc.wantUpserts)
		})
	}
}

// TestChatGate_HistoryFallbackAppliesOnlyToAnUnavailableGroup pins the one case
// the fallback exists for: a group the user has LEFT, whose history would
// otherwise be dropped entirely because no live lookup can size it.
func TestChatGate_HistoryFallbackAppliesOnlyToAnUnavailableGroup(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: errors.Join(ErrGroupUnavailable, errors.New("status 403"))}
	gate := gateWith(store, fetcher, 10)

	tracked, err := gate.ShouldTrackHistoryChat(context.Background(), testGroupChatJID,
		&ChatGroupInfo{Title: "Old Trip", MemberCount: 5}, testAccountJID)

	require.NoError(t, err)
	assert.True(t, tracked, "the chunk's own participant list sizes a group we are no longer in")
	require.Len(t, store.upserts, 1, "the adopted count is persisted, or every chunk re-decides it")
	assert.Equal(t, int32(5), *store.upserts[0].MemberCount)
	require.NotNil(t, store.upserts[0].ChatTitle)
	assert.Equal(t, "Old Trip", *store.upserts[0].ChatTitle)
	assert.Nil(t, store.upserts[0].LastLookupAt,
		"no live lookup produced this count, and that column is the record of one")
}

// TestChatGate_SizeUnknownGroupIsNotTrackedEvenWithAChunkSnapshot is the
// fail-open guard.
//
// ErrGroupSizeUnknown fires for a group we are STILL IN whose size attribute
// was merely absent. Adopting the chunk's historical count would persist it,
// and because the gate re-looks-up only while member_count is NULL, that number
// would then gate every FUTURE LIVE message from the group — the unknown-means-
// track branch this integration deliberately refuses, arriving by the back door.
func TestChatGate_SizeUnknownGroupIsNotTrackedEvenWithAChunkSnapshot(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{err: ErrGroupSizeUnknown}
	gate := gateWith(store, fetcher, 10)

	tracked, err := gate.ShouldTrackHistoryChat(context.Background(), testGroupChatJID,
		&ChatGroupInfo{Title: "Still In This One", MemberCount: 8}, testAccountJID)

	require.NoError(t, err)
	assert.False(t, tracked, "we are in this group; a historical count must not decide it")
	assert.Empty(t, store.upserts,
		"no member_count may be written, or a future live message would inherit a historical size")
}

// TestChatGate_LiveCountAlwaysWinsOverTheChunkSnapshot: a successful live
// lookup ignores the snapshot entirely, so a stale historical participant list
// can never make a currently-oversized group look small.
func TestChatGate_LiveCountAlwaysWinsOverTheChunkSnapshot(t *testing.T) {
	store := &fakeChatConfigStore{}
	fetcher := &fakeGroupInfoFetcher{info: &ChatGroupInfo{Title: "Grew A Lot", MemberCount: 300}}
	gate := gateWith(store, fetcher, 10)

	tracked, err := gate.ShouldTrackHistoryChat(context.Background(), testGroupChatJID,
		&ChatGroupInfo{Title: "Back When It Was Small", MemberCount: 4}, testAccountJID)

	require.NoError(t, err)
	assert.False(t, tracked, "the live size is 300, whatever the chunk remembers")
	require.Len(t, store.upserts, 1)
	assert.Equal(t, int32(300), *store.upserts[0].MemberCount)
	require.NotNil(t, store.upserts[0].LastLookupAt, "this WAS a live lookup")
}

// TestChatGate_HistoryFallbackIsIgnoredWithoutAUsableSnapshot: an empty
// participant list means no fallback, and the group falls to the live answer
// exactly as it does today.
func TestChatGate_HistoryFallbackIsIgnoredWithoutAUsableSnapshot(t *testing.T) {
	for _, snapshot := range []*ChatGroupInfo{nil, {Title: "Empty", MemberCount: 0}} {
		store := &fakeChatConfigStore{}
		fetcher := &fakeGroupInfoFetcher{err: errors.Join(ErrGroupUnavailable, errors.New("status 404"))}
		gate := gateWith(store, fetcher, 10)

		tracked, err := gate.ShouldTrackHistoryChat(context.Background(), testGroupChatJID, snapshot, testAccountJID)

		require.NoError(t, err)
		assert.False(t, tracked)
		assert.Empty(t, store.upserts)
	}
}

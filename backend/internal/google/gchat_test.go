package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	people "google.golang.org/api/people/v1"
)

// staticResolver builds a resolver whose fake ResolvePersonEmail returns a
// fixed mapping (and counts calls), with no caching from a prior cache.
func staticResolver(t *testing.T, mapping map[string]string, callCount *int) *cachedEmailResolver {
	t.Helper()
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if callCount != nil {
				*callCount++
			}
			return mapping[userName], nil
		},
	}}
	return newCachedEmailResolver(fetcher, nil)
}

func humanMsg(name, senderName, createTime, text string) *chat.Message {
	return &chat.Message{
		Name:       name,
		Sender:     &chat.User{Name: senderName, Type: gchatUserTypeHuman},
		CreateTime: createTime,
		Text:       text,
	}
}

func TestClassifyMessage_InboundSenderOnly(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	bob := uuid.New()
	carol := uuid.New()
	resolver := staticResolver(t, map[string]string{
		"users/alice": "alice@example.test",
		"users/bob":   "bob@example.test",
		"users/carol": "carol@example.test",
	}, nil)
	knownMap := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"bob@example.test":   {bob},
		"carol@example.test": {carol},
	}
	knownMembers := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"bob@example.test":   {bob},
		"carol@example.test": {carol},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	m := humanMsg("spaces/S/messages/1", "users/alice", "2026-06-04T10:00:00Z", "hi all")
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, nil, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "inbound", c.Direction)
	// Sender-only: exactly Alice, NOT Bob/Carol.
	assert.Equal(t, []uuid.UUID{alice}, c.Matched)
}

func TestClassifyMessage_OutboundFanOut(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	bob := uuid.New()
	resolver := staticResolver(t, map[string]string{
		"users/me":    "me@example.test",
		"users/alice": "alice@example.test",
		"users/bob":   "bob@example.test",
	}, nil)
	knownMap := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"bob@example.test":   {bob},
	}
	knownMembers := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"bob@example.test":   {bob},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	m := humanMsg("spaces/S/messages/2", "users/me", "2026-06-04T10:01:00Z", "hello")
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, nil, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "outbound", c.Direction)
	assert.ElementsMatch(t, []uuid.UUID{alice, bob}, c.Matched)
}

func TestClassifyMessage_OutboundExcludesSelfContact(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	selfContact := uuid.New()
	resolver := staticResolver(t, map[string]string{
		"users/me":    "me@example.test",
		"users/alice": "alice@example.test",
		"users/self":  "me@example.test", // a co-member resolving to my own email
	}, nil)
	knownMap := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"me@example.test":    {selfContact}, // the user's own email is also a CRM contact
	}
	// knownMembers built via resolveKnownMembers excludes self; simulate that the
	// classify fan-out also excludes any address in meSet defensively.
	knownMembers := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"me@example.test":    {selfContact},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	m := humanMsg("spaces/S/messages/3", "users/me", "2026-06-04T10:02:00Z", "hi")
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, nil, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "outbound", c.Direction)
	// The self-contact must NOT receive a self-attributed outreach row.
	assert.Equal(t, []uuid.UUID{alice}, c.Matched)
}

func TestClassifyMessage_BystanderAndUnresolved(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	resolver := staticResolver(t, map[string]string{
		"users/stranger": "stranger@example.test", // resolvable but unknown
		"users/ghost":    "",                      // unresolvable → no email
	}, nil)
	knownMap := map[string][]uuid.UUID{"alice@example.test": {alice}}
	meSet := map[string]struct{}{"me@example.test": {}}

	// Bystander: sender neither me nor known.
	mBy := humanMsg("spaces/S/messages/4", "users/stranger", "2026-06-04T10:03:00Z", "hey")
	c, err := classifyMessage(ctx, mBy, nil, knownMap, nil, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Empty(t, c.Matched)
	assert.False(t, c.Unresolved)

	// Unresolved sender: resolves to "" → flagged Unresolved, no rows.
	mUn := humanMsg("spaces/S/messages/5", "users/ghost", "2026-06-04T10:04:00Z", "hey")
	c, err = classifyMessage(ctx, mUn, nil, knownMap, nil, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Empty(t, c.Matched)
	assert.True(t, c.Unresolved)
}

// TestClassifyMessage_MatchesBySenderID is the headline DB-free behavior: a
// sender id in knownIDs matches the contact even when the People API resolves
// the id to "" (the not-in-Google-Contacts case). The peer email is carried
// from the idMatch (not the empty People-API result).
func TestClassifyMessage_MatchesBySenderID(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	// The People API returns "" for users/dave (simulating not-in-Contacts).
	resolver := staticResolver(t, map[string]string{"users/dave": ""}, nil)
	knownMap := map[string][]uuid.UUID{"dave@example.test": {dave}}
	meSet := map[string]struct{}{"me@example.test": {}}
	knownIDs := map[string]idMatch{
		"users/dave": {Email: "dave@example.test", Contacts: []uuid.UUID{dave}},
	}

	m := humanMsg("spaces/S/messages/d", "users/dave", "2026-06-04T10:00:00Z", "hi from dave")
	c, err := classifyMessage(ctx, m, nil, knownMap, knownIDs, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "inbound", c.Direction)
	assert.Equal(t, []uuid.UUID{dave}, c.Matched, "the sender id matched the contact via the id path")
	assert.Equal(t, "dave@example.test", c.SenderEmail, "the peer email is carried from the idMatch, not the empty People-API result")
}

// TestClassifyMessage_InboundIDPathYieldsToMeID is the defense-in-depth guard:
// if a stray index entry maps the account's OWN id to a contact (the anomalous
// self-alias case), the inbound id-path must NOT fire for the account's own
// message — meIDs makes it fall through to the email path, where the sender (me)
// is classified outbound, not inbound from that contact.
func TestClassifyMessage_InboundIDPathYieldsToMeID(t *testing.T) {
	ctx := context.Background()
	strayContact := uuid.New()
	// The account's own id resolves to its own email via the email path.
	resolver := staticResolver(t, map[string]string{"users/me": "me@example.test"}, nil)
	knownMap := map[string][]uuid.UUID{"me@example.test": {strayContact}}
	meSet := map[string]struct{}{"me@example.test": {}}
	// A stray index entry mapping the account's own id to a contact (what the
	// build-time meIDs exclusion normally prevents — here we assert the
	// point-of-use guard independently).
	knownIDs := map[string]idMatch{
		"users/me": {Email: "me@example.test", Contacts: []uuid.UUID{strayContact}},
	}
	meIDs := map[string]struct{}{"users/me": {}}

	m := humanMsg("spaces/S/messages/self", "users/me", "2026-06-04T10:00:00Z", "my own message")
	c, err := classifyMessage(ctx, m, nil, knownMap, knownIDs, meIDs, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "outbound", c.Direction, "the account's own message is outbound, never inbound from a stray self-alias contact")
}

// TestClassifyMessage_OutboundFanOutIncludesIDResolvedMembers proves an outbound
// message fans out to the UNION of email-resolved and id-resolved known members,
// deduped and self-excluded.
func TestClassifyMessage_OutboundFanOutIncludesIDResolvedMembers(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	dave := uuid.New()
	resolver := staticResolver(t, map[string]string{"users/me": "me@example.test"}, nil)
	knownMap := map[string][]uuid.UUID{
		"alice@example.test": {alice},
		"dave@example.test":  {dave},
	}
	// Alice is email-resolved; Dave is id-resolved (not in Contacts).
	knownMembers := map[string][]uuid.UUID{"alice@example.test": {alice}}
	knownIDs := map[string]idMatch{
		"users/dave": {Email: "dave@example.test", Contacts: []uuid.UUID{dave}},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	m := humanMsg("spaces/S/messages/o", "users/me", "2026-06-04T10:01:00Z", "team update")
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, knownIDs, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "outbound", c.Direction)
	assert.ElementsMatch(t, []uuid.UUID{alice, dave}, c.Matched, "fan-out unions email-resolved and id-resolved members")
}

// TestClassifyMessage_OutboundFanOutDedupsContact proves the union dedups a
// contact that is present on BOTH the email-resolved and id-resolved sides (a
// contact reachable both via Contacts and via canonical id). The index builder
// guarantees meSet ids are never in knownIDs, so the inbound id-path's
// meSet-independence is safe; self-exclusion of a meSet *email* member is
// covered directly in TestFlattenKnownMembersAndIDs_UnionDedupSelfExclude.
func TestClassifyMessage_OutboundFanOutDedupsContact(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	resolver := staticResolver(t, map[string]string{"users/me": "me@example.test"}, nil)
	knownMap := map[string][]uuid.UUID{"alice@example.test": {alice}}
	knownMembers := map[string][]uuid.UUID{"alice@example.test": {alice}}
	// Same contact also id-resolved (must dedup to a single row).
	knownIDs := map[string]idMatch{
		"users/alice": {Email: "alice@example.test", Contacts: []uuid.UUID{alice}},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	m := humanMsg("spaces/S/messages/o2", "users/me", "2026-06-04T10:02:00Z", "hi")
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, knownIDs, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "outbound", c.Direction)
	assert.Equal(t, []uuid.UUID{alice}, c.Matched, "the shared contact appears once, not twice")
}

// TestFlattenKnownMembersAndIDs_UnionDedupSelfExclude pins the fan-out helper:
// the union dedups across the email-resolved and id-resolved sides and drops any
// id-resolved member whose email is in meSet (defensive self-exclusion).
func TestFlattenKnownMembersAndIDs_UnionDedupSelfExclude(t *testing.T) {
	alice := uuid.New()
	dave := uuid.New()
	meContact := uuid.New()
	knownMembers := map[string][]uuid.UUID{"alice@example.test": {alice}}
	knownIDs := map[string]idMatch{
		"users/alice": {Email: "alice@example.test", Contacts: []uuid.UUID{alice}},  // dup of email side
		"users/dave":  {Email: "dave@example.test", Contacts: []uuid.UUID{dave}},    // id-only
		"users/me":    {Email: "me@example.test", Contacts: []uuid.UUID{meContact}}, // meSet → excluded
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	out := flattenKnownMembersAndIDs(knownMembers, knownIDs, meSet)
	assert.ElementsMatch(t, []uuid.UUID{alice, dave}, out, "union dedups the shared contact and self-excludes the meSet id member")
}

// TestClassifyMessage_IDPathPreferredEmailFallback proves an id in knownIDs
// matches WITHOUT calling the email resolver, while an id absent from knownIDs
// falls through to the (still-working) email path.
func TestClassifyMessage_IDPathPreferredEmailFallback(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	erin := uuid.New()
	calls := 0
	resolver := staticResolver(t, map[string]string{
		"users/erin": "erin@example.test", // email-resolvable
		"users/dave": "",                  // not-in-Contacts
	}, &calls)
	knownMap := map[string][]uuid.UUID{
		"dave@example.test": {dave},
		"erin@example.test": {erin},
	}
	knownIDs := map[string]idMatch{
		"users/dave": {Email: "dave@example.test", Contacts: []uuid.UUID{dave}},
	}
	meSet := map[string]struct{}{"me@example.test": {}}

	// Dave: id path → no email resolver call.
	mDave := humanMsg("spaces/S/messages/d", "users/dave", "2026-06-04T10:00:00Z", "x")
	c, err := classifyMessage(ctx, mDave, nil, knownMap, knownIDs, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{dave}, c.Matched)
	assert.Equal(t, 0, calls, "an id-path match must not call the email resolver")

	// Erin: absent from knownIDs → email path still matches.
	mErin := humanMsg("spaces/S/messages/e", "users/erin", "2026-06-04T10:01:00Z", "y")
	c, err = classifyMessage(ctx, mErin, nil, knownMap, knownIDs, nil, meSet, resolver)
	require.NoError(t, err)
	assert.Equal(t, "inbound", c.Direction)
	assert.Equal(t, []uuid.UUID{erin}, c.Matched)
	assert.Equal(t, 1, calls, "the absent id falls through to the email path")
}

func TestQualifiableContentMessage_FiltersBotsTombstonesNameless(t *testing.T) {
	// Human content message qualifies.
	assert.True(t, qualifiableContentMessage(humanMsg("spaces/S/messages/6", "users/a", "2026-06-04T10:00:00Z", "x")))

	// Bot sender filtered.
	bot := &chat.Message{Name: "spaces/S/messages/7", Sender: &chat.User{Name: "users/bot", Type: "BOT"}, CreateTime: "2026-06-04T10:00:00Z"}
	assert.False(t, qualifiableContentMessage(bot))

	// Tombstone filtered.
	tomb := &chat.Message{Name: "spaces/S/messages/8", Sender: &chat.User{Name: "users/a", Type: gchatUserTypeHuman}, DeletionMetadata: &chat.DeletionMetadata{}}
	assert.False(t, qualifiableContentMessage(tomb))

	// Nameless filtered.
	nameless := &chat.Message{Sender: &chat.User{Name: "users/a", Type: gchatUserTypeHuman}}
	assert.False(t, qualifiableContentMessage(nameless))

	// Nil sender filtered.
	noSender := &chat.Message{Name: "spaces/S/messages/9"}
	assert.False(t, qualifiableContentMessage(noSender))
}

func TestResolveKnownMembers_ResolvesAndExcludesSelf(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	resolver := staticResolver(t, map[string]string{
		"users/alice":   "alice@example.test",
		"users/me":      "me@example.test",
		"users/unknown": "unknown@example.test",
		"users/noemail": "",
	}, nil)
	knownMap := map[string][]uuid.UUID{"alice@example.test": {alice}}
	meSet := map[string]struct{}{"me@example.test": {}}

	members := []string{"users/alice", "users/me", "users/unknown", "users/noemail"}
	known, meIDs, err := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)
	require.NoError(t, err)

	// Only Alice is a known co-member; me is excluded, unknown is not in knownMap,
	// noemail is unresolvable.
	require.Len(t, known, 1)
	assert.Equal(t, []uuid.UUID{alice}, known["alice@example.test"])

	// The account's own member id is reported in meIDs (so the reverse id-index
	// can exclude it).
	assert.Contains(t, meIDs, "users/me")
	assert.NotContains(t, meIDs, "users/alice")
}

func TestResolveKnownMembers_PropagatesResolveError(t *testing.T) {
	ctx := context.Background()
	resolveErr := errors.New("people api transient")
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolvePersonEmail: func(context.Context, string) (string, error) { return "", resolveErr },
	}}
	resolver := newCachedEmailResolver(fetcher, nil)

	_, _, err := resolveKnownMembers(ctx, []string{"users/alice"}, resolver, map[string][]uuid.UUID{}, map[string]struct{}{})
	require.ErrorIs(t, err, resolveErr, "a transient co-member resolve error must propagate, not be swallowed")
}

func TestCachedEmailResolver_PositiveNegativeCachingAndTTL(t *testing.T) {
	ctx := context.Background()
	calls := 0
	resolver := staticResolver(t, map[string]string{
		"users/alice": "alice@example.test",
		"users/ghost": "", // negative
	}, &calls)

	// First resolve hits the fetcher.
	email, err := resolver.resolve(ctx, "users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice@example.test", email)
	assert.Equal(t, 1, calls)

	// In-sweep memo: second resolve does NOT hit the fetcher.
	email, err = resolver.resolve(ctx, "users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice@example.test", email)
	assert.Equal(t, 1, calls)

	// Negative result is cached too (one fetcher call, then memoized).
	email, err = resolver.resolve(ctx, "users/ghost")
	require.NoError(t, err)
	assert.Equal(t, "", email)
	assert.Equal(t, 2, calls)
	email, err = resolver.resolve(ctx, "users/ghost")
	require.NoError(t, err)
	assert.Equal(t, "", email)
	assert.Equal(t, 2, calls, "negative result re-served from cache, not re-fetched")

	// Cross-sweep: a persisted cache entry within TTL is reused (no fetch).
	now := accelerated.GetCurrentTime()
	persisted := map[string]cachedEmail{
		"users/bob": {Email: "bob@example.test", ResolvedAt: now.Add(-time.Hour).Format(chatTimeLayout)},
	}
	calls2 := 0
	fresh := &cachedEmailResolver{
		fetcher: &fakeChatFetcher{funcs: FakeChatFetcherFuncs{ResolvePersonEmail: func(context.Context, string) (string, error) { calls2++; return "SHOULD-NOT-CALL", nil }}},
		cache:   persisted,
		memo:    map[string]string{},
	}
	email, err = fresh.resolve(ctx, "users/bob")
	require.NoError(t, err)
	assert.Equal(t, "bob@example.test", email)
	assert.Equal(t, 0, calls2, "within-TTL cache entry reused")

	// Expired cache entry (older than TTL) is refetched.
	persisted["users/bob"] = cachedEmail{Email: "stale@example.test", ResolvedAt: now.Add(-25 * time.Hour).Format(chatTimeLayout)}
	delete(fresh.memo, "users/bob")
	email, err = fresh.resolve(ctx, "users/bob")
	require.NoError(t, err)
	assert.Equal(t, "SHOULD-NOT-CALL", email) // refetched value
	assert.Equal(t, 1, calls2, "expired cache entry refetched")
}

// fakeIDFetcher builds a fakeChatFetcher whose ResolveMemberID returns a fixed
// (space,email)→id mapping and counts calls. A missing mapping entry returns
// notMember. A space named in errSpaces returns a transient error.
func fakeIDFetcher(mapping map[string]string, callCount *int) *fakeChatFetcher {
	return &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolveMemberID: func(_ context.Context, spaceName, email string) (string, bool, error) {
			if callCount != nil {
				*callCount++
			}
			if id, ok := mapping[spaceName+"|"+email]; ok {
				return id, false, nil
			}
			return "", true, nil
		},
	}}
}

func TestMemberIDResolver_GlobalPositiveReusedAcrossSpaces(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/A|person@example.test": "users/777",
		"spaces/B|person@example.test": "users/777",
	}, &calls)
	cap := 10
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fpA := memberSetFingerprint([]string{"users/777"})
	fpB := memberSetFingerprint([]string{"users/777", "users/me"})

	// First resolve in space A hits the fetcher.
	id, status, err := r.resolve(ctx, "spaces/A", fpA, "person@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, resolvedKnownID, status)
	assert.Equal(t, "users/777", id)
	assert.Equal(t, 1, calls)

	// Second resolve in space B reuses the GLOBAL positive — ZERO additional calls.
	id, status, err = r.resolve(ctx, "spaces/B", fpB, "person@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, resolvedKnownID, status)
	assert.Equal(t, "users/777", id)
	assert.Equal(t, 1, calls, "the canonical id is space-independent; space B reuses the global positive")
	assert.Equal(t, 9, cap, "only one fresh resolve consumed the cap")
}

func TestMemberIDResolver_NegativeIsPerSpace(t *testing.T) {
	ctx := context.Background()
	calls := 0
	// person is a member of B only; absent from A.
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/B|person@example.test": "users/888",
	}, &calls)
	cap := 10
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fpA := memberSetFingerprint([]string{"users/me"})
	fpB := memberSetFingerprint([]string{"users/888", "users/me"})

	// A: not a member → negative for A.
	_, status, err := r.resolve(ctx, "spaces/A", fpA, "person@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, notMember, status)
	assert.Equal(t, 1, calls)

	// The negative for A is re-served (within fingerprint) without a fetch.
	_, status, err = r.resolve(ctx, "spaces/A", fpA, "person@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, notMember, status)
	assert.Equal(t, 1, calls, "per-space negative re-served from cache")

	// B: same email is STILL attempted (per-space negative does not block B) and
	// resolves.
	id, status, err := r.resolve(ctx, "spaces/B", fpB, "person@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, resolvedKnownID, status)
	assert.Equal(t, "users/888", id)
	assert.Equal(t, 2, calls, "a negative in A does not suppress a fresh resolve in B")
}

func TestMemberIDResolver_NegativeInvalidatedByFingerprintChange(t *testing.T) {
	ctx := context.Background()
	calls := 0
	// Initially absent from A; after the "join" the mapping yields an id.
	mapping := map[string]string{}
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolveMemberID: func(_ context.Context, spaceName, email string) (string, bool, error) {
			calls++
			if id, ok := mapping[spaceName+"|"+email]; ok {
				return id, false, nil
			}
			return "", true, nil
		},
	}}
	cap := 10
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fpBefore := memberSetFingerprint([]string{"users/me"})
	fpAfter := memberSetFingerprint([]string{"users/me", "users/999"})

	// Sweep 1: absent → negative stamped with fpBefore.
	_, status, err := r.resolve(ctx, "spaces/A", fpBefore, "joiner@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, notMember, status)
	assert.Equal(t, 1, calls)

	// Sweep 2 (new resolver instance to drop the in-sweep memo): the contact has
	// joined → mapping yields the id AND the fingerprint flipped. The stale
	// negative (stamped fpBefore) is NOT honored under fpAfter → re-resolved.
	mapping["spaces/A|joiner@example.test"] = "users/999"
	r2 := newMemberIDResolver(fetcher, r.snapshotPositives(), r.snapshotNegatives(), &cap)
	id, status, err := r2.resolve(ctx, "spaces/A", fpAfter, "joiner@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, resolvedKnownID, status, "fingerprint flip invalidates the stale negative")
	assert.Equal(t, "users/999", id)
	assert.Equal(t, 2, calls)
}

func TestMemberIDResolver_ResolveCapDefers(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/A|a@example.test": "users/1",
		"spaces/A|b@example.test": "users/2",
	}, &calls)
	cap := 1
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fp := memberSetFingerprint([]string{"users/1", "users/2"})

	// First fresh resolve consumes the cap.
	_, status, err := r.resolve(ctx, "spaces/A", fp, "a@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, resolvedKnownID, status)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 0, cap)

	// Second fresh resolve is deferred (cap exhausted) — NO fetcher call.
	_, status, err = r.resolve(ctx, "spaces/A", fp, "b@example.test", nil)
	require.NoError(t, err)
	assert.Equal(t, deferredCapHit, status)
	assert.Equal(t, 1, calls, "the cap-exhausted candidate is not fetched this sweep")
}

func TestMemberIDResolver_BudgetExhaustionDefers(t *testing.T) {
	ctx := context.Background()
	calls := 0
	fetcher := fakeIDFetcher(map[string]string{"spaces/A|a@example.test": "users/1"}, &calls)
	cap := 10
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fp := memberSetFingerprint([]string{"users/1"})

	pageBudget := 0
	_, status, err := r.resolve(ctx, "spaces/A", fp, "a@example.test", &pageBudget)
	require.NoError(t, err)
	assert.Equal(t, deferredBudgetHit, status)
	assert.Equal(t, 0, calls, "a zero page budget defers without a fetch")
	assert.Equal(t, 10, cap, "the resolve-cap is not consumed when the budget defers")
}

func TestMemberIDResolver_PropagatesFetcherError(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("members.get transient")
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolveMemberID: func(context.Context, string, string) (string, bool, error) {
			return "", false, wantErr
		},
	}}
	cap := 10
	r := newMemberIDResolver(fetcher, nil, nil, &cap)
	fp := memberSetFingerprint([]string{"users/1"})

	pageBudget := 5
	_, _, err := r.resolve(ctx, "spaces/A", fp, "a@example.test", &pageBudget)
	require.ErrorIs(t, err, wantErr, "a transient members.get error must propagate, not become a negative")
	// An ISSUED members.get is a real API call regardless of outcome: it consumes
	// both budgets even on error, so a persistently-failing space cannot blow past
	// the bounds across many spaces in one sweep.
	assert.Equal(t, 9, cap, "an errored members.get still consumes the resolve cap")
	assert.Equal(t, 4, pageBudget, "an errored members.get still consumes the page budget")
}

// TestBuildKnownIDIndex_SeedsFromPositiveCacheZeroCalls proves the index is
// seeded from the global positive cache with ZERO members.get calls when the
// cached id is already a member of the space, and that a fresh resolve happens
// only for an uncached non-negative email.
func TestBuildKnownIDIndex_SeedsFromPositiveCacheZeroCalls(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	frank := uuid.New()
	members := []string{"users/dave", "users/frank", "users/me"}
	fp := memberSetFingerprint(members)
	now := accelerated.GetCurrentTime()

	calls := 0
	// frank is resolvable fresh; dave is pre-seeded in the positive cache.
	fetcher := fakeIDFetcher(map[string]string{"spaces/A|frank@example.test": "users/frank"}, &calls)
	pos := map[string]cachedUserID{"dave@example.test": {UserName: "users/dave", ResolvedAt: now.Format(chatTimeLayout)}}
	cap := 10
	resolver := newMemberIDResolver(fetcher, pos, nil, &cap)

	knownMap := map[string][]uuid.UUID{
		"dave@example.test":  {dave},
		"frank@example.test": {frank},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	counters := &sweepCounters{}
	budget := 10
	idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, nil, resolver, counters, &budget)
	require.NoError(t, err)
	assert.False(t, blockedBudget)
	assert.False(t, blockedCap)

	// dave seeded from the positive cache (zero calls); frank resolved fresh (one).
	assert.Equal(t, idMatch{Email: "dave@example.test", Contacts: []uuid.UUID{dave}}, idx["users/dave"])
	assert.Equal(t, idMatch{Email: "frank@example.test", Contacts: []uuid.UUID{frank}}, idx["users/frank"])
	assert.Equal(t, 1, calls, "only the uncached email triggered a members.get")
}

// TestBuildKnownIDIndex_ExcludesMeIDs proves the account's own id never enters
// the reverse index, even when a stray non-meSet CRM email (a contact carrying
// the user's own alternate alias) resolves to the account's own canonical id —
// from BOTH the global-positive seed path AND the fresh-resolve path. Without
// the meIDs guard, the account's own outbound messages would misclassify as
// inbound from that contact.
func TestBuildKnownIDIndex_ExcludesMeIDs(t *testing.T) {
	ctx := context.Background()
	strayContact := uuid.New()
	freshStray := uuid.New()
	members := []string{"users/me", "users/other"}
	fp := memberSetFingerprint(members)
	now := accelerated.GetCurrentTime()

	calls := 0
	// A second stray alias resolves fresh to users/me too.
	fetcher := fakeIDFetcher(map[string]string{"spaces/A|stray-fresh@example.test": "users/me"}, &calls)
	// One stray alias is pre-seeded in the global positive cache as users/me.
	pos := map[string]cachedUserID{"stray-seeded@example.test": {UserName: "users/me", ResolvedAt: now.Format(chatTimeLayout)}}
	cap := 10
	resolver := newMemberIDResolver(fetcher, pos, nil, &cap)

	knownMap := map[string][]uuid.UUID{
		"stray-seeded@example.test": {strayContact}, // a contact holding the user's own alias
		"stray-fresh@example.test":  {freshStray},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	meIDs := map[string]struct{}{"users/me": {}}
	counters := &sweepCounters{}
	budget := 10
	idx, _, _, _, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, meIDs, resolver, counters, &budget)
	require.NoError(t, err)

	// users/me must NOT be in the index from either path.
	_, present := idx["users/me"]
	assert.False(t, present, "the account's own id must never enter the reverse index")
	assert.Empty(t, idx, "no contact is matched via a me-id alias")
}

// TestBuildKnownIDIndex_DiscoversMeIDFromCachedPositive proves meIDs is
// authoritatively extended from the reverse resolver's already-cached positive
// for the meSet email — even when the People-derived meIDs is EMPTY (the
// not-in-Contacts me case). This closes the residual gap where People could not
// identify the account's own member id. The discovery is cache-only (no fresh
// members.get): the me-email's id was cached by an earlier space.
func TestBuildKnownIDIndex_DiscoversMeIDFromCachedPositive(t *testing.T) {
	ctx := context.Background()
	strayContact := uuid.New()
	members := []string{"users/me", "users/other"}
	fp := memberSetFingerprint(members)
	now := accelerated.GetCurrentTime()

	calls := 0
	fetcher := fakeIDFetcher(map[string]string{}, &calls)
	// The account's own email is in the global positive cache as users/me (from an
	// earlier space), AND a stray contact alias is cached as users/me too.
	pos := map[string]cachedUserID{
		"me@example.test":           {UserName: "users/me", ResolvedAt: now.Format(chatTimeLayout)},
		"stray-seeded@example.test": {UserName: "users/me", ResolvedAt: now.Format(chatTimeLayout)},
	}
	cap := 10
	resolver := newMemberIDResolver(fetcher, pos, nil, &cap)

	knownMap := map[string][]uuid.UUID{"stray-seeded@example.test": {strayContact}}
	meSet := map[string]struct{}{"me@example.test": {}}
	// People-derived meIDs is EMPTY (People could not identify the me member id).
	counters := &sweepCounters{}
	budget := 10
	idx, meIDsOut, _, _, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, map[string]struct{}{}, resolver, counters, &budget)
	require.NoError(t, err)

	assert.Contains(t, meIDsOut, "users/me", "the me-id is discovered from the cached positive for the me-email")
	assert.Empty(t, idx, "the stray contact alias is excluded because it resolves to the discovered me-id")
	assert.Equal(t, 0, calls, "the me-id discovery is cache-only — no fresh members.get")
}

// TestBuildKnownIDIndex_DeferredCapReturnsDebtFlagOnly drives the resolution-debt
// classification. blockedByCapOnDebt is now PURELY informational (it drives the
// spacesWarmupDeferred counter and no longer implies any cursor outcome — that is
// a Sync-level concern, proven in the integration tests). The subtests assert the
// resolver/index classification only:
//
//	(a) a NEGATIVE-VALID candidate is not debt → no resolve, blockedByCapOnDebt=false;
//	(b) an UNKNOWN candidate (fingerprint-mismatched negative) with the cap
//	    exhausted → deferredCapHit → blockedByCapOnDebt=true, driven purely by the
//	    persisted-negative-vs-current-fingerprint mismatch (no transient flag);
//	(c) priority — when the cap admits only N resolves, fingerprint-invalidated
//	    candidates are resolved before never-seen ones.
func TestBuildKnownIDIndex_DeferredCapReturnsDebtFlagOnly(t *testing.T) {
	ctx := context.Background()
	now := accelerated.GetCurrentTime()
	fresh := now.Format(chatTimeLayout)

	t.Run("negative_valid_is_not_debt", func(t *testing.T) {
		absent := uuid.New()
		members := []string{"users/me"}
		fp := memberSetFingerprint(members)
		calls := 0
		fetcher := fakeIDFetcher(map[string]string{}, &calls)
		neg := map[string]map[string]memberNegative{
			"spaces/A": {"absent@example.test": {ResolvedAt: fresh, MemberSetFingerprint: fp}},
		}
		cap := 0 // cap exhausted, but a NEGATIVE-VALID candidate is not debt
		resolver := newMemberIDResolver(fetcher, nil, neg, &cap)
		knownMap := map[string][]uuid.UUID{"absent@example.test": {absent}}
		counters := &sweepCounters{}
		budget := 10
		idx, _, _, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, map[string]struct{}{"me@example.test": {}}, nil, resolver, counters, &budget)
		require.NoError(t, err)
		assert.Empty(t, idx)
		assert.False(t, blockedCap, "a NEGATIVE-VALID candidate under the current fingerprint is not debt")
		assert.Equal(t, 0, calls, "a valid negative is honored without a fetch")
	})

	t.Run("fingerprint_mismatch_is_debt_when_cap_exhausted", func(t *testing.T) {
		joiner := uuid.New()
		members := []string{"users/me", "users/joiner"} // joiner just joined → fingerprint flipped
		fpNew := memberSetFingerprint(members)
		calls := 0
		fetcher := fakeIDFetcher(map[string]string{"spaces/A|joiner@example.test": "users/joiner"}, &calls)
		// Persisted negative carries the OLD fingerprint (before the join).
		neg := map[string]map[string]memberNegative{
			"spaces/A": {"joiner@example.test": {ResolvedAt: fresh, MemberSetFingerprint: "old-fp"}},
		}
		cap := 0 // cap exhausted: the now-UNKNOWN candidate cannot resolve → debt
		resolver := newMemberIDResolver(fetcher, nil, neg, &cap)
		knownMap := map[string][]uuid.UUID{"joiner@example.test": {joiner}}
		counters := &sweepCounters{}
		budget := 10
		idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fpNew, knownMap, map[string]struct{}{"me@example.test": {}}, nil, resolver, counters, &budget)
		require.NoError(t, err)
		assert.Empty(t, idx, "the candidate could not resolve this sweep")
		assert.False(t, blockedBudget)
		assert.True(t, blockedCap, "the fingerprint mismatch alone makes the candidate UNKNOWN; cap exhaustion holds the cursor (no transient flag)")
		assert.Equal(t, 0, calls, "the cap-exhausted candidate is not fetched")
	})

	t.Run("priority_invalidated_before_neverseen", func(t *testing.T) {
		invalidated := uuid.New()
		neverSeen := uuid.New()
		members := []string{"users/me", "users/invalidated"}
		fpNew := memberSetFingerprint(members)
		calls := 0
		fetcher := fakeIDFetcher(map[string]string{
			"spaces/A|invalidated@example.test": "users/invalidated",
			// neverseen would resolve too, but the cap admits only ONE.
			"spaces/A|neverseen@example.test": "users/neverseen",
		}, &calls)
		neg := map[string]map[string]memberNegative{
			"spaces/A": {"invalidated@example.test": {ResolvedAt: fresh, MemberSetFingerprint: "old-fp"}},
		}
		cap := 1 // only ONE fresh resolve allowed → the priority candidate wins
		resolver := newMemberIDResolver(fetcher, nil, neg, &cap)
		knownMap := map[string][]uuid.UUID{
			"invalidated@example.test": {invalidated},
			"neverseen@example.test":   {neverSeen},
		}
		counters := &sweepCounters{}
		budget := 10
		idx, _, _, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fpNew, knownMap, map[string]struct{}{"me@example.test": {}}, nil, resolver, counters, &budget)
		require.NoError(t, err)
		// The invalidated (priority) candidate consumed the single cap slot and
		// matched; the never-seen one was deferred (debt) → cursor held.
		assert.Contains(t, idx, "users/invalidated", "the fingerprint-invalidated candidate is resolved first")
		assert.NotContains(t, idx, "users/neverseen")
		assert.True(t, blockedCap, "the deferred never-seen candidate is debt")
		assert.Equal(t, 1, calls, "exactly the single capped resolve was issued")
	})
}

// TestBuildKnownIDIndex_ActivityDoesNotReincurDebt proves the no-starvation
// fix: with an UNCHANGED member set (same fingerprint), repeated index builds
// issue ZERO members.get calls for a known-absent contact and never set
// blockedByCapOnDebt — mere activity does not re-incur debt. Contrast: when a
// member is actually added (fingerprint flips), the affected candidate becomes
// UNKNOWN and is re-resolved.
func TestBuildKnownIDIndex_ActivityDoesNotReincurDebt(t *testing.T) {
	ctx := context.Background()
	now := accelerated.GetCurrentTime()
	fresh := now.Format(chatTimeLayout)
	absent := uuid.New()

	members := []string{"users/me", "users/other"}
	fp := memberSetFingerprint(members)
	calls := 0
	fetcher := fakeIDFetcher(map[string]string{}, &calls) // absent never resolves
	neg := map[string]map[string]memberNegative{
		"spaces/A": {"absent@example.test": {ResolvedAt: fresh, MemberSetFingerprint: fp}},
	}
	cap := 50
	resolver := newMemberIDResolver(fetcher, nil, neg, &cap)
	knownMap := map[string][]uuid.UUID{"absent@example.test": {absent}}
	meSet := map[string]struct{}{"me@example.test": {}}

	// Many sweeps with the SAME member set (fingerprint stable). Each uses a fresh
	// resolver instance (drops the in-sweep memo) reading the persisted negatives.
	for i := 0; i < 5; i++ {
		r := newMemberIDResolver(fetcher, resolver.snapshotPositives(), resolver.snapshotNegatives(), &cap)
		counters := &sweepCounters{}
		budget := 10
		idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, nil, r, counters, &budget)
		require.NoError(t, err)
		assert.Empty(t, idx)
		assert.False(t, blockedBudget)
		assert.False(t, blockedCap, "a stable fingerprint never re-incurs debt (sweep %d)", i)
	}
	assert.Equal(t, 0, calls, "no members.get calls across many stable-membership sweeps")

	// Now a member is actually added → fingerprint flips → the absent candidate
	// becomes UNKNOWN and IS re-resolved (here it resolves to a real id).
	membersAfter := []string{"users/me", "users/other", "users/absent"}
	fpAfter := memberSetFingerprint(membersAfter)
	fetcher2 := fakeIDFetcher(map[string]string{"spaces/A|absent@example.test": "users/absent"}, &calls)
	r := newMemberIDResolver(fetcher2, resolver.snapshotPositives(), resolver.snapshotNegatives(), &cap)
	counters := &sweepCounters{}
	budget := 10
	idx, _, _, _, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, membersAfter, fpAfter, knownMap, meSet, nil, r, counters, &budget)
	require.NoError(t, err)
	assert.Contains(t, idx, "users/absent", "a real membership change re-resolves the candidate")
	assert.Equal(t, 1, calls, "exactly one re-resolve after the membership actually changed")
}

// TestBuildKnownIDIndex_SkipsResolutionWhenAllMembersCovered proves the lossless
// skip: when every member id is already covered by an id-keyed signal (a me-id or
// already in the global positive cache), the candidate-resolution loop is skipped
// entirely — ZERO members.get calls and ZERO negatives written — because no fresh
// resolution could ever newly attach to a known email. The index is seeded only
// from the positive cache. Keyed strictly on id signals (never People-resolution).
func TestBuildKnownIDIndex_SkipsResolutionWhenAllMembersCovered(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	otherContact := uuid.New()
	members := []string{"users/dave", "users/other", "users/me"}
	fp := memberSetFingerprint(members)
	now := accelerated.GetCurrentTime()

	calls := 0
	// The fetcher would resolve BOTH known emails if asked — but it must NOT be
	// asked, because every member id is already covered.
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/A|dave@example.test":  "users/dave",
		"spaces/A|other@example.test": "users/other",
	}, &calls)
	// dave + other are already in the global positive cache; users/me is a me-id.
	pos := map[string]cachedUserID{
		"dave@example.test":  {UserName: "users/dave", ResolvedAt: now.Format(chatTimeLayout)},
		"other@example.test": {UserName: "users/other", ResolvedAt: now.Format(chatTimeLayout)},
	}
	cap := 10
	resolver := newMemberIDResolver(fetcher, pos, nil, &cap)

	knownMap := map[string][]uuid.UUID{
		"dave@example.test":  {dave},
		"other@example.test": {otherContact},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	meIDs := map[string]struct{}{"users/me": {}}
	counters := &sweepCounters{}
	budget := 10
	idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, meIDs, resolver, counters, &budget)
	require.NoError(t, err)
	assert.False(t, blockedBudget)
	assert.False(t, blockedCap, "no uncovered member id remains → no debt")

	// Both contacts seeded from the positive cache.
	assert.Equal(t, idMatch{Email: "dave@example.test", Contacts: []uuid.UUID{dave}}, idx["users/dave"])
	assert.Equal(t, idMatch{Email: "other@example.test", Contacts: []uuid.UUID{otherContact}}, idx["users/other"])
	assert.Equal(t, 0, calls, "every member id is covered (me-id or positive-cached) → zero members.get calls")
	assert.Equal(t, 0, counters.memberResolveNegativesWritten, "no negatives written when the loop is skipped")
	assert.Equal(t, 10, cap, "the resolve cap is untouched")
}

// TestBuildKnownIDIndex_StopsAfterAllUncoveredMemberIDsMatched proves the
// SET-based early-exit. U_id contains TWO distinct uncovered member ids, and a
// DUAL-SOURCE alias maps TWO known addresses to the SAME canonical id. A scalar
// "count of successful resolutions" would terminate after the alias double-counts
// (count reaches 2 while only ONE distinct uncovered id is attached), dropping the
// second distinct member id. The set-based test continues until BOTH distinct
// uncovered ids are attached, then STOPS (no further members.get).
func TestBuildKnownIDIndex_StopsAfterAllUncoveredMemberIDsMatched(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	daveAlt := uuid.New()
	frank := uuid.New()
	bystander := uuid.New()
	// Two distinct uncovered member ids: users/dave and users/frank.
	members := []string{"users/dave", "users/frank", "users/me"}
	fp := memberSetFingerprint(members)

	calls := 0
	// Dual-source alias: dave@ and dave-alt@ BOTH resolve to users/dave (same
	// canonical id reachable from two known addresses). frank@ resolves to
	// users/frank. zzz-bystander@ resolves to a non-member id and sorts LAST, so it
	// is the candidate the early-exit must skip once both uncovered ids are matched.
	// Emails are sorted in classifyIDCandidates, so the resolution order is
	// deterministic: dave-alt@, dave@, frank@, zzz-bystander@.
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/A|dave@example.test":          "users/dave",
		"spaces/A|dave-alt@example.test":      "users/dave",
		"spaces/A|frank@example.test":         "users/frank",
		"spaces/A|zzz-bystander@example.test": "users/elsewhere",
	}, &calls)
	cap := 10
	resolver := newMemberIDResolver(fetcher, nil, nil, &cap)

	knownMap := map[string][]uuid.UUID{
		"dave@example.test":          {dave},
		"dave-alt@example.test":      {daveAlt},
		"frank@example.test":         {frank},
		"zzz-bystander@example.test": {bystander},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	// users/me is the account's own member id (covered via meIDs, as
	// resolveKnownMembers would populate in the real sweep) → not part of U_id.
	meIDs := map[string]struct{}{"users/me": {}}
	counters := &sweepCounters{}
	budget := 10
	idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, meIDs, resolver, counters, &budget)
	require.NoError(t, err)
	assert.False(t, blockedBudget)
	assert.False(t, blockedCap)

	// (b) BOTH distinct uncovered member ids are attached.
	require.Contains(t, idx, "users/dave", "the first uncovered member id is attached")
	require.Contains(t, idx, "users/frank", "the second distinct uncovered member id is attached (a scalar count would have stopped before reaching it)")
	// The dual-source alias unioned both contacts onto the shared id.
	assert.ElementsMatch(t, []uuid.UUID{dave, daveAlt}, idx["users/dave"].Contacts, "both contacts sharing the canonical id are unioned, not last-writer-wins")

	// (c) Once both uncovered ids matched, the loop STOPPED before zzz-bystander@.
	// Resolution order: dave-alt@ (→users/dave), dave@ (→users/dave, unioned),
	// frank@ (→users/frank, second distinct id matched), then the early-exit fires
	// and zzz-bystander@ is NOT resolved → exactly THREE members.get calls.
	// Critically, the early-exit did NOT fire after only the dave aliases (a scalar
	// count==2 bug) — users/frank is present AND zzz-bystander@ was skipped only
	// AFTER frank, proving the SET semantics.
	assert.Equal(t, 3, calls, "the loop resolves until both distinct uncovered ids match, then stops before the trailing bystander candidate")
}

// TestBuildKnownIDIndex_DualSourceIdentityNotLost is the losslessness boundary.
// A member id (users/dave) whose People-API resolution is a NON-known email
// (looks like a bystander) is STILL matched, because a DIFFERENT known address
// resolves (members.get) to that SAME canonical id. The optimization keys U_id on
// id signals (me-id + positive cache) ONLY, never on People-resolution, so the
// dual-source identity is never skipped/early-exited out. Built on the
// ListGChatIdentitiesForSync dual-source shape: one contact with both a gchat and
// an email value that differ.
func TestBuildKnownIDIndex_DualSourceIdentityNotLost(t *testing.T) {
	ctx := context.Background()
	dave := uuid.New()
	// users/dave is a member; People would resolve it to dave-people@ (a NON-known
	// address — not in knownMap). But the contact is reachable via TWO known
	// addresses (a gchat handle and an email), and the email resolves via
	// members.get to users/dave.
	members := []string{"users/dave", "users/me"}
	fp := memberSetFingerprint(members)

	calls := 0
	// Only the known email resolves to users/dave; the gchat-handle alias is a
	// no-member alias here (it would match in a different space). The point: U_id
	// includes users/dave (People-resolved-to-non-known), so it IS resolved.
	fetcher := fakeIDFetcher(map[string]string{
		"spaces/A|dave-email@example.test": "users/dave",
	}, &calls)
	cap := 10
	resolver := newMemberIDResolver(fetcher, nil, nil, &cap)

	// Dual-source: the SAME contact carries both a gchat value and an email value.
	knownMap := map[string][]uuid.UUID{
		"dave-gchat@example.test": {dave},
		"dave-email@example.test": {dave},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	counters := &sweepCounters{}
	budget := 10
	idx, _, blockedBudget, blockedCap, err := buildKnownIDIndex(ctx, &chat.Space{Name: "spaces/A"}, members, fp, knownMap, meSet, nil, resolver, counters, &budget)
	require.NoError(t, err)
	assert.False(t, blockedBudget)
	assert.False(t, blockedCap)

	// users/dave IS in the index — the optimization did NOT skip it on the basis
	// that People resolves it to a non-known email. The contact matches.
	require.Contains(t, idx, "users/dave", "a dual-source identity is matched via the alternate known address")
	assert.Equal(t, []uuid.UUID{dave}, idx["users/dave"].Contacts)
	assert.GreaterOrEqual(t, calls, 1, "the alternate known address WAS resolved (U_id keys on id signals, not People-resolution)")
}

func TestMembershipNeedsRefresh(t *testing.T) {
	now := accelerated.GetCurrentTime()
	fresh := now.Add(-time.Hour).Format(chatTimeLayout)
	old := now.Add(-25 * time.Hour).Format(chatTimeLayout)

	// Within TTL, lastActiveTime unchanged → reuse.
	cached := spaceMembers{Version: "2026-06-04T09:00:00Z", FetchedAt: fresh, Members: []string{"users/a"}}
	assert.False(t, membershipNeedsRefresh(cached, "2026-06-04T09:00:00Z", now))

	// lastActiveTime advanced → refetch.
	assert.True(t, membershipNeedsRefresh(cached, "2026-06-04T11:00:00Z", now))

	// Age exceeds TTL even though lastActiveTime unchanged → refetch (quiet-space
	// safety net).
	cachedOld := spaceMembers{Version: "2026-06-04T09:00:00Z", FetchedAt: old, Members: []string{"users/a"}}
	assert.True(t, membershipNeedsRefresh(cachedOld, "2026-06-04T09:00:00Z", now))

	// Missing/unparseable fetched_at → refetch.
	cachedNoFetched := spaceMembers{Version: "2026-06-04T09:00:00Z", FetchedAt: "", Members: []string{"users/a"}}
	assert.True(t, membershipNeedsRefresh(cachedNoFetched, "2026-06-04T09:00:00Z", now))
}

func TestReapStaleCursors_ArchiveRestoreDrop(t *testing.T) {
	now := accelerated.GetCurrentTime()
	g := &gchatMetadata{
		SpaceCursors: map[string]spaceCursor{
			"spaces/live":     {CreateCursor: "2026-06-04T10:00:00Z", EditCursor: "2026-06-04T09:00:00Z"},
			"spaces/vanished": {CreateCursor: "2026-06-03T10:00:00Z", EditCursor: "2026-06-03T09:00:00Z"},
		},
		ArchivedCursors: map[string]archivedCursor{
			"spaces/reappears": {CreateCursor: "2026-06-01T10:00:00Z", EditCursor: "2026-06-01T09:00:00Z", ArchivedAt: now.Add(-time.Hour).Format(chatTimeLayout)},
			"spaces/expired":   {CreateCursor: "2026-05-01T10:00:00Z", ArchivedAt: now.Add(-40 * 24 * time.Hour).Format(chatTimeLayout)},
		},
		SpaceMembers:   map[string]spaceMembers{},
		UserEmailCache: map[string]cachedEmail{},
		MeIdentities:   map[string]struct{}{},
	}
	spaces := []*chat.Space{
		{Name: "spaces/live"},
		{Name: "spaces/reappears"},
	}
	reapStaleCursors(g, spaces, now)

	// vanished → archived.
	_, liveOK := g.SpaceCursors["spaces/live"]
	assert.True(t, liveOK)
	_, vanishedActive := g.SpaceCursors["spaces/vanished"]
	assert.False(t, vanishedActive)
	arch, ok := g.ArchivedCursors["spaces/vanished"]
	require.True(t, ok)
	assert.Equal(t, "2026-06-03T10:00:00Z", arch.CreateCursor)

	// reappears → restored at its archived cursor value.
	restored, ok := g.SpaceCursors["spaces/reappears"]
	require.True(t, ok)
	assert.Equal(t, "2026-06-01T10:00:00Z", restored.CreateCursor)
	_, stillArchived := g.ArchivedCursors["spaces/reappears"]
	assert.False(t, stillArchived)

	// expired (>30d) → dropped.
	_, expiredOK := g.ArchivedCursors["spaces/expired"]
	assert.False(t, expiredOK)
}

func TestGChatMetadata_RoundTrip(t *testing.T) {
	// Use a recent resolved_at so the new id caches survive the prune-on-load.
	recent := accelerated.GetCurrentTime().Add(-time.Hour).Format(chatTimeLayout)
	g := &gchatMetadata{
		SpaceCursors:    map[string]spaceCursor{"spaces/A": {CreateCursor: "2026-06-04T10:00:00Z", EditCursor: "2026-06-04T09:00:00Z"}},
		ArchivedCursors: map[string]archivedCursor{"spaces/B": {CreateCursor: "x", ArchivedAt: "2026-06-04T08:00:00Z"}},
		SpaceMembers:    map[string]spaceMembers{"spaces/A": {Version: "v1", FetchedAt: "2026-06-04T10:00:00Z", Members: []string{"users/a"}}},
		UserEmailCache:  map[string]cachedEmail{"users/a": {Email: "a@example.test", ResolvedAt: "2026-06-04T10:00:00Z"}},
		MeIdentities:    map[string]struct{}{"me@example.test": {}},
		EmailUserIDs:    map[string]cachedUserID{"a@example.test": {UserName: "users/a", ResolvedAt: recent}},
		SpaceMemberNegatives: map[string]map[string]memberNegative{
			// Negative under spaces/A, which has a cached membership entry, so it
			// survives the prune-on-load (a negative for a space with no cached
			// membership is dropped — covered by TestPruneIDCaches).
			"spaces/A": {"absent@example.test": {ResolvedAt: recent, MemberSetFingerprint: "fp-1"}},
		},
	}
	// Preserve a non-gchat key to prove read-modify-write doesn't clobber it.
	raw := map[string]any{"backfill_since": "2026-01-01", "some_other_key": 42}
	merged := g.writeInto(raw)

	// Round-trip through JSON (the persistence boundary).
	b, err := json.Marshal(merged)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(b, &back))

	// Non-gchat keys preserved.
	assert.Equal(t, "2026-01-01", back["backfill_since"])
	assert.EqualValues(t, 42, back["some_other_key"])

	reloaded := loadGChatMetadata(back)
	assert.Equal(t, g.SpaceCursors, reloaded.SpaceCursors)
	assert.Equal(t, g.ArchivedCursors, reloaded.ArchivedCursors)
	assert.Equal(t, g.SpaceMembers, reloaded.SpaceMembers)
	assert.Equal(t, g.UserEmailCache, reloaded.UserEmailCache)
	assert.Equal(t, g.MeIdentities, reloaded.MeIdentities)
	// The two new id caches survive writeInto → loadGChatMetadata.
	assert.Equal(t, g.EmailUserIDs, reloaded.EmailUserIDs)
	assert.Equal(t, g.SpaceMemberNegatives, reloaded.SpaceMemberNegatives)
}

func TestMemberSetFingerprint_OrderIndependentAndChangeSensitive(t *testing.T) {
	base := []string{"users/a", "users/b", "users/c"}
	shuffled := []string{"users/c", "users/a", "users/b"}
	withDup := []string{"users/b", "users/a", "users/c", "users/a"}

	fp := memberSetFingerprint(base)
	assert.Equal(t, fp, memberSetFingerprint(shuffled), "fingerprint is order-independent")
	assert.Equal(t, fp, memberSetFingerprint(withDup), "fingerprint dedups before hashing")

	// Adding a member changes the fingerprint (a join is detected).
	assert.NotEqual(t, fp, memberSetFingerprint(append([]string{"users/d"}, base...)),
		"adding a member must change the fingerprint")
	// Removing a member changes the fingerprint (a leave is detected).
	assert.NotEqual(t, fp, memberSetFingerprint([]string{"users/a", "users/b"}),
		"removing a member must change the fingerprint")

	// The empty set is a stable sentinel (and not equal to any non-empty hash).
	assert.Equal(t, memberSetFingerprintEmpty, memberSetFingerprint(nil))
	assert.Equal(t, memberSetFingerprintEmpty, memberSetFingerprint([]string{}))
	assert.Equal(t, memberSetFingerprintEmpty, memberSetFingerprint([]string{""}), "blank ids are skipped")
}

func TestPruneIDCaches_DropsExpiredAndOrphanNegatives(t *testing.T) {
	now := accelerated.GetCurrentTime()
	fresh := now.Add(-time.Hour).Format(chatTimeLayout)
	expired := now.Add(-25 * time.Hour).Format(chatTimeLayout)

	g := &gchatMetadata{
		SpaceMembers: map[string]spaceMembers{
			"spaces/live": {Members: []string{"users/a"}},
		},
		EmailUserIDs: map[string]cachedUserID{
			"keep@example.test": {UserName: "users/a", ResolvedAt: fresh},
			"drop@example.test": {UserName: "users/b", ResolvedAt: expired},
		},
		SpaceMemberNegatives: map[string]map[string]memberNegative{
			"spaces/live": {
				"absent@example.test":  {ResolvedAt: fresh, MemberSetFingerprint: "fp"},
				"expired@example.test": {ResolvedAt: expired, MemberSetFingerprint: "fp"},
			},
			// Orphan: no cached membership for this space → whole entry dropped.
			"spaces/gone": {"x@example.test": {ResolvedAt: fresh, MemberSetFingerprint: "fp"}},
		},
	}
	g.pruneIDCaches(now)

	// Expired global positive dropped; fresh one kept.
	assert.Contains(t, g.EmailUserIDs, "keep@example.test")
	assert.NotContains(t, g.EmailUserIDs, "drop@example.test")

	// Live-space negatives: expired dropped, fresh kept.
	live := g.SpaceMemberNegatives["spaces/live"]
	assert.Contains(t, live, "absent@example.test")
	assert.NotContains(t, live, "expired@example.test")

	// Orphan space (no cached membership) dropped entirely.
	assert.NotContains(t, g.SpaceMemberNegatives, "spaces/gone")
}

func TestGChatMetadata_CursorForSeedsBackfillFloor(t *testing.T) {
	g := loadGChatMetadata(nil)
	floor := "2026-01-01T00:00:00Z"
	cur := g.cursorFor("spaces/new", floor)
	assert.Equal(t, floor, cur.CreateCursor)
	assert.Equal(t, floor, cur.EditCursor)
}

func TestPrimaryNormalizedEmail(t *testing.T) {
	// Primary flagged → that one.
	p := &people.Person{EmailAddresses: []*people.EmailAddress{
		{Value: "secondary@Example.test"},
		{Value: "Primary@Example.test", Metadata: &people.FieldMetadata{Primary: true}},
	}}
	assert.Equal(t, "primary@example.test", primaryNormalizedEmail(p))

	// No primary → first non-empty.
	p2 := &people.Person{EmailAddresses: []*people.EmailAddress{{Value: "First@Example.test"}}}
	assert.Equal(t, "first@example.test", primaryNormalizedEmail(p2))

	// No emails → "".
	assert.Equal(t, "", primaryNormalizedEmail(&people.Person{}))
	assert.Equal(t, "", primaryNormalizedEmail(nil))
}

func TestFlattenKnownMembers_DedupAndSelfExclusion(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	known := map[string][]uuid.UUID{
		"a@example.test":  {a},
		"b@example.test":  {b, a}, // a shared across two addresses
		"me@example.test": {uuid.New()},
	}
	meSet := map[string]struct{}{"me@example.test": {}}
	out := flattenKnownMembers(known, meSet)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, out)
}

func TestFirstN(t *testing.T) {
	assert.Equal(t, "", firstN("anything", 0))
	assert.Equal(t, "ab", firstN("abcdef", 2))
	assert.Equal(t, "abc", firstN("abc", 10))
	// Rune-safe: a multi-byte rune is not split mid-byte.
	assert.Equal(t, "héll", firstN("héllo", 4))
}

// TestConsumeContentWindow_BudgetExhaustionKeepsCursor proves that when the
// shared budget runs out before a multi-page window is fully paged, the window
// returns proven=false and the ORIGINAL cursor (so the caller does NOT advance
// past an un-listed page).
func TestConsumeContentWindow_BudgetExhaustionKeepsCursor(t *testing.T) {
	ctx := context.Background()

	// A fetcher that always reports another page (never returns next == "").
	pageCalls := 0
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			pageCalls++
			// No qualifying messages; always another page.
			return nil, "next-token", nil
		},
		ResolvePersonEmail: func(context.Context, string) (string, error) { return "", nil },
	}}
	resolver := newCachedEmailResolver(fetcher, nil)
	p := &GChatSyncProvider{}
	counters := &sweepCounters{}
	budget := 3
	floor := "2026-01-01T00:00:00Z"

	newCursor, proven, err := p.consumeContentWindow(ctx, fetcher,
		&chat.Space{Name: "spaces/B", SpaceType: "SPACE"}, floor,
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, nil, nil, map[string]struct{}{},
		resolver, "acct", counters, &budget)
	require.NoError(t, err)
	assert.False(t, proven, "an un-finished window is NOT proven")
	assert.Equal(t, floor, newCursor, "cursor must NOT advance past an un-listed page")
	assert.Equal(t, 0, budget, "budget fully consumed")
	assert.Equal(t, 3, pageCalls, "exactly the budgeted number of pages were fetched")
}

// TestConsumeContentWindow_FullyPagedIsProven proves a window that pages to
// completion (next == "") returns proven=true and advances to maxCreate.
func TestConsumeContentWindow_FullyPagedIsProven(t *testing.T) {
	ctx := context.Background()
	alice := uuid.New()
	msgTime := "2026-06-04T10:00:00Z"
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ListMessages: func(_ context.Context, _, _ string, _ bool, token string) ([]*chat.Message, string, error) {
			if token == "" {
				return []*chat.Message{humanMsg("spaces/B/messages/1", "users/alice", msgTime, "hi")}, "", nil
			}
			return nil, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, u string) (string, error) {
			if u == "users/alice" {
				return "alice@example.test", nil
			}
			return "", nil
		},
	}}
	resolver := newCachedEmailResolver(fetcher, nil)
	p := &GChatSyncProvider{commsRepo: nil}
	// Inbound from a known sender → classifyMessage returns Matched; but upsert
	// needs commsRepo. To keep this DB-free, use a bystander (unknown) sender so
	// no upsert happens but the page still advances maxCreate.
	counters := &sweepCounters{}
	budget := 10
	floor := "2026-01-01T00:00:00Z"
	newCursor, proven, err := p.consumeContentWindow(ctx, fetcher,
		&chat.Space{Name: "spaces/B", SpaceType: "SPACE"}, floor,
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{"someone-else@example.test": {alice}}, nil, nil, map[string]struct{}{},
		resolver, "acct", counters, &budget)
	require.NoError(t, err)
	assert.True(t, proven, "a fully-paged window is proven")
	assert.Equal(t, msgTime, newCursor, "cursor advances to the max createTime seen")
}

// TestConsumeContentWindow_CursorAdvancesByInstantNotLexical proves the cursor
// advances to the chronologically-latest createTime even when an EARLIER-listed
// message has a HIGHER fractional-second precision than a same-second
// later-listed message. "...00.001Z" is chronologically newer than "...00Z" but
// sorts lexically SMALLER (the '.' byte < the 'Z' byte); a raw string compare
// would leave the cursor at "...00Z" and re-list the ".001Z" message every
// sweep. The cursor must end at "...00.001Z".
func TestConsumeContentWindow_CursorAdvancesByInstantNotLexical(t *testing.T) {
	ctx := context.Background()
	fracMsg := "2026-06-04T10:00:00.001Z" // chronologically LATER, lexically SMALLER
	zMsg := "2026-06-04T10:00:00Z"        // chronologically EARLIER, lexically LARGER
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ListMessages: func(_ context.Context, _, _ string, _ bool, token string) ([]*chat.Message, string, error) {
			if token == "" {
				// The ".001Z" message is listed FIRST; the same-second "Z" message
				// SECOND. A lexical max would wrongly pick "Z" (the later string).
				return []*chat.Message{
					humanMsg("spaces/B/messages/frac", "users/x", fracMsg, "a"),
					humanMsg("spaces/B/messages/z", "users/x", zMsg, "b"),
				}, "", nil
			}
			return nil, "", nil
		},
		// Bystander sender → no upsert (DB-free), but createTime still advances.
		ResolvePersonEmail: func(context.Context, string) (string, error) { return "stranger@example.test", nil },
	}}
	resolver := newCachedEmailResolver(fetcher, nil)
	p := &GChatSyncProvider{commsRepo: nil}
	counters := &sweepCounters{}
	budget := 10
	floor := "2026-01-01T00:00:00Z"
	newCursor, proven, err := p.consumeContentWindow(ctx, fetcher,
		&chat.Space{Name: "spaces/B", SpaceType: "SPACE"}, floor,
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, nil, nil, map[string]struct{}{},
		resolver, "acct", counters, &budget)
	require.NoError(t, err)
	assert.True(t, proven)
	assert.Equal(t, fracMsg, newCursor,
		"cursor must advance to the chronologically-latest createTime, not the lexically-largest")
}

// TestLaterChatTime pins the instant-comparison cursor primitive directly.
func TestLaterChatTime(t *testing.T) {
	// Fractional-second: ".001Z" is chronologically later than "Z" despite
	// sorting lexically smaller.
	assert.Equal(t, "2026-06-04T10:00:00.001Z",
		laterChatTime("2026-06-04T10:00:00Z", "2026-06-04T10:00:00.001Z"))
	// Reverse order of args → same chronological winner.
	assert.Equal(t, "2026-06-04T10:00:00.001Z",
		laterChatTime("2026-06-04T10:00:00.001Z", "2026-06-04T10:00:00Z"))
	// Strictly later candidate wins.
	assert.Equal(t, "2026-06-04T11:00:00Z",
		laterChatTime("2026-06-04T10:00:00Z", "2026-06-04T11:00:00Z"))
	// Earlier candidate is ignored (keep current).
	assert.Equal(t, "2026-06-04T11:00:00Z",
		laterChatTime("2026-06-04T11:00:00Z", "2026-06-04T10:00:00Z"))
	// Unparseable candidate → keep current (fallback).
	assert.Equal(t, "2026-06-04T10:00:00Z",
		laterChatTime("2026-06-04T10:00:00Z", "not-a-time"))
	// Unparseable current → keep current (we can't prove candidate is later).
	assert.Equal(t, "bad-current",
		laterChatTime("bad-current", "2026-06-04T10:00:00Z"))
}

// TestPaginateMembers_BudgetIncomplete proves membership pagination stops and
// signals incomplete=true when the shared budget runs out before the member
// list is fully paged (so the caller does not act on a partial list).
func TestPaginateMembers_BudgetIncomplete(t *testing.T) {
	ctx := context.Background()
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ListMembers: func(_ context.Context, _, _ string) ([]*chat.Membership, string, error) {
			// Always another page → never completes on its own.
			return []*chat.Membership{membershipForTest("users/x")}, "more", nil
		},
	}}
	budget := 2
	names, pages, incomplete, err := paginateMembers(ctx, fetcher, "spaces/M", &budget)
	require.NoError(t, err)
	assert.True(t, incomplete, "budget exhausted before full paging → incomplete")
	assert.Equal(t, 2, pages, "exactly the budgeted pages were fetched")
	assert.Equal(t, 0, budget)
	assert.Len(t, names, 2, "collected the members from the budgeted pages")

	// A membership that completes within budget is NOT incomplete.
	fetcher2 := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ListMembers: func(_ context.Context, _, token string) ([]*chat.Membership, string, error) {
			if token == "" {
				return []*chat.Membership{membershipForTest("users/a"), membershipForTest("users/b")}, "", nil
			}
			return nil, "", nil
		},
	}}
	budget2 := 10
	names2, _, incomplete2, err := paginateMembers(ctx, fetcher2, "spaces/M2", &budget2)
	require.NoError(t, err)
	assert.False(t, incomplete2)
	assert.ElementsMatch(t, []string{"users/a", "users/b"}, names2)
}

func membershipForTest(userName string) *chat.Membership {
	return &chat.Membership{State: "JOINED", Member: &chat.User{Name: userName, Type: "HUMAN"}}
}

func TestEditLookbackFloor(t *testing.T) {
	backfill := "2026-01-01T00:00:00Z"

	// createCursor − 7d is AFTER the backfill floor → use the lookback.
	got := editLookbackFloor("2026-06-04T00:00:00Z", backfill)
	want := "2026-05-28T00:00:00Z" // 7 days before
	assert.Equal(t, want, got)

	// createCursor − 7d is BEFORE the backfill floor → clamp to backfill.
	got = editLookbackFloor("2026-01-03T00:00:00Z", backfill)
	assert.Equal(t, backfill, got)

	// Unparseable createCursor → fall back to backfill (the safe wider scan).
	got = editLookbackFloor("not-a-time", backfill)
	assert.Equal(t, backfill, got)
}

func TestBuildContentMetadata(t *testing.T) {
	space := &chat.Space{Name: "spaces/A", SpaceType: "SPACE"}
	m := &chat.Message{
		Name:           "spaces/A/messages/1",
		LastUpdateTime: "2026-06-04T10:00:00Z",
		Thread:         &chat.Thread{Name: "spaces/A/threads/t1"},
		Attachment:     []*chat.Attachment{{Name: "att1", ContentName: "file.pdf", ContentType: "application/pdf", Source: "DRIVE_FILE"}},
	}
	raw := buildContentMetadata(space, m)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(raw, &meta))
	assert.Equal(t, "SPACE", meta["space_type"])
	assert.Equal(t, "spaces/A/threads/t1", meta["thread_name"])
	assert.Equal(t, "2026-06-04T10:00:00Z", meta["last_update_time"])
	atts, ok := meta["attachments"].([]any)
	require.True(t, ok)
	require.Len(t, atts, 1)
}

// TestConsumeContentWindow_DeepHistoryCompletesUnderRaisedBudget exercises the
// budget boundary: a window that needs 30 pages strands its cursor under a tight
// 24-page budget but fully pages and advances under gchatMaxWindowsPerSync (100).
// This is the property that lets a deep-history space complete its backfill in a
// single sweep.
//
// NOTE: the fake fetcher here ignores page size entirely — it returns a fixed
// number of pages regardless of the requested page size. So this test validates
// ONLY that a window needing >24 but <=100 pages completes under
// gchatMaxWindowsPerSync. It does NOT and cannot prove the production list call
// requests larger pages; the PageSize(1000) request is pinned separately by
// TestListMessages_RequestsMaxPageSize.
func TestConsumeContentWindow_DeepHistoryCompletesUnderRaisedBudget(t *testing.T) {
	ctx := context.Background()
	floor := "2026-01-01T00:00:00Z"
	// The window needs exactly 30 pages to fully list: pages 1..29 each report
	// another page (a bystander/unknown sender so no upsert and no commsRepo is
	// needed), and page 30 returns next == "" with the latest message.
	const totalPages = 30
	latest := "2026-02-15T08:30:00Z"

	newFetcher := func(callCount *int) *fakeChatFetcher {
		return &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
			ListMessages: func(_ context.Context, _, _ string, _ bool, token string) ([]*chat.Message, string, error) {
				*callCount++
				if *callCount < totalPages {
					// Bystander sender → no upsert; still a non-final page.
					return []*chat.Message{humanMsg("spaces/B/messages/x", "users/stranger", "2026-02-01T00:00:00Z", "x")}, "next", nil
				}
				// Final page carries the latest createTime, no more pages.
				return []*chat.Message{humanMsg("spaces/B/messages/last", "users/stranger", latest, "last")}, "", nil
			},
			ResolvePersonEmail: func(context.Context, string) (string, error) { return "stranger@example.test", nil },
		}}
	}
	p := &GChatSyncProvider{}

	// Sub-case A — under gchatMaxWindowsPerSync (100) the 30-page window fully
	// pages, proven=true, the cursor advances to the latest createTime, and the
	// budget is decremented by exactly the pages consumed.
	t.Run("completes_under_full_budget", func(t *testing.T) {
		calls := 0
		fetcher := newFetcher(&calls)
		resolver := newCachedEmailResolver(fetcher, nil)
		budget := gchatMaxWindowsPerSync
		counters := &sweepCounters{}
		newCursor, proven, err := p.consumeContentWindow(ctx, fetcher,
			&chat.Space{Name: "spaces/B", SpaceType: "SPACE"}, floor,
			map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, nil, nil, map[string]struct{}{},
			resolver, "acct", counters, &budget)
		require.NoError(t, err)
		assert.True(t, proven, "a 30-page window fully pages under the 100-page budget")
		assert.Equal(t, latest, newCursor, "cursor advances to the latest createTime")
		assert.Equal(t, totalPages, calls, "exactly the 30 pages were fetched")
		assert.Equal(t, gchatMaxWindowsPerSync-totalPages, budget, "budget decremented by exactly the pages consumed")
	})

	// Sub-case B — under a tight 24-page budget (a local literal) the same 30-page
	// window cannot complete: it returns proven=false + the original cursor, with
	// the budget fully drained and exactly 24 pages fetched. This is the stranding
	// behavior the raised budget avoids for realistically sized spaces.
	t.Run("strands_under_tight_budget", func(t *testing.T) {
		calls := 0
		fetcher := newFetcher(&calls)
		resolver := newCachedEmailResolver(fetcher, nil)
		budget := 24 // a tight budget smaller than the window's page count
		counters := &sweepCounters{}
		newCursor, proven, err := p.consumeContentWindow(ctx, fetcher,
			&chat.Space{Name: "spaces/B", SpaceType: "SPACE"}, floor,
			map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, nil, nil, map[string]struct{}{},
			resolver, "acct", counters, &budget)
		require.NoError(t, err)
		assert.False(t, proven, "a 30-page window cannot complete under a 24-page budget")
		assert.Equal(t, floor, newCursor, "cursor must NOT advance past an un-listed page")
		assert.Equal(t, 0, budget, "the tight budget is fully drained")
		assert.Equal(t, 24, calls, "exactly the budgeted number of pages were fetched")
	})
}

// TestChatServiceFetcher_ResolveMemberID_RequestPath pins the actual request the
// godoc promises: members.get with the BARE email as the member resource-name
// segment (NOT "members/users/{email}"), parsing the canonical Member.Name from
// the response, and mapping the unrecognized-member statuses (400/404) to
// notMember=true. Like TestListMessages_RequestsMaxPageSize, it stands up an
// httptest.Server and points a real *chat.Service at it (no OAuth, no live
// Google), so it guards against the doubled-prefix mistake at the production
// seam where the unit fakes cannot.
func TestChatServiceFetcher_ResolveMemberID_RequestPath(t *testing.T) {
	t.Run("success_parses_canonical_id", func(t *testing.T) {
		ctx := context.Background()
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			// Membership response whose member carries the CANONICAL users/{id}
			// (the request used the email alias; the response is always canonical).
			_, _ = w.Write([]byte(`{"member":{"name":"users/123456789","type":"HUMAN"}}`))
		}))
		defer server.Close()

		svc, err := chat.NewService(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
		require.NoError(t, err)
		fetcher := &chatServiceFetcher{chat: svc}

		userName, notMember, err := fetcher.ResolveMemberID(ctx, "spaces/ABC", "person@example.test")
		require.NoError(t, err)
		assert.False(t, notMember)
		assert.Equal(t, "users/123456789", userName)
		// The bare email is the member segment — NOT "members/users/{email}".
		assert.Equal(t, "/v1/spaces/ABC/members/person@example.test", gotPath,
			"members.get must use the bare-email member resource name (reserved expansion preserves @/.)")
	})

	// A 400 (unrecognized member) and a 404 both mean "not a member of this
	// space" — a cacheable negative, not an error.
	for _, code := range []int{400, 404} {
		code := code
		t.Run("status_maps_to_notMember", func(t *testing.T) {
			ctx := context.Background()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":{"code":` + strconv.Itoa(code) + `,"message":"Invalid membership state, user, group or request ID"}}`))
			}))
			defer server.Close()

			svc, err := chat.NewService(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
			require.NoError(t, err)
			fetcher := &chatServiceFetcher{chat: svc}

			userName, notMember, err := fetcher.ResolveMemberID(ctx, "spaces/ABC", "stranger@example.test")
			require.NoError(t, err, "an unrecognized-member %d must be a cacheable negative, not an error", code)
			assert.True(t, notMember)
			assert.Empty(t, userName)
		})
	}

	t.Run("server_error_propagates", func(t *testing.T) {
		ctx := context.Background()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"backend error"}}`))
		}))
		defer server.Close()

		svc, err := chat.NewService(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
		require.NoError(t, err)
		fetcher := &chatServiceFetcher{chat: svc}

		_, notMember, err := fetcher.ResolveMemberID(ctx, "spaces/ABC", "person@example.test")
		require.Error(t, err, "a transient 5xx must propagate so the window aborts and retries")
		assert.False(t, notMember)
	})
}

// TestListMessages_RequestsMaxPageSize pins the PageSize(1000) request at the
// production seam. The chatFetcher interface does not carry a page size
// (it is set on the *chat.Service call builder, whose urlParams_ is unexported
// and unreadable from a test), so we stand up an httptest.Server, point a real
// *chat.Service at it via WithoutAuthentication (no OAuth, no live Google), and
// assert the recorded request's pageSize query parameter is 1000. We run it for
// both showDeleted paths to make the "covers both passes" guarantee explicit —
// the content pass (false) and the edit/delete pass (true) both route through
// chatServiceFetcher.ListMessages.
func TestListMessages_RequestsMaxPageSize(t *testing.T) {
	for _, showDeleted := range []bool{false, true} {
		showDeleted := showDeleted
		name := "contentPass"
		if showDeleted {
			name = "editDeletePass"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				// {} decodes to an empty ListMessagesResponse (no messages, no
				// next page token), so ListMessages returns cleanly.
				_, _ = w.Write([]byte("{}"))
			}))
			defer server.Close()

			svc, err := chat.NewService(ctx,
				option.WithEndpoint(server.URL),
				option.WithoutAuthentication())
			require.NoError(t, err)
			fetcher := &chatServiceFetcher{chat: svc}

			msgs, next, err := fetcher.ListMessages(ctx, "spaces/B", `create_time > "2026-01-01T00:00:00Z"`, showDeleted, "")
			require.NoError(t, err)
			assert.Empty(t, msgs)
			assert.Empty(t, next)

			require.NotNil(t, gotQuery, "the server handler must have recorded a request")
			assert.Equal(t, "1000", gotQuery.Get("pageSize"), "ListMessages must request the documented Chat API max page size")
			// Bonus: documents that the create_time ASC ordering is preserved.
			assert.Equal(t, "create_time ASC", gotQuery.Get("orderBy"))
		})
	}
}

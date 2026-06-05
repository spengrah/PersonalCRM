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
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, meSet, resolver)
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
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, meSet, resolver)
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
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, meSet, resolver)
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
	c, err := classifyMessage(ctx, mBy, nil, knownMap, meSet, resolver)
	require.NoError(t, err)
	assert.Empty(t, c.Matched)
	assert.False(t, c.Unresolved)

	// Unresolved sender: resolves to "" → flagged Unresolved, no rows.
	mUn := humanMsg("spaces/S/messages/5", "users/ghost", "2026-06-04T10:04:00Z", "hey")
	c, err = classifyMessage(ctx, mUn, nil, knownMap, meSet, resolver)
	require.NoError(t, err)
	assert.Empty(t, c.Matched)
	assert.True(t, c.Unresolved)
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
	known, err := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)
	require.NoError(t, err)

	// Only Alice is a known co-member; me is excluded, unknown is not in knownMap,
	// noemail is unresolvable.
	require.Len(t, known, 1)
	assert.Equal(t, []uuid.UUID{alice}, known["alice@example.test"])
}

func TestResolveKnownMembers_PropagatesResolveError(t *testing.T) {
	ctx := context.Background()
	resolveErr := errors.New("people api transient")
	fetcher := &fakeChatFetcher{funcs: FakeChatFetcherFuncs{
		ResolvePersonEmail: func(context.Context, string) (string, error) { return "", resolveErr },
	}}
	resolver := newCachedEmailResolver(fetcher, nil)

	_, err := resolveKnownMembers(ctx, []string{"users/alice"}, resolver, map[string][]uuid.UUID{}, map[string]struct{}{})
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
	assert.Equal(t, "empty", memberSetFingerprint(nil))
	assert.Equal(t, "empty", memberSetFingerprint([]string{}))
	assert.Equal(t, "empty", memberSetFingerprint([]string{""}), "blank ids are skipped")
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
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, map[string]struct{}{},
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
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{"someone-else@example.test": {alice}}, map[string]struct{}{},
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
		map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, map[string]struct{}{},
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
			map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, map[string]struct{}{},
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
			map[string][]uuid.UUID{}, map[string][]uuid.UUID{}, map[string]struct{}{},
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

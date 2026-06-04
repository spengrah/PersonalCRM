package google

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
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
	known := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)

	// Only Alice is a known co-member; me is excluded, unknown is not in knownMap,
	// noemail is unresolvable.
	require.Len(t, known, 1)
	assert.Equal(t, []uuid.UUID{alice}, known["alice@example.test"])
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
	g := &gchatMetadata{
		SpaceCursors:    map[string]spaceCursor{"spaces/A": {CreateCursor: "2026-06-04T10:00:00Z", EditCursor: "2026-06-04T09:00:00Z"}},
		ArchivedCursors: map[string]archivedCursor{"spaces/B": {CreateCursor: "x", ArchivedAt: "2026-06-04T08:00:00Z"}},
		SpaceMembers:    map[string]spaceMembers{"spaces/A": {Version: "v1", FetchedAt: "2026-06-04T10:00:00Z", Members: []string{"users/a"}}},
		UserEmailCache:  map[string]cachedEmail{"users/a": {Email: "a@example.test", ResolvedAt: "2026-06-04T10:00:00Z"}},
		MeIdentities:    map[string]struct{}{"me@example.test": {}},
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

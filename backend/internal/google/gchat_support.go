package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
)

// chatTimeLayout is the RFC-3339 form the Chat API returns for createTime /
// lastActiveTime (always with fractional seconds + Z). time.RFC3339Nano parses
// it (and the zero-fractional form) without losing precision.
const chatTimeLayout = time.RFC3339Nano

// --- metadata model --------------------------------------------------

// spaceCursor is one space's create/edit watermarks (RFC-3339 strings, the
// native Chat createTime form, compared as instants in SQL / via parseChatTime).
type spaceCursor struct {
	CreateCursor string `json:"create_cursor"`
	EditCursor   string `json:"edit_cursor"`
}

// archivedCursor is a disappeared space's cursor held for restore-on-rediscovery.
type archivedCursor struct {
	CreateCursor string `json:"create_cursor"`
	EditCursor   string `json:"edit_cursor"`
	ArchivedAt   string `json:"archived_at"`
}

// spaceMembers is the cached membership of one space. version is the cached
// lastActiveTime (drives the activity-based refresh); fetchedAt drives the
// age-based refresh (the safety net for quiet spaces whose lastActiveTime never
// advances). members holds "users/{id}" resource names only.
type spaceMembers struct {
	Version   string   `json:"version"`
	FetchedAt string   `json:"fetched_at"`
	Members   []string `json:"members"`
}

// cachedEmail is one id→email resolution (positive or negative). Email == ""
// is an explicit cached negative (id resolves to no email under the granted
// scope) — REQUIRED so a large space of non-contacts does not re-issue
// people.get every sweep.
type cachedEmail struct {
	Email      string `json:"email"`
	ResolvedAt string `json:"resolved_at"`
}

// cachedUserID is one email→canonical-id resolution in the GLOBAL positive
// cache (the reverse of cachedEmail). UserName is the canonical "users/{id}".
// A canonical id is the same person in every space, so this cache is global
// (not per-space) and reused everywhere once an email resolves anywhere.
type cachedUserID struct {
	UserName   string `json:"user_name"`
	ResolvedAt string `json:"resolved_at"`
}

// memberNegative records that one known email is NOT a member of one space.
// MemberSetFingerprint is the space's member-set fingerprint at the time the
// negative was written: the negative is honored ONLY while within TTL AND its
// fingerprint still matches the space's CURRENT member-set fingerprint. An
// actual join/leave flips the fingerprint and invalidates the negative (so the
// email is re-resolved before the cursor advances past new messages); mere
// message activity does NOT change the fingerprint, so a hot space's negatives
// are not churned (§3.2.1).
type memberNegative struct {
	ResolvedAt           string `json:"resolved_at"`
	MemberSetFingerprint string `json:"member_set_fingerprint"`
}

// gchatMetadata is the typed view of the gchat-owned keys in
// external_sync_state.metadata. It is read from state.Metadata at sweep start
// and written back (read-modify-write — only the gchat keys) at sweep end.
type gchatMetadata struct {
	SpaceCursors    map[string]spaceCursor
	ArchivedCursors map[string]archivedCursor
	SpaceMembers    map[string]spaceMembers
	UserEmailCache  map[string]cachedEmail
	MeIdentities    map[string]struct{}
	// EmailUserIDs is the GLOBAL positive cache: normalizedEmail → canonical id.
	EmailUserIDs map[string]cachedUserID
	// SpaceMemberNegatives is the per-(space,email) negative cache:
	// space.Name → normalizedEmail → memberNegative.
	SpaceMemberNegatives map[string]map[string]memberNegative
}

// loadGChatMetadata decodes the gchat keys from the raw metadata map, tolerating
// missing/malformed keys (a fresh state has none).
func loadGChatMetadata(raw map[string]any) *gchatMetadata {
	gcm := &gchatMetadata{
		SpaceCursors:         map[string]spaceCursor{},
		ArchivedCursors:      map[string]archivedCursor{},
		SpaceMembers:         map[string]spaceMembers{},
		UserEmailCache:       map[string]cachedEmail{},
		MeIdentities:         map[string]struct{}{},
		EmailUserIDs:         map[string]cachedUserID{},
		SpaceMemberNegatives: map[string]map[string]memberNegative{},
	}
	if raw == nil {
		return gcm
	}
	decodeMetaKey(raw, gchatMetaSpaceCursors, &gcm.SpaceCursors)
	decodeMetaKey(raw, gchatMetaArchivedCursors, &gcm.ArchivedCursors)
	decodeMetaKey(raw, gchatMetaSpaceMembers, &gcm.SpaceMembers)
	decodeMetaKey(raw, gchatMetaUserEmailCache, &gcm.UserEmailCache)
	decodeMetaKey(raw, gchatMetaEmailUserIDs, &gcm.EmailUserIDs)
	decodeMetaKey(raw, gchatMetaSpaceMemberNegatives, &gcm.SpaceMemberNegatives)

	var ids []string
	decodeMetaKey(raw, gchatMetaMeIdentities, &ids)
	for _, id := range ids {
		if id != "" {
			gcm.MeIdentities[id] = struct{}{}
		}
	}
	gcm.pruneIDCaches(accelerated.GetCurrentTime())
	return gcm
}

// pruneIDCaches drops expired global positives and expired negatives, plus any
// negative whose space no longer has a cached membership entry. This keeps
// space_member_negatives bounded by live (space × known-absent-email) pairs
// within TTL across the estate. Negatives whose fingerprint no longer matches
// the current member set are NOT pruned here (they are treated as UNKNOWN on
// next read and overwritten when re-resolved — eager pruning is unnecessary for
// correctness). A decode that produced nil maps (very old metadata) is tolerated.
func (g *gchatMetadata) pruneIDCaches(now time.Time) {
	if g.EmailUserIDs == nil {
		g.EmailUserIDs = map[string]cachedUserID{}
	}
	if g.SpaceMemberNegatives == nil {
		g.SpaceMemberNegatives = map[string]map[string]memberNegative{}
	}
	for email, entry := range g.EmailUserIDs {
		if cachedUserIDExpired(entry, now) {
			delete(g.EmailUserIDs, email)
		}
	}
	for spaceName, byEmail := range g.SpaceMemberNegatives {
		if _, hasMembership := g.SpaceMembers[spaceName]; !hasMembership {
			delete(g.SpaceMemberNegatives, spaceName)
			continue
		}
		for email, neg := range byEmail {
			if memberNegativeExpired(neg, now) {
				delete(byEmail, email)
			}
		}
		if len(byEmail) == 0 {
			delete(g.SpaceMemberNegatives, spaceName)
		}
	}
}

// cachedUserIDExpired reports whether a global positive is older than the TTL
// (an unparseable resolved_at counts as expired).
func cachedUserIDExpired(entry cachedUserID, now time.Time) bool {
	resolvedAt, err := time.Parse(chatTimeLayout, entry.ResolvedAt)
	if err != nil {
		return true
	}
	return now.Sub(resolvedAt) > gchatMembershipCacheTTL
}

// memberNegativeExpired reports whether a per-space negative is older than the
// TTL (an unparseable resolved_at counts as expired).
func memberNegativeExpired(neg memberNegative, now time.Time) bool {
	resolvedAt, err := time.Parse(chatTimeLayout, neg.ResolvedAt)
	if err != nil {
		return true
	}
	return now.Sub(resolvedAt) > gchatMembershipCacheTTL
}

// decodeMetaKey round-trips one metadata key through JSON into dst, ignoring
// absent/malformed values (the gchat keys are best-effort caches).
func decodeMetaKey(raw map[string]any, key string, dst any) {
	v, ok := raw[key]
	if !ok {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, dst)
}

// cursorFor returns the cursor for a space, seeding both watermarks to the
// backfill floor on first sight.
func (g *gchatMetadata) cursorFor(spaceName, backfillFloor string) spaceCursor {
	if cur, ok := g.SpaceCursors[spaceName]; ok {
		if cur.CreateCursor == "" {
			cur.CreateCursor = backfillFloor
		}
		if cur.EditCursor == "" {
			cur.EditCursor = backfillFloor
		}
		return cur
	}
	return spaceCursor{CreateCursor: backfillFloor, EditCursor: backfillFloor}
}

func (g *gchatMetadata) setCursor(spaceName string, cur spaceCursor) {
	g.SpaceCursors[spaceName] = cur
}

// writeInto merges the gchat keys back into the raw metadata map (read-modify-
// write: never replaces the whole blob; non-gchat keys are preserved by the
// caller passing the existing map).
func (g *gchatMetadata) writeInto(raw map[string]any) map[string]any {
	if raw == nil {
		raw = map[string]any{}
	}
	raw[gchatMetaSpaceCursors] = g.SpaceCursors
	raw[gchatMetaArchivedCursors] = g.ArchivedCursors
	raw[gchatMetaSpaceMembers] = g.SpaceMembers
	raw[gchatMetaUserEmailCache] = g.UserEmailCache
	raw[gchatMetaEmailUserIDs] = g.EmailUserIDs
	raw[gchatMetaSpaceMemberNegatives] = g.SpaceMemberNegatives
	ids := make([]string, 0, len(g.MeIdentities))
	for id := range g.MeIdentities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	raw[gchatMetaMeIdentities] = ids
	return raw
}

// gchatBackfillFloor returns the per-state onboarding floor as an RFC-3339
// timestamp string (start-of-day UTC), reusing the email backfill_since key so
// the two Google sources share one onboarding knob.
func gchatBackfillFloor(metadata map[string]any) string {
	epoch := sync.EmailBackfillFloorEpoch(metadata)
	return time.Unix(epoch, 0).UTC().Format(chatTimeLayout)
}

// --- stale-cursor reaper + restore -----------------------------------

// reapStaleCursors reconciles space_cursors against the authoritative live
// spaces.list: cursors for vanished spaces move to archived_space_cursors (30d
// grace), reappeared spaces restore their archived cursor, and over-grace
// archived entries are dropped. Logs counts only (never resource names).
func reapStaleCursors(g *gchatMetadata, spaces []*chat.Space, now time.Time) {
	live := make(map[string]struct{}, len(spaces))
	for _, s := range spaces {
		if s != nil && s.Name != "" {
			live[s.Name] = struct{}{}
		}
	}

	archivedCount := 0
	for name, cur := range g.SpaceCursors {
		if _, ok := live[name]; !ok {
			g.ArchivedCursors[name] = archivedCursor{
				CreateCursor: cur.CreateCursor,
				EditCursor:   cur.EditCursor,
				ArchivedAt:   now.Format(chatTimeLayout),
			}
			delete(g.SpaceCursors, name)
			archivedCount++
		}
	}

	restoredCount := 0
	for name := range live {
		if arch, ok := g.ArchivedCursors[name]; ok {
			g.SpaceCursors[name] = spaceCursor{
				CreateCursor: arch.CreateCursor,
				EditCursor:   arch.EditCursor,
			}
			delete(g.ArchivedCursors, name)
			restoredCount++
		}
	}

	droppedCount := 0
	for name, arch := range g.ArchivedCursors {
		archivedAt, err := time.Parse(chatTimeLayout, arch.ArchivedAt)
		if err != nil || now.Sub(archivedAt) > gchatArchivedCursorGrace {
			delete(g.ArchivedCursors, name)
			droppedCount++
		}
	}

	if archivedCount > 0 || restoredCount > 0 || droppedCount > 0 {
		logger.Info().
			Str("source", GChatSourceName).
			Int("archived", archivedCount).
			Int("restored", restoredCount).
			Int("dropped", droppedCount).
			Msg("gchat: reconciled stale space cursors")
	}
}

// --- membership resolution + cache -----------------------------------

// resolveMembership returns the "users/{id}" member set for a space, using the
// cached list unless the activity-based (lastActiveTime advanced) or age-based
// (fetched_at older than TTL) trigger fires. Returns the number of ListMembers
// pages fetched (0 on a cache hit) for the sweep window budget.
// resolveMembership returns the cached members on a cache hit (incomplete=false,
// no budget consumed). On a miss/refresh it pages membership against the shared
// budget; if the budget runs out before the membership is fully paged it returns
// incomplete=true and does NOT cache the partial list — the caller skips the
// space and retries it next sweep (a partial member list would break the
// fan-out).
func (p *GChatSyncProvider) resolveMembership(
	ctx context.Context,
	fetcher chatFetcher,
	space *chat.Space,
	g *gchatMetadata,
	budget *int,
) (members []string, fingerprint string, incomplete bool, err error) {
	now := accelerated.GetCurrentTime()
	cached, ok := g.SpaceMembers[space.Name]
	if ok && !membershipNeedsRefresh(cached, space.LastActiveTime, now) {
		return cached.Members, memberSetFingerprint(cached.Members), false, nil
	}

	fetched, _, incomplete, err := paginateMembers(ctx, fetcher, space.Name, budget)
	if err != nil {
		return nil, "", false, err
	}
	if incomplete {
		return nil, "", true, nil
	}
	g.SpaceMembers[space.Name] = spaceMembers{
		Version:   space.LastActiveTime,
		FetchedAt: now.Format(chatTimeLayout),
		Members:   fetched,
	}
	return fetched, memberSetFingerprint(fetched), false, nil
}

// membershipNeedsRefresh reports whether a cached membership must be refetched:
// the space's lastActiveTime advanced past the cached version, OR the cache is
// older than the TTL (the quiet-space safety net).
func membershipNeedsRefresh(cached spaceMembers, lastActiveTime string, now time.Time) bool {
	if lastActiveTime != "" && lastActiveTime != cached.Version {
		if newer, ok := chatTimeAfter(lastActiveTime, cached.Version); ok && newer {
			return true
		}
		if cached.Version == "" {
			return true
		}
	}
	fetchedAt, err := time.Parse(chatTimeLayout, cached.FetchedAt)
	if err != nil {
		return true // unparseable/absent fetched_at → refetch
	}
	return now.Sub(fetchedAt) > gchatMembershipCacheTTL
}

// resolveKnownMembers maps a space's "users/{id}" members to the
// normalized-address → []contactID entries they match, EXCLUDING any member
// that resolves to one of the connected accounts' own emails (self is never a
// co-member to fan out to). The map is keyed by normalized address.
//
// A resolver ERROR is propagated (NOT swallowed): a transient People API failure
// for a co-member must abort the space's window so the cursor stays unadvanced
// and the whole window retries next sweep. Swallowing it would silently drop the
// member, produce a partial outbound fan-out, and then advance the cursor past
// rows that were never persisted. A resolved-to-no-email member ("" with no
// error) is simply not a known co-member.
func resolveKnownMembers(
	ctx context.Context,
	members []string,
	resolver *cachedEmailResolver,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
) (map[string][]uuid.UUID, error) {
	out := make(map[string][]uuid.UUID)
	for _, userName := range members {
		email, err := resolver.resolve(ctx, userName)
		if err != nil {
			return nil, err
		}
		if email == "" {
			continue // resolved to no email → not a known co-member
		}
		if inSet(meSet, email) {
			continue // self
		}
		if contacts := knownMap[email]; len(contacts) > 0 {
			out[email] = contacts
		}
	}
	return out, nil
}

// memberSetFingerprint returns a deterministic, order-independent hash of a
// space's member-id set. It SORTs + DEDUPs the canonical "users/{id}" names and
// returns the sha256 hex of the newline-joined result, so the same member set
// always yields the same fingerprint regardless of ListMembers page ordering.
// An empty set yields a fixed sentinel. The fingerprint is the §3.2.1 stability
// signal: it changes ONLY when membership actually changes (a join/leave), never
// on mere message activity (which advances lastActiveTime but not the set).
func memberSetFingerprint(members []string) string {
	if len(members) == 0 {
		return "empty"
	}
	uniq := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	if len(uniq) == 0 {
		return "empty"
	}
	sort.Strings(uniq)
	sum := sha256.Sum256([]byte(strings.Join(uniq, "\n")))
	return hex.EncodeToString(sum[:])
}

// --- cached email resolver -------------------------------------------

// cachedEmailResolver resolves "users/{id}" → normalized email, backed by the
// persisted metadata cache (positive AND negative entries, 24h TTL) plus an
// in-sweep memo. New/refreshed resolutions are accumulated in dirty so the
// provider can fold them back into metadata after the sweep.
type cachedEmailResolver struct {
	fetcher chatFetcher
	cache   map[string]cachedEmail
	memo    map[string]string // userName → email (in-sweep, includes "" negatives)
	dirty   bool
}

func newCachedEmailResolver(fetcher chatFetcher, cache map[string]cachedEmail) *cachedEmailResolver {
	if cache == nil {
		cache = map[string]cachedEmail{}
	}
	return &cachedEmailResolver{
		fetcher: fetcher,
		cache:   cache,
		memo:    map[string]string{},
	}
}

// resolve returns the normalized email for a "users/{id}" name ("" for an
// unresolved/no-email sender). A people.get error propagates (so the caller can
// abort the window and retry), but a resolved-to-no-email result is a cached
// negative, not an error.
func (r *cachedEmailResolver) resolve(ctx context.Context, userName string) (string, error) {
	if userName == "" {
		return "", nil
	}
	if email, ok := r.memo[userName]; ok {
		return email, nil
	}
	if entry, ok := r.cache[userName]; ok && !cachedEmailExpired(entry) {
		r.memo[userName] = entry.Email
		return entry.Email, nil
	}

	email, err := r.fetcher.ResolvePersonEmail(ctx, userName)
	if err != nil {
		return "", err
	}
	r.memo[userName] = email
	r.cache[userName] = cachedEmail{
		Email:      email,
		ResolvedAt: accelerated.GetCurrentTime().Format(chatTimeLayout),
	}
	r.dirty = true
	return email, nil
}

// snapshot returns the (possibly-grown) cache for persistence.
func (r *cachedEmailResolver) snapshot() map[string]cachedEmail {
	return r.cache
}

func cachedEmailExpired(entry cachedEmail) bool {
	resolvedAt, err := time.Parse(chatTimeLayout, entry.ResolvedAt)
	if err != nil {
		return true
	}
	return accelerated.GetCurrentTime().Sub(resolvedAt) > gchatMembershipCacheTTL
}

// --- metadata persistence --------------------------------------------

// persistMetadata folds the resolver's grown email cache into the typed
// metadata and writes the merged gchat keys back via UpdateSyncStateMetadata
// (read-modify-write of state.Metadata — only the gchat keys change).
func (p *GChatSyncProvider) persistMetadata(
	ctx context.Context,
	state *repository.SyncState,
	g *gchatMetadata,
	resolver *cachedEmailResolver,
) error {
	g.UserEmailCache = resolver.snapshot()
	merged := g.writeInto(state.Metadata)
	_, err := p.syncRepo.UpdateSyncStateMetadata(ctx, state.ID, merged)
	return err
}

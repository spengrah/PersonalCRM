package google

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	chat "google.golang.org/api/chat/v1"

	"github.com/google/uuid"

	"personal-crm/backend/internal/accelerated"
)

// --- pagination ------------------------------------------------------

// paginateSpaces drains all pages of the user's spaces.
func paginateSpaces(ctx context.Context, fetcher chatFetcher) ([]*chat.Space, error) {
	var all []*chat.Space
	pageToken := ""
	for {
		page, next, err := fetcher.ListSpaces(ctx, pageToken)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		pageToken = next
	}
}

// paginateMembers drains all pages of a space's JOINED human members, returning
// their "users/{id}" resource names and the number of pages fetched. budget is
// the shared remaining-page allowance, decremented per list page; if it runs out
// before the membership is fully paged, incomplete is true and the (partial)
// names MUST NOT be used or cached — a partial member list would produce a wrong
// fan-out. The caller skips the space and retries it next sweep.
func paginateMembers(ctx context.Context, fetcher chatFetcher, spaceName string, budget *int) (names []string, pages int, incomplete bool, err error) {
	pageToken := ""
	for {
		if *budget <= 0 {
			return names, pages, true, nil
		}
		page, next, listErr := fetcher.ListMembers(ctx, spaceName, pageToken)
		if listErr != nil {
			return nil, pages, false, listErr
		}
		*budget--
		pages++
		for _, m := range page {
			if m == nil || m.Member == nil {
				continue
			}
			if m.State != "" && m.State != gchatMembershipStateJoined {
				continue
			}
			if m.Member.Type != gchatUserTypeHuman {
				continue
			}
			if m.Member.Name != "" {
				names = append(names, m.Member.Name)
			}
		}
		if next == "" {
			return names, pages, false, nil
		}
		pageToken = next
	}
}

// --- time helpers ----------------------------------------------------

// parseChatTime parses a Chat RFC-3339 timestamp into a time.Time.
func parseChatTime(s string) (time.Time, error) {
	return time.Parse(chatTimeLayout, s)
}

// chatTimeAfter reports whether a is strictly after b as instants. ok is false
// when either side is unparseable (the caller decides the fallback).
func chatTimeAfter(a, b string) (after, ok bool) {
	ta, errA := time.Parse(chatTimeLayout, a)
	if errA != nil {
		return false, false
	}
	tb, errB := time.Parse(chatTimeLayout, b)
	if errB != nil {
		return false, false
	}
	return ta.After(tb), true
}

// laterChatTime returns the chronologically-later of current and candidate,
// compared as INSTANTS (not lexicographically). It keeps current when candidate
// is not strictly after it OR when either side is unparseable. This is the
// cursor-advance primitive: a raw string > comparison is wrong because RFC-3339
// is not lexically ordered across varying fractional-second precision (e.g.
// "...:00.001Z" is chronologically newer than "...:00Z" but sorts smaller),
// which would under-advance the cursor and re-list already-ingested messages.
func laterChatTime(current, candidate string) string {
	if after, ok := chatTimeAfter(candidate, current); ok && after {
		return candidate
	}
	return current
}

// --- set + slice helpers ---------------------------------------------

// inSet reports membership in a set of normalized addresses.
func inSet(set map[string]struct{}, v string) bool {
	_, ok := set[v]
	return ok
}

// containsUUID reports whether id is already in the slice.
func containsUUID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// flattenKnownMembers collects the deduped contact ids across all known
// co-members, excluding any address in meSet (self never receives an
// outreach row). Deterministic ordering is not required (the caller upserts
// per-contact and the dedup unique makes order irrelevant).
func flattenKnownMembers(knownMembers map[string][]uuid.UUID, meSet map[string]struct{}) []uuid.UUID {
	var out []uuid.UUID
	for addr, contacts := range knownMembers {
		if inSet(meSet, addr) {
			continue
		}
		for _, c := range contacts {
			if !containsUUID(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// flattenKnownMembersAndIDs is the outbound fan-out recipient set: the UNION of
// the email-resolved known co-members (flattenKnownMembers) and the id-resolved
// known members (knownIDs values), deduped by contact id and self-excluded. An
// idMatch's Email is checked against meSet defensively (the index already drops
// meSet members, so this never fires in practice but keeps the self-exclusion
// invariant local).
func flattenKnownMembersAndIDs(knownMembers map[string][]uuid.UUID, knownIDs map[string]idMatch, meSet map[string]struct{}) []uuid.UUID {
	out := flattenKnownMembers(knownMembers, meSet)
	for _, idm := range knownIDs {
		if inSet(meSet, idm.Email) {
			continue
		}
		for _, c := range idm.Contacts {
			if !containsUUID(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// firstN returns the first n characters (runes) of s.
func firstN(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// --- content metadata ------------------------------------------------

// gchatAttachmentMeta is the metadata-only descriptor for one Chat attachment.
// No content is ever fetched.
type gchatAttachmentMeta struct {
	Name        string `json:"name,omitempty"`
	ContentName string `json:"content_name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Source      string `json:"source,omitempty"`
}

// gchatContentMetadata is the non-provenance JSON the provider assembles into
// comms_message.source_metadata. The provenance keys (observed_accounts,
// account_gmail_ids) are added by the UpsertCommsMessage query, not here.
type gchatContentMetadata struct {
	SpaceType      string                `json:"space_type,omitempty"`
	ThreadName     string                `json:"thread_name,omitempty"`
	LastUpdateTime string                `json:"last_update_time,omitempty"`
	Attachments    []gchatAttachmentMeta `json:"attachments,omitempty"`
}

// buildContentMetadata assembles the source_metadata blob for a content-pass
// message. Marshalling never fails for these fields; on the impossible error we
// fall back to an empty object so the upsert's ::jsonb cast stays valid.
func buildContentMetadata(space *chat.Space, m *chat.Message) []byte {
	meta := gchatContentMetadata{
		SpaceType:      space.SpaceType,
		LastUpdateTime: m.LastUpdateTime,
	}
	if m.Thread != nil {
		meta.ThreadName = m.Thread.Name
	}
	for _, a := range m.Attachment {
		if a == nil {
			continue
		}
		meta.Attachments = append(meta.Attachments, gchatAttachmentMeta{
			Name:        a.Name,
			ContentName: a.ContentName,
			ContentType: a.ContentType,
			Source:      a.Source,
		})
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// --- known-id index (reverse email→id matching) ----------------------

// idMatch is the value of the per-space known-id index: the CRM email that
// resolved to a canonical id, plus the contact ids that email maps to. The
// email is carried so an id-path match populates the peer columns with the CRM
// email (an id-path People-API resolution is "", which would otherwise write an
// empty peer).
type idMatch struct {
	Email    string
	Contacts []uuid.UUID
}

// buildKnownIDIndex builds the per-space "users/{id}" → idMatch index that lets
// a sender/member match a CRM contact by CANONICAL ID even when the People API
// (Contacts) cannot resolve that id to an email. It has two zero-API-call
// sources (global positive cache + the space's member-id list) plus bounded
// fresh members.get resolutions for the rest.
//
// members is the space's current "users/{id}" member set; fingerprint is its
// member-set fingerprint. knownMap is the dual-source known-contact
// map (normalizedEmail → []contactID); meSet is the connected-account self set.
// resolver is the reverse resolver; pageBudget is the shared page allowance.
//
// Returns:
//   - knownIDs: the index. A member/sender id present here is a known contact.
//   - blockedByBudget: a fresh resolution hit the page budget (incomplete window
//     — the caller must NOT advance the cursor; treat like a partial page).
//   - blockedByCapOnDebt: an UNKNOWN candidate could not be resolved because the
//     per-sweep resolve-cap was exhausted (resolution debt). This is purely
//     informational now — it drives the spacesWarmupDeferred observability counter
//     and NEVER holds the cursor (id-resolution is a best-effort background
//     warm-up; the caller pages content and advances regardless).
//
// Candidate classification under the current fingerprint: POSITIVE (global
// positive hit whose id ∈ members), NEGATIVE-VALID (per-space negative with a
// matching fingerprint, within TTL), or UNKNOWN (must resolve). UNKNOWN
// fingerprint-invalidated candidates (a negative whose fingerprint no longer
// matches — a membership change happened) get PRIORITY access to the cap over
// never-seen candidates, so a one-time debt drains efficiently.
// meIDs is the set of the account's OWN canonical ids within this space, seeded
// by resolveKnownMembers (People-path) and extended here from the reverse
// resolver's ALREADY-CACHED positives for the meSet emails (no fresh members.get,
// no budget impact) — so a me-id already discovered in an earlier space is caught
// even when People cannot resolve it (the not-in-Contacts me case). Any resolved
// id in meIDs is dropped from the index so the inbound id-path can never fire for
// the account itself, even if a stray non-meSet CRM alias resolves to the
// account's own id. classifyMessage ALSO guards its inbound id-path with meIDs as
// a defense-in-depth check at the point of use.
//
// Cost bound (lossless): a fresh members.get can only ADD a NEW index entry for a
// member id that is not ALREADY definitively covered by an id-keyed signal — i.e.
// a member id that is neither a me-id nor already in the global positive cache.
// Call that set U_id = members \ (meIDs ∪ positive-cached ids). When U_id is
// empty, every fresh resolution is a guaranteed not-a-member, so the candidate
// loop is skipped entirely. Otherwise the loop early-exits once every DISTINCT id
// in U_id has been attached (a SET, not a count — two known addresses can resolve
// to the SAME canonical id, so counting raw resolutions would terminate early and
// drop a still-unmatched member id). U_id deliberately keys ONLY on me-id +
// positive-cache, NEVER on People-resolution: a member id People resolves to a
// non-known email can still be a valid CRM contact reachable via a DIFFERENT known
// gchat/email address (dual-source identity, ListGChatIdentitiesForSync), so it
// MUST remain a candidate.
func buildKnownIDIndex(
	ctx context.Context,
	space *chat.Space,
	members []string,
	fingerprint string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	meIDs map[string]struct{},
	resolver *memberIDResolver,
	counters *sweepCounters,
	pageBudget *int,
) (knownIDs map[string]idMatch, meIDsOut map[string]struct{}, blockedByBudget bool, blockedByCapOnDebt bool, err error) {
	knownIDs = map[string]idMatch{}

	// The member-id set of this space, for the "id ∈ members" check.
	memberSet := make(map[string]struct{}, len(members))
	for _, id := range members {
		memberSet[id] = struct{}{}
	}

	// Extend meIDs from the reverse resolver's already-cached positives for the
	// meSet emails (cache-only — no fresh members.get, no budget consumption, no
	// negative writes). This catches a me-id discovered in an earlier space.
	for meEmail := range meSet {
		if id, ok := resolver.cachedPositive(meEmail); ok {
			if _, already := meIDs[id]; !already {
				// Copy-on-write so we never mutate the caller's map.
				meIDs = mergeMeID(meIDs, id)
			}
		}
	}
	meIDsOut = meIDs // the (possibly extended) me-id set, for the classify guard

	// coveredIDs is the set of member ids already definitively covered by an
	// id-keyed signal: a me-id, or an id already in the global positive cache.
	// These are the ONLY ids a fresh members.get can never newly attach, so
	// U_id = members \ coveredIDs are the only ids worth resolving for. Seed it from
	// meIDs and grow it with every positive-cache hit in the seed pass below.
	coveredIDs := make(map[string]struct{}, len(meIDs))
	for id := range meIDs {
		coveredIDs[id] = struct{}{}
	}

	// Seed from the GLOBAL positive cache: every known email whose cached id is a
	// member of this space matches with zero API calls. Track which emails are
	// already matched so they are not re-resolved.
	seeded := map[string]struct{}{}
	for email, contacts := range knownMap {
		if inSet(meSet, email) || len(contacts) == 0 {
			continue
		}
		id, ok := resolver.cachedPositive(email)
		if !ok {
			continue
		}
		// The cached id is definitively covered whether or not it is a member of THIS
		// space (it can only be excluded from U_id by being a member, but tracking it
		// here keeps coveredIDs the exact "id-keyed covered" set).
		coveredIDs[id] = struct{}{}
		if _, isMe := meIDs[id]; isMe {
			continue // the account's own id never enters the reverse index
		}
		if _, isMember := memberSet[id]; isMember {
			knownIDs[id] = mergeIDMatch(knownIDs[id], email, contacts)
			seeded[email] = struct{}{}
		}
	}

	// U_id: the distinct member ids NOT yet covered by an id-keyed signal. These
	// are the only ids a fresh resolution could newly attach to a known email.
	uncovered := make(map[string]struct{}, len(members))
	for _, id := range members {
		if _, covered := coveredIDs[id]; !covered {
			uncovered[id] = struct{}{}
		}
	}
	// Lossless skip: no member id any known email could newly match → every fresh
	// resolution is a guaranteed not-a-member. Skip the candidate loop entirely
	// (the zero-API-call positive seed above still applied).
	if len(uncovered) == 0 {
		return knownIDs, meIDsOut, blockedByBudget, blockedByCapOnDebt, nil
	}

	// Classify the remaining candidates and order them so fingerprint-invalidated
	// (was-NEGATIVE-now-UNKNOWN) candidates get priority access to the cap.
	priority, normal := classifyIDCandidates(space.Name, fingerprint, knownMap, meSet, seeded, resolver)

	// matchedUncovered tracks the DISTINCT ids in U_id actually attached. Once it
	// equals U_id, no fresh resolution can add a new member-id entry, so the loop
	// early-exits (every remaining candidate is a guaranteed not-a-member). A SET,
	// not a count: dual-source identities can resolve two known addresses to one id.
	matchedUncovered := make(map[string]struct{}, len(uncovered))

	// Resolve priority candidates first, then never-seen ones (two ranges instead
	// of a merged slice to avoid a per-call allocation in the hot per-space path).
	for _, group := range [2][]string{priority, normal} {
		for _, email := range group {
			if len(matchedUncovered) == len(uncovered) {
				// Every uncovered member id is attached → nothing left to resolve.
				return knownIDs, meIDsOut, blockedByBudget, blockedByCapOnDebt, nil
			}
			id, status, rerr := resolver.resolve(ctx, space.Name, fingerprint, email, pageBudget)
			if rerr != nil {
				return nil, nil, false, false, rerr
			}
			switch status {
			case resolvedKnownID:
				if _, isMe := meIDs[id]; isMe {
					break // the account's own id never enters the reverse index
				}
				if _, isMember := memberSet[id]; isMember {
					knownIDs[id] = mergeIDMatch(knownIDs[id], email, knownMap[email])
					counters.memberIDsResolved++
					if _, isUncovered := uncovered[id]; isUncovered {
						matchedUncovered[id] = struct{}{}
					}
				}
				// A resolved id that is NOT a member of this space is a co-member of
				// some OTHER space (the positive is global) — not a match here, but the
				// global positive is now populated for future spaces.
			case notMember:
				counters.memberResolveNegativesWritten++
			case deferredCapHit:
				counters.memberResolveDeferredCap++
				blockedByCapOnDebt = true
			case deferredBudgetHit:
				blockedByBudget = true
				return knownIDs, meIDsOut, blockedByBudget, blockedByCapOnDebt, nil
			}
		}
	}
	return knownIDs, meIDsOut, blockedByBudget, blockedByCapOnDebt, nil
}

// mergeIDMatch folds an additional (email, contacts) seed for one canonical id
// into the existing index entry, UNIONing the contact ids so all contacts that
// share a canonical id via the positive-cache / resolve paths are matched (rather
// than the second writer silently overwriting the first). The Email is kept as
// the first writer's (the peer identity is any of the equivalent known addresses;
// they all map to the same person). A nil/zero prev starts a fresh entry.
func mergeIDMatch(prev idMatch, email string, contacts []uuid.UUID) idMatch {
	if prev.Email == "" && len(prev.Contacts) == 0 {
		return idMatch{Email: email, Contacts: append([]uuid.UUID(nil), contacts...)}
	}
	out := idMatch{Email: prev.Email, Contacts: prev.Contacts}
	for _, c := range contacts {
		if !containsUUID(out.Contacts, c) {
			out.Contacts = append(out.Contacts, c)
		}
	}
	return out
}

// mergeMeID returns a new set that is src plus id (copy-on-write so the caller's
// map is never mutated). src may be nil.
func mergeMeID(src map[string]struct{}, id string) map[string]struct{} {
	out := make(map[string]struct{}, len(src)+1)
	for k := range src {
		out[k] = struct{}{}
	}
	out[id] = struct{}{}
	return out
}

// classifyIDCandidates partitions the not-yet-matched known emails into those
// whose per-space negative was invalidated by a fingerprint change (priority —
// a membership change happened, so re-resolve first) and the rest (never-seen,
// or no negative). meSet emails and already-seeded emails are skipped. Both
// slices are sorted for deterministic ordering across runs.
func classifyIDCandidates(
	spaceName, fingerprint string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	seeded map[string]struct{},
	resolver *memberIDResolver,
) (priority, normal []string) {
	for email, contacts := range knownMap {
		if inSet(meSet, email) || len(contacts) == 0 {
			continue
		}
		if _, ok := seeded[email]; ok {
			continue
		}
		// A within-TTL, fingerprint-matching negative is NEGATIVE-VALID → not a
		// candidate (no debt). Anything else is UNKNOWN → a candidate.
		if neg, ok := resolver.negativeFor(spaceName, email); ok {
			if !memberNegativeExpired(neg, accelerated.GetCurrentTime()) && neg.MemberSetFingerprint == fingerprint {
				continue // NEGATIVE-VALID
			}
			// A stale negative (fingerprint changed or expired) is UNKNOWN with
			// PRIORITY — a membership change happened, so drain it first.
			priority = append(priority, email)
			continue
		}
		normal = append(normal, email) // never seen → UNKNOWN
	}
	sort.Strings(priority)
	sort.Strings(normal)
	return priority, normal
}

// --- test seams ------------------------------------------------------

// SetFetcherFactoryForTest overrides the per-account fetcher factory so tests
// inject a fake chatFetcher with no OAuth/token state.
func (p *GChatSyncProvider) SetFetcherFactoryForTest(factory func(ctx context.Context, accountID string) (chatFetcher, error)) {
	p.newFetcher = factory
}

// SetMeSetForTest overrides the me-set factory so tests inject the connected-
// account address set with no OAuth state.
func (p *GChatSyncProvider) SetMeSetForTest(meSet map[string]struct{}) {
	p.newMeSet = func(context.Context) (map[string]struct{}, error) {
		return meSet, nil
	}
}

// SetMemberResolveCapForTest overrides the per-sweep reverse-resolve cap so a
// test can drive the resolve-cap deferral (resolution-debt) path
// deterministically. Production code must NOT call this.
func (p *GChatSyncProvider) SetMemberResolveCapForTest(cap int) {
	p.memberResolveCapOverride = &cap
}

// FakeChatFetcherFuncs lets a cross-package test supply a fake chatFetcher by
// closures, since the chatFetcher interface is unexported. Build the fetcher
// with NewFakeChatFetcherFactoryForTest and inject via SetFetcherFactoryForTest.
type FakeChatFetcherFuncs struct {
	ListSpaces         func(ctx context.Context, pageToken string) ([]*chat.Space, string, error)
	ListMembers        func(ctx context.Context, spaceName, pageToken string) ([]*chat.Membership, string, error)
	ListMessages       func(ctx context.Context, spaceName, filter string, showDeleted bool, pageToken string) ([]*chat.Message, string, error)
	ResolvePersonEmail func(ctx context.Context, userName string) (string, error)
	// ResolveMemberID is optional: a fake that omits it defaults to "not a member
	// of this space" (notMember=true, no error), so existing tests that exercise
	// only the People-API email path compile and keep their current outcomes — the
	// id path is purely additive.
	ResolveMemberID func(ctx context.Context, spaceName, normalizedEmail string) (string, bool, error)
}

type fakeChatFetcher struct {
	funcs FakeChatFetcherFuncs
}

func (f *fakeChatFetcher) ListSpaces(ctx context.Context, pageToken string) ([]*chat.Space, string, error) {
	return f.funcs.ListSpaces(ctx, pageToken)
}

func (f *fakeChatFetcher) ListMembers(ctx context.Context, spaceName, pageToken string) ([]*chat.Membership, string, error) {
	return f.funcs.ListMembers(ctx, spaceName, pageToken)
}

func (f *fakeChatFetcher) ListMessages(ctx context.Context, spaceName, filter string, showDeleted bool, pageToken string) ([]*chat.Message, string, error) {
	return f.funcs.ListMessages(ctx, spaceName, filter, showDeleted, pageToken)
}

func (f *fakeChatFetcher) ResolvePersonEmail(ctx context.Context, userName string) (string, error) {
	return f.funcs.ResolvePersonEmail(ctx, userName)
}

func (f *fakeChatFetcher) ResolveMemberID(ctx context.Context, spaceName, normalizedEmail string) (string, bool, error) {
	if f.funcs.ResolveMemberID == nil {
		// Default: the email is not a member of this space (the id path is purely
		// additive, so a fake that doesn't opt in behaves as the email-only path).
		return "", true, nil
	}
	return f.funcs.ResolveMemberID(ctx, spaceName, normalizedEmail)
}

// NewFakeChatFetcherFactoryForTest returns a fetcher factory (accountID-keyed,
// the SetFetcherFactoryForTest shape) yielding one closure-backed fake fetcher,
// with NO OAuth/token state. Production code must NOT call this.
func NewFakeChatFetcherFactoryForTest(funcs FakeChatFetcherFuncs) func(ctx context.Context, accountID string) (chatFetcher, error) {
	fetcher := &fakeChatFetcher{funcs: funcs}
	return func(context.Context, string) (chatFetcher, error) {
		return fetcher, nil
	}
}

// CachedEmailResolverForTest wraps the unexported cachedEmailResolver so a
// cross-package test can build one for RunQualifyForTest without reaching the
// unexported type. Production code must NOT use this.
type CachedEmailResolverForTest struct {
	inner *cachedEmailResolver
}

// NewCachedEmailResolverForTest builds a resolver over a fake fetcher (the same
// FakeChatFetcherFuncs shape) with an empty cache. Production code must NOT call
// this.
func NewCachedEmailResolverForTest(funcs FakeChatFetcherFuncs) *CachedEmailResolverForTest {
	return &CachedEmailResolverForTest{inner: newCachedEmailResolver(&fakeChatFetcher{funcs: funcs}, nil)}
}

// Resolve exposes the unexported resolver's resolve for cross-package tests
// (id→email caching + TTL coverage). Production code must NOT call this.
func (r *CachedEmailResolverForTest) Resolve(ctx context.Context, userName string) (string, error) {
	return r.inner.resolve(ctx, userName)
}

// SweepCountersForTest is the exported view of the per-sweep counters so tests
// can assert qualification outcomes. Production code must NOT use this.
type SweepCountersForTest struct {
	Processed                     int
	Matched                       int
	SendersUnresolved             int
	EditsApplied                  int
	DeletesApplied                int
	MemberIDsResolved             int
	MemberResolveDeferredCap      int
	MemberResolveNegativesWritten int
	SpacesWarmupDeferred          int
}

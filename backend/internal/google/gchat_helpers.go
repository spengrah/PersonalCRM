package google

import (
	"context"
	"encoding/json"
	"time"

	chat "google.golang.org/api/chat/v1"

	"github.com/google/uuid"
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
// their "users/{id}" resource names and the number of pages fetched.
func paginateMembers(ctx context.Context, fetcher chatFetcher, spaceName string) ([]string, int, error) {
	var names []string
	pageToken := ""
	pages := 0
	for {
		page, next, err := fetcher.ListMembers(ctx, spaceName, pageToken)
		if err != nil {
			return nil, pages, err
		}
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
			return names, pages, nil
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

// FakeChatFetcherFuncs lets a cross-package test supply a fake chatFetcher by
// closures, since the chatFetcher interface is unexported. Build the fetcher
// with NewFakeChatFetcherFactoryForTest and inject via SetFetcherFactoryForTest.
type FakeChatFetcherFuncs struct {
	ListSpaces         func(ctx context.Context, pageToken string) ([]*chat.Space, string, error)
	ListMembers        func(ctx context.Context, spaceName, pageToken string) ([]*chat.Membership, string, error)
	ListMessages       func(ctx context.Context, spaceName, filter string, showDeleted bool, pageToken string) ([]*chat.Message, string, error)
	ResolvePersonEmail func(ctx context.Context, userName string) (string, error)
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
	Processed                  int
	Matched                    int
	SpacesSkippedNoKnownMember int
	SendersUnresolved          int
	EditsApplied               int
	DeletesApplied             int
}

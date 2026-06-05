package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	people "google.golang.org/api/people/v1"
)

const (
	// GChatDefaultInterval is the default sync interval for Google Chat (every
	// 15 minutes), matching the Gmail/Calendar tick.
	GChatDefaultInterval = 15 * time.Minute

	// gchatMaxWindowsPerSync bounds the total list-page iterations a single
	// sweep or scan may consume (membership + content + edit/delete passes
	// across all spaces). It is the shared page budget for BOTH the steady-state
	// sweep (Sync) AND the one-shot rematch scan (ScanIdentifier). Because
	// ListMessages now requests up to 1000 messages per page, a realistically
	// sized space pages in one or very few calls, so 100 gives generous headroom
	// for accounts with many (or deep) spaces without one sweep/scan starving the
	// rest. Once exhausted the sweep returns and un-advanced space cursors restart
	// next tick (crash-safe); the rematch scan instead fails its River job and
	// retries (idempotent via the upsert dedup).
	gchatMaxWindowsPerSync = 100

	// gchatMaxMemberResolvesPerSync caps the number of FRESH members.get calls a
	// single sweep may issue across ALL spaces (the reverse email→id resolutions).
	// 50 is comfortably under quota, amortizes a cold 67-space start over a handful
	// of 15-min ticks, and is sized to cover a single space's debt re-resolution
	// set in one sweep (§3.2.1). When the cap is hit, remaining UNKNOWN candidates
	// are left unresolved THIS sweep and HOLD their space's cursor (resolution
	// debt) until a later tick resolves them. Revisit if prod logs show persistent
	// spacesHeldByCapOnDebt.
	gchatMaxMemberResolvesPerSync = 50

	// gchatMembershipCacheTTL bounds how long a cached space-member list (and a
	// cached id→email resolution) stays valid before a forced refetch, even when
	// the space's lastActiveTime never advances. Caps People/Chat API quota.
	gchatMembershipCacheTTL = 24 * time.Hour

	// gchatEditLookbackWindow is how far before a space's create_cursor the
	// edit/delete re-list reaches. Edits/deletes of messages CREATED within this
	// trailing window are synced; older ones are an accepted lost-update (the
	// Chat API filters only by create_time, never by update_time/delete_time).
	gchatEditLookbackWindow = 7 * 24 * time.Hour

	// gchatArchivedCursorGrace is how long an archived (disappeared) space's
	// cursor is retained for restore-on-rediscovery before it is dropped.
	gchatArchivedCursorGrace = 30 * 24 * time.Hour

	// gchatSnippetMaxLen caps the stored snippet length.
	gchatSnippetMaxLen = 200

	// Chat API enum values used in qualification.
	gchatUserTypeHuman         = "HUMAN"
	gchatSpaceTypeDM           = "DIRECT_MESSAGE"
	gchatMembershipStateJoined = "JOINED"

	// gchatUserNamePrefix is the prefix on a Chat user resource name
	// ("users/{id}") and the People person resource name ("people/{id}").
	gchatUserNamePrefix   = "users/"
	gchatPersonNamePrefix = "people/"

	// Metadata keys persisted into external_sync_state.metadata. Read-modify-write
	// only the gchat keys (never replace the blob wholesale).
	gchatMetaSpaceCursors    = "space_cursors"
	gchatMetaArchivedCursors = "archived_space_cursors"
	gchatMetaSpaceMembers    = "space_members"
	gchatMetaUserEmailCache  = "user_email_cache"
	gchatMetaMeIdentities    = "me_identities"
	// gchatMetaEmailUserIDs is the GLOBAL positive cache (normalizedEmail →
	// canonical "users/{id}") from the reverse resolver: a canonical id is the
	// same person in every space, so once an email resolves anywhere it is reused
	// everywhere with zero further members.get calls.
	gchatMetaEmailUserIDs = "email_user_ids"
	// gchatMetaSpaceMemberNegatives is the PER-(space,email) negative cache
	// (space → normalizedEmail → memberNegative): "this known email is NOT a
	// member of this space," stamped with the space's member-set fingerprint so a
	// negative is only honored while membership is unchanged (see §3.2.1).
	gchatMetaSpaceMemberNegatives = "space_member_negatives"
)

// chatFetcher is the narrow Google Chat + People read surface the provider
// needs. The production implementation (chatServiceFetcher) wraps *chat.Service
// + *people.Service; tests inject a fake returning canned pages so unit and
// integration tests never touch HTTP or OAuth. The *chat.Space / *chat.Message
// / *chat.Membership library structs stay in the signatures so tests exercise
// the real field-mapping code.
type chatFetcher interface {
	// ListSpaces returns one page of the user's spaces (all space types).
	ListSpaces(ctx context.Context, pageToken string) (spaces []*chat.Space, nextPageToken string, err error)
	// ListMembers returns one page of a space's memberships.
	ListMembers(ctx context.Context, spaceName, pageToken string) (members []*chat.Membership, nextPageToken string, err error)
	// ListMessages returns one page of a space's messages matching filter,
	// ordered by create_time ASC. showDeleted includes tombstoned messages.
	ListMessages(ctx context.Context, spaceName, filter string, showDeleted bool, pageToken string) (msgs []*chat.Message, nextPageToken string, err error)
	// ResolvePersonEmail resolves a Chat user resource name ("users/{id}") to a
	// normalized email via the People API. Returns "" (no error) when the person
	// has no email or is not resolvable under the granted scope.
	ResolvePersonEmail(ctx context.Context, userName string) (normalizedEmail string, err error)
	// ResolveMemberID resolves a normalized email TO its canonical Chat user
	// resource name ("users/{id}") within a space, via members.get with the email
	// as the member alias. Unlike ResolvePersonEmail (which reads the caller's
	// Contacts graph), this reads the space's membership graph, so it resolves a
	// co-member even when that person is not in the user's Google Contacts.
	// notMember is true (with no error) when the email is not a member of the
	// space — the API returns HTTP 400 (unrecognized member) or 404 for that case;
	// the caller treats it as a cacheable negative. Transient/auth errors
	// propagate so the window aborts and retries.
	ResolveMemberID(ctx context.Context, spaceName, normalizedEmail string) (canonicalUserName string, notMember bool, err error)
}

// chatServiceFetcher is the production chatFetcher backed by *chat.Service +
// *people.Service, both built from the same per-account OAuth client.
type chatServiceFetcher struct {
	chat   *chat.Service
	people *people.Service
}

func (f *chatServiceFetcher) ListSpaces(ctx context.Context, pageToken string) ([]*chat.Space, string, error) {
	call := f.chat.Spaces.List()
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	resp, err := call.Context(ctx).Do()
	if err != nil {
		return nil, "", fmt.Errorf("list spaces: %w", err)
	}
	return resp.Spaces, resp.NextPageToken, nil
}

func (f *chatServiceFetcher) ListMembers(ctx context.Context, spaceName, pageToken string) ([]*chat.Membership, string, error) {
	call := f.chat.Spaces.Members.List(spaceName)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	resp, err := call.Context(ctx).Do()
	if err != nil {
		return nil, "", fmt.Errorf("list members for %s: %w", hashIdentifier(spaceName), err)
	}
	return resp.Memberships, resp.NextPageToken, nil
}

func (f *chatServiceFetcher) ListMessages(ctx context.Context, spaceName, filter string, showDeleted bool, pageToken string) ([]*chat.Message, string, error) {
	// PageSize(1000) requests the documented Chat API maximum (default is 25,
	// max is 1000, values >1000 are clamped by the API). This collapses a
	// deep-history space's content pass from ~one page per 25 messages to one or
	// very few pages, so its window completes within the shared sweep budget and
	// its cursor advances. Both the content pass (showDeleted=false) and the
	// edit/delete pass (showDeleted=true) route through this method, so both
	// inherit the larger page size.
	call := f.chat.Spaces.Messages.List(spaceName).
		Filter(filter).
		ShowDeleted(showDeleted).
		OrderBy("create_time ASC").
		PageSize(1000)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	resp, err := call.Context(ctx).Do()
	if err != nil {
		return nil, "", fmt.Errorf("list messages for %s: %w", hashIdentifier(spaceName), err)
	}
	return resp.Messages, resp.NextPageToken, nil
}

func (f *chatServiceFetcher) ResolvePersonEmail(ctx context.Context, userName string) (string, error) {
	numericID := strings.TrimPrefix(userName, gchatUserNamePrefix)
	if numericID == "" || numericID == userName {
		// Not a "users/{id}" form — nothing to resolve.
		return "", nil
	}
	person, err := f.people.People.Get(gchatPersonNamePrefix + numericID).
		PersonFields("emailAddresses").
		Context(ctx).Do()
	if err != nil {
		// A 404 means the person is not in the user's address book (or not
		// resolvable under contacts.readonly) — that is a normal unresolved
		// sender/member, NOT a failure. Return "" so the caller treats it as a
		// bystander and the resolver caches a negative. Only transient/other
		// errors (auth, quota, 5xx) propagate so the window aborts and retries.
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return "", nil
		}
		return "", fmt.Errorf("resolve person %s: %w", hashIdentifier(userName), err)
	}
	return primaryNormalizedEmail(person), nil
}

// ResolveMemberID resolves a normalized email to its canonical "users/{id}"
// within a space by calling members.get with the email as the membership alias.
// The member resource name is space.Name + "/members/" + email — i.e. the BARE
// email after "members/" (NOT "members/users/{email}"). The chat/v1 client
// expands "{+name}" with reserved expansion, so the "@"/"." in the email are
// preserved (no manual escaping). On success it returns the canonical
// Member.Name (the alias is request-only; responses are always canonical). An
// unrecognized member yields an HTTP 400 ("Invalid membership state, user,
// group or request ID") — that, like a 404, means "not a member of this space",
// so both map to notMember=true (a cacheable negative, NOT a window-aborting
// error). Other errors (auth, quota, 5xx) propagate.
func (f *chatServiceFetcher) ResolveMemberID(ctx context.Context, spaceName, normalizedEmail string) (string, bool, error) {
	if spaceName == "" || normalizedEmail == "" {
		return "", true, nil
	}
	name := spaceName + "/members/" + normalizedEmail
	resp, err := f.chat.Spaces.Members.Get(name).Context(ctx).Do()
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && (apiErr.Code == 400 || apiErr.Code == 404) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("resolve member id for %s: %w", hashIdentifier(spaceName), err)
	}
	if resp == nil || resp.Member == nil {
		return "", true, nil
	}
	return resp.Member.Name, false, nil
}

// primaryNormalizedEmail extracts the person's primary email (or the first
// email when none is flagged primary), normalized. Returns "" when the person
// carries no email.
func primaryNormalizedEmail(person *people.Person) string {
	if person == nil || len(person.EmailAddresses) == 0 {
		return ""
	}
	first := ""
	for _, e := range person.EmailAddresses {
		if e == nil || strings.TrimSpace(e.Value) == "" {
			continue
		}
		if first == "" {
			first = e.Value
		}
		if e.Metadata != nil && e.Metadata.Primary {
			return matching.NormalizeEmail(e.Value)
		}
	}
	return matching.NormalizeEmail(first)
}

// GChatSyncProvider implements sync.SyncProvider for Google Chat. It is
// store-only and event-free: each qualifying message is upserted into
// comms_message(source='gchat') and the shared aggregation engine (wired
// separately) derives interactions on its own pass. Unlike Gmail there is NO
// event bus and NO pgxpool — the provider never publishes events and never
// opens a tx spanning network I/O. The provider owns its rich per-space cursor
// / membership / email-cache state, which it persists into
// external_sync_state.metadata via syncRepo (the framework round-trips only
// SyncResult.NewCursor, which GChat leaves empty).
type GChatSyncProvider struct {
	oauthService *OAuthService
	commsRepo    *repository.CommsMessageRepository
	syncRepo     *repository.SyncRepository

	// newFetcher builds the per-account chatFetcher, encapsulating the OAuth call
	// (GetClientForAccount + chat/people NewService). Tests override via
	// SetFetcherFactoryForTest so a fake fetcher is injected with no OAuth state.
	newFetcher func(ctx context.Context, accountID string) (chatFetcher, error)

	// newMeSet builds the "me" set (the normalized address of every connected
	// account). Tests override via SetMeSetForTest.
	newMeSet func(ctx context.Context) (map[string]struct{}, error)
}

// Ensure GChatSyncProvider implements the sync.SyncProvider interface.
var _ syncpkg.SyncProvider = (*GChatSyncProvider)(nil)

// NewGChatSyncProvider builds the Google Chat provider. It takes syncRepo (not a
// bus/pool) because GChat persists its per-space state into
// external_sync_state.metadata itself.
func NewGChatSyncProvider(
	oauthService *OAuthService,
	commsRepo *repository.CommsMessageRepository,
	syncRepo *repository.SyncRepository,
) *GChatSyncProvider {
	p := &GChatSyncProvider{
		oauthService: oauthService,
		commsRepo:    commsRepo,
		syncRepo:     syncRepo,
	}
	p.newFetcher = func(ctx context.Context, accountID string) (chatFetcher, error) {
		client, err := oauthService.GetClientForAccount(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("get OAuth client: %w", err)
		}
		chatSvc, err := chat.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			return nil, fmt.Errorf("create Chat service: %w", err)
		}
		peopleSvc, err := people.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			return nil, fmt.Errorf("create People service: %w", err)
		}
		return &chatServiceFetcher{chat: chatSvc, people: peopleSvc}, nil
	}
	p.newMeSet = func(ctx context.Context) (map[string]struct{}, error) {
		accounts, err := oauthService.ListAccounts(ctx)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		meSet := make(map[string]struct{}, len(accounts))
		for _, a := range accounts {
			normalized := matching.NormalizeEmail(a.AccountID)
			if normalized != "" {
				meSet[normalized] = struct{}{}
			}
		}
		return meSet, nil
	}
	return p
}

// Config returns the provider's configuration.
func (p *GChatSyncProvider) Config() syncpkg.SourceConfig {
	return syncpkg.SourceConfig{
		Name:                 GChatSourceName,
		DisplayName:          "Google Chat",
		Strategy:             repository.SyncStrategyContactDriven,
		SupportsMultiAccount: true,
		SupportsDiscovery:    false,
		DefaultInterval:      GChatDefaultInterval,
	}
}

// ValidateCredentials checks the Google credentials are valid for the account.
func (p *GChatSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	if accountID == nil {
		accounts, err := p.oauthService.ListAccounts(ctx)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		if len(accounts) == 0 {
			return fmt.Errorf("no Google accounts connected")
		}
		return nil
	}
	if _, err := p.oauthService.GetClientForAccount(ctx, *accountID); err != nil {
		return fmt.Errorf("get OAuth client for account: %w", err)
	}
	return nil
}

// MeSet builds the "me" set through the provider's seam (tests override it via
// SetMeSetForTest). Exposed so the rematch handlers can reuse it without real
// OAuth.
func (p *GChatSyncProvider) MeSet(ctx context.Context) (map[string]struct{}, error) {
	return p.newMeSet(ctx)
}

// Sync performs one Google Chat sweep for one account. The framework's contacts
// slice is ignored; the provider loads its own dual-source known-contact map.
func (p *GChatSyncProvider) Sync(
	ctx context.Context,
	state *repository.SyncState,
	_ []repository.Contact,
) (*syncpkg.SyncResult, error) {
	if state.AccountID == nil {
		return nil, fmt.Errorf("account ID required for GChat sync")
	}
	accountID := *state.AccountID

	logger.Info().
		Str("source", GChatSourceName).
		Str("account", accountID).
		Msg("starting GChat sync")

	gcm := loadGChatMetadata(state.Metadata)
	knownMap, err := p.buildKnownMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("build known map: %w", err)
	}

	meSet, err := p.newMeSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("build me-set: %w", err)
	}
	for addr := range gcm.MeIdentities {
		meSet[addr] = struct{}{}
	}

	fetcher, err := p.newFetcher(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("build chat fetcher: %w", err)
	}

	spaces, err := paginateSpaces(ctx, fetcher)
	if err != nil {
		// An auth/quota error on the sweep's first call aborts the whole sweep
		// (cursors unchanged) → service exponential backoff retries next tick.
		return nil, fmt.Errorf("list spaces: %w", err)
	}
	reapStaleCursors(gcm, spaces, accelerated.GetCurrentTime())

	resolver := newCachedEmailResolver(fetcher, gcm.UserEmailCache)
	// The reverse (email→id) resolver shares the global positive cache + per-space
	// negatives across the whole sweep, capped at gchatMaxMemberResolvesPerSync
	// fresh members.get calls so a cold start amortizes over many ticks.
	resolveCap := gchatMaxMemberResolvesPerSync
	idResolver := newMemberIDResolver(fetcher, gcm.EmailUserIDs, gcm.SpaceMemberNegatives, &resolveCap)
	counters := &sweepCounters{}
	backfillFloor := gchatBackfillFloor(state.Metadata)
	// Shared list-page budget for the WHOLE sweep (membership + content + edit
	// passes across all spaces), decremented as pages are consumed. Once it hits
	// zero the sweep stops advancing cursors; un-advanced spaces restart next
	// tick (crash-safe).
	budget := gchatMaxWindowsPerSync

	for _, space := range spaces {
		if budget <= 0 {
			break
		}
		members, fingerprint, incomplete, err := p.resolveMembership(ctx, fetcher, space, gcm, &budget)
		if err != nil {
			// Transient membership error: abort THIS space (cursor unadvanced),
			// continue with already-proven spaces. Don't fail the whole sweep.
			logger.Warn().Err(err).Str("source", GChatSourceName).Msg("gchat: membership resolution failed; skipping space")
			continue
		}
		_ = fingerprint // wired into buildKnownIDIndex in the id-matching step
		if incomplete {
			// Budget ran out before the membership was fully paged → skip this
			// space (don't act on a partial member list); it retries next sweep.
			break
		}
		knownMembers, err := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)
		if err != nil {
			// Transient resolve error while resolving co-members: abort THIS space
			// (cursor unadvanced) so the window retries next sweep with the full
			// membership. Never advance the cursor on a partial fan-out.
			logger.Warn().Err(err).Str("source", GChatSourceName).Msg("gchat: co-member resolution failed; skipping space")
			continue
		}

		isDM := space.SpaceType == gchatSpaceTypeDM
		if len(knownMembers) == 0 && !isDM {
			counters.spacesSkippedNoKnownMember++
			continue
		}

		cur := gcm.cursorFor(space.Name, backfillFloor)
		newCreateCursor, proven, err := p.consumeContentWindow(ctx, fetcher, space, cur.CreateCursor, knownMembers, knownMap, meSet, resolver, accountID, counters, &budget)
		if err != nil {
			// A list/resolve/upsert error mid-window leaves create_cursor
			// UNADVANCED so the whole window restarts next sweep (idempotent via
			// the upsert dedup). Never advance past an unpersisted message.
			logger.Warn().Err(err).Str("source", GChatSourceName).Msg("gchat: content window aborted; cursor unadvanced")
			continue
		}
		if !proven {
			// Budget ran out before the window was fully paged → keep the original
			// cursor (do NOT advance over an un-listed page) and stop the sweep.
			break
		}
		cur.CreateCursor = newCreateCursor

		// Bounded edit/delete reconciliation: re-list create_time within the
		// trailing lookback with ShowDeleted, applying edits/deletes. Skip it when
		// the shared budget is already exhausted (the content cursor we just
		// proved is still persisted below). A failure here does NOT roll back the
		// content cursor (the edit pass is best-effort, idempotent via the SQL
		// guards), but edit_cursor stays unadvanced so the window retries.
		if budget > 0 {
			newEditCursor, editProven, editErr := p.reconcileEditsDeletes(ctx, fetcher, space, cur, backfillFloor, counters, &budget)
			if editErr != nil {
				logger.Warn().Err(editErr).Str("source", GChatSourceName).Msg("gchat: edit/delete pass aborted; edit cursor unadvanced")
			} else if editProven {
				cur.EditCursor = newEditCursor
			}
		}
		gcm.setCursor(space.Name, cur)
	}

	if err := p.persistMetadata(ctx, state, gcm, resolver, idResolver); err != nil {
		return nil, fmt.Errorf("persist gchat metadata: %w", err)
	}

	logger.Info().
		Str("source", GChatSourceName).
		Str("account", accountID).
		Int("processed", counters.processed).
		Int("matched", counters.matched).
		Int("spaces", len(spaces)).
		Int("spaces_skipped_no_known_member", counters.spacesSkippedNoKnownMember).
		Int("senders_unresolved", counters.sendersUnresolved).
		Int("edits_applied", counters.editsApplied).
		Int("deletes_applied", counters.deletesApplied).
		Msg("GChat sync completed")

	return &syncpkg.SyncResult{
		ItemsProcessed: counters.processed,
		ItemsMatched:   counters.matched,
		NewCursor:      "",
	}, nil
}

// sweepCounters accumulates per-sweep counts surfaced via the standard
// SyncResult item counts (processed/matched → sync_log) and the structured
// completion log line (the gchat-specific counters).
type sweepCounters struct {
	processed                  int
	matched                    int
	spacesSkippedNoKnownMember int
	sendersUnresolved          int
	editsApplied               int
	deletesApplied             int
}

// buildKnownMap loads the dual-source (gchat+email) known-contact map:
// normalized address → []contactID.
func (p *GChatSyncProvider) buildKnownMap(ctx context.Context) (map[string][]uuid.UUID, error) {
	return buildKnownMapFromIdentities(ctx, p.commsRepo)
}

// buildKnownMapFromIdentities builds the dual-source known-contact map
// (normalized address → []contactID) from ListGChatIdentitiesForSync. Shared by
// the steady-state sweep and the rematch handlers. The same (address, contact)
// can appear under both a gchat and an email source_type row, so it dedups.
func buildKnownMapFromIdentities(ctx context.Context, commsRepo *repository.CommsMessageRepository) (map[string][]uuid.UUID, error) {
	identities, err := commsRepo.ListGChatIdentitiesForSync(ctx)
	if err != nil {
		return nil, err
	}
	knownMap := make(map[string][]uuid.UUID)
	for _, id := range identities {
		if !containsUUID(knownMap[id.ValueNormalized], id.ContactID) {
			knownMap[id.ValueNormalized] = append(knownMap[id.ValueNormalized], id.ContactID)
		}
	}
	return knownMap, nil
}

// ScanIdentifier runs a one-shot, address-scoped historical scan for one
// connected account: it iterates the account's spaces and upserts only the rows
// that involve addrNormalized (inbound FROM the address, or outbound TO a space
// where the address is a known co-member). knownMap is the FULL map (so
// co-member/direction resolution matches the steady-state sweep); scanMap
// restricts which rows actually get written to the just-added address. Re-running
// is idempotent (the upsert dedup), so a River retry that re-scans an
// already-succeeded account produces no duplicate rows. Returns the number of
// rows upserted.
func (p *GChatSyncProvider) ScanIdentifier(
	ctx context.Context,
	accountID, addrNormalized string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	afterCursor string,
) (matched int, err error) {
	contacts := knownMap[addrNormalized]
	if len(contacts) == 0 {
		// The address maps to no live contact (already removed, or never linked)
		// → nothing to scan.
		return 0, nil
	}
	scanMap := map[string][]uuid.UUID{addrNormalized: contacts}

	fetcher, err := p.newFetcher(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("build chat fetcher: %w", err)
	}
	spaces, err := paginateSpaces(ctx, fetcher)
	if err != nil {
		return 0, fmt.Errorf("list spaces: %w", err)
	}

	resolver := newCachedEmailResolver(fetcher, nil)
	counters := &sweepCounters{}
	// The rematch scan is a one-shot historical backfill. It gets its OWN full
	// page budget. If the budget is exhausted before every space's window is
	// fully paged, the scan is INCOMPLETE — we fail the job (return an error) so
	// River retries the WHOLE scan, which is idempotent via the upsert dedup.
	// Silently returning success on a partial scan would strand the rest of the
	// address's history forever (the rematch is a one-shot trigger, not a cursor-
	// advancing sweep that would pick up the remainder next tick).
	budget := gchatMaxWindowsPerSync
	for _, space := range spaces {
		if budget <= 0 {
			return counters.matched, fmt.Errorf("rematch scan budget exhausted before completing: retry to finish backfill")
		}
		members, _, memberIncomplete, mErr := paginateMembers(ctx, fetcher, space.Name, &budget)
		if mErr != nil {
			return counters.matched, fmt.Errorf("list members: %w", mErr)
		}
		if memberIncomplete {
			// Budget ran out mid-membership → the scan is incomplete; fail so
			// River retries the whole (idempotent) backfill.
			return counters.matched, fmt.Errorf("rematch scan budget exhausted paging membership: retry to finish backfill")
		}
		// Co-member resolution uses the FULL knownMap (so a space qualifies when
		// the target address is one of its members); row-writing uses scanMap.
		knownMembers, rErr := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)
		if rErr != nil {
			return counters.matched, fmt.Errorf("resolve co-members: %w", rErr)
		}
		scanMembers := restrictKnownMembers(knownMembers, addrNormalized)
		isDM := space.SpaceType == gchatSpaceTypeDM
		if len(scanMembers) == 0 && !isDM {
			// The target address is not a member of this space → it can still be
			// the inbound SENDER, so we must still page messages. But to bound the
			// scan we skip spaces where the address is neither a member nor (for a
			// DM) the implicit peer — an inbound message from the address can only
			// arrive in a space the address belongs to.
			continue
		}
		_, proven, cErr := p.consumeContentWindow(ctx, fetcher, space, afterCursor, scanMembers, scanMap, meSet, resolver, accountID, counters, &budget)
		if cErr != nil {
			return counters.matched, fmt.Errorf("scan space: %w", cErr)
		}
		if !proven {
			// Budget ran out mid-window → the scan is incomplete; fail so River
			// retries the whole (idempotent) backfill rather than dropping history.
			return counters.matched, fmt.Errorf("rematch scan budget exhausted mid-window: retry to finish backfill")
		}
	}
	return counters.matched, nil
}

// restrictKnownMembers narrows a resolved known-member map to a single address
// (for the rematch fan-out, so an outbound message only writes the just-added
// contact's row, not every co-member's).
func restrictKnownMembers(knownMembers map[string][]uuid.UUID, addr string) map[string][]uuid.UUID {
	if contacts, ok := knownMembers[addr]; ok {
		return map[string][]uuid.UUID{addr: contacts}
	}
	return map[string][]uuid.UUID{}
}

// consumeContentWindow pages a space's messages with create_time > cursor,
// upserts qualifying rows, and returns the new create_cursor. proven is true
// ONLY when the window was fully paged (next == "") — that is the only case the
// caller may advance the cursor. When the SHARED sweep budget runs out mid-
// window (more pages remain), it returns proven=false + the ORIGINAL cursor so
// the whole window restarts next sweep (advancing maxCreate would risk skipping
// an un-listed later page whose first message shares maxCreate's timestamp,
// since the next sweep filters create_time > cursor). On any error it likewise
// returns the original cursor. budget is the shared remaining-page allowance,
// decremented per list page.
func (p *GChatSyncProvider) consumeContentWindow(
	ctx context.Context,
	fetcher chatFetcher,
	space *chat.Space,
	createCursor string,
	knownMembers map[string][]uuid.UUID,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	resolver *cachedEmailResolver,
	accountID string,
	counters *sweepCounters,
	budget *int,
) (newCursor string, proven bool, err error) {
	filter := fmt.Sprintf(`create_time > "%s"`, createCursor)
	maxCreate := createCursor
	pageToken := ""
	for {
		if *budget <= 0 {
			// Shared sweep budget exhausted before this window was fully paged →
			// unproven, keep the original cursor.
			return createCursor, false, nil
		}
		msgs, next, listErr := fetcher.ListMessages(ctx, space.Name, filter, false, pageToken)
		if listErr != nil {
			return createCursor, false, fmt.Errorf("list messages: %w", listErr)
		}
		*budget--
		for _, m := range msgs {
			if !qualifiableContentMessage(m) {
				continue
			}
			matchErr := p.qualifyAndUpsert(ctx, m, space, knownMembers, knownMap, meSet, resolver, accountID, counters)
			if matchErr != nil {
				// Transient resolve/upsert error: abort the window, keep the
				// original cursor (do NOT advance past this message).
				return createCursor, false, matchErr
			}
			// Advance by INSTANT comparison, not lexical order, so varying
			// fractional-second precision can't under-advance the cursor.
			maxCreate = laterChatTime(maxCreate, m.CreateTime)
			counters.processed++
		}
		if next == "" {
			// Fully paged → the window is proven, advance to maxCreate.
			return maxCreate, true, nil
		}
		pageToken = next
	}
}

// qualifiableContentMessage reports whether a content-pass message should be
// considered for upsert: human sender, not a tombstone, with a resource name.
func qualifiableContentMessage(m *chat.Message) bool {
	if m == nil || m.Name == "" {
		return false
	}
	if m.DeletionMetadata != nil {
		return false
	}
	if m.Sender == nil || m.Sender.Type != gchatUserTypeHuman {
		return false
	}
	return true
}

// reconcileEditsDeletes runs the bounded edit/delete pass for one space: re-list
// create_time within the trailing lookback (max(create_cursor−7d, backfill
// floor)) with ShowDeleted=true, and for each returned message:
//   - DeletionMetadata != nil → soft-delete every stored row for the resource
//     name (the row drops out of future aggregation);
//   - LastUpdateTime set AND the body differs from the stored row → apply the
//     edit, whose SQL ::timestamptz guard authoritatively decides recency +
//     idempotency (there is NO Go-side timestamp comparison).
//
// proven is true ONLY when the lookback window was fully paged (next == "") —
// the only case the caller may advance edit_cursor. On a list error or shared-
// budget exhaustion mid-window it returns the ORIGINAL edit cursor + proven=
// false so the window retries next sweep. budget is the shared remaining-page
// allowance, decremented per list page. Edits/deletes of messages created
// before the lookback are an accepted lost-update (the API offers no
// update_time/delete_time filter).
func (p *GChatSyncProvider) reconcileEditsDeletes(
	ctx context.Context,
	fetcher chatFetcher,
	space *chat.Space,
	cur spaceCursor,
	backfillFloor string,
	counters *sweepCounters,
	budget *int,
) (newEditCursor string, proven bool, err error) {
	floor := editLookbackFloor(cur.CreateCursor, backfillFloor)
	filter := fmt.Sprintf(`create_time > "%s"`, floor)
	maxCreate := cur.EditCursor
	pageToken := ""
	for {
		if *budget <= 0 {
			return cur.EditCursor, false, nil
		}
		msgs, next, listErr := fetcher.ListMessages(ctx, space.Name, filter, true, pageToken)
		if listErr != nil {
			return cur.EditCursor, false, fmt.Errorf("list messages (show_deleted): %w", listErr)
		}
		*budget--
		for _, m := range msgs {
			if m == nil || m.Name == "" {
				continue
			}
			if applyErr := p.applyEditOrDelete(ctx, m, counters); applyErr != nil {
				return cur.EditCursor, false, applyErr
			}
			// Advance by INSTANT comparison, not lexical order (same fractional-
			// second precision pitfall as the content pass).
			maxCreate = laterChatTime(maxCreate, m.CreateTime)
		}
		if next == "" {
			return maxCreate, true, nil
		}
		pageToken = next
	}
}

// applyEditOrDelete applies one re-listed message's edit or delete to the stored
// rows. A tombstone soft-deletes all fanned-out rows; an edit (non-empty
// LastUpdateTime + a body that differs from the stored row) is handed to the
// SQL ::timestamptz guard. The Go pre-filter is BODY-ONLY — it never compares
// timestamps (recency is decided in SQL).
func (p *GChatSyncProvider) applyEditOrDelete(ctx context.Context, m *chat.Message, counters *sweepCounters) error {
	if m.DeletionMetadata != nil {
		n, err := p.commsRepo.SoftDeleteByExternalID(ctx, GChatSourceName, m.Name, accelerated.GetCurrentTime())
		if err != nil {
			return fmt.Errorf("soft-delete by external id: %w", err)
		}
		counters.deletesApplied += int(n)
		return nil
	}
	if m.LastUpdateTime == "" {
		return nil // never edited
	}

	// Body-only no-op-avoidance pre-check: only round-trip the edit UPDATE when
	// the stored body actually differs. The stored row is fetched solely for its
	// body; its last_update_time is NEVER string-compared here.
	stored, err := p.commsRepo.GetLatestByExternalID(ctx, GChatSourceName, m.Name)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // nothing stored for this message → nothing to edit
		}
		return fmt.Errorf("get latest by external id: %w", err)
	}
	if stored.Body != nil && *stored.Body == m.Text {
		return nil // body unchanged → skip the round-trip
	}

	body := m.Text
	snippet := firstN(body, gchatSnippetMaxLen)
	editedAt := accelerated.GetCurrentTime().UTC().Format(chatTimeLayout)
	n, err := p.commsRepo.ApplyEditByExternalID(ctx, GChatSourceName, m.Name, &body, &snippet, editedAt, m.LastUpdateTime)
	if err != nil {
		return fmt.Errorf("apply edit by external id: %w", err)
	}
	// n == 0 means a concurrent sweep already applied this edit OR the stored
	// last_update_time is already >= this one — not an error.
	counters.editsApplied += int(n)
	return nil
}

// editLookbackFloor returns max(createCursor − gchatEditLookbackWindow,
// backfillFloor) as an RFC-3339 string. An unparseable createCursor falls back
// to the backfill floor (the safe wider scan).
func editLookbackFloor(createCursor, backfillFloor string) string {
	created, err := parseChatTime(createCursor)
	if err != nil {
		return backfillFloor
	}
	lookback := created.Add(-gchatEditLookbackWindow).UTC().Format(chatTimeLayout)
	if after, ok := chatTimeAfter(backfillFloor, lookback); ok && after {
		return backfillFloor
	}
	return lookback
}

// messageClassification is the pure (DB-free) result of qualifying one message:
// its direction and the set of matched contact ids (one row will be upserted per
// contact). matched is empty for a bystander/unresolved sender.
type messageClassification struct {
	SenderEmail string
	Direction   string
	Matched     []uuid.UUID
	Unresolved  bool // sender resolved to no email (counts senders_unresolved)
}

// classifyMessage resolves the sender and decides direction + fan-out WITHOUT
// touching the DB. A resolve error propagates (so the caller aborts the window
// and retries). An unresolved sender or a bystander yields an empty Matched set.
func classifyMessage(
	ctx context.Context,
	m *chat.Message,
	knownMembers map[string][]uuid.UUID,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	resolver *cachedEmailResolver,
) (messageClassification, error) {
	senderEmail, err := resolver.resolve(ctx, m.Sender.Name)
	if err != nil {
		return messageClassification{}, fmt.Errorf("resolve sender: %w", err)
	}
	if senderEmail == "" {
		return messageClassification{Unresolved: true}, nil
	}

	c := messageClassification{SenderEmail: senderEmail}
	switch {
	case inSet(meSet, senderEmail):
		c.Direction = repository.InteractionDirectionOutbound
		// Fan out to every known co-member, EXCLUDING any self-contact.
		c.Matched = flattenKnownMembers(knownMembers, meSet)
	case len(knownMap[senderEmail]) > 0:
		c.Direction = repository.InteractionDirectionInbound
		c.Matched = knownMap[senderEmail] // sender-only
	default:
		// Sender neither me nor known → bystander, no row.
	}
	return c, nil
}

// qualifyAndUpsert classifies one message and upserts a row per matched contact.
// A resolve/upsert error is returned (aborts the window); a bystander/unresolved
// sender produces no row (and is not an error).
func (p *GChatSyncProvider) qualifyAndUpsert(
	ctx context.Context,
	m *chat.Message,
	space *chat.Space,
	knownMembers map[string][]uuid.UUID,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	resolver *cachedEmailResolver,
	accountID string,
	counters *sweepCounters,
) error {
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, meSet, resolver)
	if err != nil {
		return err
	}
	if c.Unresolved {
		counters.sendersUnresolved++
		return nil
	}
	if len(c.Matched) == 0 {
		return nil // bystander
	}

	sentAt, perr := parseChatTime(m.CreateTime)
	if perr != nil {
		// A message with no parseable create time can never succeed — skip it
		// (deterministic, not transient) rather than abort the window forever.
		logger.Warn().Str("source", GChatSourceName).Str("message", hashIdentifier(m.Name)).Msg("gchat: unparseable create time; skipping message")
		return nil
	}

	body := m.Text
	snippet := firstN(body, gchatSnippetMaxLen)
	metadata := buildContentMetadata(space, m)
	threadID := space.Name

	for _, contactID := range c.Matched {
		_, err := p.commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
			Source:           GChatSourceName,
			ExternalID:       m.Name,
			ThreadID:         &threadID,
			Body:             &body,
			Snippet:          &snippet,
			PeerHandle:       &c.SenderEmail,
			PeerNormalized:   &c.SenderEmail,
			Direction:        c.Direction,
			SentAt:           sentAt,
			AccountID:        &accountID,
			SourceMetadata:   metadata,
			MatchedContactID: contactID,
			GmailMessageID:   nil,
		})
		if err != nil {
			return fmt.Errorf("upsert comms_message: %w", err)
		}
		counters.matched++
	}
	return nil
}

// MessageClassificationForTest is the exported view of classifyMessage's result
// so cross-package tests can assert direction + fan-out without a DB.
type MessageClassificationForTest struct {
	SenderEmail string
	Direction   string
	Matched     []uuid.UUID
	Unresolved  bool
}

// RunClassifyForTest drives the DB-free classifyMessage for cross-package tests,
// using a fake-fetcher-backed resolver. Production code must NOT call this.
func RunClassifyForTest(
	ctx context.Context,
	m *chat.Message,
	knownMembers map[string][]uuid.UUID,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	resolver *CachedEmailResolverForTest,
) (MessageClassificationForTest, error) {
	c, err := classifyMessage(ctx, m, knownMembers, knownMap, meSet, resolver.inner)
	return MessageClassificationForTest(c), err
}

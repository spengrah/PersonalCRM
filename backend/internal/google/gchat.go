package google

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	people "google.golang.org/api/people/v1"
)

const (
	// GChatDefaultInterval is the default sync interval for Google Chat (every
	// 15 minutes), matching the Gmail/Calendar tick.
	GChatDefaultInterval = 15 * time.Minute

	// gchatMaxWindowsPerSync bounds one sweep's total list-page iterations
	// (content + edit/delete passes) across all spaces, mirroring Gmail's
	// catch-up bound. Once exhausted the sweep returns; un-advanced space
	// cursors restart next tick (crash-safe).
	gchatMaxWindowsPerSync = 24

	// gchatMembershipCacheTTL bounds how long a cached space-member list (and a
	// cached id→email resolution) stays valid before a forced refetch, even when
	// the space's lastActiveTime never advances. Caps People/Chat API quota.
	gchatMembershipCacheTTL = 24 * time.Hour

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
	call := f.chat.Spaces.Messages.List(spaceName).
		Filter(filter).
		ShowDeleted(showDeleted).
		OrderBy("create_time ASC")
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
		return "", fmt.Errorf("resolve person %s: %w", hashIdentifier(userName), err)
	}
	return primaryNormalizedEmail(person), nil
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
	counters := &sweepCounters{}
	backfillFloor := gchatBackfillFloor(state.Metadata)
	windowsUsed := 0

	for _, space := range spaces {
		if windowsUsed >= gchatMaxWindowsPerSync {
			break
		}
		members, fetchedPages, err := p.resolveMembership(ctx, fetcher, space, gcm)
		windowsUsed += fetchedPages
		if err != nil {
			// Transient membership error: abort THIS space (cursor unadvanced),
			// continue with already-proven spaces. Don't fail the whole sweep.
			logger.Warn().Err(err).Str("source", GChatSourceName).Msg("gchat: membership resolution failed; skipping space")
			continue
		}
		knownMembers := resolveKnownMembers(ctx, members, resolver, knownMap, meSet)

		isDM := space.SpaceType == gchatSpaceTypeDM
		if len(knownMembers) == 0 && !isDM {
			counters.spacesSkippedNoKnownMember++
			continue
		}

		cur := gcm.cursorFor(space.Name, backfillFloor)
		newCreateCursor, pages, err := p.consumeContentWindow(ctx, fetcher, space, cur.CreateCursor, knownMembers, knownMap, meSet, resolver, accountID, counters)
		windowsUsed += pages
		if err != nil {
			// A list/resolve/upsert error mid-window leaves create_cursor
			// UNADVANCED so the whole window restarts next sweep (idempotent via
			// the upsert dedup). Never advance past an unpersisted message.
			logger.Warn().Err(err).Str("source", GChatSourceName).Msg("gchat: content window aborted; cursor unadvanced")
			continue
		}
		cur.CreateCursor = newCreateCursor
		gcm.setCursor(space.Name, cur)
	}

	if err := p.persistMetadata(ctx, state, gcm, resolver); err != nil {
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
}

// buildKnownMap loads the dual-source (gchat+email) known-contact map:
// normalized address → []contactID.
func (p *GChatSyncProvider) buildKnownMap(ctx context.Context) (map[string][]uuid.UUID, error) {
	identities, err := p.commsRepo.ListGChatIdentitiesForSync(ctx)
	if err != nil {
		return nil, err
	}
	knownMap := make(map[string][]uuid.UUID)
	for _, id := range identities {
		// Defensive dedup: the same (address, contact) can appear under both a
		// gchat and an email source_type row.
		if !containsUUID(knownMap[id.ValueNormalized], id.ContactID) {
			knownMap[id.ValueNormalized] = append(knownMap[id.ValueNormalized], id.ContactID)
		}
	}
	return knownMap, nil
}

// consumeContentWindow pages a space's messages with create_time > cursor,
// upserts qualifying rows, and returns the new create_cursor (the max createTime
// successfully processed) plus the number of list pages consumed. On any error
// it returns the ORIGINAL cursor so the window restarts next sweep.
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
) (newCursor string, pages int, err error) {
	filter := fmt.Sprintf(`create_time > "%s"`, createCursor)
	maxCreate := createCursor
	pageToken := ""
	for {
		msgs, next, listErr := fetcher.ListMessages(ctx, space.Name, filter, false, pageToken)
		if listErr != nil {
			return createCursor, pages, fmt.Errorf("list messages: %w", listErr)
		}
		pages++
		for _, m := range msgs {
			if !qualifiableContentMessage(m) {
				continue
			}
			matchErr := p.qualifyAndUpsert(ctx, m, space, knownMembers, knownMap, meSet, resolver, accountID, counters)
			if matchErr != nil {
				// Transient resolve/upsert error: abort the window, keep the
				// original cursor (do NOT advance past this message).
				return createCursor, pages, matchErr
			}
			if m.CreateTime > maxCreate {
				maxCreate = m.CreateTime
			}
			counters.processed++
		}
		if next == "" {
			break
		}
		pageToken = next
		if pages >= gchatMaxWindowsPerSync {
			// Window budget exhausted mid-space: commit what we proved. maxCreate
			// only reflects fully-upserted messages, so this is safe.
			break
		}
	}
	return maxCreate, pages, nil
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

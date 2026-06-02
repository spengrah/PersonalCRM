package google

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

const (
	// GmailSourceName is the source identifier for Gmail. It matches the
	// external_sync_state.source, comms_message.source, and interaction.source
	// value 'email' (spec §6.2/§6.3).
	GmailSourceName = "email"
	// GmailDefaultInterval is the default sync interval for Gmail (every 15
	// minutes), matching the calendar provider's tick.
	GmailDefaultInterval = 15 * time.Minute
	// gmailCategoryFilter restricts the search to the Primary category, the
	// only category that carries 1:1 correspondence (spec §2.2).
	gmailCategoryFilter = "category:primary"
	// gmailChunkByteCap caps the URL-encoded length of a single chunk query at
	// ~6 KB, well under the practical ~8 KB GET-URL limit (spec §3.1/§7).
	gmailChunkByteCap = 6000
	// gmailDefaultBackfillSince is the onboarding floor used when an account
	// has no cursor and no metadata override (spec §3.2/§7).
	gmailDefaultBackfillSince = "2026-01-01"
	// gmailUserID is Gmail's special-value alias for the authenticated account.
	gmailUserID = "me"
)

// emailSafeTermRegex matches an address that is safe to embed verbatim in a
// Gmail `q` operator term. Stored value_normalized values usually come through
// the handler-level email validator, but the external-enrichment path
// (BuildMethodsFromExternal) does not validate, so a stray space, quote, or
// paren could corrupt the OR-group. We restrict to a conservative email-safe
// set and skip anything else.
var emailSafeTermRegex = regexp.MustCompile(`^[a-z0-9@._+%-]+$`)

// htmlTagRegex strips HTML tags during the text/html → plaintext fallback.
var htmlTagRegex = regexp.MustCompile(`(?s)<[^>]*>`)

// gmailMessageRef is a (id, threadId) stub returned by users.messages.list.
type gmailMessageRef struct {
	ID       string
	ThreadID string
}

// gmailFetcher is the narrow Gmail read surface the provider needs. The real
// implementation (gmailServiceFetcher) wraps *gmail.Service; tests inject a
// fake returning canned list pages and messages, so unit/integration tests
// never touch HTTP or OAuth. The *gmail.Message type stays in the signature so
// tests exercise the real MIME-walk / header-extraction code against real
// library structs.
type gmailFetcher interface {
	// ListMessageIDs returns one page of (id, threadId) stubs for the query.
	ListMessageIDs(ctx context.Context, query, pageToken string) (ids []gmailMessageRef, nextPageToken string, err error)
	// GetMessage fetches one message with format=full.
	GetMessage(ctx context.Context, id string) (*gmail.Message, error)
}

// gmailServiceFetcher is the production gmailFetcher backed by *gmail.Service.
type gmailServiceFetcher struct {
	svc *gmail.Service
}

func (f *gmailServiceFetcher) ListMessageIDs(ctx context.Context, query, pageToken string) ([]gmailMessageRef, string, error) {
	call := f.svc.Users.Messages.List(gmailUserID).Q(query)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	resp, err := call.Context(ctx).Do()
	if err != nil {
		return nil, "", fmt.Errorf("list messages: %w", err)
	}
	refs := make([]gmailMessageRef, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		refs = append(refs, gmailMessageRef{ID: m.Id, ThreadID: m.ThreadId})
	}
	return refs, resp.NextPageToken, nil
}

func (f *gmailServiceFetcher) GetMessage(ctx context.Context, id string) (*gmail.Message, error) {
	msg, err := f.svc.Users.Messages.Get(gmailUserID, id).Format("full").Context(ctx).Do()
	if err != nil {
		// id is a per-mailbox Gmail message id (third-party identifier); hash it
		// so the error (which flows into River logs) carries no raw third-party
		// reference while staying correlatable.
		return nil, fmt.Errorf("get message %s: %w", hashIdentifier(id), err)
	}
	return msg, nil
}

// attachmentMeta is the metadata-only descriptor for one message attachment.
// No content is ever fetched (spec §2.2).
type attachmentMeta struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// emailMetadata is the non-provenance JSON the provider assembles into
// comms_message.source_metadata. The provenance keys (observed_accounts,
// account_gmail_ids) are added by the UpsertCommsMessage query, NOT here.
type emailMetadata struct {
	HTML        string           `json:"html,omitempty"`
	Attachments []attachmentMeta `json:"attachments,omitempty"`
	Labels      []string         `json:"labels,omitempty"`
	From        string           `json:"from,omitempty"`
	To          []string         `json:"to,omitempty"`
	Cc          []string         `json:"cc,omitempty"`
	Bcc         []string         `json:"bcc,omitempty"`
}

// qualifiedRow is the provider's internal per-(message, contact, direction)
// result. Returning it from RunProcessMessageForTest lets unit tests assert
// resolution + content extraction without a DB or bus.
type qualifiedRow struct {
	ContactID      uuid.UUID
	Direction      string
	ExternalID     string
	ThreadID       string
	Subject        *string
	Body           *string
	Snippet        *string
	PeerHandle     string
	PeerNormalized string
	LocalDay       string
	SentAt         time.Time
	AccountID      string
	GmailMessageID string
	Metadata       []byte
}

// GmailSyncProvider implements sync.SyncProvider for Gmail. It is store-only:
// each qualifying message is upserted into comms_message and the matching
// email.received/email.sent event is published in the same tx
// (publish-before-mutate), and the EmailInteractionConsumer derives the
// interaction from that event. The provider holds the event bus and pgxpool
// directly because publish-before-mutate is its entire purpose; a nil bus or
// pool is a programming error caught by the Sync guard (no off mode).
type GmailSyncProvider struct {
	oauthService *OAuthService
	commsRepo    *repository.CommsMessageRepository
	bus          busTx
	pool         *pgxpool.Pool

	// newFetcher builds the per-account gmailFetcher, encapsulating the OAuth
	// call (GetClientForAccount + gmail.NewService). Defaulted to the real
	// *gmail.Service-backed factory in the constructor; tests override via
	// SetFetcherFactoryForTest so a fake fetcher is injected with NO OAuth /
	// token state. Keyed by accountID so the integration path never needs a
	// stored credential.
	newFetcher func(ctx context.Context, accountID string) (gmailFetcher, error)

	// newMeSet builds the "me" set (the normalized address of every connected
	// account). Defaulted to an impl that calls oauthService.ListAccounts;
	// tests override via SetMeSetForTest so the M-set is injected with no OAuth
	// state — symmetric with newFetcher.
	newMeSet func(ctx context.Context) (map[string]struct{}, error)
}

// NewGmailSyncProvider builds the Gmail provider. bus and pool are REQUIRED:
// the provider's entire purpose is publish-before-mutate, so a nil bus or pool
// is a programming error caught by the Sync guard. This differs from the
// calendar provider's off-mode (nil bus = skip publish); Gmail has no off mode.
func NewGmailSyncProvider(
	oauthService *OAuthService,
	commsRepo *repository.CommsMessageRepository,
	bus *events.Bus,
	pool *pgxpool.Pool,
) *GmailSyncProvider {
	p := &GmailSyncProvider{
		oauthService: oauthService,
		commsRepo:    commsRepo,
		bus:          bus,
		pool:         pool,
	}
	p.newFetcher = func(ctx context.Context, accountID string) (gmailFetcher, error) {
		client, err := oauthService.GetClientForAccount(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("get OAuth client: %w", err)
		}
		svc, err := gmail.NewService(ctx, option.WithHTTPClient(client))
		if err != nil {
			return nil, fmt.Errorf("create Gmail service: %w", err)
		}
		return &gmailServiceFetcher{svc: svc}, nil
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
func (p *GmailSyncProvider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 GmailSourceName,
		DisplayName:          "Gmail",
		Strategy:             repository.SyncStrategyContactDriven,
		SupportsMultiAccount: true,
		SupportsDiscovery:    false,
		DefaultInterval:      GmailDefaultInterval,
	}
}

// ValidateCredentials checks the Google credentials are valid for the account.
func (p *GmailSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
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

// Sync performs a Gmail sweep for one account. The framework's contacts slice
// is ignored (not hydrated with methods); the provider loads its own
// known-contact map via ListEmailIdentitiesForSync (spec §3.1).
func (p *GmailSyncProvider) Sync(
	ctx context.Context,
	state *repository.SyncState,
	_ []repository.Contact,
) (*sync.SyncResult, error) {
	if p.bus == nil || p.pool == nil {
		return nil, fmt.Errorf("gmail provider requires event bus and pool")
	}
	if state.AccountID == nil {
		return nil, fmt.Errorf("account ID required for Gmail sync")
	}
	accountID := *state.AccountID

	logger.Info().
		Str("source", GmailSourceName).
		Str("account", accountID).
		Msg("starting Gmail sync")

	// Known-contact map: normalized email → []contact_id (many-to-one).
	identities, err := p.commsRepo.ListEmailIdentitiesForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("list email identities: %w", err)
	}
	knownMap := make(map[string][]uuid.UUID)
	for _, id := range identities {
		knownMap[id.ValueNormalized] = append(knownMap[id.ValueNormalized], id.ContactID)
	}
	for addr, contacts := range knownMap {
		if len(contacts) > 1 {
			logger.Debug().
				Str("address", hashIdentifier(addr)).
				Int("contacts", len(contacts)).
				Msg("gmail: ambiguous shared address maps to multiple contacts")
		}
	}

	meSet, err := p.newMeSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("build me-set: %w", err)
	}

	fetcher, err := p.newFetcher(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("build gmail fetcher: %w", err)
	}

	// Resolve the after: floor. An empty cursor means onboarding — fall back to
	// backfill_since (default 2026-01-01) converted to epoch seconds.
	priorCursor := ""
	if state.SyncCursor != nil {
		priorCursor = *state.SyncCursor
	}
	priorCursorSecs, afterEpoch := resolveAfterFloor(priorCursor, state.Metadata)

	addresses := make([]string, 0, len(knownMap))
	for addr := range knownMap {
		addresses = append(addresses, addr)
	}
	chunks := buildORChunks(addresses, afterEpoch)

	processed, matched, maxInternalDateSecs, fetchedAny, err := p.scanChunks(ctx, fetcher, chunks, accountID, knownMap, meSet)
	if err != nil {
		// Hard failure: abort the sweep, return the prior cursor unchanged so
		// the whole after:<prior> window re-runs next tick.
		return p.failResult(priorCursor), fmt.Errorf("scan chunks: %w", err)
	}

	result := &sync.SyncResult{
		ItemsProcessed: processed,
		ItemsMatched:   matched,
	}
	result.NewCursor = computeNewCursor(fetchedAny, maxInternalDateSecs, priorCursorSecs, priorCursor, afterEpoch)

	logger.Info().
		Str("source", GmailSourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Int("matched", result.ItemsMatched).
		Str("cursor", result.NewCursor).
		Msg("Gmail sync completed")

	return result, nil
}

// scanChunks runs the fetch → per-call `seen` cross-chunk dedup → GetMessage →
// processMessage → persistRow inner loop for one account against the given
// chunk queries. It owns NEITHER cursor math NOR external_sync_state NOR
// failResult — it returns the RAW error to the caller, which applies its own
// failure semantics (Sync wraps it with failResult + cursor; ScanIdentifier
// returns it directly).
//
// The `seen` set is created fresh INSIDE this call — a per-account, per-sweep
// invariant (spec §3.1). Callers MUST NOT hoist it: a `seen` shared across
// accounts would skip the same Message-ID in account B and defeat the
// cross-account provenance merge.
func (p *GmailSyncProvider) scanChunks(
	ctx context.Context,
	fetcher gmailFetcher,
	chunks []string,
	accountID string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
) (processed, matched int, maxInternalDateSecs int64, fetchedAny bool, err error) {
	seen := make(map[string]struct{})
	for _, query := range chunks {
		pageToken := ""
		for {
			refs, next, listErr := fetcher.ListMessageIDs(ctx, query, pageToken)
			if listErr != nil {
				return processed, matched, maxInternalDateSecs, fetchedAny, fmt.Errorf("list message ids: %w", listErr)
			}
			for _, ref := range refs {
				if _, dup := seen[ref.ID]; dup {
					continue // cross-chunk dedup: body already fetched this sweep
				}
				seen[ref.ID] = struct{}{}
				fetchedAny = true

				msg, getErr := fetcher.GetMessage(ctx, ref.ID)
				if getErr != nil {
					// ref.ID is a per-mailbox Gmail message id (third-party);
					// hash it so the error in River logs carries no raw id.
					return processed, matched, maxInternalDateSecs, fetchedAny, fmt.Errorf("get message %s: %w", hashIdentifier(ref.ID), getErr)
				}
				rows, procErr := p.processMessage(ctx, msg, accountID, knownMap, meSet)
				if procErr != nil {
					return processed, matched, maxInternalDateSecs, fetchedAny, fmt.Errorf("process message %s: %w", hashIdentifier(ref.ID), procErr)
				}
				processed++

				// Processed (even if zero qualifying rows) contributes its
				// internalDate to the cursor advance.
				if secs := msg.InternalDate / 1000; secs > maxInternalDateSecs {
					maxInternalDateSecs = secs
				}

				for _, row := range rows {
					if perr := p.persistRow(ctx, row); perr != nil {
						return processed, matched, maxInternalDateSecs, fetchedAny, fmt.Errorf("persist row for message %s: %w", hashIdentifier(ref.ID), perr)
					}
					matched++
				}
			}
			pageToken = next
			if pageToken == "" {
				break
			}
		}
	}
	return processed, matched, maxInternalDateSecs, fetchedAny, nil
}

// ScanIdentifier runs a one-shot, identifier-scoped historical Gmail scan for a
// SINGLE normalized address against ONE connected account, reusing the
// steady-state Sync pipeline (build chunk → list → per-sweep seen dedup →
// GetMessage → processMessage → persistRow). It is the backfill seam for
// GmailRematchHandler: it publishes email.received/sent + upserts comms_message
// exactly as Sync does, but does NOT read or write external_sync_state
// (steady-state cursors are not rewound — spec §3.3). Returns the number of
// qualifying (message, contact) rows persisted.
func (p *GmailSyncProvider) ScanIdentifier(
	ctx context.Context,
	accountID, addrNormalized string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	afterEpoch int64,
) (matched int, err error) {
	if p.bus == nil || p.pool == nil {
		return 0, fmt.Errorf("gmail provider requires event bus and pool")
	}

	// A single address yields one OR-group → one chunk (or zero chunks if the
	// address is sanitized away, e.g. malformed — nothing to scan).
	chunks := buildORChunks([]string{addrNormalized}, afterEpoch)
	if len(chunks) == 0 {
		return 0, nil
	}

	fetcher, err := p.newFetcher(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("build gmail fetcher: %w", err)
	}

	// scanChunks creates its own per-account `seen` set; no cursor math here.
	_, matched, _, _, err = p.scanChunks(ctx, fetcher, chunks, accountID, knownMap, meSet)
	if err != nil {
		return matched, fmt.Errorf("scan identifier: %w", err)
	}
	return matched, nil
}

// MeSet builds the "me" set (the normalized address of every connected
// account) via the provider's me-set factory. Exported so the rematch handler
// can reuse the same seam tests override with SetMeSetForTest, routing an
// injected me-set through with zero real OAuth.
func (p *GmailSyncProvider) MeSet(ctx context.Context) (map[string]struct{}, error) {
	return p.newMeSet(ctx)
}

// failResult returns a SyncResult that does NOT advance the cursor: NewCursor
// is the prior cursor verbatim so the after:<prior> window re-runs next tick.
// A blank prior cursor stays blank — there is nothing to advance past on a
// first-sweep failure, and the next tick re-derives the onboarding floor.
func (p *GmailSyncProvider) failResult(priorCursor string) *sync.SyncResult {
	return &sync.SyncResult{NewCursor: priorCursor}
}

// persistRow writes one qualifying (message, contact, direction) in a single
// tx: publish-before-mutate (event publish, then content upsert), then commit.
// Each row is its own tx so a single bad message can't strand a whole sweep and
// no tx spans network I/O (content is already in memory). SourceID is
// per-(message, contact) so a message qualifying N contacts produces N distinct
// event rows (avoids the multi-entity (source, source_id) collapse).
func (p *GmailSyncProvider) persistRow(ctx context.Context, row qualifiedRow) (err error) {
	kind := events.KindEmailReceived
	if row.Direction == "outbound" {
		kind = events.KindEmailSent
	}
	payload := events.EmailEventPayload{
		Version:    1,
		ContactID:  row.ContactID,
		ExternalID: row.ExternalID,
		ThreadID:   row.ThreadID,
		LocalDay:   row.LocalDay,
		SentAt:     row.SentAt,
		Direction:  row.Direction,
		Subject:    row.Subject,
	}
	raw, err := events.Marshal(kind, payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", kind, err)
	}
	env := &events.Envelope{
		Source:     repository.InteractionSourceEmail,
		SourceID:   row.ExternalID + ":" + row.ContactID.String(),
		Kind:       kind,
		Payload:    raw,
		ObservedAt: row.SentAt,
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			// ExternalID is the RFC822 Message-ID, a third-party identifier, so
			// it is dropped from the log; source keeps the line attributable.
			logger.Warn().Err(rollbackErr).Str("source", GmailSourceName).Msg("gmail: tx rollback failed")
		}
	}()

	// Publish-before-mutate: a publish failure rolls back the content upsert.
	if err := p.bus.PublishTx(ctx, tx, env); err != nil {
		return fmt.Errorf("publish %s: %w", kind, err)
	}

	if _, err := p.commsRepo.UpsertMessageTx(ctx, tx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       row.ExternalID,
		ThreadID:         &row.ThreadID,
		Subject:          row.Subject,
		Body:             row.Body,
		Snippet:          row.Snippet,
		PeerHandle:       &row.PeerHandle,
		PeerNormalized:   &row.PeerNormalized,
		Direction:        row.Direction,
		SentAt:           row.SentAt,
		AccountID:        &row.AccountID,
		SourceMetadata:   row.Metadata,
		MatchedContactID: row.ContactID,
		GmailMessageID:   &row.GmailMessageID,
	}); err != nil {
		return fmt.Errorf("upsert comms_message: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// resolveAfterFloor derives the after:<epoch> floor and the prior cursor in
// epoch seconds. On an empty/unparseable cursor (onboarding) the floor is
// backfill_since (default 2026-01-01), and priorCursorSecs is 0. On a numeric
// cursor the floor is that value and priorCursorSecs mirrors it.
func resolveAfterFloor(cursor string, metadata map[string]any) (priorCursorSecs, afterEpoch int64) {
	if secs, err := strconv.ParseInt(strings.TrimSpace(cursor), 10, 64); err == nil && secs > 0 {
		return secs, secs
	}
	return 0, backfillSinceEpoch(metadata)
}

// backfillSinceEpoch reads metadata["backfill_since"] (a YYYY-MM-DD string,
// default 2026-01-01) and converts it to epoch seconds in UTC.
func backfillSinceEpoch(metadata map[string]any) int64 {
	since := gmailDefaultBackfillSince
	if metadata != nil {
		if v, ok := metadata["backfill_since"].(string); ok && strings.TrimSpace(v) != "" {
			since = strings.TrimSpace(v)
		}
	}
	t, err := time.Parse("2006-01-02", since)
	if err != nil {
		t, _ = time.Parse("2006-01-02", gmailDefaultBackfillSince)
	}
	return t.Unix()
}

// computeNewCursor implements the cursor-write contract. It NEVER returns
// an empty string (an empty NewCursor would NULL-clear the stored cursor and
// trigger a full re-backfill):
//   - ≥1 message fetched: advance to max(maxInternalDateSecs, priorCursorSecs),
//     monotonic so an out-of-order older message can't pull the floor back.
//   - zero messages fetched: re-write the prior cursor verbatim (idempotent
//     no-advance); on an empty prior cursor, write the backfill_since epoch so
//     the next sweep doesn't re-scan from the dawn of time.
func computeNewCursor(fetchedAny bool, maxInternalDateSecs, priorCursorSecs int64, priorCursor string, afterEpoch int64) string {
	if fetchedAny {
		advanced := maxInternalDateSecs
		if priorCursorSecs > advanced {
			advanced = priorCursorSecs
		}
		return strconv.FormatInt(advanced, 10)
	}
	if strings.TrimSpace(priorCursor) != "" {
		return priorCursor
	}
	return strconv.FormatInt(afterEpoch, 10)
}

// sanitizeAddresses filters the address list to terms safe to embed in a Gmail
// `q` operator. Addresses that are empty, contain whitespace, or contain a
// character outside the conservative email-safe set are dropped (and logged) so
// a single malformed contact_method can't corrupt a chunk query.
func sanitizeAddresses(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !emailSafeTermRegex.MatchString(a) {
			logger.Debug().Str("address", hashIdentifier(a)).Msg("gmail: skipping address unsafe for query term")
			continue
		}
		out = append(out, a)
	}
	return out
}

// buildORChunks builds byte-budgeted chunk queries. Each address contributes
// one group `(from:a OR to:a OR cc:a OR bcc:a)`; groups are greedily packed
// (joined by ` OR `) into a chunk while the URL-encoded length of the full
// candidate query stays ≤ gmailChunkByteCap. A group that alone exceeds the cap
// still forms its own chunk (groups are never split — splitting would break the
// participant-dimension coverage invariant). Addresses are sorted for
// deterministic chunk contents. Returns no chunks for an empty address list.
func buildORChunks(addresses []string, afterEpoch int64) []string {
	safe := sanitizeAddresses(addresses)
	if len(safe) == 0 {
		return nil
	}
	sort.Strings(safe)

	prefix := fmt.Sprintf("%s after:%d", gmailCategoryFilter, afterEpoch)

	var chunks []string
	var groups []string
	flush := func() {
		if len(groups) == 0 {
			return
		}
		chunks = append(chunks, fmt.Sprintf("%s (%s)", prefix, strings.Join(groups, " OR ")))
		groups = nil
	}
	for _, a := range safe {
		group := fmt.Sprintf("(from:%s OR to:%s OR cc:%s OR bcc:%s)", a, a, a, a)
		candidate := append(append([]string{}, groups...), group)
		query := fmt.Sprintf("%s (%s)", prefix, strings.Join(candidate, " OR "))
		if len(groups) > 0 && encodedLen(query) > gmailChunkByteCap {
			flush()
		}
		groups = append(groups, group)
	}
	flush()
	return chunks
}

// encodedLen returns the URL-encoded byte length of a query — the dimension the
// byte cap governs (matches the practical GET-URL limit the spec cites).
func encodedLen(q string) int {
	return len(urlQueryEscape(q))
}

// processMessage extracts content, resolves qualifying (contact, direction)
// participants per spec §4.1, and returns one qualifiedRow per qualifying
// (contact, direction). A non-qualifying (bystander) message returns zero rows
// and no error. Returns an error only on a hard extraction failure (base64
// decode of the chosen body part).
func (p *GmailSyncProvider) processMessage(
	_ context.Context,
	msg *gmail.Message,
	accountID string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
) ([]qualifiedRow, error) {
	body, htmlBody, attachments, err := extractContent(msg.Payload)
	if err != nil {
		return nil, fmt.Errorf("extract content: %w", err)
	}

	headers := newHeaderLookup(msg.Payload)
	subject := headers.first("Subject")
	fromRaw, fromNorm := parseSingleAddress(headers.first("From"))
	toRaw, toNorm := parseAddressList(headers.first("To"))
	ccRaw, ccNorm := parseAddressList(headers.first("Cc"))
	bccRaw, bccNorm := parseAddressList(headers.first("Bcc"))

	externalID := extractExternalID(headers, accountID, msg.Id)
	localDay := computeLocalDay(epochMillisToTime(msg.InternalDate), time.Local)
	sentAt := epochMillisToTime(msg.InternalDate)

	metadata := buildMetadataJSON(htmlBody, attachments, msg.LabelIds, fromRaw, toRaw, ccRaw, bccRaw)

	// Recipient set R (normalized To ∪ Cc ∪ Bcc), and whether M ∩ R ≠ ∅.
	recipientSet := make(map[string]struct{})
	for _, n := range toNorm {
		recipientSet[n] = struct{}{}
	}
	for _, n := range ccNorm {
		recipientSet[n] = struct{}{}
	}
	for _, n := range bccNorm {
		recipientSet[n] = struct{}{}
	}
	meInR := false
	for n := range recipientSet {
		if _, ok := meSet[n]; ok {
			meInR = true
			break
		}
	}
	_, fromIsMe := meSet[fromNorm]

	// Ordered, deduped recipient buckets (raw+normalized aligned) for the
	// deterministic outbound peer-handle precedence (To, then Cc, then Bcc;
	// header order within a bucket).
	orderedRecipients := buildOrderedRecipients(toRaw, toNorm, ccRaw, ccNorm, bccRaw, bccNorm)

	contentSubject := strPtrIfNotEmpty(subject)
	contentBody := strPtrIfNotEmpty(body)
	contentSnippet := strPtrIfNotEmpty(msg.Snippet)

	// Candidate contacts: every contact sharing a matched address in {f} ∪ R.
	candidateContacts := make(map[uuid.UUID]struct{})
	if cs, ok := knownMap[fromNorm]; ok {
		for _, c := range cs {
			candidateContacts[c] = struct{}{}
		}
	}
	for n := range recipientSet {
		if cs, ok := knownMap[n]; ok {
			for _, c := range cs {
				candidateContacts[c] = struct{}{}
			}
		}
	}

	var rows []qualifiedRow
	for contactID := range candidateContacts {
		addrSet := contactAddressSet(knownMap, contactID)

		// Inbound for C iff f ∈ A_C AND M ∩ R ≠ ∅.
		if _, fromIsContact := addrSet[fromNorm]; fromIsContact && meInR {
			rows = append(rows, qualifiedRow{
				ContactID:      contactID,
				Direction:      "inbound",
				ExternalID:     externalID,
				ThreadID:       msg.ThreadId,
				Subject:        contentSubject,
				Body:           contentBody,
				Snippet:        contentSnippet,
				PeerHandle:     fromRaw,
				PeerNormalized: fromNorm,
				LocalDay:       localDay,
				SentAt:         sentAt,
				AccountID:      accountID,
				GmailMessageID: msg.Id,
				Metadata:       metadata,
			})
			continue // a message can't be both inbound and outbound for the same C
		}

		// Outbound for C iff f ∈ M AND A_C ∩ R ≠ ∅.
		if fromIsMe {
			if rawPeer, normPeer, ok := firstContactRecipient(orderedRecipients, addrSet); ok {
				rows = append(rows, qualifiedRow{
					ContactID:      contactID,
					Direction:      "outbound",
					ExternalID:     externalID,
					ThreadID:       msg.ThreadId,
					Subject:        contentSubject,
					Body:           contentBody,
					Snippet:        contentSnippet,
					PeerHandle:     rawPeer,
					PeerNormalized: normPeer,
					LocalDay:       localDay,
					SentAt:         sentAt,
					AccountID:      accountID,
					GmailMessageID: msg.Id,
					Metadata:       metadata,
				})
			}
		}
		// Otherwise C is a bystander — skip (spec §4.1).
	}

	// Deterministic ordering of rows (contact id, then direction) so multi-row
	// messages persist in a stable order.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ContactID != rows[j].ContactID {
			return rows[i].ContactID.String() < rows[j].ContactID.String()
		}
		return rows[i].Direction < rows[j].Direction
	})
	return rows, nil
}

// orderedRecipient is one recipient address with its raw + normalized forms,
// preserving header order across the To → Cc → Bcc buckets.
type orderedRecipient struct {
	raw        string
	normalized string
}

// buildOrderedRecipients concatenates the recipient buckets in fixed order (To,
// Cc, Bcc), each in header-listed order, deduping by normalized address so a
// repeated address keeps its first (highest-precedence) position.
func buildOrderedRecipients(toRaw, toNorm, ccRaw, ccNorm, bccRaw, bccNorm []string) []orderedRecipient {
	var out []orderedRecipient
	seen := make(map[string]struct{})
	add := func(raws, norms []string) {
		for i := range norms {
			n := norms[i]
			if n == "" {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, orderedRecipient{raw: raws[i], normalized: n})
		}
	}
	add(toRaw, toNorm)
	add(ccRaw, ccNorm)
	add(bccRaw, bccNorm)
	return out
}

// firstContactRecipient returns the first ordered recipient whose normalized
// address is in the contact's address set, giving the deterministic outbound
// peer-handle (To wins over Cc wins over Bcc; first-listed wins within a
// bucket).
func firstContactRecipient(recipients []orderedRecipient, addrSet map[string]struct{}) (raw, normalized string, ok bool) {
	for _, r := range recipients {
		if _, in := addrSet[r.normalized]; in {
			return r.raw, r.normalized, true
		}
	}
	return "", "", false
}

// contactAddressSet returns the normalized address set A_C for a contact (the
// known-map keys that map to it).
func contactAddressSet(knownMap map[string][]uuid.UUID, contactID uuid.UUID) map[string]struct{} {
	out := make(map[string]struct{})
	for addr, contacts := range knownMap {
		for _, c := range contacts {
			if c == contactID {
				out[addr] = struct{}{}
				break
			}
		}
	}
	return out
}

// headerLookup provides case-insensitive access to a message part's headers.
type headerLookup struct {
	headers []*gmail.MessagePartHeader
}

func newHeaderLookup(payload *gmail.MessagePart) headerLookup {
	if payload == nil {
		return headerLookup{}
	}
	return headerLookup{headers: payload.Headers}
}

// first returns the first header value matching name (case-insensitive), or "".
func (h headerLookup) first(name string) string {
	for _, hdr := range h.headers {
		if strings.EqualFold(hdr.Name, name) {
			return hdr.Value
		}
	}
	return ""
}

// parseSingleAddress parses a single-address header (From). Returns the raw and
// normalized address. On parse failure it falls back to trimming the raw value.
func parseSingleAddress(header string) (raw, normalized string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ""
	}
	if addr, err := mail.ParseAddress(header); err == nil {
		return addr.Address, matching.NormalizeEmail(addr.Address)
	}
	return header, matching.NormalizeEmail(header)
}

// parseAddressList parses an address-list header (To/Cc/Bcc) robustly. On a
// ParseAddressList failure it falls back to a lenient comma-split so a single
// malformed recipient doesn't drop the whole list. Raw and normalized slices
// are index-aligned.
func parseAddressList(header string) (raws, normalized []string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil
	}
	if addrs, err := mail.ParseAddressList(header); err == nil {
		for _, a := range addrs {
			raws = append(raws, a.Address)
			normalized = append(normalized, matching.NormalizeEmail(a.Address))
		}
		return raws, normalized
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		raw := part
		if addr, err := mail.ParseAddress(part); err == nil {
			raw = addr.Address
		}
		raws = append(raws, raw)
		normalized = append(normalized, matching.NormalizeEmail(raw))
	}
	return raws, normalized
}

// extractExternalID reads the RFC822 Message-ID header (trimming surrounding
// <> and whitespace). If absent/empty it synthesizes a deterministic
// nomsgid:<account_id>:<gmail_message_id> fallback (spec §5.4).
func extractExternalID(headers headerLookup, accountID, gmailID string) string {
	msgID := strings.TrimSpace(headers.first("Message-ID"))
	if msgID == "" {
		msgID = strings.TrimSpace(headers.first("Message-Id"))
	}
	msgID = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(msgID, "<"), ">"))
	if msgID != "" {
		return msgID
	}
	return fmt.Sprintf("nomsgid:%s:%s", accountID, gmailID)
}

// computeLocalDay returns the calendar day of sentAt in loc (the server's
// time.Local for cadence math), formatted YYYY-MM-DD (spec §4.2).
func computeLocalDay(sentAt time.Time, loc *time.Location) string {
	return sentAt.In(loc).Format("2006-01-02")
}

// epochMillisToTime converts Gmail's internalDate (epoch millis) to a UTC time.
func epochMillisToTime(millis int64) time.Time {
	return time.UnixMilli(millis).UTC()
}

// isAttachmentPart reports whether a MIME part is an attachment: it has a
// filename, or its body is an external attachment referenced by AttachmentId
// (inline images and some forwarded parts carry an attachment id without a
// filename). Either signal means the part's bytes live out-of-band, so we
// record metadata only and never treat it as the message body.
func isAttachmentPart(part *gmail.MessagePart) bool {
	if part.Filename != "" {
		return true
	}
	return part.Body != nil && part.Body.AttachmentId != ""
}

// extractContent MIME-walks payload, returning the canonical plaintext body,
// the raw HTML body (when present), and attachment metadata. It prefers the
// first text/plain part; if none exists anywhere, it strips the first text/html
// part to plaintext for the body and retains the raw HTML. A part is an
// attachment when it has a non-empty filename and/or a Body.AttachmentId
// (inline attachments often carry an attachment id without a filename); only
// metadata is collected (no content is fetched). Returns an error only on a
// base64 decode failure of a chosen body part.
func extractContent(payload *gmail.MessagePart) (body, htmlBody string, attachments []attachmentMeta, err error) {
	if payload == nil {
		return "", "", nil, nil
	}
	var firstPlain, firstHTML *gmail.MessagePart
	var walk func(part *gmail.MessagePart)
	walk = func(part *gmail.MessagePart) {
		if part == nil {
			return
		}
		mimeType := strings.ToLower(part.MimeType)
		if isAttachmentPart(part) {
			size := int64(0)
			if part.Body != nil {
				size = part.Body.Size
			}
			attachments = append(attachments, attachmentMeta{
				Filename: part.Filename,
				MimeType: part.MimeType,
				Size:     size,
			})
		} else if mimeType == "text/plain" && firstPlain == nil {
			firstPlain = part
		} else if mimeType == "text/html" && firstHTML == nil {
			firstHTML = part
		}
		for _, child := range part.Parts {
			walk(child)
		}
	}
	walk(payload)

	if firstPlain != nil {
		decoded, derr := decodePartBody(firstPlain.Body)
		if derr != nil {
			return "", "", nil, fmt.Errorf("decode text/plain: %w", derr)
		}
		body = decoded
	}
	if firstHTML != nil {
		decoded, derr := decodePartBody(firstHTML.Body)
		if derr != nil {
			return "", "", nil, fmt.Errorf("decode text/html: %w", derr)
		}
		htmlBody = decoded
		if body == "" {
			body = stripHTML(decoded)
		}
	}
	return body, htmlBody, attachments, nil
}

// decodePartBody decodes a MIME part body. Gmail uses base64url (RFC 4648 §5);
// missing padding is tolerated via a RawURLEncoding fallback. An empty body
// decodes to "".
func decodePartBody(b *gmail.MessagePartBody) (string, error) {
	if b == nil || b.Data == "" {
		return "", nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(b.Data); err == nil {
		return string(decoded), nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(b.Data)
	if err != nil {
		return "", fmt.Errorf("base64url decode: %w", err)
	}
	return string(decoded), nil
}

// stripHTML reduces an HTML body to conservative plaintext: tags removed, basic
// entities unescaped, whitespace collapsed. The high-fidelity HTML is retained
// verbatim in source_metadata.html, so this only feeds the canonical body when
// no text/plain part exists.
func stripHTML(s string) string {
	s = htmlTagRegex.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// buildMetadataJSON assembles the non-provenance source_metadata JSON the
// provider owns (html, attachments, labels, from/to/cc/bcc). The provenance
// keys (observed_accounts, account_gmail_ids) are added by the
// UpsertCommsMessage query, not here.
func buildMetadataJSON(htmlBody string, attachments []attachmentMeta, labels []string, fromRaw string, toRaw, ccRaw, bccRaw []string) []byte {
	meta := emailMetadata{
		HTML:        htmlBody,
		Attachments: attachments,
		Labels:      labels,
		From:        fromRaw,
		To:          toRaw,
		Cc:          ccRaw,
		Bcc:         bccRaw,
	}
	b, err := json.Marshal(meta)
	if err != nil {
		// emailMetadata is composed entirely of marshalable types, so this is
		// unreachable; fall back to an empty object to keep the upsert valid.
		return []byte("{}")
	}
	return b
}

// urlQueryEscape is a thin wrapper over url.QueryEscape used by the byte-cap
// measurement so the chunk builder governs the encoded GET-URL length.
func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

// hashIdentifier returns a short, stable, non-reversible tag for a THIRD-PARTY
// identifier (a contact's email address) so Gmail-path logs can correlate lines
// for the same value within/across runs without writing a real third party's
// address into the operator log. It is NEVER applied to the connected-account
// (own-mailbox) address, which is operational provenance and logged raw. Empty
// input returns "" so an empty field stays empty rather than hashing to a
// constant. Lowercased before hashing so header-casing variants share one tag.
func hashIdentifier(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(v)))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// --- Test-only shims. Production code must NOT call these. ---

// RunProcessMessageForTest drives the unexported processMessage entry point so
// cross-package tests can assert participant/direction resolution + content
// extraction against real *gmail.Message structs without a DB or bus.
func (p *GmailSyncProvider) RunProcessMessageForTest(
	ctx context.Context,
	msg *gmail.Message,
	accountID string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
) ([]qualifiedRow, error) {
	return p.processMessage(ctx, msg, accountID, knownMap, meSet)
}

// SetFetcherFactoryForTest overrides the per-account fetcher factory so tests
// inject a fake gmailFetcher with no OAuth/token state (the OAuth seam).
func (p *GmailSyncProvider) SetFetcherFactoryForTest(factory func(ctx context.Context, accountID string) (gmailFetcher, error)) {
	p.newFetcher = factory
}

// SetMeSetForTest overrides the me-set factory so tests inject the connected-
// account address set with no OAuth state.
func (p *GmailSyncProvider) SetMeSetForTest(meSet map[string]struct{}) {
	p.newMeSet = func(context.Context) (map[string]struct{}, error) {
		return meSet, nil
	}
}

// SetBusForTest substitutes the publish bus so a test can assert
// publish-before-mutate (a failing PublishTx must leave no comms_message row).
func (p *GmailSyncProvider) SetBusForTest(b busTx) {
	p.bus = b
}

// GmailMessageRefForTest is the exported alias of the internal (id, threadId)
// stub, so cross-package tests can build canned list pages without reaching the
// unexported gmailMessageRef.
type GmailMessageRefForTest = gmailMessageRef

// FakeGmailFetcherFuncs lets a cross-package test supply a fake gmailFetcher by
// closures, since the gmailFetcher interface is unexported. Both funcs must be
// set. Build the fetcher with NewFakeGmailFetcherForTest and inject the
// resulting factory via SetFetcherFactoryForTest.
type FakeGmailFetcherFuncs struct {
	ListMessageIDs func(ctx context.Context, query, pageToken string) ([]GmailMessageRefForTest, string, error)
	GetMessage     func(ctx context.Context, id string) (*gmail.Message, error)
}

type fakeGmailFetcher struct {
	funcs FakeGmailFetcherFuncs
}

func (f *fakeGmailFetcher) ListMessageIDs(ctx context.Context, query, pageToken string) ([]gmailMessageRef, string, error) {
	return f.funcs.ListMessageIDs(ctx, query, pageToken)
}

func (f *fakeGmailFetcher) GetMessage(ctx context.Context, id string) (*gmail.Message, error) {
	return f.funcs.GetMessage(ctx, id)
}

// NewFakeGmailFetcherFactoryForTest returns a fetcher factory (accountID-keyed,
// the SetFetcherFactoryForTest shape) that always yields the same closure-backed
// fake fetcher, with NO OAuth/token state. Production code must NOT call this.
func NewFakeGmailFetcherFactoryForTest(funcs FakeGmailFetcherFuncs) func(ctx context.Context, accountID string) (gmailFetcher, error) {
	fetcher := &fakeGmailFetcher{funcs: funcs}
	return func(context.Context, string) (gmailFetcher, error) {
		return fetcher, nil
	}
}

// NewFakeGmailFetcherFactoryByAccountForTest returns a fetcher factory that
// picks the FakeGmailFetcherFuncs per accountID, so a cross-package test can
// make one account's fetcher fail while another succeeds (the partial-failure
// scan path) without reaching the unexported gmailFetcher. Production code must
// NOT call this.
func NewFakeGmailFetcherFactoryByAccountForTest(pick func(accountID string) FakeGmailFetcherFuncs) func(ctx context.Context, accountID string) (gmailFetcher, error) {
	return func(_ context.Context, accountID string) (gmailFetcher, error) {
		return &fakeGmailFetcher{funcs: pick(accountID)}, nil
	}
}

// EncodeBase64URLForTest exposes the base64url encoding used for message body
// data, so cross-package tests can build *gmail.Message bodies the provider
// will decode. Production code must NOT call this.
func EncodeBase64URLForTest(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

// BuildORChunksForTest exposes the byte-budgeted chunk builder so a
// cross-package integration test can assert how many OR-chunks a given address
// set produces (e.g., to prove a cross-chunk dedup scenario is meaningful).
// Production code must NOT call this.
func BuildORChunksForTest(addresses []string, afterEpoch int64) []string {
	return buildORChunks(addresses, afterEpoch)
}

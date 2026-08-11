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

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
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
	// gmailCategoryFilter excludes Gmail's bulk-mail categories
	// (Promotions/Social/Updates/Forums) so a known contact's promotional/bulk
	// mail stays out of the cadence signal (spec §2.2). It is a NEGATIVE filter
	// on purpose: an earlier `category:primary` positive filter matched NOTHING
	// on accounts where the category tabs are disabled (the Primary category is
	// unpopulated there), silently ingesting zero messages. Negative exclusions
	// degrade gracefully — on accounts with categories off they exclude nothing,
	// so all known-contact mail flows through; on accounts with categories on
	// they still drop bulk mail. The real noise gate is the known-contact
	// `from:/to:/cc:/bcc:` address filter, which already excludes all
	// non-contact mail.
	gmailCategoryFilter = "-category:promotions -category:social -category:updates -category:forums"
	// gmailChunkByteCap caps the URL-encoded length of a single chunk query at
	// ~6 KB, well under the practical ~8 KB GET-URL limit (spec §3.1/§7).
	gmailChunkByteCap = 6000
	// gmailUserID is Gmail's special-value alias for the authenticated account.
	gmailUserID = "me"
	// gmailWindowSpan bounds each proven cursor window. Catch-up scans several
	// windows per run, but the cursor only moves after a whole window is listed,
	// paged, fetched, and filtered successfully.
	gmailWindowSpan = 7 * 24 * time.Hour
	// gmailMaxWindowsPerSync lets a rewound account catch up promptly while
	// keeping one run bounded against unexpectedly large mailboxes or Gmail
	// rate limits.
	gmailMaxWindowsPerSync = 24
	// gmailSearchSafetyLag leaves recent Gmail index churn out of the proven
	// window. A later run picks it up once the index has had time to settle.
	gmailSearchSafetyLag = 10 * time.Minute
	// gmailSearchBoundaryOverlap compensates for Gmail search date operators
	// behaving coarser than internalDate. The query is deliberately broad and
	// the provider applies the exact internalDate window in Go.
	gmailSearchBoundaryOverlap = 48 * time.Hour
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
		return nil, fmt.Errorf("get message %s: %w", hashGmailMessageID(id), err)
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
//
// The *Name(s) fields carry the parsed display names index-aligned with their
// bare-address siblings (FromName ↔ From; ToNames[i] ↔ To[i]; etc.). They are
// additive: the bare-address keys keep their original shape so existing
// consumers and the provenance-merge JSON contract are undisturbed. The
// correspondence-enrichment producer pairs each address with its display name
// to trigram-match unknown correspondents against CRM contacts. A header
// without a display part stores the empty string at that index.
//
// FromName deliberately has NO omitempty: it is always written (even as "")
// so every row ingested after display-name capture shipped carries the
// from_name key. The historical re-derivation selects rows with NO from_name
// key; if a bare-From row omitted the key it would look "pre-capture" forever
// and be re-fetched on every catchup run. Always writing the key marks a row
// as "names already captured at ingest". (ToNames/CcNames/BccNames keep
// omitempty — they are not part of the re-derivation predicate.)
type emailMetadata struct {
	HTML        string           `json:"html,omitempty"`
	Attachments []attachmentMeta `json:"attachments,omitempty"`
	Labels      []string         `json:"labels,omitempty"`
	From        string           `json:"from,omitempty"`
	To          []string         `json:"to,omitempty"`
	Cc          []string         `json:"cc,omitempty"`
	Bcc         []string         `json:"bcc,omitempty"`
	FromName    string           `json:"from_name"`
	ToNames     []string         `json:"to_names,omitempty"`
	CcNames     []string         `json:"cc_names,omitempty"`
	BccNames    []string         `json:"bcc_names,omitempty"`
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

	// discoverer runs the in-sync correspondence-discovery hook over each
	// fetched message's From/To/Cc participants (between fetch and storage),
	// surfacing unknown addresses that strong-match an existing contact as
	// link-only candidates. Wired in cutover mode via
	// SetCorrespondenceDiscoverer; nil-safe — when nil the hook is a no-op, so
	// every existing constructor call site keeps working unchanged. Discovery
	// is best-effort: a discovery error is logged, never returned from
	// Sync/ScanIdentifier, so it can't rewind the cursor or strand email ingest.
	discoverer *CorrespondenceDiscoverer
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

// SetCorrespondenceDiscoverer wires the in-sync correspondence-discovery hook.
// Unlike SetMeSetForTest/SetBusForTest this is PRODUCTION wiring (main.go calls
// it in cutover mode), not a test seam: the provider has 9 constructor call
// sites, so a setter avoids a constructor-signature churn while keeping the
// hook nil-safe (a no-op) for every site that doesn't wire it.
func (p *GmailSyncProvider) SetCorrespondenceDiscoverer(d *CorrespondenceDiscoverer) {
	p.discoverer = d
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
		RequiresAccount:      true, // OAuth token is keyed by account; Sync nil-checks AccountID
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

	priorCursor := ""
	if state.SyncCursor != nil {
		priorCursor = *state.SyncCursor
	}
	cursorState := parseGmailCursor(priorCursor, state.Metadata)
	safeHorizon := accelerated.GetCurrentTime().UTC().Add(-gmailSearchSafetyLag).Unix()
	if safeHorizon <= cursorState.CompletedThrough {
		logger.Info().
			Str("source", GmailSourceName).
			Str("account", accountID).
			Int64("completed_through", cursorState.CompletedThrough).
			Int64("safe_horizon", safeHorizon).
			Msg("Gmail sync has no closed window to scan")
		return &sync.SyncResult{NewCursor: priorCursor}, nil
	}

	addresses := make([]string, 0, len(knownMap))
	for addr := range knownMap {
		addresses = append(addresses, addr)
	}

	// Per-pass correspondence-discovery state (Q3): a LOCAL aggregate map +
	// known-address set threaded down into the window loop, evaluated once after
	// the loop. It MUST be local (not a provider field): the provider is a
	// shared singleton and River runs SyncProviderAccount jobs for different
	// accounts concurrently, so a field would be clobbered across concurrent
	// account syncs. knownSet is the key set of knownMap (every known
	// contact_method address), used to drop known addresses from discovery.
	discoveryAgg := make(map[string]*correspondenceAggregate)
	knownSet := make(map[string]struct{}, len(knownMap))
	for addr := range knownMap {
		knownSet[addr] = struct{}{}
	}
	// ownDomains anchors trust for participant discovery and drops its own
	// addresses from BOTH candidate pools (discovery-only; storage's meSet is
	// untouched — see foldDiscovery).
	ownDomains := sync.EmailOwnDomains(state.Metadata)

	result := &sync.SyncResult{}
	windowsScanned := 0
	fetchedMessages := make(map[string]*gmail.Message)
	for windowsScanned < gmailMaxWindowsPerSync && cursorState.CompletedThrough < safeHorizon {
		windowEnd := cursorState.CompletedThrough + int64(gmailWindowSpan/time.Second)
		if windowEnd > safeHorizon {
			windowEnd = safeHorizon
		}
		if windowEnd <= cursorState.CompletedThrough {
			break
		}

		window := gmailScanWindow{
			StartEpoch:          cursorState.CompletedThrough,
			EndEpoch:            windowEnd,
			PriorBoundaryHashes: cursorState.BoundaryHashes,
		}
		chunks := buildWindowORChunks(addresses, window.StartEpoch, window.EndEpoch)

		scanned, err := p.scanWindowChunks(ctx, fetcher, chunks, accountID, knownMap, meSet, window, fetchedMessages, knownSet, ownDomains, discoveryAgg)
		if err != nil {
			// Hard failure: abort the sweep, return the stored cursor unchanged
			// so no unproven window is skipped on the next tick.
			return p.failResult(priorCursor), fmt.Errorf("scan window: %w", err)
		}

		result.ItemsProcessed += scanned.Processed
		result.ItemsMatched += scanned.Matched
		cursorState.CompletedThrough = windowEnd
		cursorState.BoundaryHashes = hashesToSet(scanned.BoundaryHashes)
		windowsScanned++
	}

	// Best-effort correspondence discovery over the per-pass aggregate. Logged,
	// never fatal: a FindSimilarContacts/Upsert hiccup must NOT rewind the
	// cursor, zero result, or fail the sync — email ingest is the primary job.
	p.runDiscovery(ctx, accountID, discoveryAgg)

	if windowsScanned == 0 {
		result.NewCursor = priorCursor
	} else {
		result.NewCursor = encodeGmailCursor(cursorState)
	}

	logger.Info().
		Str("source", GmailSourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Int("matched", result.ItemsMatched).
		Int("windows_scanned", windowsScanned).
		Int64("completed_through", cursorState.CompletedThrough).
		Int("boundary_count", len(cursorState.BoundaryHashes)).
		Msg("Gmail sync completed")

	return result, nil
}

type gmailCursorState struct {
	CompletedThrough int64
	BoundaryHashes   map[string]struct{}
}

type gmailCursorJSON struct {
	Version          int      `json:"v"`
	CompletedThrough int64    `json:"completed_through"`
	BoundaryHashes   []string `json:"boundary_hashes"`
}

type gmailScanWindow struct {
	StartEpoch          int64
	EndEpoch            int64
	PriorBoundaryHashes map[string]struct{}
}

type gmailWindowScanResult struct {
	Processed      int
	Matched        int
	BoundaryHashes []string
}

// scanWindowChunks runs one closed cursor window. Gmail's search operators are
// intentionally broad; each fetched message is filtered by internalDate seconds
// before processing so coarse search-boundary behavior cannot skip mail inside
// the proven window.
func (p *GmailSyncProvider) scanWindowChunks(
	ctx context.Context,
	fetcher gmailFetcher,
	chunks []string,
	accountID string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	window gmailScanWindow,
	fetchedMessages map[string]*gmail.Message,
	knownSet map[string]struct{},
	ownDomains map[string]struct{},
	discoveryAgg map[string]*correspondenceAggregate,
) (gmailWindowScanResult, error) {
	var result gmailWindowScanResult
	seen := make(map[string]struct{})
	boundaryHashes := make(map[string]struct{})
	if fetchedMessages == nil {
		fetchedMessages = make(map[string]*gmail.Message)
	}

	for _, query := range chunks {
		pageToken := ""
		for {
			refs, next, listErr := fetcher.ListMessageIDs(ctx, query, pageToken)
			if listErr != nil {
				return result, fmt.Errorf("list message ids: %w", listErr)
			}
			for _, ref := range refs {
				idHash := hashGmailMessageID(ref.ID)
				if _, dup := seen[idHash]; dup {
					continue
				}
				seen[idHash] = struct{}{}

				if _, replay := window.PriorBoundaryHashes[idHash]; replay {
					continue
				}

				msg, fetched := fetchedMessages[idHash]
				if !fetched {
					var getErr error
					msg, getErr = fetcher.GetMessage(ctx, ref.ID)
					if getErr != nil {
						return result, fmt.Errorf("get message %s: %w", idHash, getErr)
					}
					fetchedMessages[idHash] = msg
				}

				msgEpoch := msg.InternalDate / 1000
				if msgEpoch < window.StartEpoch || msgEpoch > window.EndEpoch {
					continue
				}
				if msgEpoch == window.EndEpoch {
					boundaryHashes[idHash] = struct{}{}
				}

				rows, procErr := p.processMessage(ctx, msg, accountID, knownMap, meSet)
				if procErr != nil {
					return result, fmt.Errorf("process message %s: %w", idHash, procErr)
				}
				result.Processed++

				for _, row := range rows {
					if perr := p.persistRow(ctx, row); perr != nil {
						return result, fmt.Errorf("persist row for message %s: %w", idHash, perr)
					}
					result.Matched++
				}

				// In-sync correspondence discovery: fold this fetched message's
				// From/To/Cc participants into the per-pass aggregate, regardless
				// of whether the storage gate qualified it. No-op when no
				// discoverer is wired.
				p.foldDiscovery(msg, knownMap, knownSet, meSet, ownDomains, discoveryAgg)
			}
			pageToken = next
			if pageToken == "" {
				break
			}
		}
	}

	result.BoundaryHashes = setToSortedHashes(boundaryHashes)
	return result, nil
}

// scanChunks runs ScanIdentifier's fetch → per-call `seen` cross-chunk dedup →
// GetMessage → processMessage → persistRow inner loop for one account against
// the given chunk queries. It does not read or write steady-state cursors.
//
// The `seen` set is created fresh INSIDE this call — a per-account, per-sweep
// invariant. Callers MUST NOT hoist it: a `seen` shared across
// accounts would skip the same Message-ID in account B and defeat the
// cross-account provenance merge.
func (p *GmailSyncProvider) scanChunks(
	ctx context.Context,
	fetcher gmailFetcher,
	chunks []string,
	accountID string,
	knownMap map[string][]uuid.UUID,
	meSet map[string]struct{},
	knownSet map[string]struct{},
	ownDomains map[string]struct{},
	discoveryAgg map[string]*correspondenceAggregate,
) (processed, matched int, err error) {
	seen := make(map[string]struct{})
	for _, query := range chunks {
		pageToken := ""
		for {
			refs, next, listErr := fetcher.ListMessageIDs(ctx, query, pageToken)
			if listErr != nil {
				return processed, matched, fmt.Errorf("list message ids: %w", listErr)
			}
			for _, ref := range refs {
				if _, dup := seen[ref.ID]; dup {
					continue // cross-chunk dedup: body already fetched this sweep
				}
				seen[ref.ID] = struct{}{}

				msg, getErr := fetcher.GetMessage(ctx, ref.ID)
				if getErr != nil {
					// ref.ID is a per-mailbox Gmail message id (third-party);
					// hash it so the error in River logs carries no raw id.
					return processed, matched, fmt.Errorf("get message %s: %w", hashGmailMessageID(ref.ID), getErr)
				}
				rows, procErr := p.processMessage(ctx, msg, accountID, knownMap, meSet)
				if procErr != nil {
					return processed, matched, fmt.Errorf("process message %s: %w", hashGmailMessageID(ref.ID), procErr)
				}
				processed++

				for _, row := range rows {
					if perr := p.persistRow(ctx, row); perr != nil {
						return processed, matched, fmt.Errorf("persist row for message %s: %w", hashGmailMessageID(ref.ID), perr)
					}
					matched++
				}

				// In-sync correspondence discovery over the rematch backfill seam
				// (harmless + idempotent). No-op when no discoverer is wired.
				p.foldDiscovery(msg, knownMap, knownSet, meSet, ownDomains, discoveryAgg)
			}
			pageToken = next
			if pageToken == "" {
				break
			}
		}
	}
	return processed, matched, nil
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
	ownDomains map[string]struct{},
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

	// Per-call correspondence-discovery state (local, like Sync's): the rematch
	// backfill seam runs discovery too, idempotently. knownSet is the key set of
	// knownMap.
	discoveryAgg := make(map[string]*correspondenceAggregate)
	knownSet := make(map[string]struct{}, len(knownMap))
	for addr := range knownMap {
		knownSet[addr] = struct{}{}
	}

	// scanChunks creates its own per-account `seen` set; no cursor math here.
	_, matched, err = p.scanChunks(ctx, fetcher, chunks, accountID, knownMap, meSet, knownSet, ownDomains, discoveryAgg)
	if err != nil {
		return matched, fmt.Errorf("scan identifier: %w", err)
	}

	// Best-effort discovery: logged, NEVER returned. GmailRematchHandler treats
	// any ScanIdentifier error as a job failure, so a discovery hiccup must not
	// fail the contact-method rematch backfill.
	p.runDiscovery(ctx, accountID, discoveryAgg)

	return matched, nil
}

// RefetchParticipantNames re-fetches one already-ingested message by its
// per-mailbox Gmail id and re-parses the From/To/Cc/Bcc display names. It is
// the re-fetch seam for the one-time historical display-name re-derivation
// (crm-admin --rederive-correspondence-names): the bare addresses stored at
// first ingest cannot recover the display names, so the runner must re-fetch
// the original headers. Reuses the existing fetcher + name-returning parsers;
// tests inject a fake fetcher via SetFetcherFactoryForTest (no OAuth).
//
// Error classification (so the runner can distinguish a since-deleted message
// from a retryable failure): a Gmail 404 → errCorrespondenceUnavailable
// (PERMANENT — count SkippedUnavailable, never a non-zero exit); a 429/5xx →
// errCorrespondenceTransient (retryable). Other errors propagate as-is and the
// runner counts them Failed.
func (p *GmailSyncProvider) RefetchParticipantNames(ctx context.Context, accountID, gmailMessageID string) (repository.ParticipantNames, error) {
	fetcher, err := p.newFetcher(ctx, accountID)
	if err != nil {
		return repository.ParticipantNames{}, fmt.Errorf("build gmail fetcher: %w", err)
	}
	msg, err := fetcher.GetMessage(ctx, gmailMessageID)
	if err != nil {
		return repository.ParticipantNames{}, classifyRefetchError(err)
	}
	headers := newHeaderLookup(msg.Payload)
	_, _, fromName := parseSingleAddress(headers.first("From"))
	_, _, toNames := parseAddressList(headers.first("To"))
	_, _, ccNames := parseAddressList(headers.first("Cc"))
	_, _, bccNames := parseAddressList(headers.first("Bcc"))
	return repository.ParticipantNames{
		FromName: fromName,
		ToNames:  toNames,
		CcNames:  ccNames,
		BccNames: bccNames,
	}, nil
}

// classifyRefetchError maps a GetMessage error to a permanent
// (errCorrespondenceUnavailable) or transient (errCorrespondenceTransient)
// sentinel the re-derivation runner branches on. A googleapi 404 is permanent
// (the message is gone upstream); 429 / 5xx are transient (backoff + retry).
// Anything else is returned wrapped (the runner counts it Failed).
func classifyRefetchError(err error) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Code == 404:
			return fmt.Errorf("%w: %v", errCorrespondenceUnavailable, err)
		case apiErr.Code == 429 || (apiErr.Code >= 500 && apiErr.Code < 600):
			return fmt.Errorf("%w: %v", errCorrespondenceTransient, err)
		}
	}
	return fmt.Errorf("refetch message: %w", err)
}

// MeSet builds the "me" set (the normalized address of every connected
// account) via the provider's me-set factory. Exported so the rematch handler
// can reuse the same seam tests override with SetMeSetForTest, routing an
// injected me-set through with zero real OAuth.
func (p *GmailSyncProvider) MeSet(ctx context.Context) (map[string]struct{}, error) {
	return p.newMeSet(ctx)
}

// foldDiscovery folds one fetched message's From/To/Cc participants into the
// per-pass discovery aggregate. No-op when no discoverer is wired (the common
// case for the many constructor call sites that never call
// SetCorrespondenceDiscoverer). Headers are parsed ONCE (parseDiscoveryHeaders)
// to compute the participant list plus this message's trust-anchor facts:
// trustAnchored = From ∈ knownSet ∪ meSet ∪ an own-domain; recipientCount =
// |unique normalized To∪Cc|; createEligible = trustAnchored && recipientCount
// <= 20 (the cap suppresses the CREATE path only — link discovery, driven by
// aggregateParticipants' known/own/ownDomains pool filtering plus the
// evaluator's separate trigram gate, stays uncapped). The known/own filtering
// and the BCC-free co-occurrence id set are computed here so
// aggregateParticipants stays a pure fold.
func (p *GmailSyncProvider) foldDiscovery(
	msg *gmail.Message,
	knownMap map[string][]uuid.UUID,
	knownSet, meSet, ownDomains map[string]struct{},
	discoveryAgg map[string]*correspondenceAggregate,
) {
	if p.discoverer == nil {
		return
	}
	h := parseDiscoveryHeaders(msg)
	coOccurIDs := discoveryCoOccurIDs(h.parts, knownMap)

	_, fromKnown := knownSet[h.fromNorm]
	_, fromMe := meSet[h.fromNorm]
	fromOwnDomain := false
	if h.fromNorm != "" {
		if _, ok := ownDomains[domainOf(h.fromNorm)]; ok {
			fromOwnDomain = true
		}
	}
	trustAnchored := fromKnown || fromMe || fromOwnDomain

	recipients := make(map[string]struct{}, len(h.toNorm)+len(h.ccNorm))
	for _, n := range h.toNorm {
		if n != "" {
			recipients[n] = struct{}{}
		}
	}
	for _, n := range h.ccNorm {
		if n != "" {
			recipients[n] = struct{}{}
		}
	}
	createEligible := trustAnchored && len(recipients) <= gmailParticipantRecipientCap

	var senderContactID uuid.UUID
	if ids := knownMap[h.fromNorm]; len(ids) > 0 {
		senderContactID = smallestUUID(ids)
	}

	msgCtx := participantMessageContext{
		createEligible:  createEligible,
		senderNorm:      h.fromNorm,
		senderContactID: senderContactID,
		senderIsSelf:    fromMe || fromOwnDomain,
		subject:         h.subject,
		epochSeconds:    h.epochSeconds,
	}

	aggregateParticipants(h.parts, knownSet, meSet, ownDomains, coOccurIDs, msgCtx, discoveryAgg)
}

// gmailParticipantRecipientCap is the participant-discovery CREATE-path
// recipient cap (spec: To∪Cc > 20 suppresses create proposals from that
// message; link discovery is unaffected). Exactly 20 passes.
const gmailParticipantRecipientCap = 20

// domainOf returns the substring after the last '@' in a normalized address,
// or "" when absent.
func domainOf(normAddr string) string {
	idx := strings.LastIndex(normAddr, "@")
	if idx < 0 || idx == len(normAddr)-1 {
		return ""
	}
	return normAddr[idx+1:]
}

// smallestUUID returns the lexicographically smallest id in ids (deterministic
// tie-break for a shared address mapping to multiple contacts), mirroring
// strongestCoOccurrence's tie-break. Returns uuid.Nil for an empty slice.
func smallestUUID(ids []uuid.UUID) uuid.UUID {
	best := uuid.Nil
	for _, id := range ids {
		if best == uuid.Nil || id.String() < best.String() {
			best = id
		}
	}
	return best
}

// runDiscovery evaluates the per-pass discovery aggregate once, best-effort. A
// discovery error is logged (not returned) so it can NEVER rewind the cursor,
// fail the sync sweep, or fail the rematch backfill. No-op when no discoverer
// is wired or the aggregate is empty.
func (p *GmailSyncProvider) runDiscovery(ctx context.Context, accountID string, discoveryAgg map[string]*correspondenceAggregate) {
	if p.discoverer == nil || len(discoveryAgg) == 0 {
		return
	}
	upserted, err := p.discoverer.EvaluateAddresses(ctx, sortedAggregates(discoveryAgg))
	if err != nil {
		logger.Warn().
			Err(err).
			Str("source", GmailSourceName).
			Str("account", accountID).
			Msg("gmail: correspondence discovery had per-address failures (non-fatal)")
		return
	}
	if upserted > 0 {
		logger.Info().
			Str("source", GmailSourceName).
			Str("account", accountID).
			Int("candidates_upserted", upserted).
			Msg("gmail: correspondence discovery upserted candidates")
	}
}

// collectDiscoveryParticipants extracts the (display_name, address) pairs from a
// fetched message's From, To, and Cc headers using the SAME parsers
// processMessage uses. Bcc is intentionally omitted from discovery per spec;
// Gmail strips it from sent copies and it is out of scope. A thin wrapper over
// parseDiscoveryHeaders (the single per-message header parse foldDiscovery
// also uses), kept as its own entry point for RunDiscoverParticipantsForTest.
func (p *GmailSyncProvider) collectDiscoveryParticipants(msg *gmail.Message) []participant {
	return parseDiscoveryHeaders(msg).parts
}

// discoveryHeaders is the result of parsing a fetched message's From/To/Cc/
// Subject headers ONCE for discovery: the participant list plus the facts
// foldDiscovery needs to compute this message's trust-anchor context
// (fromNorm/toNorm/ccNorm/subject/epochSeconds).
type discoveryHeaders struct {
	parts        []participant
	fromNorm     string
	toNorm       []string
	ccNorm       []string
	subject      string
	epochSeconds int64
}

// parseDiscoveryHeaders parses a fetched message's From/To/Cc/Subject headers
// once. Bcc is intentionally excluded (Gmail strips it from sent copies; the
// spec scopes discovery to To ∪ Cc).
func parseDiscoveryHeaders(msg *gmail.Message) discoveryHeaders {
	headers := newHeaderLookup(msg.Payload)
	fromRaw, fromNorm, fromName := parseSingleAddress(headers.first("From"))
	toRaws, toNorm, toNames := parseAddressList(headers.first("To"))
	ccRaws, ccNorm, ccNames := parseAddressList(headers.first("Cc"))
	subject := headers.first("Subject")

	parts := make([]participant, 0, 1+len(toRaws)+len(ccRaws))
	if fromRaw != "" {
		parts = append(parts, participant{name: fromName, address: fromRaw})
	}
	parts = appendParticipants(parts, toRaws, toNames)
	parts = appendParticipants(parts, ccRaws, ccNames)

	return discoveryHeaders{
		parts:        parts,
		fromNorm:     fromNorm,
		toNorm:       toNorm,
		ccNorm:       ccNorm,
		subject:      subject,
		epochSeconds: msg.InternalDate / 1000,
	}
}

// appendParticipants pairs each raw address with its index-aligned name (a
// missing name becomes ""), skipping empty addresses. The parsers guarantee the
// raw + name slices are index-aligned.
func appendParticipants(out []participant, raws, names []string) []participant {
	for i, addr := range raws {
		if addr == "" {
			continue
		}
		name := ""
		if i < len(names) {
			name = names[i]
		}
		out = append(out, participant{name: name, address: addr})
	}
	return out
}

// discoveryCoOccurIDs returns the set of known contact ids present on a
// message's From/To/Cc participants (BCC-free by construction —
// collectDiscoveryParticipants never emits Bcc). This is the co-occurring-contact
// evidence the candidate records: the KNOWN contacts on the message, never the
// suggested match. It deliberately does NOT reuse processMessage's
// candidateContacts set (which is built over To ∪ Cc ∪ Bcc), so a Bcc'd known
// contact can never leak into discovery evidence.
func discoveryCoOccurIDs(parts []participant, knownMap map[string][]uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{})
	var out []uuid.UUID
	for _, part := range parts {
		norm := matching.NormalizeEmail(part.address)
		if norm == "" {
			continue
		}
		for _, id := range knownMap[norm] {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
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

// parseGmailCursor accepts the legacy numeric cursor and the v2 JSON cursor.
// Empty or invalid cursors fall back to the configured backfill floor.
func parseGmailCursor(cursor string, metadata map[string]any) gmailCursorState {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return gmailCursorState{CompletedThrough: backfillSinceEpoch(metadata), BoundaryHashes: map[string]struct{}{}}
	}

	if secs, err := strconv.ParseInt(cursor, 10, 64); err == nil && secs > 0 {
		return gmailCursorState{CompletedThrough: secs, BoundaryHashes: map[string]struct{}{}}
	}

	var encoded gmailCursorJSON
	if err := json.Unmarshal([]byte(cursor), &encoded); err == nil &&
		encoded.Version == 2 &&
		encoded.CompletedThrough > 0 {
		return gmailCursorState{
			CompletedThrough: encoded.CompletedThrough,
			BoundaryHashes:   hashesToSet(encoded.BoundaryHashes),
		}
	}

	return gmailCursorState{CompletedThrough: backfillSinceEpoch(metadata), BoundaryHashes: map[string]struct{}{}}
}

func encodeGmailCursor(state gmailCursorState) string {
	encoded := gmailCursorJSON{
		Version:          2,
		CompletedThrough: state.CompletedThrough,
		BoundaryHashes:   setToSortedHashes(state.BoundaryHashes),
	}
	b, err := json.Marshal(encoded)
	if err != nil {
		return strconv.FormatInt(state.CompletedThrough, 10)
	}
	return string(b)
}

func hashesToSet(hashes []string) map[string]struct{} {
	out := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		out[h] = struct{}{}
	}
	return out
}

func setToSortedHashes(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// backfillSinceEpoch reads the shared email backfill floor from sync metadata.
func backfillSinceEpoch(metadata map[string]any) int64 {
	return sync.EmailBackfillFloorEpoch(metadata)
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
	prefix := fmt.Sprintf("%s after:%d", gmailCategoryFilter, afterEpoch)
	return buildORChunksWithPrefix(addresses, prefix)
}

// buildWindowORChunks builds Sync's closed-window queries. The Gmail search
// bounds are intentionally wider than the proven internalDate window; exact
// inclusion/exclusion happens after GetMessage using internalDate seconds.
func buildWindowORChunks(addresses []string, startEpoch, endEpoch int64) []string {
	queryAfter, queryBefore := gmailSearchQueryBounds(startEpoch, endEpoch)
	prefix := fmt.Sprintf("%s after:%d before:%d", gmailCategoryFilter, queryAfter, queryBefore)
	return buildORChunksWithPrefix(addresses, prefix)
}

func buildORChunksWithPrefix(addresses []string, prefix string) []string {
	safe := sanitizeAddresses(addresses)
	if len(safe) == 0 {
		return nil
	}
	sort.Strings(safe)

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

func gmailSearchQueryBounds(startEpoch, endEpoch int64) (afterEpoch, beforeEpoch int64) {
	afterEpoch = time.Unix(startEpoch, 0).UTC().Add(-gmailSearchBoundaryOverlap).Unix()
	if afterEpoch < 0 {
		afterEpoch = 0
	}
	beforeEpoch = time.Unix(endEpoch, 0).UTC().Add(gmailSearchBoundaryOverlap).Unix()
	return afterEpoch, beforeEpoch
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
	fromRaw, fromNorm, fromName := parseSingleAddress(headers.first("From"))
	toRaw, toNorm, toNames := parseAddressList(headers.first("To"))
	ccRaw, ccNorm, ccNames := parseAddressList(headers.first("Cc"))
	bccRaw, bccNorm, bccNames := parseAddressList(headers.first("Bcc"))

	externalID := extractExternalID(headers, accountID, msg.Id)
	localDay := computeLocalDay(epochMillisToTime(msg.InternalDate), time.Local)
	sentAt := epochMillisToTime(msg.InternalDate)

	metadata := buildMetadataJSON(htmlBody, attachments, msg.LabelIds,
		fromRaw, toRaw, ccRaw, bccRaw, fromName, toNames, ccNames, bccNames)

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
// normalized address plus the parsed display name (empty when the header had no
// display part or failed to parse). On parse failure it falls back to trimming
// the raw value. The name return is additive — existing callers ignore it.
func parseSingleAddress(header string) (raw, normalized, name string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", "", ""
	}
	if addr, err := mail.ParseAddress(header); err == nil {
		return addr.Address, matching.NormalizeEmail(addr.Address), strings.TrimSpace(addr.Name)
	}
	return header, matching.NormalizeEmail(header), ""
}

// parseAddressList parses an address-list header (To/Cc/Bcc) robustly. On a
// ParseAddressList failure it falls back to a lenient comma-split so a single
// malformed recipient doesn't drop the whole list. The raw, normalized, and
// name slices are index-aligned on BOTH paths (a recipient with no display part
// gets an empty-string name at its index), so the correspondence producer can
// pair names[i] with the address at the same index. The names return is
// additive — existing callers ignore it.
func parseAddressList(header string) (raws, normalized, names []string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil, nil
	}
	if addrs, err := mail.ParseAddressList(header); err == nil {
		for _, a := range addrs {
			raws = append(raws, a.Address)
			normalized = append(normalized, matching.NormalizeEmail(a.Address))
			names = append(names, strings.TrimSpace(a.Name))
		}
		return raws, normalized, names
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		raw := part
		name := ""
		if addr, err := mail.ParseAddress(part); err == nil {
			raw = addr.Address
			name = strings.TrimSpace(addr.Name)
		}
		raws = append(raws, raw)
		normalized = append(normalized, matching.NormalizeEmail(raw))
		names = append(names, name)
	}
	return raws, normalized, names
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
// provider owns (html, attachments, labels, from/to/cc/bcc + their index-aligned
// display names). The provenance keys (observed_accounts, account_gmail_ids) are
// added by the UpsertCommsMessage query, not here. The *Name(s) slices stay
// index-aligned with their address siblings (the parsers guarantee this on both
// the happy path and the lenient comma-split fallback).
func buildMetadataJSON(
	htmlBody string,
	attachments []attachmentMeta,
	labels []string,
	fromRaw string,
	toRaw, ccRaw, bccRaw []string,
	fromName string,
	toNames, ccNames, bccNames []string,
) []byte {
	meta := emailMetadata{
		HTML:        htmlBody,
		Attachments: attachments,
		Labels:      labels,
		From:        fromRaw,
		To:          toRaw,
		Cc:          ccRaw,
		Bcc:         bccRaw,
		FromName:    fromName,
		ToNames:     toNames,
		CcNames:     ccNames,
		BccNames:    bccNames,
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

// hashGmailMessageID hashes a per-mailbox Gmail message id as an exact opaque
// identifier. Unlike email-address hashing, it is case-sensitive because the id
// itself is the identity key used by boundary replay suppression.
func hashGmailMessageID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
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

// RunDiscoverParticipantsForTest drives the unexported
// collectDiscoveryParticipants entry point so cross-package tests can assert
// participant collection (From/To/Cc, no Bcc) against real *gmail.Message
// structs. The returned pairs are exposed as DiscoveryParticipantForTest values.
func (p *GmailSyncProvider) RunDiscoverParticipantsForTest(msg *gmail.Message) []DiscoveryParticipantForTest {
	parts := p.collectDiscoveryParticipants(msg)
	out := make([]DiscoveryParticipantForTest, 0, len(parts))
	for _, part := range parts {
		out = append(out, DiscoveryParticipantForTest{Name: part.name, Address: part.address})
	}
	return out
}

// DiscoveryParticipantForTest is the exported view of a discovery participant
// (display_name, address) pair, so cross-package tests can assert
// collectDiscoveryParticipants output without reaching the unexported
// participant type. Production code must NOT use this.
type DiscoveryParticipantForTest struct {
	Name    string
	Address string
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

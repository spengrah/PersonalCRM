package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/mac"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// PairingTokenTTL is the lifetime of a freshly-minted pairing token.
// Short enough to limit blast radius; long enough that a human can copy
// the token from the UI to a terminal before it expires.
const PairingTokenTTL = 10 * time.Minute

// pairingTokenByteLen produces 24 random bytes which encode to 32 chars
// of base64 (without padding). 192 bits of entropy is well past
// brute-force range even against a non-rate-limited consume path.
const pairingTokenByteLen = 24

// apiKeyByteLen is the random byte length of a freshly-minted host
// API key. 32 bytes → 256 bits of entropy; presented to the daemon as
// hex (so 64 chars).
const apiKeyByteLen = 32

// ErrPairingTokenInvalid is returned by PairWithToken when the token
// is unknown. Handler maps to 410 (gone) so leaking the existence of
// a valid-but-consumed token is impossible.
var ErrPairingTokenInvalid = errors.New("pairing token invalid")

// ErrPairingTokenExpired is returned when the token has aged out.
// Same 410 mapping as ErrPairingTokenInvalid.
var ErrPairingTokenExpired = errors.New("pairing token expired")

// ErrPairingTokenAlreadyUsed is returned when the token has already
// been consumed. Same 410 mapping.
var ErrPairingTokenAlreadyUsed = errors.New("pairing token already used")

// ErrHostAlreadyPaired is returned when the singleton mac_host index
// rejects a second pairing. Handler maps to 409.
var ErrHostAlreadyPaired = errors.New("another Mac host is already paired; revoke it first or uninstall the existing daemon")

// ErrAPIKeyStaleAuth is returned by RotateAPIKey when the api-key the
// caller authenticated with is no longer the live key (because a
// concurrent rotation committed a new key between this request's
// middleware auth and the rotate tx's FOR UPDATE read). Handler maps to
// 401 STALE_AUTH. The pairing token is NOT consumed; the operator's
// recovery is to re-pair against the new live key.
var ErrAPIKeyStaleAuth = errors.New("api-key stale (rotated by another request)")

// ErrPairingValidation is returned when an input field is missing or
// malformed (e.g. empty hostname). Handler maps to 400. The HTTP
// handler is the primary input-validation layer; this is a defence-in-
// depth path for direct service-layer callers.
var ErrPairingValidation = errors.New("pairing input validation failed")

// ErrUnknownPushSource is returned by CommitCursor / GetCursor when
// the supplied source is not in mac.AllowedPushSources. Handler maps
// to 400 (validation error). Defence-in-depth — the HTTP handler is
// the primary gate.
var ErrUnknownPushSource = errors.New("unknown push source")

// ContactMethodLister is the narrow interface MacHostService needs for
// the /known-identifiers endpoint. Concrete is
// *repository.ContactMethodRepository. Defined here so the service can
// be unit-tested with a stub without depending on the full repo.
type ContactMethodLister interface {
	ListCanonicalIdentifiersByType(ctx context.Context, types []string) ([]string, error)
}

// KnownExternalContactID is the service-layer alias for the
// repository's wire DTO. Re-exporting here keeps handlers and other
// callers from importing the repository package just to spell the
// return type of MacHostService.KnownIDsForSource.
type KnownExternalContactID = repository.KnownExternalContactID

// ExternalContactReader is the narrow surface MacHostService needs for
// the per-(host, source) /known-ids endpoint and the per-host
// /source-counts endpoint. Concrete is
// *repository.ExternalContactRepository. Defined here so the service
// can be unit-tested with a stub.
type ExternalContactReader interface {
	ListKnownIDsByHostAndSource(ctx context.Context, hostID uuid.UUID, source string) ([]KnownExternalContactID, error)
	CountByHostAndSource(ctx context.Context, hostID uuid.UUID) (map[string]int, error)
}

// MeetingNoteReader is the narrow surface MacHostService needs for the
// anarlog_sessions arm of /known-ids. Concrete is
// *repository.MeetingNoteRepository. Returns the same wire DTO shape
// as ExternalContactReader so the handler treats both sources uniformly.
type MeetingNoteReader interface {
	ListKnownMeetingNoteSessionIDsByHost(ctx context.Context, hostID uuid.UUID) ([]KnownExternalContactID, error)
}

// MacHostService owns business logic for pairing, heartbeat, cursor
// commit, and revoke-cascade. Stateless aside from the constructor-
// injected dependencies — safe to share across requests.
type MacHostService struct {
	hostRepo            *repository.MacHostRepository
	tokenRepo           *repository.MacHostPairingTokenRepository
	syncRepo            *repository.SyncRepository
	contactMethodRepo   ContactMethodLister
	externalContactRepo ExternalContactReader
	meetingNoteRepo     MeetingNoteReader
	pool                *pgxpool.Pool
	bcryptCost          int
}

// NewMacHostService constructs a service. bcryptCost defaults to
// bcrypt.DefaultCost when 0 — at cost=10 the hash is ~50ms on a Pi 4,
// fine for once-per-pair. contactMethodRepo, externalContactRepo, and
// meetingNoteRepo may be nil in test fixtures that don't exercise the
// corresponding endpoint; the affected method returns a clear "not
// wired" error in that state.
func NewMacHostService(
	hostRepo *repository.MacHostRepository,
	tokenRepo *repository.MacHostPairingTokenRepository,
	syncRepo *repository.SyncRepository,
	contactMethodRepo ContactMethodLister,
	externalContactRepo ExternalContactReader,
	meetingNoteRepo MeetingNoteReader,
	pool *pgxpool.Pool,
	bcryptCost int,
) *MacHostService {
	if bcryptCost == 0 {
		bcryptCost = bcrypt.DefaultCost
	}
	return &MacHostService{
		hostRepo:            hostRepo,
		tokenRepo:           tokenRepo,
		syncRepo:            syncRepo,
		contactMethodRepo:   contactMethodRepo,
		externalContactRepo: externalContactRepo,
		meetingNoteRepo:     meetingNoteRepo,
		pool:                pool,
		bcryptCost:          bcryptCost,
	}
}

// KnownIdentifiersResult is the cross-source identifier set returned to
// the daemon. Both arrays are alphabetically sorted and deduplicated
// (DISTINCT value_normalized across all contact_method rows that join
// a non-deleted contact). Empty arrays on a fresh CRM.
type KnownIdentifiersResult struct {
	Phones []string `json:"phones"`
	Emails []string `json:"emails"`
}

// KnownIdentifiers returns the canonical phone + email set the daemon
// uses to filter incoming Apple Messages senders against the user's
// known contacts. Two SQL queries (phones, emails) — cleaner than a
// single union because the daemon receives them under separate JSON
// keys.
func (s *MacHostService) KnownIdentifiers(ctx context.Context) (*KnownIdentifiersResult, error) {
	if s.contactMethodRepo == nil {
		return nil, fmt.Errorf("known identifiers: contact_method repository not wired")
	}
	emails, err := s.contactMethodRepo.ListCanonicalIdentifiersByType(ctx, []string{"email"})
	if err != nil {
		return nil, fmt.Errorf("list canonical emails: %w", err)
	}
	phones, err := s.contactMethodRepo.ListCanonicalIdentifiersByType(ctx, []string{"phone"})
	if err != nil {
		return nil, fmt.Errorf("list canonical phones: %w", err)
	}
	if emails == nil {
		emails = []string{}
	}
	if phones == nil {
		phones = []string{}
	}
	return &KnownIdentifiersResult{Phones: phones, Emails: emails}, nil
}

// KnownIDsForSource returns the per-(host, source) known IDs and their
// last observed content hashes. Empty slice on a fresh CRM or for
// sources without rows. The handler validates the source against
// mac.IsAllowedPushSource before dispatching. The slice is sorted by
// source_id for deterministic responses.
//
// Dispatch:
//   - source == "anarlog_sessions" → meeting_note repository.
//   - any other allowed source → external_contact repository (current
//     coverage: icloud_contacts, anarlog_humans).
//
// Used by the Mac daemon's tombstone-reconciliation flow after a full
// upstream scan: the daemon set-diffs its scan results against the
// returned IDs and emits *.deleted events for entries that disappeared.
// The last_content_hash supplies the prior-hash component of the
// spec-defined delete source_id (`<entity>@deleted@<prev_hash>`).
func (s *MacHostService) KnownIDsForSource(
	ctx context.Context,
	hostID uuid.UUID,
	source string,
) ([]KnownExternalContactID, error) {
	if source == "anarlog_sessions" {
		if s.meetingNoteRepo == nil {
			return nil, fmt.Errorf("known IDs: meeting_note repository not wired")
		}
		return s.meetingNoteRepo.ListKnownMeetingNoteSessionIDsByHost(ctx, hostID)
	}
	if s.externalContactRepo == nil {
		return nil, fmt.Errorf("known IDs: external_contact repository not wired")
	}
	return s.externalContactRepo.ListKnownIDsByHostAndSource(ctx, hostID, source)
}

// GetSourceCounts returns a per-source count of live external_contact
// rows owned by hostID. Powers GET /api/v1/host/:id/source-counts
// (issue #327). Returns db.ErrNotFound when the host doesn't exist;
// returns an empty map when the host has no external_contact rows.
//
// The host-existence pre-check is what lets the handler distinguish
// 404 ("no such host") from 200 with empty counts ("host has no
// rows"). Without it, the count query returns an empty result for both
// cases.
func (s *MacHostService) GetSourceCounts(
	ctx context.Context,
	hostID uuid.UUID,
) (map[string]int, error) {
	if s.externalContactRepo == nil {
		return nil, fmt.Errorf("source counts: external_contact repository not wired")
	}
	if _, err := s.hostRepo.GetHost(ctx, hostID); err != nil {
		// Propagate db.ErrNotFound for the handler's 404 mapping.
		return nil, err
	}
	counts, err := s.externalContactRepo.CountByHostAndSource(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if counts == nil {
		counts = map[string]int{}
	}
	return counts, nil
}

// CreatePairingToken mints a new short-lived pairing token. Returns
// the plaintext token (single-show) and the expiry time. The token is
// stored only as a SHA-256 hash; the plaintext leaves the service in
// the return value and is never persisted.
func (s *MacHostService) CreatePairingToken(ctx context.Context) (plaintext string, expiresAt time.Time, err error) {
	buf := make([]byte, pairingTokenByteLen)
	if _, err = rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("generate pairing token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	expiresAt = accelerated.GetCurrentTime().Add(PairingTokenTTL)
	if _, err = s.tokenRepo.CreateToken(ctx, hashPairingToken(plaintext), expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("persist pairing token: %w", err)
	}
	return plaintext, expiresAt, nil
}

// PairResult is returned by PairWithToken on success.
type PairResult struct {
	HostID      uuid.UUID
	APIKey      string // plaintext, single-show; daemon stores in Keychain
	CursorEpoch int64
}

// PairWithToken consumes a pairing token + creates a paired mac_host
// row + mints an API key, all in a single tx. Daemon's first request.
//
// On token-already-consumed / expired / unknown, returns the
// corresponding ErrPairingToken*. On the singleton-index conflict
// (a host is already paired), returns ErrHostAlreadyPaired AND the
// pairing token is NOT consumed (transaction rolled back). The
// operator must revoke the active host before re-pairing.
func (s *MacHostService) PairWithToken(
	ctx context.Context,
	plaintextToken, hostname, daemonVersion string,
	protocolVersion int32,
) (*PairResult, error) {
	if plaintextToken == "" {
		return nil, ErrPairingTokenInvalid
	}
	if hostname == "" {
		return nil, fmt.Errorf("%w: hostname is required", ErrPairingValidation)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pair tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Validate the token BEFORE minting an API key — bcrypt is ~50ms
	// at cost=10 and we don't want every garbage POST to pay that
	// cost. The token lookup is a single indexed SELECT FOR UPDATE
	// which is cheap.
	token, err := s.tokenRepo.GetTokenByHashForUpdateTx(ctx, tx, hashPairingToken(plaintextToken))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrPairingTokenInvalid
		}
		return nil, fmt.Errorf("lookup pairing token: %w", err)
	}
	if token.ConsumedAt != nil {
		return nil, ErrPairingTokenAlreadyUsed
	}
	if !token.ExpiresAt.After(accelerated.GetCurrentTime()) {
		return nil, ErrPairingTokenExpired
	}

	apiKey, apiKeyHash, err := mintAPIKey(s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("mint api key: %w", err)
	}

	host, err := s.hostRepo.CreateHostTx(ctx, tx, hostname, daemonVersion, protocolVersion, apiKeyHash)
	if err != nil {
		// Singleton index violation surfaces here. Distinguish via PG error code.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrHostAlreadyPaired
		}
		return nil, err
	}

	if _, err := s.tokenRepo.MarkConsumedTx(ctx, tx, token.ID, host.ID); err != nil {
		return nil, fmt.Errorf("mark token consumed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pair tx: %w", err)
	}

	return &PairResult{
		HostID:      host.ID,
		APIKey:      apiKey,
		CursorEpoch: host.CursorEpoch,
	}, nil
}

// RotateAPIKeyResult is returned by RotateAPIKey on success.
type RotateAPIKeyResult struct {
	HostID          uuid.UUID
	APIKey          string // plaintext, single-show; daemon stores in api-key file
	APIKeyRotatedAt time.Time
}

// RotateAPIKey atomically consumes a pairing token, mints a new
// api-key, and swaps it into the existing mac_host row. The same id
// is preserved; cursor_epoch, hostname, source_health, permissions
// are all unchanged. Returns the new plaintext key for the daemon to
// persist locally.
//
// Trust model: caller has ALREADY authenticated as hostID via
// MacHostAuthMiddleware (proven control of the CURRENT pair-key).
// The pairing token is the operator-side acknowledgment that this
// rotation was intentional. Both are required.
//
// Concurrent-rotation safety (compare-and-swap):
// expectedCurrentHash is the api_key_hash the caller's middleware
// authenticated against. The tx re-reads the row under FOR UPDATE
// lock; if the live hash no longer matches, returns ErrAPIKeyStaleAuth
// and rolls back (token NOT consumed). This prevents two parallel
// rotations with the same starting pair-key from both committing —
// the second one's CAS check fails and it gets 401.
//
// Failure mapping (the handler translates these to HTTP codes):
//   - ErrPairingTokenInvalid     → 400 INVALID_PAIRING_TOKEN
//   - ErrPairingTokenAlreadyUsed → 400 TOKEN_ALREADY_USED
//   - ErrPairingTokenExpired     → 400 TOKEN_EXPIRED
//   - ErrAPIKeyStaleAuth         → 401 STALE_AUTH
//   - db.ErrNotFound (host)      → 404 (host was revoked or deleted
//     between auth and tx-internal lookup)
//   - any other error            → 500
func (s *MacHostService) RotateAPIKey(
	ctx context.Context,
	hostID uuid.UUID,
	expectedCurrentHash string,
	plaintextToken string,
) (*RotateAPIKeyResult, error) {
	if plaintextToken == "" {
		return nil, ErrPairingTokenInvalid
	}
	if expectedCurrentHash == "" {
		// Defence-in-depth: the handler must always pass the
		// authenticated host's APIKeyHash from gin context. An empty
		// string here would weaken the CAS check.
		return nil, fmt.Errorf("rotate api key: expectedCurrentHash is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin rotate tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the host row FOR UPDATE to serialize concurrent rotations
	// on the same host. Filters revoked hosts (returns ErrNotFound).
	host, err := s.hostRepo.GetActiveHostByIDForUpdateTx(ctx, tx, hostID)
	if err != nil {
		return nil, err
	}

	// Stale-auth CAS check. If a concurrent rotation committed
	// between our middleware read and the FOR UPDATE read, the
	// locked row's hash will differ from what we authenticated
	// against. Reject without consuming the token. Constant-time
	// string compare to avoid timing leak.
	if subtle.ConstantTimeCompare([]byte(host.APIKeyHash), []byte(expectedCurrentHash)) != 1 {
		return nil, ErrAPIKeyStaleAuth
	}

	// Validate + lock the pairing token. Same semantics as
	// PairWithToken but with distinct error codes (operator already
	// proved current-key control, so leaking token-state via distinct
	// codes is safe).
	token, err := s.tokenRepo.GetTokenByHashForUpdateTx(ctx, tx, hashPairingToken(plaintextToken))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrPairingTokenInvalid
		}
		return nil, fmt.Errorf("lookup pairing token: %w", err)
	}
	if token.ConsumedAt != nil {
		return nil, ErrPairingTokenAlreadyUsed
	}
	if !token.ExpiresAt.After(accelerated.GetCurrentTime()) {
		return nil, ErrPairingTokenExpired
	}

	// Mint new api-key.
	newAPIKey, newAPIKeyHash, err := mintAPIKey(s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("mint api key: %w", err)
	}

	// Atomically swap the hash + bump api_key_rotated_at.
	rotated, err := s.hostRepo.RotateAPIKeyTx(ctx, tx, host.ID, newAPIKeyHash)
	if err != nil {
		return nil, err
	}

	// Consume the pairing token (mark consumed_at + consumed_host_id).
	if _, err := s.tokenRepo.MarkConsumedTx(ctx, tx, token.ID, rotated.ID); err != nil {
		return nil, fmt.Errorf("mark token consumed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit rotate tx: %w", err)
	}

	// Compute the timestamp BEFORE logging so the audit log tolerates
	// a hypothetical nil (column is nullable in the schema; the SQL
	// UPDATE sets NOW() but a future schema migration / converter
	// regression could leave it nil). Dereferencing the pointer inside
	// the log call without a guard would panic after a successful
	// commit but before the daemon receives the new key.
	rotatedAt := time.Time{}
	if rotated.APIKeyRotatedAt != nil {
		rotatedAt = *rotated.APIKeyRotatedAt
	}

	// Audit log AFTER successful commit. Structured fields only, never
	// the plaintext key.
	logger.Info().
		Str("host_id", rotated.ID.String()).
		Time("api_key_rotated_at", rotatedAt).
		Msg("mac host api-key rotated")

	return &RotateAPIKeyResult{
		HostID:          rotated.ID,
		APIKey:          newAPIKey,
		APIKeyRotatedAt: rotatedAt,
	}, nil
}

// Heartbeat updates the host's heartbeat fields. Returns the updated
// host (so the handler can echo cursor_epoch back to the daemon) or
// db.ErrNotFound if the host has been revoked.
func (s *MacHostService) Heartbeat(ctx context.Context, hostID uuid.UUID, payload repository.HeartbeatPayload) (*repository.MacHost, error) {
	return s.hostRepo.UpdateHeartbeat(ctx, hostID, payload)
}

// CommitCursor wraps SyncRepository.CommitMacHostCursor. Returns the
// repository's typed errors directly so handlers can map them to
// status codes. Defence-in-depth: rejects sources outside the
// AllowedPushSources allowlist even if a non-HTTP caller reaches the
// service.
func (s *MacHostService) CommitCursor(ctx context.Context, params repository.CommitMacHostCursorParams) error {
	if !mac.IsAllowedPushSource(params.Source) {
		return fmt.Errorf("%w: unknown push source %q", ErrUnknownPushSource, params.Source)
	}
	return s.syncRepo.CommitMacHostCursor(ctx, params)
}

// GetCursor returns the cursor for (source, hostID). Returns
// db.ErrNotFound if no cursor has been committed yet — handler treats
// that as an empty cursor + current cursor_epoch. Defence-in-depth:
// rejects sources outside the AllowedPushSources allowlist.
func (s *MacHostService) GetCursor(ctx context.Context, source string, hostID uuid.UUID) (*repository.MacHostCursor, error) {
	if !mac.IsAllowedPushSource(source) {
		return nil, fmt.Errorf("%w: unknown push source %q", ErrUnknownPushSource, source)
	}
	return s.syncRepo.GetMacHostSyncCursor(ctx, source, hostID)
}

// ListActiveHosts returns the admin-view list. Returned slice may be
// empty (no host paired yet).
func (s *MacHostService) ListActiveHosts(ctx context.Context) ([]*repository.MacHost, error) {
	return s.hostRepo.ListActiveHosts(ctx)
}

// GetHost is the admin-view detail. Returns db.ErrNotFound for an
// unknown UUID; revoked hosts are returned (the UI may want to show
// historical detail).
func (s *MacHostService) GetHost(ctx context.Context, id uuid.UUID) (*repository.MacHost, error) {
	return s.hostRepo.GetHost(ctx, id)
}

// RevokeHost marks the host revoked AND cascades: deletes the host's
// push-strategy cursor rows in external_sync_state. Returns
// db.ErrNotFound if the host is missing or already revoked.
//
// The cascade is required because the singleton mac_host index only
// considers live (non-revoked) hosts — leaving a stale cursor row
// behind would block re-pairing under a different host UUID once
// re-pair semantics evolve.
func (s *MacHostService) RevokeHost(ctx context.Context, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin revoke tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := s.hostRepo.RevokeHostTx(ctx, tx, id); err != nil {
		return err
	}
	rows, err := s.syncRepo.DeleteMacHostSyncStatesTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit revoke tx: %w", err)
	}
	logger.Info().
		Str("host_id", id.String()).
		Int64("cursor_rows_deleted", rows).
		Msg("mac host revoked")
	return nil
}

// hashPairingToken returns the SHA-256 hex digest of plaintext. The
// token table stores only this hash; the plaintext never lives at rest.
func hashPairingToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// mintAPIKey generates a fresh plaintext API key and returns it along
// with its bcrypt hash. The plaintext is returned to the caller for
// one-time display; only the hash is stored on the host row.
func mintAPIKey(bcryptCost int) (plaintext, hash string, err error) {
	buf := make([]byte, apiKeyByteLen)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	plaintext = hex.EncodeToString(buf)
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", "", fmt.Errorf("bcrypt: %w", err)
	}
	return plaintext, string(hashBytes), nil
}

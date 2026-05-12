package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

// MacHostService owns business logic for pairing, heartbeat, cursor
// commit, and revoke-cascade. Stateless aside from the constructor-
// injected dependencies — safe to share across requests.
type MacHostService struct {
	hostRepo   *repository.MacHostRepository
	tokenRepo  *repository.MacHostPairingTokenRepository
	syncRepo   *repository.SyncRepository
	pool       *pgxpool.Pool
	bcryptCost int
}

// NewMacHostService constructs a service. bcryptCost defaults to
// bcrypt.DefaultCost when 0 — at cost=10 the hash is ~50ms on a Pi 4,
// fine for once-per-pair.
func NewMacHostService(
	hostRepo *repository.MacHostRepository,
	tokenRepo *repository.MacHostPairingTokenRepository,
	syncRepo *repository.SyncRepository,
	pool *pgxpool.Pool,
	bcryptCost int,
) *MacHostService {
	if bcryptCost == 0 {
		bcryptCost = bcrypt.DefaultCost
	}
	return &MacHostService{
		hostRepo:   hostRepo,
		tokenRepo:  tokenRepo,
		syncRepo:   syncRepo,
		pool:       pool,
		bcryptCost: bcryptCost,
	}
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

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MacHost is the repository-layer view of a paired Mac daemon. Pointer
// fields are nil when the corresponding DB column is NULL.
type MacHost struct {
	ID              uuid.UUID
	Hostname        string
	DaemonVersion   string
	ProtocolVersion int32
	LastHeartbeatAt *time.Time
	Permissions     json.RawMessage
	SourceHealth    json.RawMessage
	CursorEpoch     int64
	APIKeyHash      string
	APIKeyRevokedAt *time.Time
	APIKeyRotatedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MacHostPairingToken is the repository-layer view of a single pairing
// token. The plaintext token is never persisted — only token_hash.
type MacHostPairingToken struct {
	ID             uuid.UUID
	TokenHash      string
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
	ConsumedHostID *uuid.UUID
	CreatedAt      time.Time
}

func convertDbMacHost(m *db.MacHost) *MacHost {
	out := &MacHost{
		Hostname:        m.Hostname,
		DaemonVersion:   m.DaemonVersion,
		ProtocolVersion: m.ProtocolVersion,
		Permissions:     append(json.RawMessage(nil), m.Permissions...),
		SourceHealth:    append(json.RawMessage(nil), m.SourceHealth...),
		CursorEpoch:     m.CursorEpoch,
		APIKeyHash:      m.ApiKeyHash,
	}
	if m.ID.Valid {
		out.ID = uuid.UUID(m.ID.Bytes)
	}
	if m.LastHeartbeatAt.Valid {
		t := m.LastHeartbeatAt.Time
		out.LastHeartbeatAt = &t
	}
	if m.ApiKeyRevokedAt.Valid {
		t := m.ApiKeyRevokedAt.Time
		out.APIKeyRevokedAt = &t
	}
	if m.ApiKeyRotatedAt.Valid {
		t := m.ApiKeyRotatedAt.Time
		out.APIKeyRotatedAt = &t
	}
	if m.CreatedAt.Valid {
		out.CreatedAt = m.CreatedAt.Time
	}
	if m.UpdatedAt.Valid {
		out.UpdatedAt = m.UpdatedAt.Time
	}
	return out
}

func convertDbPairingToken(t *db.MacHostPairingToken) *MacHostPairingToken {
	out := &MacHostPairingToken{
		TokenHash: t.TokenHash,
	}
	if t.ID.Valid {
		out.ID = uuid.UUID(t.ID.Bytes)
	}
	if t.ExpiresAt.Valid {
		out.ExpiresAt = t.ExpiresAt.Time
	}
	if t.ConsumedAt.Valid {
		c := t.ConsumedAt.Time
		out.ConsumedAt = &c
	}
	if t.ConsumedHostID.Valid {
		id := uuid.UUID(t.ConsumedHostID.Bytes)
		out.ConsumedHostID = &id
	}
	if t.CreatedAt.Valid {
		out.CreatedAt = t.CreatedAt.Time
	}
	return out
}

// MacHostRepository handles persistence for paired Mac daemons.
type MacHostRepository struct {
	queries db.Querier
}

// NewMacHostRepository constructs a MacHostRepository.
func NewMacHostRepository(queries db.Querier) *MacHostRepository {
	return &MacHostRepository{queries: queries}
}

// CreateHost inserts a new mac_host row and returns the inserted state.
// hostname is required; the rest of the fields default to safe values.
// API-key-hash is the bcrypt hash of the plaintext key — the plaintext
// is never persisted.
func (r *MacHostRepository) CreateHost(
	ctx context.Context,
	hostname, daemonVersion string,
	protocolVersion int32,
	apiKeyHash string,
) (*MacHost, error) {
	return createMacHost(ctx, r.queries, hostname, daemonVersion, protocolVersion, apiKeyHash)
}

// CreateHostTx is the tx-bound variant of CreateHost — used by the
// pairing-flow service so token-consume + host-create are atomic.
func (r *MacHostRepository) CreateHostTx(
	ctx context.Context,
	tx pgx.Tx,
	hostname, daemonVersion string,
	protocolVersion int32,
	apiKeyHash string,
) (*MacHost, error) {
	return createMacHost(ctx, db.New(tx), hostname, daemonVersion, protocolVersion, apiKeyHash)
}

func createMacHost(
	ctx context.Context,
	q db.Querier,
	hostname, daemonVersion string,
	protocolVersion int32,
	apiKeyHash string,
) (*MacHost, error) {
	row, err := q.CreateMacHost(ctx, db.CreateMacHostParams{
		Hostname:        hostname,
		DaemonVersion:   daemonVersion,
		ProtocolVersion: protocolVersion,
		ApiKeyHash:      apiKeyHash,
	})
	if err != nil {
		return nil, fmt.Errorf("create mac_host: %w", err)
	}
	return convertDbMacHost(row), nil
}

// GetActiveHostByID returns the host row only if api_key_revoked_at is
// NULL. Used by MacHostAuthMiddleware. Returns db.ErrNotFound for both
// "no such id" and "id is revoked" — the middleware does not need to
// distinguish.
func (r *MacHostRepository) GetActiveHostByID(ctx context.Context, id uuid.UUID) (*MacHost, error) {
	row, err := r.queries.GetActiveMacHostByID(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get active mac_host: %w", err)
	}
	return convertDbMacHost(row), nil
}

// GetActiveHostByIDForUpdateTx is the tx-bound, row-locking variant
// of GetActiveHostByID. The underlying SELECT ... FOR UPDATE blocks
// any concurrent UPDATE (e.g. RevokeMacHost) on the same row until
// the caller's tx commits or rolls back. Used by IngestService's
// per-batch host-liveness check to close the race window between
// MacHostAuthMiddleware's read and the batch's commit. Returns
// db.ErrNotFound for both "no such id" and "id is revoked".
func (r *MacHostRepository) GetActiveHostByIDForUpdateTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
) (*MacHost, error) {
	row, err := db.New(tx).GetActiveMacHostByIDForUpdate(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get active mac_host for update: %w", err)
	}
	return convertDbMacHost(row), nil
}

// GetHost returns the row for id regardless of revocation status. Used
// by admin handlers + delete-cascade flow.
func (r *MacHostRepository) GetHost(ctx context.Context, id uuid.UUID) (*MacHost, error) {
	row, err := r.queries.GetMacHost(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get mac_host: %w", err)
	}
	return convertDbMacHost(row), nil
}

// ListActiveHosts returns all non-revoked hosts. UI consumer.
func (r *MacHostRepository) ListActiveHosts(ctx context.Context) ([]*MacHost, error) {
	rows, err := r.queries.ListActiveMacHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active mac_hosts: %w", err)
	}
	out := make([]*MacHost, 0, len(rows))
	for _, row := range rows {
		out = append(out, convertDbMacHost(row))
	}
	return out, nil
}

// RevokeHost marks the host revoked. Returns db.ErrNotFound when the
// host doesn't exist OR is already revoked — callers (the delete-host
// handler) can treat both as 404.
func (r *MacHostRepository) RevokeHost(ctx context.Context, id uuid.UUID) (*MacHost, error) {
	return r.revokeHost(ctx, r.queries, id)
}

// RevokeHostTx revokes a host inside the caller-supplied tx. Used by
// the delete-host handler so revoke + cascade-delete of cursor rows are
// atomic.
func (r *MacHostRepository) RevokeHostTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*MacHost, error) {
	return r.revokeHost(ctx, db.New(tx), id)
}

func (r *MacHostRepository) revokeHost(ctx context.Context, q db.Querier, id uuid.UUID) (*MacHost, error) {
	row, err := q.RevokeMacHost(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("revoke mac_host: %w", err)
	}
	return convertDbMacHost(row), nil
}

// HeartbeatPayload carries the heartbeat fields the handler hands to
// the repository. Permissions + SourceHealth are opaque JSONB blobs
// owned by the daemon; the Pi pass-through validates only that they're
// valid JSON.
type HeartbeatPayload struct {
	DaemonVersion   string
	ProtocolVersion int32
	Permissions     json.RawMessage
	SourceHealth    json.RawMessage
}

// UpdateHeartbeat refreshes last_heartbeat_at + the daemon-supplied
// fields on the host. Returns db.ErrNotFound if the host is revoked or
// missing (so the daemon's next request gets 401 via middleware).
func (r *MacHostRepository) UpdateHeartbeat(ctx context.Context, id uuid.UUID, payload HeartbeatPayload) (*MacHost, error) {
	permissions := payload.Permissions
	if len(permissions) == 0 {
		permissions = json.RawMessage("{}")
	}
	sourceHealth := payload.SourceHealth
	if len(sourceHealth) == 0 {
		sourceHealth = json.RawMessage("{}")
	}
	row, err := r.queries.UpdateMacHostHeartbeat(ctx, db.UpdateMacHostHeartbeatParams{
		ID:              uuidToPgUUID(id),
		DaemonVersion:   payload.DaemonVersion,
		ProtocolVersion: payload.ProtocolVersion,
		Permissions:     permissions,
		SourceHealth:    sourceHealth,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("update mac_host heartbeat: %w", err)
	}
	return convertDbMacHost(row), nil
}

// RotateAPIKeyTx atomically replaces the host's api_key_hash and bumps
// api_key_rotated_at. Returns db.ErrNotFound if the host is missing or
// already revoked. Tx-bound variant only — rotation is always inside
// the service's pair-token-consume tx for atomicity.
func (r *MacHostRepository) RotateAPIKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
	newAPIKeyHash string,
) (*MacHost, error) {
	row, err := db.New(tx).RotateMacHostAPIKey(ctx, db.RotateMacHostAPIKeyParams{
		ID:         uuidToPgUUID(id),
		ApiKeyHash: newAPIKeyHash,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("rotate mac_host api_key: %w", err)
	}
	return convertDbMacHost(row), nil
}

// MacHostCursorEpoch is the lock-and-read result returned by
// GetCursorEpochForCommit. The host is locked FOR UPDATE so concurrent
// commits for the same host serialize on this row.
type MacHostCursorEpoch struct {
	ID          uuid.UUID
	CursorEpoch int64
	Revoked     bool
}

// GetCursorEpochForCommitTx reads the host's cursor_epoch with a row
// lock inside the caller-supplied tx. Returns db.ErrNotFound when the
// host id doesn't exist.
func (r *MacHostRepository) GetCursorEpochForCommitTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*MacHostCursorEpoch, error) {
	row, err := db.New(tx).GetMacHostCursorEpoch(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get mac_host cursor_epoch: %w", err)
	}
	out := &MacHostCursorEpoch{
		CursorEpoch: row.CursorEpoch,
		Revoked:     row.ApiKeyRevokedAt.Valid,
	}
	if row.ID.Valid {
		out.ID = uuid.UUID(row.ID.Bytes)
	}
	return out, nil
}

// SeedHostForTest is a test-only helper that creates a host AND
// patches permissions / source_health in one shot — production code
// never calls this (the pairing flow + the daemon's heartbeat are
// the real writers). Surfaced on the repository so callers don't
// reach into the sqlc layer directly.
//
// Keeping this in the repository (not the service) is deliberate:
// the pairing-token flow is the only valid production path for
// creating a host, and the service refuses to bypass it. Tests need
// a fixture helper that bypasses the pairing flow, which is a
// legitimate repository-level operation.
func (r *MacHostRepository) SeedHostForTest(
	ctx context.Context,
	hostname, daemonVersion string,
	protocolVersion int32,
	apiKeyHash string,
	permissions, sourceHealth json.RawMessage,
) (*MacHost, error) {
	if len(permissions) == 0 {
		permissions = json.RawMessage("{}")
	}
	if len(sourceHealth) == 0 {
		sourceHealth = json.RawMessage("{}")
	}
	row, err := r.queries.SeedMacHost(ctx, db.SeedMacHostParams{
		Hostname:        hostname,
		DaemonVersion:   daemonVersion,
		ProtocolVersion: protocolVersion,
		ApiKeyHash:      apiKeyHash,
	})
	if err != nil {
		return nil, fmt.Errorf("seed mac_host: %w", err)
	}
	// Patch permissions + source_health via the heartbeat path — the
	// seed query only sets defaults for those JSONB columns.
	patched, err := r.queries.UpdateMacHostHeartbeat(ctx, db.UpdateMacHostHeartbeatParams{
		ID:              row.ID,
		DaemonVersion:   daemonVersion,
		ProtocolVersion: protocolVersion,
		Permissions:     permissions,
		SourceHealth:    sourceHealth,
	})
	if err != nil {
		return nil, fmt.Errorf("seed mac_host heartbeat patch: %w", err)
	}
	return convertDbMacHost(patched), nil
}

// SeedRevokedHostForTest is a test-only helper that inserts a host with
// api_key_revoked_at already set. Tests that only need a valid mac_host
// UUID as an FK target (e.g., messages_message.mac_host_id) should use
// this instead of SeedHostForTest: the singleton index
// idx_mac_host_singleton only constrains rows WHERE api_key_revoked_at
// IS NULL, so multiple revoked rows coexist freely. This isolates such
// tests from parallel packages that exercise the real pairing flow.
func (r *MacHostRepository) SeedRevokedHostForTest(
	ctx context.Context,
	hostname, daemonVersion string,
	protocolVersion int32,
	apiKeyHash string,
) (*MacHost, error) {
	row, err := r.queries.SeedRevokedMacHost(ctx, db.SeedRevokedMacHostParams{
		Hostname:        hostname,
		DaemonVersion:   daemonVersion,
		ProtocolVersion: protocolVersion,
		ApiKeyHash:      apiKeyHash,
	})
	if err != nil {
		return nil, fmt.Errorf("seed revoked mac_host: %w", err)
	}
	return convertDbMacHost(row), nil
}

// MacHostPairingTokenRepository handles pairing-token persistence.
type MacHostPairingTokenRepository struct {
	queries db.Querier
}

// NewMacHostPairingTokenRepository constructs the pairing-token repo.
func NewMacHostPairingTokenRepository(queries db.Querier) *MacHostPairingTokenRepository {
	return &MacHostPairingTokenRepository{queries: queries}
}

// CreateToken inserts a pairing token row keyed by the SHA-256 of the
// plaintext token. The plaintext is never persisted; callers pass the
// hash they computed at mint time.
func (r *MacHostPairingTokenRepository) CreateToken(ctx context.Context, tokenHash string, expiresAt time.Time) (*MacHostPairingToken, error) {
	row, err := r.queries.CreatePairingToken(ctx, db.CreatePairingTokenParams{
		TokenHash: tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("create pairing token: %w", err)
	}
	return convertDbPairingToken(row), nil
}

// GetTokenByHashForUpdateTx fetches a token by hash and locks the row.
// Used inside the consume tx so concurrent consume calls for the same
// token serialize. Returns db.ErrNotFound for an unknown hash.
func (r *MacHostPairingTokenRepository) GetTokenByHashForUpdateTx(ctx context.Context, tx pgx.Tx, tokenHash string) (*MacHostPairingToken, error) {
	row, err := db.New(tx).GetPairingTokenByHashForUpdate(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get pairing token: %w", err)
	}
	return convertDbPairingToken(row), nil
}

// MarkConsumedTx marks the token as consumed by the given host. Caller
// must run this inside the same tx as the lock-read so the consume is
// atomic.
func (r *MacHostPairingTokenRepository) MarkConsumedTx(ctx context.Context, tx pgx.Tx, tokenID, consumedHostID uuid.UUID) (*MacHostPairingToken, error) {
	row, err := db.New(tx).MarkPairingTokenConsumed(ctx, db.MarkPairingTokenConsumedParams{
		ID:             uuidToPgUUID(tokenID),
		ConsumedHostID: uuidToPgUUID(consumedHostID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("mark pairing token consumed: %w", err)
	}
	return convertDbPairingToken(row), nil
}

// DeleteExpiredTokens removes unconsumed tokens whose expires_at is in
// the past. Returns the number of rows deleted. Called by the
// PairingTokenJanitorWorker.
func (r *MacHostPairingTokenRepository) DeleteExpiredTokens(ctx context.Context) (int64, error) {
	n, err := r.queries.DeleteExpiredPairingTokens(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete expired pairing tokens: %w", err)
	}
	return n, nil
}

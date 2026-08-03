package whatsapp

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// NewDeviceContainer wraps the application's pgx pool as the whatsmeow device
// store and applies whatsmeow's own migrations.
//
// The returned container borrows the shared pool through a *sql.DB wrapper.
// That wrapper must NOT be closed independently: closing it does not close the
// pool (pgx's OpenDBFromPool documents this), but it would break the container.
// OpenDBFromPool sets MaxIdleConns(0) so the wrapper does not hold connections
// away from direct pool users.
//
// whatsmeow owns its whatsmeow_* tables and its own whatsmeow_version migration
// table, entirely outside golang-migrate. NewWithDB does not auto-upgrade, so
// the Upgrade call is explicit; it is idempotent and version-tracked.
func NewDeviceContainer(ctx context.Context, pool *pgxpool.Pool, log waLog.Logger) (*sqlstore.Container, error) {
	if pool == nil {
		return nil, fmt.Errorf("whatsapp device store: nil database pool")
	}

	// "pgx" is one of the dialect strings whatsmeow's dbutil.ParseDialect
	// accepts for Postgres (alongside any "postgres"-prefixed string).
	container := sqlstore.NewWithDB(stdlib.OpenDBFromPool(pool), "pgx", log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("whatsapp device store: %w", err)
	}
	return container, nil
}

// LoadOrCreateDevice returns the stored device when one exists (already paired),
// or a fresh unpaired device otherwise. The bool reports whether a paired device
// was found — a device carries a non-nil ID only once pairing completed, which
// is what makes the linked session survive a restart.
func LoadOrCreateDevice(ctx context.Context, c *sqlstore.Container) (*store.Device, bool, error) {
	if c == nil {
		return nil, false, fmt.Errorf("whatsapp device store: nil container")
	}
	device, err := c.GetFirstDevice(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("whatsapp load device: %w", err)
	}
	return device, device != nil && device.ID != nil, nil
}

// LoadDeviceByJID resolves one specific stored device.
//
// It exists because GetFirstDevice returns the first row of an UNORDERED scan
// and the library documents it as being for stores that hold a single session.
// A re-pair whose delete of the replaced device failed leaves two rows, and
// first-row roulette would then resume an arbitrary one. Once the linked JID is
// known, resolution is by JID and the outcome is deterministic no matter how
// many rows survived.
//
// The bool reports whether that device was found; a miss is not an error (the
// caller falls back to the ordinary load).
func LoadDeviceByJID(ctx context.Context, c *sqlstore.Container, jid types.JID) (*store.Device, bool, error) {
	if c == nil {
		return nil, false, fmt.Errorf("whatsapp device store: nil container")
	}
	device, err := c.GetDevice(ctx, jid)
	if err != nil {
		return nil, false, fmt.Errorf("whatsapp load device %s: %w", jid, err)
	}
	if device == nil || device.ID == nil {
		return nil, false, nil
	}
	return device, true, nil
}

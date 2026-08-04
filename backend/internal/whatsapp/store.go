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

// deviceResolution reports what resolveLinkedDevice decided about the store it
// read, alongside the device itself.
type deviceResolution struct {
	// paired reports whether a stored, linked device was resumed.
	paired bool
	// jid is the resumed device's own JID.
	jid *types.JID
	// extraRows records that the store held rows besides the one resumed. A
	// selector hit over a multi-row store is a DEGRADED store, not a clean
	// resume: the remedy is a forced disconnect, which purges the enumerated
	// set.
	extraRows bool
	// healSelector records that the selector was absent or stale and should be
	// re-persisted as this device's JID.
	healSelector bool
}

// resolveLinkedDevice picks the device to resume, and REFUSES rather than guess.
//
// The invariant is one sentence: it refuses if and only if the store holds two
// or more devices and none of them is the one the selector names. A selector is
// a tie-breaker; with no tie there is nothing for it to break, so a stale or
// absent selector over a single row HEALS rather than refuses.
//
// This replaces the library's GetFirstDevice fallback, which returns the first
// row of an unordered GetAllDevices and is documented as being only for stores
// holding a single session. Resuming a stale device is not a bug that has been
// made unlikely here; it is a state this function has no branch for.
func resolveLinkedDevice(ctx context.Context, c *sqlstore.Container, linked *types.JID) (*store.Device, deviceResolution, error) {
	if c == nil {
		return nil, deviceResolution{}, fmt.Errorf("whatsapp device store: nil container")
	}

	devices, err := c.GetAllDevices(ctx)
	if err != nil {
		return nil, deviceResolution{}, fmt.Errorf("whatsapp load devices: %w", err)
	}

	stored := make([]*store.Device, 0, len(devices))
	for _, d := range devices {
		if d != nil && d.ID != nil {
			stored = append(stored, d)
		}
	}

	if linked != nil {
		for _, d := range stored {
			if d.ID.ToNonAD() == linked.ToNonAD() {
				jid := *d.ID
				return d, deviceResolution{paired: true, jid: &jid, extraRows: len(stored) > 1}, nil
			}
		}
	}

	switch len(stored) {
	case 0:
		// Nothing stored: a fresh unpaired device, which reports not_paired.
		return c.NewDevice(), deviceResolution{}, nil
	case 1:
		// The healing row. A selector that could not be written after a
		// successful replacement delete leaves exactly this state, and refusing
		// it would strand a perfectly good single device.
		d := stored[0]
		jid := *d.ID
		return d, deviceResolution{paired: true, jid: &jid, healSelector: true}, nil
	default:
		return nil, deviceResolution{}, fmt.Errorf("%w: %d stored devices and no matching selector", ErrDeviceStoreAmbiguous, len(stored))
	}
}

// listDevices enumerates the stored device JIDs. It is the NON-destructive
// stage of the staged purge, and it goes through the container so it needs
// neither a client nor a device row of its own.
func listDevices(ctx context.Context, c *sqlstore.Container) ([]types.JID, error) {
	if c == nil {
		return nil, nil
	}
	devices, err := c.GetAllDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp list devices: %w", err)
	}
	out := make([]types.JID, 0, len(devices))
	for _, d := range devices {
		if d != nil && d.ID != nil {
			out = append(out, *d.ID)
		}
	}
	return out, nil
}

// deleteDevices removes EXACTLY the supplied JIDs — never "whatever is in the
// store now". A row created after the enumeration is left alone; the caller
// detects it and reports a supersession.
func deleteDevices(ctx context.Context, c *sqlstore.Container, jids []types.JID) error {
	if c == nil || len(jids) == 0 {
		return nil
	}
	for _, jid := range jids {
		if err := deleteDeviceByJID(ctx, c, jid); err != nil {
			return err
		}
	}
	return nil
}

// deleteDeviceByJID removes one stored device. A device that is already gone is
// not an error: a purge enumerates before it deletes, so a row that vanished in
// between is a row already in the state the caller wanted.
func deleteDeviceByJID(ctx context.Context, c *sqlstore.Container, jid types.JID) error {
	if c == nil {
		return nil
	}
	device, err := c.GetDevice(ctx, jid)
	if err != nil {
		return fmt.Errorf("whatsapp load device %s: %w", jid, err)
	}
	if device == nil || device.ID == nil {
		return nil
	}
	if err := c.DeleteDevice(ctx, device); err != nil {
		return fmt.Errorf("whatsapp delete device %s: %w", jid, err)
	}
	return nil
}

// LoadDeviceByJID resolves one specific stored device.
//
// It exists because GetFirstDevice returns the first row of an UNORDERED scan
// and the library documents it as being for stores that hold a single session.
// The bool reports whether that device was found; a miss is not an error.
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

package db

import "errors"

// Common database errors
var (
	ErrNotFound = errors.New("record not found")
	// ErrDuplicate indicates an ON CONFLICT DO NOTHING collision — the row
	// the caller tried to insert already exists. Callers typically treat
	// this as an idempotent no-op.
	ErrDuplicate = errors.New("duplicate record")
)

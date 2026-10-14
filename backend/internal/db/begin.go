package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Begin starts a transaction on the connection (or a savepoint inside the
// transaction) the Queries wraps, so a repository that holds only Queries can
// run a multi-statement write atomically.
func (q *Queries) Begin(ctx context.Context) (pgx.Tx, error) {
	b, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("db: connection cannot begin a transaction")
	}
	return b.Begin(ctx)
}

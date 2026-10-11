package repository

import "errors"

// errNilTx is returned by every *Tx repository method when the caller passes a
// nil transaction. A nil tx must never fall back to the pool: the query would
// then run outside the caller's transaction.
var errNilTx = errors.New("repository: nil transaction")

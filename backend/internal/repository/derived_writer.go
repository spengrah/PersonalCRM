package repository

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5"
)

// DerivedWriter names the owner a transaction is authorized to write derived
// contact columns for. The reject_unauthorized_derived_contact_write trigger
// (migration 079) reads it from the session GUC crm.derived_writer and
// authorizes a DISJOINT column set per value: a knowledge-cache transaction
// cannot move a cadence column and a cadence transaction cannot move a
// knowledge column. A single truthy "authorized" flag would satisfy a
// no-declaration rejection test while destroying that ownership split.
//
// The trigger compares with exact string equality — no trimming, no case
// folding. A named string type does NOT close the value set in Go
// (DerivedWriter("bogus") is legal), so SetDerivedWriterTx validates against
// the two constants below rather than relying on the type. Without that check
// a typo would reach the database as an unknown owner and surface as a
// confusing trigger rejection at the UPDATE instead of a clear error at the
// declaration.
type DerivedWriter string

const (
	// DerivedWriterCadence authorizes last_contacted, last_interaction_at,
	// last_outreach_at, last_response_at, and contact_by. Owner:
	// CadenceUpdater.applyTx and the delete-rollback recompute.
	DerivedWriterCadence DerivedWriter = "cadence"

	// DerivedWriterKnowledgeCache authorizes location, birthday, and how_met.
	// Owner: KnowledgeCacheUpdater.RefreshTx via the three cache wrappers.
	DerivedWriterKnowledgeCache DerivedWriter = "knowledge_cache"
)

// SetDerivedWriterTx declares the calling transaction's derived-column owner.
// Call it on the same tx as the UPDATE and before it. The declaration expires
// with the transaction; it is not savepoint-scoped, so a ROLLBACK TO SAVEPOINT
// preserves a declaration made before the savepoint and discards one made
// inside it.
//
// This function is the Go-side authorization door. It does not and cannot
// constrain which Go code opens it — that remains the job of the AST guard in
// backend/tests/sole_writer_static_test.go and the grep guard in
// scripts/check-cadence-sole-writer.sh. What the trigger adds is enforcement
// against writers those two cannot see: psql sessions, admin SQL, and any
// future non-Go client.
func SetDerivedWriterTx(ctx context.Context, tx pgx.Tx, owner DerivedWriter) error {
	// Owner first, tx second: the owner check needs no transaction, so both
	// guard clauses stay reachable from a DB-free unit test.
	switch owner {
	case DerivedWriterCadence, DerivedWriterKnowledgeCache:
	default:
		return fmt.Errorf("set derived writer: unknown owner %q", owner)
	}
	if tx == nil {
		return fmt.Errorf("set derived writer %q: nil tx", owner)
	}
	if err := db.New(tx).SetDerivedWriter(ctx, string(owner)); err != nil {
		return fmt.Errorf("set derived writer %q: %w", owner, err)
	}
	return nil
}

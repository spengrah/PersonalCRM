package repository

import (
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyTransferConflict pins the recoverability decision
// TransferAutomatedTasksForMergeTx makes on a failed transfer: recoverable
// only for a unique_violation on exactly one of the three indexes the
// transfer query's own NOT EXISTS clauses guard. Anything else — an
// unrelated constraint, or a non-pg error entirely — must propagate rather
// than be silently absorbed into "the transfer lost a race".
func TestClassifyTransferConflict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		err             error
		wantRecoverable bool
	}{
		{
			name:            "unique_contact_provider_cadence violation is recoverable",
			err:             &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "unique_contact_provider_cadence"},
			wantRecoverable: true,
		},
		{
			name:            "idx_contact_task_followup_unique_live violation is recoverable",
			err:             &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "idx_contact_task_followup_unique_live"},
			wantRecoverable: true,
		},
		{
			name:            "idx_contact_task_followup_idempotency violation is recoverable",
			err:             &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "idx_contact_task_followup_idempotency"},
			wantRecoverable: true,
		},
		{
			name:            "unique_violation on an unrelated constraint propagates",
			err:             &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "idx_note_contact_notepad_unique"},
			wantRecoverable: false,
		},
		{
			name:            "a non-23505 pg error propagates even on a guarded constraint name",
			err:             &pgconn.PgError{Code: pgerrcode.NotNullViolation, ConstraintName: "unique_contact_provider_cadence"},
			wantRecoverable: false,
		},
		{
			name:            "a non-pg error propagates",
			err:             errors.New("connection reset"),
			wantRecoverable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pgErr, recoverable := classifyTransferConflict(tc.err)
			assert.Equal(t, tc.wantRecoverable, recoverable)
			if tc.wantRecoverable {
				require.NotNil(t, pgErr)
			}
		})
	}
}

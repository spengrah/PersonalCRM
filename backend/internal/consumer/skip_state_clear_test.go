package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestSkipStateClear_BuildInteractionWrite(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		direction string
		source    string
		want      bool
	}{
		{"inbound manual", repository.InteractionDirectionInbound, repository.InteractionSourceManual, true},
		{"inbound todoist", repository.InteractionDirectionInbound, repository.InteractionSourceTodoist, true},
		{"mutual manual", repository.InteractionDirectionMutual, repository.InteractionSourceManual, true},
		{"mutual todoist", repository.InteractionDirectionMutual, repository.InteractionSourceTodoist, true},
		{"outbound manual", repository.InteractionDirectionOutbound, repository.InteractionSourceManual, false},
		{"outbound todoist", repository.InteractionDirectionOutbound, repository.InteractionSourceTodoist, false},
		{"unknown", "unknown", repository.InteractionSourceManual, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := h.buildInteractionWrite(uuid.New(), tc.direction, tc.source, now, repository.ContactCadenceFields{}, "monthly")
			require.Equal(t, tc.want, req.ClearSkipState)
		})
	}
}

type skipStateErrorTx struct{ pgx.Tx }

func (*skipStateErrorTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("skip state test tx")
}

func TestSkipStateClear_ApplyTxShortCircuit(t *testing.T) {
	h, _, _ := newUnitUpdater(CadenceModeCutover)
	req := cadenceWriteRequest{ContactID: uuid.New(), Branch: repository.CadenceBranchForward, ClearSkipState: true}
	var tx *skipStateErrorTx
	err := h.applyTx(context.Background(), tx, req)
	require.Error(t, err, "clearing skip state must reach the writer rather than short-circuit")

	req.ClearSkipState = false
	require.NoError(t, h.applyTx(context.Background(), tx, req))
}

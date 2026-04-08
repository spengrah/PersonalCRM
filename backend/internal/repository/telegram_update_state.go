package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5"
)

// TelegramUpdateState represents the MTProto update state for gap recovery
type TelegramUpdateState struct {
	UserID int64
	Pts    int32
	Qts    int32
	Seq    int32
	Date   int32
}

// TelegramChannelState represents channel-specific pts state
type TelegramChannelState struct {
	ChannelID  int64
	Pts        int32
	AccessHash int64
}

// TelegramUpdateStateRepository handles telegram update state and channel state persistence
type TelegramUpdateStateRepository struct {
	queries db.Querier
}

// NewTelegramUpdateStateRepository creates a new telegram update state repository
func NewTelegramUpdateStateRepository(queries db.Querier) *TelegramUpdateStateRepository {
	return &TelegramUpdateStateRepository{queries: queries}
}

// GetState retrieves the update state for a user
func (r *TelegramUpdateStateRepository) GetState(ctx context.Context, userID int64) (*TelegramUpdateState, error) {
	dbState, err := r.queries.GetTelegramUpdateState(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return &TelegramUpdateState{
		UserID: dbState.UserID,
		Pts:    dbState.Pts,
		Qts:    dbState.Qts,
		Seq:    dbState.Seq,
		Date:   dbState.Date,
	}, nil
}

// UpsertState creates or updates the full update state
func (r *TelegramUpdateStateRepository) UpsertState(ctx context.Context, state TelegramUpdateState) (*TelegramUpdateState, error) {
	dbState, err := r.queries.UpsertTelegramUpdateState(ctx, db.UpsertTelegramUpdateStateParams{
		UserID: state.UserID,
		Pts:    state.Pts,
		Qts:    state.Qts,
		Seq:    state.Seq,
		Date:   state.Date,
	})
	if err != nil {
		return nil, err
	}
	return &TelegramUpdateState{
		UserID: dbState.UserID,
		Pts:    dbState.Pts,
		Qts:    dbState.Qts,
		Seq:    dbState.Seq,
		Date:   dbState.Date,
	}, nil
}

// SetPts updates just the pts value
func (r *TelegramUpdateStateRepository) SetPts(ctx context.Context, userID int64, pts int32) error {
	return r.queries.SetTelegramPts(ctx, db.SetTelegramPtsParams{Pts: pts, UserID: userID})
}

// SetQts updates just the qts value
func (r *TelegramUpdateStateRepository) SetQts(ctx context.Context, userID int64, qts int32) error {
	return r.queries.SetTelegramQts(ctx, db.SetTelegramQtsParams{Qts: qts, UserID: userID})
}

// SetSeq updates just the seq value
func (r *TelegramUpdateStateRepository) SetSeq(ctx context.Context, userID int64, seq int32) error {
	return r.queries.SetTelegramSeq(ctx, db.SetTelegramSeqParams{Seq: seq, UserID: userID})
}

// SetDate updates just the date value
func (r *TelegramUpdateStateRepository) SetDate(ctx context.Context, userID int64, date int32) error {
	return r.queries.SetTelegramDate(ctx, db.SetTelegramDateParams{Date: date, UserID: userID})
}

// DeleteState deletes the update state for a user
func (r *TelegramUpdateStateRepository) DeleteState(ctx context.Context, userID int64) error {
	return r.queries.DeleteTelegramUpdateState(ctx, userID)
}

// GetChannelState retrieves channel-specific state
func (r *TelegramUpdateStateRepository) GetChannelState(ctx context.Context, channelID int64) (*TelegramChannelState, error) {
	dbState, err := r.queries.GetTelegramChannelState(ctx, channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return &TelegramChannelState{
		ChannelID:  dbState.ChannelID,
		Pts:        dbState.Pts,
		AccessHash: dbState.AccessHash,
	}, nil
}

// UpsertChannelState creates or updates channel state
func (r *TelegramUpdateStateRepository) UpsertChannelState(ctx context.Context, state TelegramChannelState) (*TelegramChannelState, error) {
	dbState, err := r.queries.UpsertTelegramChannelState(ctx, db.UpsertTelegramChannelStateParams{
		ChannelID:  state.ChannelID,
		Pts:        state.Pts,
		AccessHash: state.AccessHash,
	})
	if err != nil {
		return nil, err
	}
	return &TelegramChannelState{
		ChannelID:  dbState.ChannelID,
		Pts:        dbState.Pts,
		AccessHash: dbState.AccessHash,
	}, nil
}

// UpsertChannelAccessHash upserts only the access hash, preserving existing pts.
func (r *TelegramUpdateStateRepository) UpsertChannelAccessHash(ctx context.Context, channelID int64, accessHash int64) (*TelegramChannelState, error) {
	dbState, err := r.queries.UpsertTelegramChannelAccessHash(ctx, db.UpsertTelegramChannelAccessHashParams{
		ChannelID:  channelID,
		AccessHash: accessHash,
	})
	if err != nil {
		return nil, err
	}
	return &TelegramChannelState{
		ChannelID:  dbState.ChannelID,
		Pts:        dbState.Pts,
		AccessHash: dbState.AccessHash,
	}, nil
}

// SetChannelPts updates just the pts for a channel
func (r *TelegramUpdateStateRepository) SetChannelPts(ctx context.Context, channelID int64, pts int32) error {
	return r.queries.SetTelegramChannelPts(ctx, db.SetTelegramChannelPtsParams{Pts: pts, ChannelID: channelID})
}

// ListChannelStates retrieves all channel states
func (r *TelegramUpdateStateRepository) ListChannelStates(ctx context.Context) ([]TelegramChannelState, error) {
	dbStates, err := r.queries.ListTelegramChannelStates(ctx)
	if err != nil {
		return nil, err
	}
	states := make([]TelegramChannelState, len(dbStates))
	for i, s := range dbStates {
		states[i] = TelegramChannelState{
			ChannelID:  s.ChannelID,
			Pts:        s.Pts,
			AccessHash: s.AccessHash,
		}
	}
	return states, nil
}

// DeleteChannelState deletes a channel state
func (r *TelegramUpdateStateRepository) DeleteChannelState(ctx context.Context, channelID int64) error {
	return r.queries.DeleteTelegramChannelState(ctx, channelID)
}

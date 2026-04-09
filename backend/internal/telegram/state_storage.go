package telegram

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gotd/td/telegram/updates"
)

// Compile-time interface checks.
var (
	_ updates.StateStorage        = (*PostgresStateStorage)(nil)
	_ updates.ChannelAccessHasher = (*PostgresChannelHasher)(nil)
)

// PostgresStateStorage implements updates.StateStorage backed by PostgreSQL.
type PostgresStateStorage struct {
	repo *repository.TelegramUpdateStateRepository
}

// NewPostgresStateStorage creates a new state storage.
func NewPostgresStateStorage(repo *repository.TelegramUpdateStateRepository) *PostgresStateStorage {
	return &PostgresStateStorage{repo: repo}
}

// GetState implements updates.StateStorage.
func (s *PostgresStateStorage) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	state, err := s.repo.GetState(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return updates.State{}, false, nil
		}
		return updates.State{}, false, fmt.Errorf("get state: %w", err)
	}
	return updates.State{
		Pts:  int(state.Pts),
		Qts:  int(state.Qts),
		Seq:  int(state.Seq),
		Date: int(state.Date),
	}, true, nil
}

// SetState implements updates.StateStorage.
func (s *PostgresStateStorage) SetState(ctx context.Context, userID int64, state updates.State) error {
	_, err := s.repo.UpsertState(ctx, repository.TelegramUpdateState{
		UserID: userID,
		Pts:    int32(state.Pts),
		Qts:    int32(state.Qts),
		Seq:    int32(state.Seq),
		Date:   int32(state.Date),
	})
	return err
}

// SetPts implements updates.StateStorage.
func (s *PostgresStateStorage) SetPts(ctx context.Context, userID int64, pts int) error {
	return s.repo.SetPts(ctx, userID, int32(pts))
}

// SetQts implements updates.StateStorage.
func (s *PostgresStateStorage) SetQts(ctx context.Context, userID int64, qts int) error {
	return s.repo.SetQts(ctx, userID, int32(qts))
}

// SetDate implements updates.StateStorage.
func (s *PostgresStateStorage) SetDate(ctx context.Context, userID int64, date int) error {
	return s.repo.SetDate(ctx, userID, int32(date))
}

// SetSeq implements updates.StateStorage.
func (s *PostgresStateStorage) SetSeq(ctx context.Context, userID int64, seq int) error {
	return s.repo.SetSeq(ctx, userID, int32(seq))
}

// SetDateSeq implements updates.StateStorage.
func (s *PostgresStateStorage) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	// No single query for this — update both individually.
	if err := s.repo.SetDate(ctx, userID, int32(date)); err != nil {
		return err
	}
	return s.repo.SetSeq(ctx, userID, int32(seq))
}

// GetChannelPts implements updates.StateStorage.
func (s *PostgresStateStorage) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	state, err := s.repo.GetChannelState(ctx, channelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get channel pts: %w", err)
	}
	return int(state.Pts), true, nil
}

// SetChannelPts implements updates.StateStorage.
func (s *PostgresStateStorage) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	return s.repo.SetChannelPts(ctx, channelID, int32(pts))
}

// ForEachChannels implements updates.StateStorage.
func (s *PostgresStateStorage) ForEachChannels(ctx context.Context, userID int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	states, err := s.repo.ListChannelStates(ctx)
	if err != nil {
		return fmt.Errorf("list channel states: %w", err)
	}
	for _, state := range states {
		if err := f(ctx, state.ChannelID, int(state.Pts)); err != nil {
			return err
		}
	}
	return nil
}

// PostgresChannelHasher implements updates.ChannelAccessHasher backed by PostgreSQL.
type PostgresChannelHasher struct {
	repo *repository.TelegramUpdateStateRepository
}

// NewPostgresChannelHasher creates a new channel access hasher.
func NewPostgresChannelHasher(repo *repository.TelegramUpdateStateRepository) *PostgresChannelHasher {
	return &PostgresChannelHasher{repo: repo}
}

// GetChannelAccessHash implements updates.ChannelAccessHasher.
func (h *PostgresChannelHasher) GetChannelAccessHash(ctx context.Context, userID, channelID int64) (int64, bool, error) {
	state, err := h.repo.GetChannelState(ctx, channelID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get channel access hash: %w", err)
	}
	return state.AccessHash, true, nil
}

// SetChannelAccessHash implements updates.ChannelAccessHasher.
// Uses a dedicated query that only updates access_hash, preserving existing pts.
func (h *PostgresChannelHasher) SetChannelAccessHash(ctx context.Context, userID, channelID, accessHash int64) error {
	_, err := h.repo.UpsertChannelAccessHash(ctx, channelID, accessHash)
	return err
}

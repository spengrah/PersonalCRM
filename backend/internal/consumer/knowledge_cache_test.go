package consumer

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knowledgeCacheStubs implements the three KnowledgeCacheUpdater dependencies for
// unit tests. The tx argument is ignored (these tests don't touch a real DB).
type knowledgeCacheStubs struct {
	current    *repository.Assertion
	currentErr error
	node       *repository.Node

	gotLocation *string
	gotBirthday *time.Time
	gotHowMet   *string
	wroteLoc    bool
	wroteBday   bool
	wroteHow    bool
}

func (s *knowledgeCacheStubs) GetCurrentAcceptedTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ string, _ time.Time) (*repository.Assertion, error) {
	if s.currentErr != nil {
		return nil, s.currentErr
	}
	return s.current, nil
}

func (s *knowledgeCacheStubs) GetNodeTx(_ context.Context, _ pgx.Tx, _ uuid.UUID) (*repository.Node, error) {
	return s.node, nil
}

func (s *knowledgeCacheStubs) UpdateContactLocationCacheTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, location *string) error {
	s.gotLocation = location
	s.wroteLoc = true
	return nil
}

func (s *knowledgeCacheStubs) UpdateContactBirthdayCacheTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, birthday *time.Time) error {
	s.gotBirthday = birthday
	s.wroteBday = true
	return nil
}

func (s *knowledgeCacheStubs) UpdateContactHowMetCacheTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, howMet *string) error {
	s.gotHowMet = howMet
	s.wroteHow = true
	return nil
}

func strPtr(s string) *string { return &s }

func TestKnowledgeCacheUpdater_HandleEvent_NoOpsForNonCutoverPredicate(t *testing.T) {
	t.Parallel()
	stubs := &knowledgeCacheStubs{}
	u := NewKnowledgeCacheUpdater(stubs, stubs, stubs)

	payload, err := events.Marshal(events.KindAssertionAccepted, events.AssertionEventPayload{
		Version:       1,
		AssertionID:   uuid.New(),
		SubjectNodeID: uuid.New(),
		PredicateKey:  "works_at", // not a cache predicate
	})
	require.NoError(t, err)
	env := &events.Envelope{Source: "assertion", Kind: events.KindAssertionAccepted, Payload: payload}

	require.NoError(t, u.HandleEvent(context.Background(), nil, env))
	assert.False(t, stubs.wroteLoc, "non-cutover predicate must not touch the cache")
	assert.False(t, stubs.wroteBday)
	assert.False(t, stubs.wroteHow)
}

func TestKnowledgeCacheUpdater_RefreshTx_LivesInWritesPlaceLabel(t *testing.T) {
	t.Parallel()
	placeID := uuid.New()
	stubs := &knowledgeCacheStubs{
		current: &repository.Assertion{ID: uuid.New(), PredicateKey: repository.PredicateLivesIn, ObjectNodeID: &placeID},
		node:    &repository.Node{ID: placeID, CanonicalLabel: "Portland"},
	}
	u := NewKnowledgeCacheUpdater(stubs, stubs, stubs)

	require.NoError(t, u.RefreshTx(context.Background(), nil, uuid.New(), repository.PredicateLivesIn))
	require.True(t, stubs.wroteLoc)
	require.NotNil(t, stubs.gotLocation)
	assert.Equal(t, "Portland", *stubs.gotLocation, "cache gets the place node's canonical_label")
}

func TestKnowledgeCacheUpdater_RefreshTx_NoCurrentClearsToNull(t *testing.T) {
	t.Parallel()
	stubs := &knowledgeCacheStubs{currentErr: db.ErrNotFound}
	u := NewKnowledgeCacheUpdater(stubs, stubs, stubs)

	require.NoError(t, u.RefreshTx(context.Background(), nil, uuid.New(), repository.PredicateBirthday))
	require.True(t, stubs.wroteBday)
	assert.Nil(t, stubs.gotBirthday, "no current accepted birthday clears the cache to NULL")
}

func TestKnowledgeCacheUpdater_RefreshTx_BirthdayAndHowMet(t *testing.T) {
	t.Parallel()
	bday := time.Date(1991, 5, 2, 0, 0, 0, 0, time.UTC)
	stubsB := &knowledgeCacheStubs{current: &repository.Assertion{PredicateKey: repository.PredicateBirthday, ValueDate: &bday}}
	uB := NewKnowledgeCacheUpdater(stubsB, stubsB, stubsB)
	require.NoError(t, uB.RefreshTx(context.Background(), nil, uuid.New(), repository.PredicateBirthday))
	require.NotNil(t, stubsB.gotBirthday)
	assert.Equal(t, bday, *stubsB.gotBirthday)

	stubsH := &knowledgeCacheStubs{current: &repository.Assertion{PredicateKey: repository.PredicateHowMet, ValueText: strPtr("at a wedding")}}
	uH := NewKnowledgeCacheUpdater(stubsH, stubsH, stubsH)
	require.NoError(t, uH.RefreshTx(context.Background(), nil, uuid.New(), repository.PredicateHowMet))
	require.NotNil(t, stubsH.gotHowMet)
	assert.Equal(t, "at a wedding", *stubsH.gotHowMet)
}

package messages

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type stubStrandedLister struct {
	rows      []repository.MessagesMessage
	listErr   error
	updateErr error
	updates   []repository.UpdateMatchedContactParams
}

func (s *stubStrandedLister) ListStranded(_ context.Context) ([]repository.MessagesMessage, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows, nil
}

func (s *stubStrandedLister) UpdateMatchedContact(_ context.Context, params repository.UpdateMatchedContactParams) error {
	s.updates = append(s.updates, params)
	return s.updateErr
}

type stubAdminMatcher struct {
	// resultsByHandle maps a peer handle to its match result. Missing
	// handles return (nil match, nil err) which the rematch code treats
	// as "still unmatched".
	resultsByHandle map[string]*service.MatchResult
	errByHandle     map[string]error
}

func (s *stubAdminMatcher) MatchOrCreate(_ context.Context, req service.MatchRequest) (*service.MatchResult, error) {
	if s.errByHandle != nil {
		if err, ok := s.errByHandle[req.RawIdentifier]; ok {
			return nil, err
		}
	}
	if r, ok := s.resultsByHandle[req.RawIdentifier]; ok {
		return r, nil
	}
	// Default: still unmatched.
	return &service.MatchResult{ContactID: nil, MatchType: repository.MatchTypeUnmatched}, nil
}

type stubAdminInserter struct {
	calls []consumerjobs.MessagingAggregateForContactArgs
	err   error
}

func (s *stubAdminInserter) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if a, ok := args.(consumerjobs.MessagingAggregateForContactArgs); ok {
		s.calls = append(s.calls, a)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &rivertype.JobInsertResult{}, nil
}

// TestRematchStranded_HappyPath_MatchesAndEnqueues asserts a stranded
// row gets matched, updated, and enqueued.
func TestRematchStranded_HappyPath_MatchesAndEnqueues(t *testing.T) {
	contactID := uuid.New()
	rowID := uuid.New()
	lister := &stubStrandedLister{
		rows: []repository.MessagesMessage{
			{ID: rowID, Guid: "guid-1", PeerHandle: "+15551234567"},
		},
	}
	matcher := &stubAdminMatcher{
		resultsByHandle: map[string]*service.MatchResult{
			"+15551234567": {
				ContactID: &contactID,
				MatchType: repository.MatchTypeExact,
				Identity:  &repository.ExternalIdentity{Identifier: "+15551234567"},
			},
		},
	}
	inserter := &stubAdminInserter{}

	res, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages:    lister,
		Identity:    matcher,
		RiverClient: inserter,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned)
	require.Equal(t, 1, res.Matched)
	require.Equal(t, 0, res.StillStranded)
	require.Equal(t, 1, res.Enqueued)
	require.Equal(t, 0, res.Errors)

	require.Len(t, lister.updates, 1)
	require.Equal(t, rowID, lister.updates[0].ID)
	require.Equal(t, contactID, lister.updates[0].MatchedContactID)
	require.Equal(t, "+15551234567", lister.updates[0].PeerNormalized)

	require.Len(t, inserter.calls, 1)
	require.Equal(t, contactID, inserter.calls[0].ContactID)
	require.Equal(t, "messages", inserter.calls[0].Source)
}

// TestRematchStranded_StillUnmatched_NoUpdateOrEnqueue verifies a
// stranded row that still has no contact_method match produces no
// updates or enqueues.
func TestRematchStranded_StillUnmatched_NoUpdateOrEnqueue(t *testing.T) {
	lister := &stubStrandedLister{
		rows: []repository.MessagesMessage{
			{ID: uuid.New(), Guid: "guid-2", PeerHandle: "+15559999999"},
		},
	}
	matcher := &stubAdminMatcher{} // no entries → unmatched
	inserter := &stubAdminInserter{}

	res, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages:    lister,
		Identity:    matcher,
		RiverClient: inserter,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned)
	require.Equal(t, 0, res.Matched)
	require.Equal(t, 1, res.StillStranded)
	require.Equal(t, 0, res.Enqueued)
	require.Empty(t, lister.updates)
	require.Empty(t, inserter.calls)
}

// TestRematchStranded_DuplicateContact_EnqueuesOnce verifies that when
// multiple stranded rows match the same contact, only ONE River job
// is enqueued for that contact.
func TestRematchStranded_DuplicateContact_EnqueuesOnce(t *testing.T) {
	contactID := uuid.New()
	lister := &stubStrandedLister{
		rows: []repository.MessagesMessage{
			{ID: uuid.New(), Guid: "guid-A", PeerHandle: "+15551234567"},
			{ID: uuid.New(), Guid: "guid-B", PeerHandle: "+15551234567"},
		},
	}
	matcher := &stubAdminMatcher{
		resultsByHandle: map[string]*service.MatchResult{
			"+15551234567": {
				ContactID: &contactID,
				MatchType: repository.MatchTypeExact,
				Identity:  &repository.ExternalIdentity{Identifier: "+15551234567"},
			},
		},
	}
	inserter := &stubAdminInserter{}

	res, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages:    lister,
		Identity:    matcher,
		RiverClient: inserter,
	})
	require.NoError(t, err)
	require.Equal(t, 2, res.Scanned)
	require.Equal(t, 2, res.Matched)
	require.Equal(t, 1, res.Enqueued, "duplicate contact must enqueue exactly one job")
	require.Len(t, inserter.calls, 1)
}

// TestRematchStranded_IdentityError_CountsAndContinues asserts a per-
// row identity error increments Errors and the loop continues.
func TestRematchStranded_IdentityError_CountsAndContinues(t *testing.T) {
	contactID := uuid.New()
	lister := &stubStrandedLister{
		rows: []repository.MessagesMessage{
			{ID: uuid.New(), Guid: "guid-A", PeerHandle: "boom"},
			{ID: uuid.New(), Guid: "guid-B", PeerHandle: "+15551234567"},
		},
	}
	matcher := &stubAdminMatcher{
		errByHandle: map[string]error{
			"boom": errors.New("db unavailable"),
		},
		resultsByHandle: map[string]*service.MatchResult{
			"+15551234567": {
				ContactID: &contactID,
				MatchType: repository.MatchTypeExact,
				Identity:  &repository.ExternalIdentity{Identifier: "+15551234567"},
			},
		},
	}
	inserter := &stubAdminInserter{}

	res, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages:    lister,
		Identity:    matcher,
		RiverClient: inserter,
	})
	require.NoError(t, err)
	require.Equal(t, 2, res.Scanned)
	require.Equal(t, 1, res.Matched, "the healthy row must still be matched")
	require.Equal(t, 1, res.Errors)
}

// TestRematchStranded_NilRiverClient_StillUpdates verifies the
// remediation handler updates rows even when no river client is
// wired — useful for dry-run / preview mode.
func TestRematchStranded_NilRiverClient_StillUpdates(t *testing.T) {
	contactID := uuid.New()
	lister := &stubStrandedLister{
		rows: []repository.MessagesMessage{
			{ID: uuid.New(), Guid: "guid-1", PeerHandle: "+15551234567"},
		},
	}
	matcher := &stubAdminMatcher{
		resultsByHandle: map[string]*service.MatchResult{
			"+15551234567": {
				ContactID: &contactID,
				MatchType: repository.MatchTypeExact,
				Identity:  &repository.ExternalIdentity{Identifier: "+15551234567"},
			},
		},
	}

	res, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages: lister,
		Identity: matcher,
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Matched)
	require.Equal(t, 0, res.Enqueued, "no enqueue without river client")
	require.Len(t, lister.updates, 1)
}

// TestRematchStranded_RequiresMessagesAndIdentity verifies the
// precondition checks.
func TestRematchStranded_RequiresMessagesAndIdentity(t *testing.T) {
	_, err := RematchStranded(context.Background(), RematchStrandedDeps{})
	require.Error(t, err)

	_, err = RematchStranded(context.Background(), RematchStrandedDeps{
		Messages: &stubStrandedLister{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "identity service")
}

// TestRematchStranded_ListError_Bubbles surfaces a list-level error
// (vs. per-row errors which count and continue).
func TestRematchStranded_ListError_Bubbles(t *testing.T) {
	lister := &stubStrandedLister{listErr: errors.New("list down")}
	matcher := &stubAdminMatcher{}

	_, err := RematchStranded(context.Background(), RematchStrandedDeps{
		Messages: lister,
		Identity: matcher,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "list stranded rows")
}

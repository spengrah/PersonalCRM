package messages

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// AdminStrandedLister is the narrow surface RematchStranded uses to
// enumerate stranded rows. Concrete is *repository.MessagesMessageRepository.
type AdminStrandedLister interface {
	ListStranded(ctx context.Context) ([]repository.MessagesMessage, error)
	UpdateMatchedContact(ctx context.Context, params repository.UpdateMatchedContactParams) error
}

// AdminIdentityMatcher is the narrow surface RematchStranded uses to
// run identity match. Concrete is *service.IdentityService (non-tx —
// the admin path is a one-shot batch utility, not the hot ingest
// path).
type AdminIdentityMatcher interface {
	MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error)
}

// AdminRiverInserter is the narrow surface RematchStranded uses to
// enqueue MessagingAggregateForContactArgs jobs. Concrete is
// *river.Client[pgx.Tx]. Pass nil to disable enqueue (useful in
// dry-run mode).
type AdminRiverInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// RematchStrandedDeps bundles the repos + services used by the
// stranded-row remediation handler.
//
// IdentityService runs in non-tx mode here intentionally: the admin
// path is a one-shot batch utility, not the hot ingest path. A
// transient match error per row degrades to MatchTypeUnmatched (the
// existing forgiving behavior) and the operator can re-run.
type RematchStrandedDeps struct {
	Messages      AdminStrandedLister
	Identity      AdminIdentityMatcher
	RiverClient   AdminRiverInserter
	DefaultSource string // "messages"
}

// RematchStrandedResult summarizes a remediation pass for the
// operator. Counts are emitted on stdout/log when the binary returns.
type RematchStrandedResult struct {
	Scanned       int
	Matched       int
	StillStranded int
	Enqueued      int
	Errors        int
}

// RematchStranded retroactively matches messages_message rows whose
// matched_contact_id is NULL against the current contact_method set.
// Newly-matched rows get their matched_contact_id + peer_normalized
// updated; a MessagingAggregateForContactArgs River job is enqueued
// per unique newly-matched (contact, source) pair so the aggregator
// drains them.
//
// Idempotent — re-running on an already-matched row is a no-op (the
// SQL predicate scopes updates to still-unmatched rows). Safe to run
// from a cron-style cleanup, though it's intended as a one-shot
// operator utility right now.
func RematchStranded(ctx context.Context, deps RematchStrandedDeps) (*RematchStrandedResult, error) {
	if deps.Messages == nil {
		return nil, fmt.Errorf("messages repository is required")
	}
	if deps.Identity == nil {
		return nil, fmt.Errorf("identity service is required")
	}
	source := deps.DefaultSource
	if source == "" {
		source = SourceName
	}

	rows, err := deps.Messages.ListStranded(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stranded rows: %w", err)
	}

	result := &RematchStrandedResult{Scanned: len(rows)}
	enqueued := make(map[uuid.UUID]struct{})

	for i := range rows {
		row := rows[i]
		idType := identity.DetectIdentifierType(row.PeerHandle)
		matchReq := service.MatchRequest{
			RawIdentifier: row.PeerHandle,
			Type:          idType,
			Source:        source,
			SourceID:      &row.Guid,
		}
		match, err := deps.Identity.MatchOrCreate(ctx, matchReq)
		if err != nil {
			result.Errors++
			logger.Warn().Err(err).Str("guid", row.Guid).Msg("rematch: identity match failed")
			continue
		}
		if match.ContactID == nil {
			result.StillStranded++
			continue
		}
		normalized := ""
		if match.Identity != nil {
			normalized = match.Identity.Identifier
		}
		err = deps.Messages.UpdateMatchedContact(ctx, repository.UpdateMatchedContactParams{
			ID:               row.ID,
			MatchedContactID: *match.ContactID,
			PeerNormalized:   normalized,
		})
		if err != nil {
			result.Errors++
			logger.Warn().Err(err).Str("guid", row.Guid).Msg("rematch: update failed")
			continue
		}
		result.Matched++

		// Enqueue one River job per unique newly-matched contact.
		// UniqueOpts dedup against in-flight jobs makes this cheap if
		// the regular ingest path already enqueued one.
		if _, seen := enqueued[*match.ContactID]; seen {
			continue
		}
		if deps.RiverClient != nil {
			_, err = deps.RiverClient.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
				ContactID: *match.ContactID,
				Source:    source,
			}, &river.InsertOpts{
				UniqueOpts: consumerjobs.MessagingAggregateUniqueOpts(),
			})
			if err != nil {
				result.Errors++
				logger.Warn().Err(err).Str("contact_id", match.ContactID.String()).Msg("rematch: enqueue failed")
				continue
			}
			result.Enqueued++
		}
		enqueued[*match.ContactID] = struct{}{}
	}

	return result, nil
}

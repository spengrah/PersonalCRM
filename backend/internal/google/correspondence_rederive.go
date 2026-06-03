package google

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Re-fetch error sentinels the runner branches on (see classifyRefetchError).
var (
	// errCorrespondenceUnavailable marks a PERMANENT re-fetch failure (the
	// message is gone upstream, e.g. Gmail 404). Counted SkippedUnavailable,
	// never a non-zero exit.
	errCorrespondenceUnavailable = errors.New("correspondence: message unavailable")
	// errCorrespondenceTransient marks a retryable re-fetch failure (429 / 5xx).
	errCorrespondenceTransient = errors.New("correspondence: transient error")
)

const (
	// rederiveBatchSize bounds each keyset page (and thus each burst of Gmail
	// re-fetches). Small + rate-limit-friendly.
	rederiveBatchSize = 50
	// rederiveInterCallDelay is a short pause between per-message re-fetches so a
	// full pass stays well under Gmail's per-user rate limits.
	rederiveInterCallDelay = 50 * time.Millisecond
	// rederiveMaxTransientRetries bounds per-row backoff retries on a 429/5xx
	// before the row is counted Failed (retried on the next subcommand run).
	rederiveMaxTransientRetries = 3
	// rederiveBackoffBase is the first transient-retry backoff; it doubles each
	// retry.
	rederiveBackoffBase = 200 * time.Millisecond
	// correspondenceUnknownAccountKey is the sentinel account_gmail_ids key the
	// upsert writes when account_id was NULL. It holds a gmail id but no account
	// to fetch with, so it is never a valid re-fetch target.
	correspondenceUnknownAccountKey = "__unknown__"
)

// CorrespondenceRederiveResult is the counts-only summary the subcommand prints.
type CorrespondenceRederiveResult struct {
	Scanned            int
	Rederived          int
	SkippedNoGmailID   int
	SkippedUnavailable int
	Failed             int
}

// rederiveCommsRepo is the narrow comms_message surface the runner needs.
// Concrete: *repository.CommsMessageRepository.
type rederiveCommsRepo interface {
	ListMissingParticipantNames(ctx context.Context, since time.Time, afterID uuid.UUID, batchSize int32) ([]repository.MissingParticipantNamesRow, error)
	BackfillParticipantNames(ctx context.Context, id uuid.UUID, names repository.ParticipantNames) (int64, error)
}

// rederiveFetcher is the narrow provider surface the runner needs. Concrete:
// *GmailSyncProvider.RefetchParticipantNames. An interface so the runner unit
// test injects a fake with no OAuth.
type rederiveFetcher interface {
	RefetchParticipantNames(ctx context.Context, accountID, gmailMessageID string) (repository.ParticipantNames, error)
}

// CorrespondenceNameRederiveService performs the one-time historical
// display-name re-derivation: it keyset-pages name-less email rows, re-fetches
// each from Gmail, and additively backfills the display names onto the stored
// source_metadata. Idempotent and re-runnable: a row that already has names is
// skipped by the query's NOT (? 'from_name') guard, and a fresh run restarts
// the keyset at the nil id.
type CorrespondenceNameRederiveService struct {
	comms    rederiveCommsRepo
	fetcher  rederiveFetcher
	sleep    func(time.Duration)
	maxBatch int32
}

// NewCorrespondenceNameRederiveService builds the runner.
func NewCorrespondenceNameRederiveService(comms rederiveCommsRepo, fetcher rederiveFetcher) *CorrespondenceNameRederiveService {
	return &CorrespondenceNameRederiveService{
		comms:    comms,
		fetcher:  fetcher,
		sleep:    time.Sleep,
		maxBatch: rederiveBatchSize,
	}
}

// RederiveNames keyset-pages every name-less email row at/after `since`,
// re-fetches its display names, and backfills them. The keyset cursor advances
// to max(id) of each batch regardless of per-row outcome, so a skipped/failed
// row never blocks later rows (livelock avoidance) and is simply retried on the
// next run (it still lacks from_name). Continue-on-error; returns the counts.
func (s *CorrespondenceNameRederiveService) RederiveNames(ctx context.Context, since time.Time) (CorrespondenceRederiveResult, error) {
	var result CorrespondenceRederiveResult
	afterID := uuid.Nil

	for {
		rows, err := s.comms.ListMissingParticipantNames(ctx, since, afterID, s.maxBatch)
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			result.Scanned++
			s.processRow(ctx, row, &result)
		}
		// Advance the keyset cursor to the last row's id. The query orders by id
		// ascending, so the last row carries the batch's max id — advancing here
		// (unconditionally, regardless of per-row outcome) guarantees a
		// skipped/failed row is never re-queried in this pass (livelock
		// avoidance). It is retried on the NEXT run, which still finds it
		// name-less and restarts the keyset at uuid.Nil.
		afterID = rows[len(rows)-1].ID
	}
	return result, nil
}

// processRow re-derives one row's names, updating the result counts. It never
// returns an error: each outcome maps to a counter (continue-on-error).
func (s *CorrespondenceNameRederiveService) processRow(ctx context.Context, row repository.MissingParticipantNamesRow, result *CorrespondenceRederiveResult) {
	accountID, gmailID, ok := resolveRefetchTarget(row)
	if !ok {
		result.SkippedNoGmailID++
		return
	}

	names, err := s.refetchWithRetry(ctx, accountID, gmailID)
	if err != nil {
		switch {
		case errors.Is(err, errCorrespondenceUnavailable):
			result.SkippedUnavailable++
		default:
			// Transient-after-retries and any other hard error are Failed
			// (retried on the next run, which still finds the row name-less).
			result.Failed++
			logger.Warn().
				Err(err).
				Str("message_id", hashIdentifier(gmailID)).
				Msg("correspondence_rederive: re-fetch failed")
		}
		return
	}

	affected, err := s.comms.BackfillParticipantNames(ctx, row.ID, names)
	if err != nil {
		result.Failed++
		logger.Warn().
			Err(err).
			Str("message_id", hashIdentifier(gmailID)).
			Msg("correspondence_rederive: backfill failed")
		return
	}
	if affected > 0 {
		result.Rederived++
	}
	s.sleep(rederiveInterCallDelay)
}

// refetchWithRetry calls the provider re-fetch, retrying transient (429/5xx)
// failures with bounded exponential backoff. A permanent unavailability or a
// hard error returns immediately.
func (s *CorrespondenceNameRederiveService) refetchWithRetry(ctx context.Context, accountID, gmailID string) (repository.ParticipantNames, error) {
	backoff := rederiveBackoffBase
	var lastErr error
	for attempt := 0; attempt <= rederiveMaxTransientRetries; attempt++ {
		names, err := s.fetcher.RefetchParticipantNames(ctx, accountID, gmailID)
		if err == nil {
			return names, nil
		}
		if !errors.Is(err, errCorrespondenceTransient) {
			return repository.ParticipantNames{}, err
		}
		lastErr = err
		if attempt < rederiveMaxTransientRetries {
			s.sleep(backoff)
			backoff *= 2
		}
	}
	return repository.ParticipantNames{}, lastErr
}

// resolveRefetchTarget extracts the (accountID, gmailID) re-fetch target from a
// row. A re-fetch needs BOTH a real connected account_id (newFetcher calls
// GetClientForAccount) AND that account's per-mailbox gmail id under
// source_metadata.account_gmail_ids.<account_id>. The __unknown__ key holds a
// gmail id but no account to fetch with, so it is never a valid target. Returns
// ok=false (→ SkippedNoGmailID) when either is missing.
func resolveRefetchTarget(row repository.MissingParticipantNamesRow) (accountID, gmailID string, ok bool) {
	if row.AccountID == nil || *row.AccountID == "" {
		return "", "", false
	}
	account := *row.AccountID
	if account == correspondenceUnknownAccountKey {
		return "", "", false
	}
	var meta struct {
		AccountGmailIDs map[string]string `json:"account_gmail_ids"`
	}
	if len(row.SourceMetadata) == 0 {
		return "", "", false
	}
	if err := json.Unmarshal(row.SourceMetadata, &meta); err != nil {
		return "", "", false
	}
	gid, present := meta.AccountGmailIDs[account]
	if !present || gid == "" {
		return "", "", false
	}
	return account, gid, true
}

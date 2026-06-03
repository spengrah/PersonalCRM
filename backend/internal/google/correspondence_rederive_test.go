package google

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

// fakeRederiveComms serves keyset pages and records backfills. Rows are stored
// in id order; ListMissingParticipantNames pages by id > afterID. A backfilled
// id is marked done so a second pass (fresh keyset) returns nothing
// (idempotency).
type fakeRederiveComms struct {
	rows       []repository.MissingParticipantNamesRow
	done       map[uuid.UUID]bool
	backfills  []uuid.UUID
	pageCalls  int
	maxPerCall int32
}

func newFakeRederiveComms(rows []repository.MissingParticipantNamesRow) *fakeRederiveComms {
	return &fakeRederiveComms{rows: rows, done: map[uuid.UUID]bool{}}
}

func (f *fakeRederiveComms) ListMissingParticipantNames(_ context.Context, _ time.Time, afterID uuid.UUID, batchSize int32) ([]repository.MissingParticipantNamesRow, error) {
	f.pageCalls++
	if f.maxPerCall == 0 {
		f.maxPerCall = batchSize
	}
	out := []repository.MissingParticipantNamesRow{}
	for _, r := range f.rows {
		if f.done[r.ID] {
			continue
		}
		// keyset: id strictly greater than the cursor (string order matches the
		// hyphenated-UUID byte order Postgres uses).
		if afterID != uuid.Nil && r.ID.String() <= afterID.String() {
			continue
		}
		out = append(out, r)
		if int32(len(out)) >= f.maxPerCall {
			break
		}
	}
	return out, nil
}

func (f *fakeRederiveComms) BackfillParticipantNames(_ context.Context, id uuid.UUID, _ repository.ParticipantNames) (int64, error) {
	f.backfills = append(f.backfills, id)
	f.done[id] = true
	return 1, nil
}

// fakeRederiveFetcher returns canned names or canned errors keyed by gmail id.
type fakeRederiveFetcher struct {
	names     map[string]repository.ParticipantNames
	errs      map[string]error
	transient map[string]int // gmail id → remaining transient failures before success
	calls     map[string]int
}

func newFakeRederiveFetcher() *fakeRederiveFetcher {
	return &fakeRederiveFetcher{
		names:     map[string]repository.ParticipantNames{},
		errs:      map[string]error{},
		transient: map[string]int{},
		calls:     map[string]int{},
	}
}

func (f *fakeRederiveFetcher) RefetchParticipantNames(_ context.Context, _, gmailID string) (repository.ParticipantNames, error) {
	f.calls[gmailID]++
	if n, ok := f.transient[gmailID]; ok && n > 0 {
		f.transient[gmailID] = n - 1
		return repository.ParticipantNames{}, fmt.Errorf("%w: 429", errCorrespondenceTransient)
	}
	if err, ok := f.errs[gmailID]; ok {
		return repository.ParticipantNames{}, err
	}
	return f.names[gmailID], nil
}

// --- helpers ---

func rederiveRow(t *testing.T, id uuid.UUID, accountID string, gmailID string) repository.MissingParticipantNamesRow {
	t.Helper()
	var acct *string
	meta := map[string]any{}
	if accountID != "" {
		acct = &accountID
	}
	if gmailID != "" {
		key := accountID
		if key == "" {
			key = correspondenceUnknownAccountKey
		}
		meta["account_gmail_ids"] = map[string]string{key: gmailID}
	}
	b, err := json.Marshal(meta)
	require.NoError(t, err)
	return repository.MissingParticipantNamesRow{ID: id, AccountID: acct, SourceMetadata: b}
}

func newRederiveService(comms rederiveCommsRepo, fetcher rederiveFetcher) *CorrespondenceNameRederiveService {
	s := NewCorrespondenceNameRederiveService(comms, fetcher)
	s.sleep = func(time.Duration) {} // no real waits in tests
	return s
}

// orderedID returns a UUID whose string sorts by n (so keyset ordering in the
// fake is deterministic across rows).
func orderedID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
}

// --- tests ---

func TestRederive_HappyPath(t *testing.T) {
	id := orderedID(1)
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		rederiveRow(t, id, "me@example.com", "gmail-1"),
	})
	fetcher := newFakeRederiveFetcher()
	fetcher.names["gmail-1"] = repository.ParticipantNames{FromName: "Sender Name", ToNames: []string{"Me"}}
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned)
	require.Equal(t, 1, res.Rederived)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, []uuid.UUID{id}, comms.backfills)
}

func TestRederive_SkippedNoGmailID(t *testing.T) {
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		// nil account_id
		rederiveRow(t, orderedID(1), "", "gmail-x"),
		// only __unknown__ (no real account to fetch with)
		rederiveRow(t, orderedID(2), "", "gmail-y"),
		// account set but no gmail id under that account key
		rederiveRow(t, orderedID(3), "me@example.com", ""),
	})
	fetcher := newFakeRederiveFetcher()
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, res.Scanned)
	require.Equal(t, 3, res.SkippedNoGmailID)
	require.Equal(t, 0, res.Rederived)
	require.Empty(t, fetcher.calls, "no Gmail call for a row with no fetch target")
}

func TestRederive_SkippedUnavailableNotFailed(t *testing.T) {
	id := orderedID(1)
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		rederiveRow(t, id, "me@example.com", "gone-1"),
	})
	fetcher := newFakeRederiveFetcher()
	fetcher.errs["gone-1"] = fmt.Errorf("%w: 404", errCorrespondenceUnavailable)
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, res.SkippedUnavailable)
	require.Equal(t, 0, res.Failed, "a since-deleted message must NOT count as Failed (no non-zero exit)")
}

func TestRederive_TransientRetriesThenSucceeds(t *testing.T) {
	id := orderedID(1)
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		rederiveRow(t, id, "me@example.com", "flaky-1"),
	})
	fetcher := newFakeRederiveFetcher()
	fetcher.transient["flaky-1"] = 2 // fails twice, then succeeds
	fetcher.names["flaky-1"] = repository.ParticipantNames{FromName: "Recovered Name"}
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, res.Rederived)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, 3, fetcher.calls["flaky-1"], "two transient failures + one success")
}

func TestRederive_TransientExhaustedIsFailed(t *testing.T) {
	id := orderedID(1)
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		rederiveRow(t, id, "me@example.com", "always-429"),
	})
	fetcher := newFakeRederiveFetcher()
	fetcher.transient["always-429"] = 99 // never recovers within the retry budget
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, res.Rederived)
	require.Equal(t, 1, res.Failed)
	// 1 initial + rederiveMaxTransientRetries retries.
	require.Equal(t, 1+rederiveMaxTransientRetries, fetcher.calls["always-429"])
}

// Livelock guard: a batch where every row is skipped/failed STILL advances the
// keyset cursor and the pass terminates (does not re-query the same rows
// forever). With batchSize smaller than the row count, paging must complete.
func TestRederive_LivelockGuardTerminates(t *testing.T) {
	rows := []repository.MissingParticipantNamesRow{
		rederiveRow(t, orderedID(1), "", "x"),                  // SkippedNoGmailID
		rederiveRow(t, orderedID(2), "", "y"),                  // SkippedNoGmailID
		rederiveRow(t, orderedID(3), "me@example.com", "gone"), // SkippedUnavailable
	}
	comms := newFakeRederiveComms(rows)
	comms.maxPerCall = 1 // force multiple pages so the cursor must advance
	fetcher := newFakeRederiveFetcher()
	fetcher.errs["gone"] = fmt.Errorf("%w: 404", errCorrespondenceUnavailable)
	s := newRederiveService(comms, fetcher)

	res, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, res.Scanned, "all rows visited exactly once despite none being rewritten")
	require.Equal(t, 2, res.SkippedNoGmailID)
	require.Equal(t, 1, res.SkippedUnavailable)
	// 3 data pages (one row each) + 1 terminating empty page.
	require.Equal(t, 4, comms.pageCalls)
}

// Idempotency: a second full run over already-named rows re-derives nothing.
func TestRederive_IdempotentSecondRun(t *testing.T) {
	id := orderedID(1)
	comms := newFakeRederiveComms([]repository.MissingParticipantNamesRow{
		rederiveRow(t, id, "me@example.com", "gmail-1"),
	})
	fetcher := newFakeRederiveFetcher()
	fetcher.names["gmail-1"] = repository.ParticipantNames{FromName: "Sender Name"}
	s := newRederiveService(comms, fetcher)

	res1, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, res1.Rederived)

	// Second run: the fake marks backfilled rows done (mirrors the SQL's
	// NOT (? 'from_name') guard), so the keyset returns nothing.
	res2, err := s.RederiveNames(context.Background(), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, res2.Scanned)
	require.Equal(t, 0, res2.Rederived)
}

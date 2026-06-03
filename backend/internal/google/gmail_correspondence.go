package google

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

const (
	// CorrespondenceSource is the external_contact.source tag for candidates
	// the correspondence-enrichment producer emits. It is a LINK-ONLY source
	// (the import-suggestions policy never lets it create a new contact); the
	// producer only ever proposes adding an observed address to an EXISTING
	// CRM contact.
	CorrespondenceSource = "gmail_correspondence"

	// correspondenceSimThreshold is the empirically-calibrated minimum name
	// similarity (§4 gate): an unknown address qualifies only when its observed
	// display name matches a CRM contact at sim >= 0.60. The gate yielded ~93%
	// precision on the validation sample; loosening it is a conscious change a
	// regression test pins.
	correspondenceSimThreshold = 0.60

	// correspondenceSimFloor is passed to the FindSimilarContacts SQL, which
	// filters with a STRICT `>` (similarity > threshold). To honor the `>= 0.60`
	// gate we pass a floor a hair below 0.60 so the SQL returns exact-0.60 rows,
	// then re-check `>= correspondenceSimThreshold` in Go. The epsilon must
	// survive the query's float32 (`::real`) cast: 1e-9 is below the float32 ULP
	// at 0.60 (~6e-8) and would round back to 0.60, re-rejecting exact-0.60
	// matches; 1e-6 is comfortably above that ULP yet far below the next distinct
	// similarity score, so it admits exactly the rows a true `>= 0.60` test would.
	correspondenceSimFloor = correspondenceSimThreshold - 1e-6

	// correspondenceMinNameTokens is the §4 token gate: a display name must have
	// at least two whitespace-separated tokens (a full name), since bare first
	// names are the dominant noise source.
	correspondenceMinNameTokens = 2

	// correspondenceMatchLimit caps the FindSimilarContacts result set per
	// observed name. We only need the best match, but request a few so the
	// Go-side `>= 0.60` re-check has the top candidates to choose from.
	correspondenceMatchLimit = 5

	// CorrespondenceWindow is the steady-state recent-window the periodic scan
	// covers. The scan is idempotent so re-scanning the same window every tick
	// is harmless; the historical catchup subcommand passes a wider `since`.
	CorrespondenceWindow = 120 * 24 * time.Hour
)

// correspondenceMetadata is the participant-list view the producer unmarshals
// from comms_message.source_metadata. The bare-address fields are already
// parsed/normalized at ingest; the *Name(s) fields (added by display-name
// capture) are index-aligned with their address siblings.
type correspondenceMetadata struct {
	From     string   `json:"from"`
	To       []string `json:"to"`
	Cc       []string `json:"cc"`
	Bcc      []string `json:"bcc"`
	FromName string   `json:"from_name"`
	ToNames  []string `json:"to_names"`
	CcNames  []string `json:"cc_names"`
	BccNames []string `json:"bcc_names"`
}

// correspondenceContactRepo is the narrow contact surface the producer needs:
// the §4 trigram gate (FindSimilarContacts) and a name lookup for the
// co-occurring-contact evidence. Concrete: *repository.ContactRepository.
type correspondenceContactRepo interface {
	FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error)
	GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error)
}

// correspondenceExternalRepo is the narrow external_contact surface the
// producer needs (sticky-ignore read + candidate upsert). Concrete:
// *repository.ExternalContactRepository.
type correspondenceExternalRepo interface {
	GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error)
	Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
}

// correspondenceCommsRepo is the narrow comms_message surface the producer
// needs (known-address set + recent participant scan). Concrete:
// *repository.CommsMessageRepository.
type correspondenceCommsRepo interface {
	ListEmailIdentitiesForSync(ctx context.Context) ([]repository.EmailIdentity, error)
	ListParticipantsSince(ctx context.Context, since time.Time) ([]repository.CommsMessageParticipantRow, error)
}

// GmailCorrespondenceSuggester mines the participants of already-ingested
// known-contact email threads to discover addresses that belong to EXISTING
// CRM contacts but aren't on their record yet, surfacing each as a link-only
// candidate. It reads only stored comms_message participants — it never fetches
// mail. Idempotent: re-runs upsert the same per-address rows and skip
// already-known / already-ignored addresses.
type GmailCorrespondenceSuggester struct {
	commsRepo    correspondenceCommsRepo
	contactRepo  correspondenceContactRepo
	externalRepo correspondenceExternalRepo
	// meSet returns the connected-account ("own") address set so the producer
	// drops the user's own addresses. Reuses the Gmail provider's MeSet seam so
	// the set comes from the same source and tests can inject it.
	meSet func(ctx context.Context) (map[string]struct{}, error)
}

// NewGmailCorrespondenceSuggester builds the producer. All deps are constructed
// in main.go; meSet is the Gmail provider's MeSet method.
func NewGmailCorrespondenceSuggester(
	commsRepo correspondenceCommsRepo,
	contactRepo correspondenceContactRepo,
	externalRepo correspondenceExternalRepo,
	meSet func(ctx context.Context) (map[string]struct{}, error),
) *GmailCorrespondenceSuggester {
	return &GmailCorrespondenceSuggester{
		commsRepo:    commsRepo,
		contactRepo:  contactRepo,
		externalRepo: externalRepo,
		meSet:        meSet,
	}
}

// correspondenceAggregate accumulates per-unknown-address evidence across all
// scanned rows before the §4 gate runs once per address.
type correspondenceAggregate struct {
	address      string   // normalized unknown address (the candidate source_id)
	namesSeen    []string // deduped observed display names (>=1 token)
	messageCount int
	// coOccurrence counts how often this address co-appeared with each known
	// contact (the row's matched_contact_id). The most-frequent is the
	// co-occurring contact recorded as evidence (tie-break by id for stability).
	coOccurrence map[uuid.UUID]int
}

// Run scans comms_message email rows from `since` forward, applies the §4 gate
// to each unknown correspondent, and upserts a gmail_correspondence candidate
// per qualifying address. The periodic worker passes now-CorrespondenceWindow;
// the historical catchup passes the backfill floor. Returns the number of
// candidates upserted. Idempotent and error-free on empty input.
func (s *GmailCorrespondenceSuggester) Run(ctx context.Context, since time.Time) (int, error) {
	known, err := s.buildKnownSet(ctx)
	if err != nil {
		return 0, fmt.Errorf("build known set: %w", err)
	}
	own, err := s.meSet(ctx)
	if err != nil {
		return 0, fmt.Errorf("build own-account set: %w", err)
	}

	rows, err := s.commsRepo.ListParticipantsSince(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("list participants: %w", err)
	}

	aggregates := s.aggregate(rows, known, own)

	upserted := 0
	failed := 0
	var firstErr error
	for _, agg := range aggregates {
		ok, err := s.evaluateAndUpsert(ctx, agg)
		if err != nil {
			// Continue-on-error so one bad address does not abort the whole
			// scan, but remember the failure: a swallowed DB error would let the
			// worker / catchup report success while silently emitting nothing
			// (e.g. a DB outage), so Run surfaces an aggregated error at the end.
			failed++
			if firstErr == nil {
				firstErr = err
			}
			logger.Warn().
				Err(err).
				Str("address", hashIdentifier(agg.address)).
				Msg("gmail_correspondence: evaluate/upsert failed")
			continue
		}
		if ok {
			upserted++
		}
	}

	if upserted > 0 {
		logger.Info().
			Int("candidates_upserted", upserted).
			Int("addresses_scanned", len(aggregates)).
			Msg("gmail_correspondence: scan upserted candidates")
	}
	if failed > 0 {
		return upserted, fmt.Errorf("gmail_correspondence scan: %d of %d addresses failed (first: %w)", failed, len(aggregates), firstErr)
	}
	return upserted, nil
}

// buildKnownSet returns the set of normalized addresses that already belong to
// a CRM contact (every email contact_method). An address in this set is never
// emitted (it is either already on a contact, or the matched contact's own).
func (s *GmailCorrespondenceSuggester) buildKnownSet(ctx context.Context) (map[string]struct{}, error) {
	identities, err := s.commsRepo.ListEmailIdentitiesForSync(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(identities))
	for _, id := range identities {
		if id.ValueNormalized != "" {
			known[id.ValueNormalized] = struct{}{}
		}
	}
	return known, nil
}

// aggregate folds every scanned row's participants into per-unknown-address
// evidence, dropping known and own-account addresses. Returns aggregates keyed
// (and iterable) deterministically — a sorted slice — so a Go map's randomized
// iteration order never makes the output non-deterministic.
func (s *GmailCorrespondenceSuggester) aggregate(
	rows []repository.CommsMessageParticipantRow,
	known, own map[string]struct{},
) []*correspondenceAggregate {
	byAddress := make(map[string]*correspondenceAggregate)

	for _, row := range rows {
		var meta correspondenceMetadata
		if len(row.SourceMetadata) == 0 {
			continue
		}
		if err := json.Unmarshal(row.SourceMetadata, &meta); err != nil {
			// A row whose metadata won't parse is skipped, not fatal.
			continue
		}
		// Track which unknown addresses this single message contributed to, so
		// an address appearing in more than one bucket of the SAME message (e.g.
		// both To and Cc) counts as one message, not two — message_count is a
		// per-message tally, not a per-occurrence one.
		seenThisRow := make(map[string]struct{})
		for _, p := range meta.participants() {
			normAddr := matching.NormalizeEmail(p.address)
			if normAddr == "" {
				continue
			}
			if _, ok := known[normAddr]; ok {
				continue
			}
			if _, ok := own[normAddr]; ok {
				continue
			}
			agg := byAddress[normAddr]
			if agg == nil {
				agg = &correspondenceAggregate{
					address:      normAddr,
					coOccurrence: make(map[uuid.UUID]int),
				}
				byAddress[normAddr] = agg
			}
			if _, dup := seenThisRow[normAddr]; !dup {
				seenThisRow[normAddr] = struct{}{}
				agg.messageCount++
				if row.MatchedContactID != uuid.Nil {
					agg.coOccurrence[row.MatchedContactID]++
				}
			}
			if name := strings.TrimSpace(p.name); name != "" {
				agg.namesSeen = appendUnique(agg.namesSeen, name)
			}
		}
	}

	out := make([]*correspondenceAggregate, 0, len(byAddress))
	for _, agg := range byAddress {
		out = append(out, agg)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].address < out[j].address
	})
	return out
}

// evaluateAndUpsert applies the §4 gate to one aggregated address and, if it
// qualifies, upserts its gmail_correspondence candidate. Returns true iff a
// candidate was upserted.
func (s *GmailCorrespondenceSuggester) evaluateAndUpsert(ctx context.Context, agg *correspondenceAggregate) (bool, error) {
	// Take the most-informative observed display name (the most tokens, then the
	// longest) — bare first names are dropped by the token gate.
	name := bestDisplayName(agg.namesSeen)
	if tokenCount(name) < correspondenceMinNameTokens {
		return false, nil
	}

	matches, err := s.contactRepo.FindSimilarContacts(ctx, name, correspondenceSimFloor, correspondenceMatchLimit)
	if err != nil {
		return false, fmt.Errorf("find similar contacts: %w", err)
	}
	var best *repository.ContactMatch
	for i := range matches {
		// Go-side `>=` re-check: the SQL is strict `>` against the floor, so
		// honor the spec's `>= 0.60` here.
		if matches[i].Similarity < correspondenceSimThreshold {
			continue
		}
		if best == nil || matches[i].Similarity > best.Similarity {
			best = &matches[i]
		}
	}
	if best == nil {
		return false, nil
	}

	// Sticky-ignore: if a live ignored row already exists for this address, skip
	// the upsert. This is a write-avoidance optimization — the shared Upsert's
	// DO UPDATE SET never touches match_status, so an ignored row would stay
	// ignored even without this guard, but skipping avoids a pointless write.
	existing, err := s.externalRepo.GetBySource(ctx, CorrespondenceSource, agg.address, nil)
	if err != nil {
		return false, fmt.Errorf("get existing candidate: %w", err)
	}
	if existing != nil && existing.DeletedAt == nil && existing.MatchStatus == repository.MatchStatusIgnored {
		return false, nil
	}

	metadata := s.buildEvidence(ctx, agg)
	now := accelerated.GetCurrentTime()
	displayName := name
	if _, err := s.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      CorrespondenceSource,
		SourceID:    agg.address,
		DisplayName: &displayName,
		Emails:      []repository.EmailEntry{{Value: agg.address}},
		Metadata:    metadata,
		SyncedAt:    &now,
	}); err != nil {
		return false, fmt.Errorf("upsert candidate: %w", err)
	}
	return true, nil
}

// buildEvidence assembles the metadata the card renders: deduped observed
// names, message count, and the strongest co-occurring known contact (id +
// resolved name). A name-lookup failure degrades to id-only evidence rather
// than dropping the candidate.
func (s *GmailCorrespondenceSuggester) buildEvidence(ctx context.Context, agg *correspondenceAggregate) map[string]any {
	metadata := map[string]any{
		"display_names_seen": agg.namesSeen,
		"message_count":      agg.messageCount,
	}
	if co := strongestCoOccurrence(agg.coOccurrence); co != uuid.Nil {
		coContact := map[string]any{"id": co.String()}
		if contact, err := s.contactRepo.GetContact(ctx, co); err == nil && contact != nil {
			coContact["name"] = contact.FullName
		}
		metadata["co_occurring_contact"] = coContact
	}
	return metadata
}

// participant is one (display_name, address) pair extracted from a message's
// participant lists.
type participant struct {
	name    string
	address string
}

// participants flattens the metadata's from/to/cc/bcc lists into
// (display_name, address) pairs, pairing each address with its index-aligned
// name. A name slice shorter than its address slice (defensively tolerated,
// e.g. pre-capture rows or a parse mismatch) yields an empty name for the
// unpaired addresses rather than a panic.
func (m correspondenceMetadata) participants() []participant {
	out := make([]participant, 0, 1+len(m.To)+len(m.Cc)+len(m.Bcc))
	if m.From != "" {
		out = append(out, participant{name: m.FromName, address: m.From})
	}
	out = append(out, zip(m.To, m.ToNames)...)
	out = append(out, zip(m.Cc, m.CcNames)...)
	out = append(out, zip(m.Bcc, m.BccNames)...)
	return out
}

// zip pairs each address with its index-aligned name; a missing name (slice
// shorter than addresses) becomes the empty string.
func zip(addresses, names []string) []participant {
	out := make([]participant, 0, len(addresses))
	for i, addr := range addresses {
		if addr == "" {
			continue
		}
		name := ""
		if i < len(names) {
			name = names[i]
		}
		out = append(out, participant{name: name, address: addr})
	}
	return out
}

// appendUnique appends v to s only if not already present (case-insensitive).
func appendUnique(s []string, v string) []string {
	for _, existing := range s {
		if strings.EqualFold(existing, v) {
			return s
		}
	}
	return append(s, v)
}

// tokenCount counts whitespace-separated non-empty tokens in name.
func tokenCount(name string) int {
	return len(strings.Fields(name))
}

// bestDisplayName picks the most-informative observed name: most tokens, then
// longest, with a lexical tie-break for determinism. Returns "" for empty input.
func bestDisplayName(names []string) string {
	best := ""
	bestTokens := -1
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		t := tokenCount(n)
		switch {
		case t > bestTokens:
			best, bestTokens = n, t
		case t == bestTokens && len(n) > len(best):
			best = n
		case t == bestTokens && len(n) == len(best) && n < best:
			best = n
		}
	}
	return best
}

// strongestCoOccurrence returns the most-frequent co-occurring contact id,
// tie-broken by smallest id for stability. Returns uuid.Nil when empty.
func strongestCoOccurrence(counts map[uuid.UUID]int) uuid.UUID {
	best := uuid.Nil
	bestCount := 0
	for id, c := range counts {
		if c > bestCount || (c == bestCount && (best == uuid.Nil || id.String() < best.String())) {
			best, bestCount = id, c
		}
	}
	return best
}

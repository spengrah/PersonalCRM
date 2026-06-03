package google

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
)

// correspondenceContactRepo is the narrow contact surface the discoverer needs:
// the §4 trigram gate (FindSimilarContacts) and a name lookup for the
// co-occurring-contact evidence. Concrete: *repository.ContactRepository.
type correspondenceContactRepo interface {
	FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error)
	GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error)
}

// correspondenceExternalRepo is the narrow external_contact surface the
// discoverer needs (sticky-ignore read + candidate upsert). Concrete:
// *repository.ExternalContactRepository.
type correspondenceExternalRepo interface {
	GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error)
	Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
}

// CorrespondenceDiscoverer applies the §4 correspondence gate to the
// participants of fetched email messages, surfacing unknown addresses whose
// observed display name strong-matches an EXISTING CRM contact as link-only
// gmail_correspondence candidates. It is fed live participants by the Gmail
// sync provider's in-sync discovery hook (between fetch and storage) — it never
// reads stored comms_message rows and never fetches mail itself. Idempotent:
// re-evaluating the same address re-upserts the same per-address row and skips
// already-ignored addresses.
type CorrespondenceDiscoverer struct {
	contactRepo  correspondenceContactRepo
	externalRepo correspondenceExternalRepo
}

// NewCorrespondenceDiscoverer builds the discoverer over the two repos the gate
// needs. The known/own filtering happens in the provider's Sync (which already
// holds the knownMap + meSet), so the discoverer does not take a comms repo or
// a meSet seam.
func NewCorrespondenceDiscoverer(
	contactRepo correspondenceContactRepo,
	externalRepo correspondenceExternalRepo,
) *CorrespondenceDiscoverer {
	return &CorrespondenceDiscoverer{
		contactRepo:  contactRepo,
		externalRepo: externalRepo,
	}
}

// participant is one (display_name, address) pair extracted from a message's
// From/To/Cc participant lists.
type participant struct {
	name    string
	address string
}

// correspondenceAggregate accumulates per-unknown-address evidence across all
// messages in one sync pass before the §4 gate runs once per address.
type correspondenceAggregate struct {
	address      string   // normalized unknown address (the candidate source_id)
	namesSeen    []string // deduped observed display names (>=1 token)
	messageCount int
	// coOccurrence counts how often this address co-appeared with each known
	// contact (a known contact present on the same message's From/To/Cc). The
	// most-frequent is the co-occurring contact recorded as evidence (tie-break
	// by id for stability).
	coOccurrence map[uuid.UUID]int
}

// aggregateParticipants folds one message's (name, address) participant pairs
// into the shared per-pass aggregate map, dropping known and own-account
// addresses. coOccurIDs is the set of known contacts present on this same
// message (computed by the provider over From/To/Cc only) — each unknown
// address the message contributes accrues one co-occurrence count per known id.
// The aggregate map is the caller's per-pass local (NOT a shared field), so
// concurrent account syncs never clobber each other.
func aggregateParticipants(
	parts []participant,
	known, own map[string]struct{},
	coOccurIDs []uuid.UUID,
	into map[string]*correspondenceAggregate,
) {
	// Track which unknown addresses this single message contributed to, so an
	// address appearing in more than one bucket of the SAME message (e.g. both
	// To and Cc) counts as one message, not two — message_count is a per-message
	// tally, not a per-occurrence one.
	seenThisMsg := make(map[string]struct{})
	for _, p := range parts {
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
		agg := into[normAddr]
		if agg == nil {
			agg = &correspondenceAggregate{
				address:      normAddr,
				coOccurrence: make(map[uuid.UUID]int),
			}
			into[normAddr] = agg
		}
		if _, dup := seenThisMsg[normAddr]; !dup {
			seenThisMsg[normAddr] = struct{}{}
			agg.messageCount++
			for _, id := range coOccurIDs {
				if id != uuid.Nil {
					agg.coOccurrence[id]++
				}
			}
		}
		if name := strings.TrimSpace(p.name); name != "" {
			agg.namesSeen = appendUnique(agg.namesSeen, name)
		}
	}
}

// EvaluateAddresses runs the §4 gate + idempotent upsert over a pre-built
// aggregate slice (one aggregate per normalized unknown address). Returns the
// number of candidates upserted. Continue-on-error so one bad address does not
// abort the whole pass, but the first failure is surfaced as an aggregated
// error (a swallowed DB error would let the caller report success while
// silently emitting nothing). The provider logs this error and does NOT
// propagate it — discovery is best-effort enrichment.
func (d *CorrespondenceDiscoverer) EvaluateAddresses(ctx context.Context, aggregates []*correspondenceAggregate) (int, error) {
	upserted := 0
	failed := 0
	var firstErr error
	for _, agg := range aggregates {
		ok, err := d.evaluateAndUpsert(ctx, agg)
		if err != nil {
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
			Msg("gmail_correspondence: discovery upserted candidates")
	}
	if failed > 0 {
		return upserted, fmt.Errorf("gmail_correspondence discovery: %d of %d addresses failed (first: %w)", failed, len(aggregates), firstErr)
	}
	return upserted, nil
}

// evaluateAndUpsert applies the §4 gate to one aggregated address and, if it
// qualifies, upserts its gmail_correspondence candidate. Returns true iff a
// candidate was upserted.
func (d *CorrespondenceDiscoverer) evaluateAndUpsert(ctx context.Context, agg *correspondenceAggregate) (bool, error) {
	// Take the most-informative observed display name (the most tokens, then the
	// longest) — bare first names are dropped by the token gate.
	name := bestDisplayName(agg.namesSeen)
	if tokenCount(name) < correspondenceMinNameTokens {
		return false, nil
	}

	matches, err := d.contactRepo.FindSimilarContacts(ctx, name, correspondenceSimFloor, correspondenceMatchLimit)
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
	existing, err := d.externalRepo.GetBySource(ctx, CorrespondenceSource, agg.address, nil)
	if err != nil {
		return false, fmt.Errorf("get existing candidate: %w", err)
	}
	if existing != nil && existing.DeletedAt == nil && existing.MatchStatus == repository.MatchStatusIgnored {
		return false, nil
	}

	metadata := d.buildEvidence(ctx, agg)
	now := accelerated.GetCurrentTime()
	displayName := name
	if _, err := d.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
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
func (d *CorrespondenceDiscoverer) buildEvidence(ctx context.Context, agg *correspondenceAggregate) map[string]any {
	metadata := map[string]any{
		"display_names_seen": agg.namesSeen,
		"message_count":      agg.messageCount,
	}
	if co := strongestCoOccurrence(agg.coOccurrence); co != uuid.Nil {
		coContact := map[string]any{"id": co.String()}
		if contact, err := d.contactRepo.GetContact(ctx, co); err == nil && contact != nil {
			coContact["name"] = contact.FullName
		}
		metadata["co_occurring_contact"] = coContact
	}
	return metadata
}

// sortedAggregates returns the aggregate map as a slice ordered by address so a
// Go map's randomized iteration order never makes the discovery output
// non-deterministic.
func sortedAggregates(byAddress map[string]*correspondenceAggregate) []*correspondenceAggregate {
	out := make([]*correspondenceAggregate, 0, len(byAddress))
	for _, agg := range byAddress {
		out = append(out, agg)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].address < out[j].address
	})
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

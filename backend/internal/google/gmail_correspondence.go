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

// participantMessageContext carries the per-message trust-anchor facts
// foldDiscovery computes once (trust anchor, cap eligibility, sender
// identity, subject, epoch) into aggregateParticipants, which folds them onto
// every unknown address the message contributes.
type participantMessageContext struct {
	// createEligible is trustAnchored && recipientCount <= 20 — the gate that
	// lets a message anchor the gmail_participant CREATE path (link discovery
	// is unaffected by the cap).
	createEligible  bool
	senderNorm      string
	senderContactID uuid.UUID
	senderIsSelf    bool
	subject         string
	epochSeconds    int64
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

	// trustAnchored is set true the first time a create-eligible (trust-
	// anchored, cap-OK) message contributes this address. It never resets: a
	// qualifying sighting on ANY message in the pass is enough — the cap does
	// not stick across messages.
	trustAnchored bool
	// anchorEpoch/anchorSubject/anchorSenderNorm/anchorSenderContactID/
	// anchorSenderSelf record the create-eligible message with the GREATEST
	// epoch seen so far (strict `>`, so the first-folded message wins a tie) —
	// the most-recent trust-anchored message's evidence, per §1.2.
	anchorEpoch           int64
	anchorSubject         string
	anchorSenderNorm      string
	anchorSenderContactID uuid.UUID
	anchorSenderSelf      bool
	// lastMessageEpoch is the greatest epoch of ANY contributing message
	// (trust-anchored or not) — feeds evidence's last_message_at for both
	// gmail_correspondence and gmail_participant.
	lastMessageEpoch int64
}

// aggregateParticipants folds one message's (name, address) participant pairs
// into the shared per-pass aggregate map, dropping known, own-account, and
// own-domain addresses (own-domain addresses are the user, so they must never
// enter either candidate pool). coOccurIDs is the set of
// known contacts present on this same message (computed by the provider over
// From/To/Cc only) — each unknown address the message contributes accrues one
// co-occurrence count per known id. msgCtx carries this message's trust-anchor
// facts (computed once by foldDiscovery), folded onto every address the
// message contributes. The aggregate map is the caller's per-pass local (NOT a
// shared field), so concurrent account syncs never clobber each other.
func aggregateParticipants(
	parts []participant,
	known, own, ownDomains map[string]struct{},
	coOccurIDs []uuid.UUID,
	msgCtx participantMessageContext,
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
		if _, ok := ownDomains[domainOf(normAddr)]; ok {
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
			if msgCtx.epochSeconds > agg.lastMessageEpoch {
				agg.lastMessageEpoch = msgCtx.epochSeconds
			}
			if msgCtx.createEligible {
				agg.trustAnchored = true
				if msgCtx.epochSeconds > agg.anchorEpoch {
					agg.anchorEpoch = msgCtx.epochSeconds
					agg.anchorSubject = msgCtx.subject
					agg.anchorSenderNorm = msgCtx.senderNorm
					agg.anchorSenderContactID = msgCtx.senderContactID
					agg.anchorSenderSelf = msgCtx.senderIsSelf
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

// evaluateAndUpsert applies the two-gate evaluator to one aggregated address:
// the link gate (§4 trigram match) runs FIRST (precedence); only when it does
// NOT fire does the trust-anchor gate run. The two candidate sources are
// mutually exclusive per address — whichever classification wins first is
// sticky — enforced by a cross-source existence check before each source's
// own upsert. Returns true iff a candidate was upserted.
func (d *CorrespondenceDiscoverer) evaluateAndUpsert(ctx context.Context, agg *correspondenceAggregate) (bool, error) {
	// Take the most-informative observed display name (the most tokens, then the
	// longest) — bare first names are dropped by the token gate. The link gate
	// only runs when the name clears the token floor; a 1-token name never
	// reaches FindSimilarContacts.
	name := bestDisplayName(agg.namesSeen)
	if tokenCount(name) >= correspondenceMinNameTokens {
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
		if best != nil {
			return d.upsertCorrespondence(ctx, agg, name)
		}
	}

	if agg.trustAnchored {
		return d.upsertParticipant(ctx, agg)
	}
	return false, nil
}

// upsertCorrespondence writes the link-gate candidate. Cross-source check
// FIRST: a live gmail_participant row for this address (any match status)
// means the address was already classified as a participant candidate in an
// earlier pass — first classification is sticky, so no correspondence
// candidate is proposed. Then the existing same-source sticky-ignore check
// (write-avoidance only: the shared Upsert's DO UPDATE never touches
// match_status, so an ignored row stays ignored regardless).
func (d *CorrespondenceDiscoverer) upsertCorrespondence(ctx context.Context, agg *correspondenceAggregate, name string) (bool, error) {
	otherSource, err := d.externalRepo.GetBySource(ctx, ParticipantSource, agg.address, nil)
	if err != nil {
		return false, fmt.Errorf("get existing participant candidate: %w", err)
	}
	if otherSource != nil && otherSource.DeletedAt == nil {
		return false, nil
	}

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

// upsertParticipant writes the trust-anchor-gate candidate. Cross-source check
// FIRST (symmetric with upsertCorrespondence): a live gmail_correspondence row
// for this address (any match status) is sticky, so no participant candidate
// is proposed. Then the same-source sticky-ignore check. DisplayName is nil
// for an address-only sighting — a 1-token observed name IS used here (the
// 2-token trigram gate is link-only).
func (d *CorrespondenceDiscoverer) upsertParticipant(ctx context.Context, agg *correspondenceAggregate) (bool, error) {
	otherSource, err := d.externalRepo.GetBySource(ctx, CorrespondenceSource, agg.address, nil)
	if err != nil {
		return false, fmt.Errorf("get existing correspondence candidate: %w", err)
	}
	if otherSource != nil && otherSource.DeletedAt == nil {
		return false, nil
	}

	existing, err := d.externalRepo.GetBySource(ctx, ParticipantSource, agg.address, nil)
	if err != nil {
		return false, fmt.Errorf("get existing participant candidate: %w", err)
	}
	if existing != nil && existing.DeletedAt == nil && existing.MatchStatus == repository.MatchStatusIgnored {
		return false, nil
	}

	name := bestDisplayName(agg.namesSeen)
	var displayName *string
	if name != "" {
		displayName = &name
	}
	metadata := buildParticipantEvidence(ctx, d.contactRepo, agg)
	now := accelerated.GetCurrentTime()
	if _, err := d.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      ParticipantSource,
		SourceID:    agg.address,
		DisplayName: displayName,
		Emails:      []repository.EmailEntry{{Value: agg.address}},
		Metadata:    metadata,
		SyncedAt:    &now,
	}); err != nil {
		return false, fmt.Errorf("upsert candidate: %w", err)
	}
	return true, nil
}

// buildEvidence assembles the metadata the card renders: deduped observed
// names, message count, recency, and the strongest co-occurring known contact
// (id + resolved name). A name-lookup failure degrades to id-only evidence
// rather than dropping the candidate. last_message_at is omitted when no
// contributing message's epoch was recorded (cheap evidence gap, not an
// error) — the renderer treats it as optional.
func (d *CorrespondenceDiscoverer) buildEvidence(ctx context.Context, agg *correspondenceAggregate) map[string]any {
	metadata := map[string]any{
		"display_names_seen": agg.namesSeen,
		"message_count":      agg.messageCount,
	}
	if agg.lastMessageEpoch > 0 {
		metadata["last_message_at"] = formatEvidenceTime(agg.lastMessageEpoch)
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

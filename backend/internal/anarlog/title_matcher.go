package anarlog

import (
	"context"
	"sort"

	"personal-crm/backend/internal/repository"
)

const (
	// titleMinSimilarity is the trigram-similarity threshold below which
	// candidates are not even considered. Mirrors
	// matching.ImportConfig.MinSimilarityThreshold (0.3).
	titleMinSimilarity = 0.3

	// titleCollisionGap is the required similarity gap between the top
	// and runner-up matches. Tighter than the import-matcher's
	// usernameMatchGap (0.15) because a single title name token has even
	// less context than a username. Boundary rule: gap <=
	// titleCollisionGap counts as AMBIGUOUS (drop). Pairs with TC-TM5
	// (exact 0.20 gap → nil) and TC-TM6 (gap 0.21 → match).
	titleCollisionGap = 0.20

	// titleConfidenceFloor is the minimum similarity required for the
	// top match to be accepted. PostgreSQL pg_trgm.similarity() values
	// for "Alice" against "Alice Smith" land in the ~0.38-0.45 range
	// (verified against seeded test data); 0.38 catches the common
	// case while leaving false positives bounded by the collision-gap.
	// Re-tune iteratively from dogfooding (OPEN-T2).
	titleConfidenceFloor = 0.38

	// titleFindLimit is the result-cap fed to FindSimilarContacts. The
	// top 2 are needed for the gap test; 5 mirrors ImportMatchService.
	titleFindLimit int32 = 5
)

// contactMatchFinder is the narrow interface the matcher needs from the
// contact repository. Defined here so the matcher can be unit-tested
// with a hand-rolled mock without dragging in a full repository.
type contactMatchFinder interface {
	FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error)
}

// TitleMatcher applies the trigram collision-gap rule to a single
// title-extracted name token. The contact repo is bound at
// construction time.
type TitleMatcher struct {
	contactRepo contactMatchFinder
}

// NewTitleMatcher constructs a TitleMatcher with the given contact
// repository (or any contactMatchFinder).
func NewTitleMatcher(contactRepo contactMatchFinder) *TitleMatcher {
	return &TitleMatcher{contactRepo: contactRepo}
}

// MatchTitleToken queries the contact repository for trigram-similar
// names and returns the unique high-confidence match, or nil if the
// match is ambiguous (collision-gap rule) / below the confidence floor
// / no result at all.
//
// Returns (nil, nil) for an empty token — no DB call is made.
func (m *TitleMatcher) MatchTitleToken(ctx context.Context, token string) (*repository.ContactMatch, error) {
	if token == "" {
		return nil, nil
	}
	matches, err := m.contactRepo.FindSimilarContacts(ctx, token, titleMinSimilarity, titleFindLimit)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}

	// Stable sort: similarity desc, then contact_id asc as a tiebreaker
	// so equal-similarity rows produce the same top-1 / top-2 across
	// runs (Go map iteration is randomized; pg_trgm ORDER BY similarity
	// can return tied scores in arbitrary order).
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Similarity != matches[j].Similarity {
			return matches[i].Similarity > matches[j].Similarity
		}
		return matches[i].Contact.ID.String() < matches[j].Contact.ID.String()
	})

	top := matches[0]
	if top.Similarity < titleConfidenceFloor {
		return nil, nil
	}
	if len(matches) > 1 {
		runner := matches[1]
		// Strict less-or-equal: gap == titleCollisionGap counts as
		// ambiguous and drops.
		if top.Similarity-runner.Similarity <= titleCollisionGap {
			return nil, nil
		}
	}
	return &top, nil
}

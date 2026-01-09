package matching

// FuzzyConfig defines weights and thresholds for fuzzy matching.
type FuzzyConfig struct {
	MinSimilarityThreshold float64
	ConfidenceThreshold    float64
	NameWeight             float64
	MethodWeight           float64
}

// ImportConfig defines fuzzy matching behavior for import candidates.
var ImportConfig = FuzzyConfig{
	MinSimilarityThreshold: 0.3,
	ConfidenceThreshold:    0.5,
	NameWeight:             0.6,
	MethodWeight:           0.4,
}

// CalendarConfig defines fuzzy matching behavior for calendar attendee matching.
var CalendarConfig = FuzzyConfig{
	MinSimilarityThreshold: 0.3,
	ConfidenceThreshold:    0.7,
	NameWeight:             0.6,
	MethodWeight:           0.4,
}

// Score calculates a weighted confidence score for a match.
func (c FuzzyConfig) Score(nameSimilarity float64, methodMatches, totalMethods int) float64 {
	score := nameSimilarity * c.NameWeight
	if totalMethods > 0 {
		methodScore := float64(methodMatches) / float64(totalMethods)
		score += methodScore * c.MethodWeight
	}
	return score
}

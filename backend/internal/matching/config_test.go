package matching

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFuzzyConfigScore(t *testing.T) {
	tests := []struct {
		name           string
		cfg            FuzzyConfig
		nameSimilarity float64
		methodMatches  int
		totalMethods   int
		expected       float64
	}{
		{
			name:           "name only",
			cfg:            ImportConfig,
			nameSimilarity: 0.9,
			methodMatches:  0,
			totalMethods:   0,
			expected:       0.54,
		},
		{
			name:           "name and method overlap",
			cfg:            ImportConfig,
			nameSimilarity: 0.8,
			methodMatches:  1,
			totalMethods:   2,
			expected:       0.68,
		},
		{
			name:           "no methods",
			cfg:            ImportConfig,
			nameSimilarity: 0.2,
			methodMatches:  0,
			totalMethods:   3,
			expected:       0.12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.cfg.Score(tt.nameSimilarity, tt.methodMatches, tt.totalMethods)
			assert.InDelta(t, tt.expected, result, 0.0001)
		})
	}
}

func TestFuzzyConfigDefaults(t *testing.T) {
	assert.InDelta(t, 0.3, ImportConfig.MinSimilarityThreshold, 0.0001)
	assert.InDelta(t, 0.5, ImportConfig.ConfidenceThreshold, 0.0001)
	assert.InDelta(t, 0.6, ImportConfig.NameWeight, 0.0001)
	assert.InDelta(t, 0.4, ImportConfig.MethodWeight, 0.0001)

	assert.InDelta(t, 0.3, CalendarConfig.MinSimilarityThreshold, 0.0001)
	assert.InDelta(t, 0.7, CalendarConfig.ConfidenceThreshold, 0.0001)
	assert.InDelta(t, 0.6, CalendarConfig.NameWeight, 0.0001)
	assert.InDelta(t, 0.4, CalendarConfig.MethodWeight, 0.0001)
}

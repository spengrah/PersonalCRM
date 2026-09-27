package cadence

import "fmt"

// CadenceType represents different contact cadences
type CadenceType string

const (
	CadenceWeekly    CadenceType = "weekly"
	CadenceBiweekly  CadenceType = "biweekly"
	CadenceMonthly   CadenceType = "monthly"
	CadenceQuarterly CadenceType = "quarterly"
	CadenceBiannual  CadenceType = "biannual"
	CadenceAnnual    CadenceType = "annual"
)

// ParseCadence parses a cadence string into a CadenceType
func ParseCadence(cadence string) (CadenceType, error) {
	switch cadence {
	case "weekly":
		return CadenceWeekly, nil
	case "biweekly":
		return CadenceBiweekly, nil
	case "monthly":
		return CadenceMonthly, nil
	case "quarterly":
		return CadenceQuarterly, nil
	case "biannual":
		return CadenceBiannual, nil
	case "annual":
		return CadenceAnnual, nil
	default:
		return "", fmt.Errorf("unknown cadence: %s", cadence)
	}
}

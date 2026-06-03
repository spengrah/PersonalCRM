package tests

import "time"

// gmailPastNoonAnchor keeps Gmail test messages on one local day while placing
// them well outside the provider's recent-message safety lag.
func gmailPastNoonAnchor() time.Time {
	return localNoonAnchor().Add(-24 * time.Hour)
}

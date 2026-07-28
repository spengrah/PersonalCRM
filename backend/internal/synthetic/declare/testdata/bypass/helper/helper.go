// Fixture (NOT compiled): the helper that smuggles the forbidden import in.
package helper

import "personal-crm/backend/internal/cadence"

// Period is fixture math derived from the app's own cadence table — precisely
// what the independence rule forbids.
var Period = cadence.ProductionCadenceConfig().Weekly

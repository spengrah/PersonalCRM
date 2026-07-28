// Fixture tree (NOT compiled — testdata is invisible to the go tool). It models
// the subpackage-indirection bypass the import guard must catch: the root file
// itself is clean, and the forbidden import hides one package down.
package bypass

import (
	"personal-crm/backend/internal/synthetic/declare/testdata/bypass/helper"
)

var _ = helper.Period

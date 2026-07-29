// Fixture tree (NOT compiled): a clean tree, so a green scan means "no
// forbidden import" rather than "the walk found nothing".
package clean

import "fmt"

var Greeting = fmt.Sprintf("weekly")

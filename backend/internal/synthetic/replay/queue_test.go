package replay

import (
	"regexp"
	"strings"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// riverQueueNameGrammar is River's own validateQueueName regex (client.go,
// v0.34.0). A queue name outside it is rejected at NewClient, which would turn
// harness construction into a hard failure for whole classes of namespace.
var riverQueueNameGrammar = regexp.MustCompile(`^(?:[a-z0-9])+(?:[_|\-]?[a-z0-9]+)*$`)

// The queue name has to be legal for EVERY namespace the toolkit charset
// admits, which is why it is hashed rather than sanitized: `^[a-z0-9-]+$`
// tolerates leading, trailing and doubled hyphens, and a 60-character token
// that a "synthetic-" prefix would push past River's 64-character ceiling.
func TestSyntheticQueueName_IsAlwaysAValidRiverQueue(t *testing.T) {
	namespaces := []string{
		"a",
		"-leading",
		"trailing-",
		"double--hyphen",
		"---",
		"0123456789",
		strings.Repeat("a", 60),
		strings.Repeat("a-", 30),
		"w3-1753700000000-c1",
		"w3-1753700000000-c1-s7",
	}
	for _, ns := range namespaces {
		name := SyntheticQueueName(ns)
		assert.Regexp(t, riverQueueNameGrammar, name, "namespace %q", ns)
		assert.LessOrEqual(t, len(name), 64, "namespace %q", ns)
		assert.NotEqual(t, river.QueueDefault, name,
			"a harness queue that collided with `default` would isolate nothing")
		assert.True(t, strings.HasPrefix(name, syntheticQueuePrefix), "namespace %q", ns)
	}
}

// Determinism is load-bearing, not incidental: cleanup runs in a LATER request
// with no handle on the client that made the queue, and derives the name from
// the namespace alone to drop the queue's leftovers.
func TestSyntheticQueueName_IsDeterministicAndPerNamespace(t *testing.T) {
	require.Equal(t, SyntheticQueueName("w3-1700-c1"), SyntheticQueueName("w3-1700-c1"))

	seen := map[string]string{}
	for _, ns := range []string{"w3-1700-c1", "w3-1700-c2", "w3-1700-c1-s1", "w4-1700-c1"} {
		name := SyntheticQueueName(ns)
		prior, clash := seen[name]
		require.False(t, clash, "namespaces %q and %q share queue %q", prior, ns, name)
		seen[name] = ns
	}
}

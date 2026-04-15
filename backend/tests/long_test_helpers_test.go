package tests

import (
	"os"
	"testing"
)

const longTestsEnvVar = "LONG_TESTS"

func requireLongTests(t *testing.T) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping long integration test in short mode")
	}
	if os.Getenv(longTestsEnvVar) == "" {
		t.Skip("skipping long integration test; set LONG_TESTS=1 or run make test-integration-slow")
	}
}

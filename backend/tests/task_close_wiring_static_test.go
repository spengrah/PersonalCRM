// Package tests — static wiring guard for the merge-time task-close enqueuer.
//
// MergeContacts errors when it closes an enqueue-eligible contact_task and
// SetTaskCloseEnqueuer was never called, so production wiring MUST call the
// setter in main.go. Similarly, the Todoist provider's state-aware temp-ID
// finalize needs the river client + cutover-mode gate passed at construction.
// Integration tests cannot cover main.go (it is a binary), so this static
// test greps the source — the same belt the ingest-registry and sole-writer
// guards use.
package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func readMainGoSource(t *testing.T) []byte {
	t.Helper()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	mainPath := filepath.Join(moduleRoot, "cmd", "crm-api", "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	return src
}

// TestTaskCloseEnqueuerWiring_Static asserts main.go wires the merge-time
// close enqueuer with the river client and the follow-up cutover-mode gate.
func TestTaskCloseEnqueuerWiring_Static(t *testing.T) {
	t.Parallel()
	src := readMainGoSource(t)

	setter := regexp.MustCompile(
		`contactService\.SetTaskCloseEnqueuer\(\s*riverClient,\s*cfg\.EventBus\.FollowUpMode == config\.EventBusFollowUpModeCutover\s*\)`)
	if !setter.Match(src) {
		t.Error("main.go must call contactService.SetTaskCloseEnqueuer(riverClient, cfg.EventBus.FollowUpMode == config.EventBusFollowUpModeCutover) — without it MergeContacts errors on any merge that closes a real-external-id task")
	}
}

// TestCadenceProviderCloseDepsWiring_Static asserts main.go passes the river
// client + cutover-mode gate to the Todoist cadence provider so the
// state-aware temp-ID finalize can enqueue the durable remote close.
func TestCadenceProviderCloseDepsWiring_Static(t *testing.T) {
	t.Parallel()
	src := readMainGoSource(t)

	provider := regexp.MustCompile(
		`todoist\.NewCadenceSyncProvider\((?s:.*?)riverClient,\s*cfg\.EventBus\.FollowUpMode == config\.EventBusFollowUpModeCutover,\s*\)`)
	if !provider.Match(src) {
		t.Error("main.go must pass riverClient + the cutover-mode gate to todoist.NewCadenceSyncProvider — without them the temp-ID finalize on a completed row cannot enqueue the durable remote close")
	}
}

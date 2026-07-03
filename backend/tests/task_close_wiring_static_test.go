// Package tests — static wiring guard for the merge-time task-close enqueuer.
//
// MergeContacts errors when it closes an enqueue-eligible contact_task and
// SetTaskCloseEnqueuer was never called, so production wiring MUST call the
// setter. Similarly, the Todoist provider's state-aware temp-ID finalize needs
// the river client + cutover-mode gate passed at construction. This wiring
// lives in the crm-api composition root, which is split across
// cmd/crm-api/*.go (main.go + the wire_*.go files) — a binary integration
// tests cannot cover — so this static test greps the package source, the same
// belt the ingest-registry and sole-writer guards use.
package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readCrmAPISource concatenates every non-test .go file in cmd/crm-api so the
// guards match wherever the wiring lives across the composition-root files
// (main.go + wire_*.go + routes.go).
func readCrmAPISource(t *testing.T) []byte {
	t.Helper()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	dir := filepath.Join(moduleRoot, "cmd", "crm-api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var buf []byte
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		buf = append(buf, src...)
		buf = append(buf, '\n')
	}
	return buf
}

// TestTaskCloseEnqueuerWiring_Static asserts the crm-api wiring calls the
// merge-time close enqueuer with the river client and the follow-up
// cutover-mode gate (in buildContactService).
func TestTaskCloseEnqueuerWiring_Static(t *testing.T) {
	t.Parallel()
	src := readCrmAPISource(t)

	setter := regexp.MustCompile(
		`contactService\.SetTaskCloseEnqueuer\(\s*riverClient,\s*cfg\.EventBus\.FollowUpMode == config\.EventBusFollowUpModeCutover\s*\)`)
	if !setter.Match(src) {
		t.Error("crm-api must call contactService.SetTaskCloseEnqueuer(riverClient, cfg.EventBus.FollowUpMode == config.EventBusFollowUpModeCutover) — without it MergeContacts errors on any merge that closes a real-external-id task")
	}
}

// TestCadenceProviderCloseDepsWiring_Static asserts the crm-api wiring passes
// the river client + cutover-mode gate to the Todoist cadence provider so the
// state-aware temp-ID finalize can enqueue the durable remote close (in
// registerTodoistProvider, via the todoistProviderDeps fields).
func TestCadenceProviderCloseDepsWiring_Static(t *testing.T) {
	t.Parallel()
	src := readCrmAPISource(t)

	provider := regexp.MustCompile(
		`todoist\.NewCadenceSyncProvider\((?s:.*?)deps\.RiverClient,\s*deps\.Config\.EventBus\.FollowUpMode == config\.EventBusFollowUpModeCutover,\s*\)`)
	if !provider.Match(src) {
		t.Error("crm-api must pass the river client + the cutover-mode gate to todoist.NewCadenceSyncProvider — without them the temp-ID finalize on a completed row cannot enqueue the durable remote close")
	}

	// The provider reads deps.RiverClient / deps.Config, but only if the caller
	// actually POPULATES those fields in the todoistProviderDeps literal. Assert
	// the construction→wiring span: the literal must set RiverClient: riverClient
	// and Config: cfg (otherwise the provider silently receives nil/zero and the
	// remote close never enqueues).
	literal := regexp.MustCompile(
		`registerTodoistProvider\(todoistProviderDeps\{(?s:.*?)Config:\s*cfg,(?s:.*?)RiverClient:\s*riverClient,(?s:.*?)\}\)`)
	if !literal.Match(src) {
		t.Error("crm-api must populate the todoistProviderDeps literal with Config: cfg and RiverClient: riverClient — without them registerTodoistProvider passes a zero river client / config to the cadence provider and the durable remote close never enqueues")
	}
}

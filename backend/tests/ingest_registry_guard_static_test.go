// Package tests — ingest-registry grep guard self-test.
//
// The grep guard scripts/check-ingest-registry.sh asserts the IngestBatch
// function body in backend/internal/service/ingest.go names no event kind
// (constant or dotted string literal) and no per-family predicate —
// routing must go through the daemonFamily descriptor table's kindToFamily
// lookups. This test is the suspenders to that belt: it runs the ACTUAL
// guard script and proves it (a) passes on the real tree, (b) catches a
// synthetic violation injected into the IngestBatch body, and (c) ignores
// a kind reference outside the body. Without this, a future regression in
// the awk region extractor (e.g. mis-delimiting and scanning an empty
// body) could let the guard silently pass on everything.
//
// Models scripts/ci/crm-marker-construction-guard.sh's companion test
// TestCRMMarkerGrepGuard_CatchesIndexAssignment.
package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ingestRegistryGuardPath returns the absolute path to the guard script.
func ingestRegistryGuardPath(t *testing.T) string {
	t.Helper()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	repoRoot := filepath.Dir(moduleRoot)
	guard := filepath.Join(repoRoot, "scripts", "check-ingest-registry.sh")
	if _, statErr := os.Stat(guard); statErr != nil {
		t.Fatalf("guard script not found at %s: %v", guard, statErr)
	}
	return guard
}

// TestIngestRegistryGuard_PassesOnRealTree confirms the guard exits zero on
// the actual ingest.go — a false positive here would block every push.
func TestIngestRegistryGuard_PassesOnRealTree(t *testing.T) {
	guard := ingestRegistryGuardPath(t)
	if out, runErr := exec.Command(guard).CombinedOutput(); runErr != nil {
		t.Fatalf("guard unexpectedly failed on the real tree: %v\n%s", runErr, out)
	}
}

// TestIngestRegistryGuard_CatchesViolations runs the actual guard against
// synthetic fixtures whose IngestBatch body names a kind in each of the
// three forbidden forms (constant, dotted string literal, per-family
// predicate call) and asserts the guard exits non-zero. A fixture whose
// kind reference lives OUTSIDE the IngestBatch body must pass — that proves
// the guard is scoped to the body, not the whole file.
func TestIngestRegistryGuard_CatchesViolations(t *testing.T) {
	guard := ingestRegistryGuardPath(t)

	cases := []struct {
		name      string
		body      string
		wantBlock bool
	}{
		{
			name: "kind constant inside IngestBatch",
			body: `package service

func (s *IngestService) IngestBatch(ctx context.Context) error {
	if env.Kind == events.KindCallSent {
		return nil
	}
	return nil
}
`,
			wantBlock: true,
		},
		{
			name: "dotted kind literal inside IngestBatch",
			body: `package service

func (s *IngestService) IngestBatch(ctx context.Context) error {
	_ = "raw_message.received"
	return nil
}
`,
			wantBlock: true,
		},
		{
			name: "per-family predicate call inside IngestBatch",
			body: `package service

func (s *IngestService) IngestBatch(ctx context.Context) error {
	if isCallKind(env.Kind) {
		return nil
	}
	return nil
}
`,
			wantBlock: true,
		},
		{
			name: "kind reference outside IngestBatch passes",
			body: `package service

func (s *IngestService) IngestBatch(ctx context.Context) error {
	if fam, ok := kindToFamily[env.Kind]; ok {
		_ = fam
	}
	return nil
}

func handleRawMessage() {
	_ = events.KindRawMessageSent
	_ = "call.sent"
	_ = isCallKind(nil)
}
`,
			wantBlock: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := filepath.Join(t.TempDir(), "ingest_fixture.go")
			if writeErr := os.WriteFile(fixture, []byte(tc.body), 0o644); writeErr != nil {
				t.Fatalf("write fixture: %v", writeErr)
			}
			out, runErr := exec.Command(guard, fixture).CombinedOutput()
			blocked := runErr != nil
			if blocked != tc.wantBlock {
				t.Fatalf("guard block=%v, want %v; output:\n%s", blocked, tc.wantBlock, out)
			}
		})
	}
}

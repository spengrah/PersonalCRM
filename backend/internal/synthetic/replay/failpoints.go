package replay

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrConstructorResidue marks a harness-construction failure that LEFT the
// namespace's synthetic host row behind. newHarness removes that row
// best-effort on its own post-host failure paths; when the removal itself
// fails, the returned error wraps this sentinel so a caller can report the
// residue truthfully (errors.Is) rather than guessing. The residue is still
// reachable — the declared-seed cleanup path finds a host-only world from the
// requested namespace token.
var ErrConstructorResidue = errors.New("synthetic: harness construction left namespaced rows behind")

// constructorFailpointAfterHost fires immediately after the namespaced
// synthetic host row is inserted and before the River client starts — the
// realizable shape of the real client.Start failure path, which is otherwise
// unreachable from a test.
const constructorFailpointAfterHost = "after-host"

var (
	constructorFailpointMu   sync.Mutex
	constructorFailpointName string
)

// SetConstructorFailpointForTest arms a harness-construction failpoint and
// returns a restore func the caller MUST defer. It is test-only support that is
// nevertheless compiled into the package (backend/tests is an external package,
// the same reason SeedHostForTest is exported) and therefore guards itself:
// it PANICS unless CRM_ENV is test/testing, so a production build cannot arm it
// even by mistake. Pass "" to disarm.
func SetConstructorFailpointForTest(name string) (restore func()) {
	requireTestEnv("replay.SetConstructorFailpointForTest")
	constructorFailpointMu.Lock()
	prev := constructorFailpointName
	constructorFailpointName = name
	constructorFailpointMu.Unlock()
	return func() {
		constructorFailpointMu.Lock()
		constructorFailpointName = prev
		constructorFailpointMu.Unlock()
	}
}

func constructorFailpoint() string {
	constructorFailpointMu.Lock()
	defer constructorFailpointMu.Unlock()
	return constructorFailpointName
}

// requireTestEnv panics unless the process is running under a test environment.
// Fail-loud misuse guard for the exported test seams in this package.
func requireTestEnv(what string) {
	switch os.Getenv("CRM_ENV") {
	case "test", "testing":
		return
	default:
		panic(fmt.Sprintf("%s is test-only support and requires CRM_ENV=test|testing (got %q)", what, os.Getenv("CRM_ENV")))
	}
}

# PR6 implementer handoff (mid-CI)

**To the fresh agent:** the previous agent ran out of context window mid-way through
fixing CI on PR #307. CI is currently red. This doc gives you everything you need
to pick up cleanly. The plan at `.ai/log/plan/mac-daemon-phase-1-pr6-daemon-skeleton.md`
is still authoritative for design intent; this doc summarises state.

## Branch + commits

**Branch:** `feat/mac-daemon-pr6-daemon-skeleton`
**PR:** [#307](https://github.com/spengrah/PersonalCRM/pull/307) (already open, do
NOT run `gh pr create`)
**Last pushed commit:** `ad59ed9`
**Tip of branch (local + remote, in sync as of handoff):**

```
f4055f0 docs(mac-daemon): add v1 PR-by-PR decomposition with PR6 scope   [pre-existing]
ed26ecd feat(mac-daemon): bootstrap SPM package + Package.swift
de940b9 feat(mac-daemon): CI workflow + Makefile target
d544aa8 feat(mac-daemon): CRMMacCore — state + config + plugin protocol
474ed40 feat(mac-daemon): CRMMacPiClient — typed envelope + per-test URLSession
d8b95f1 feat(mac-daemon): adapter protocols + production system impls
7644c17 feat(mac-daemon): lifecycle workflows + tests
04123f9 feat(mac-daemon): crm-mac executable — CLI surface + daemon body
0ebe803 feat(crm-admin): add --mint-pairing-token + --list-hosts + --revoke-host
2104b61 docs(mac-daemon): README + first-launch Gatekeeper hint
5e21402 fix(mac-daemon): address Codex review round 1
a211ca2 fix(mac-daemon): address Codex review round 2
964cc6a fix(mac-daemon): address Codex review round 3
ecbcb17 fix(mac-daemon): address Codex review round 4
498dcf8 fix(mac-daemon): address Codex review round 5
f544d46 ci(mac-daemon): consolidate swift job into ci.yml workflow
0c6fc11 fix(mac-daemon): address Claude Code Review findings
4465826 fix(mac-daemon): address Codex review round 7
b3f2012 fix(mac-daemon): address Codex review round 8
65f63f4 fix(mac-daemon): rename ShutdownToken -> ShutdownSignal
ad59ed9 test(mac-daemon): switch assertThrows to non-autoclosure for Swift 6
```

## CI status (run 25818298257 against commit `ad59ed9`)

```
Mac Daemon Tests   fail    1m5s    https://github.com/spengrah/PersonalCRM/actions/runs/25818298257/job/75852550690
Backend Tests      pending
E2E Tests          pass    3m36s
Frontend Tests     skipping
Detect Changes     pass    5s
Codex Review       skipping
```

Backend / E2E pass, frontend correctly skipped. **Mac Daemon Tests is the only
blocker.**

### Failing tests (paste of `gh run view 25818298257 --log-failed`)

Two distinct failure clusters, both in `CRMMacLifecycleTests`:

**Cluster A — InstallerFailurePathsTests + InstallerFreshInstallTests** all fail
with: `"crm-mac is already installed. Run \`crm-mac uninstall --purge\` first, or
use --upgrade / --register-only."`

This is `InstallError.alreadyInstalled` raised by `existingInstallDetected()` in
`mac-daemon/Sources/CRMMacLifecycle/Installer.swift` (around line 388–401).

**Root cause hypothesis (HIGH confidence):** the round-8 fix at commit `b3f2012`
made `existingInstallDetected()` return `true` when the launchctl probe THROWS
(safer than failing open). But the test helper `FakeLaunchctlRunner` is now
overzealous: when invoked with no script for `printService`, it returns
`exitCode: 0` (the default), which `existingInstallDetected` interprets as
"service is registered = existing install." This was always the case (look at
`FakeLaunchctlRunner.printService`, line ~40, the default script value is
`[0]`), but earlier rounds' tests were passing — meaning either (a) the local
build I was relying on was caching stale results, or (b) the round-8 fix shifted
semantics in a way I didn't anticipate.

Failed cases include:
- `test409CleansTempBinary`, `test410CleansTempBinaryAndDoesNotPersist`,
  `test5xxCleansTempBinaryAndSurfacesAmbiguous`,
  `testCodesignFailureCleansTempBinaryNoPair`,
  `testLaunchctlBootstrapFailureLeavesBinaryInPlace`,
  `testNetworkErrorSurfacesAmbiguous`,
  `testPlistWriteFailureLeavesBinaryInPlaceAsLaunchctlFailed`
  (all in `InstallerFailurePathsTests.swift`)
- `testDirectoriesCreated`, `testHappyPathPersistsAndBootstraps`,
  `testRequiresHostname`, `testEmptyPairingTokenRejected`
  (all in `InstallerFreshInstallTests.swift`)

**The fix is one of:**
1. Change `FakeLaunchctlRunner.Script.printService` default from `[0]` to `[1]`
   (means "service not registered"; tests that DO want it registered already
   override the script).
2. OR each test explicitly initializes `FakeLaunchctlRunner` with
   `Script(printService: [1])`.

Option (1) is the right one — exit 1 (service unknown) is the "no existing
install" baseline, and the tests that DO want a registered service already set
`script.printService = [0]` explicitly (see `testRefusesWhenLaunchctlReportsRegistered`).

**Cluster B — DoctorTests + HeartbeatLoopTests + InstallRequestParserTests**:

- `DoctorTests.testAllPass` line 36 fails on `XCTAssertTrue` — likely related to
  a status mismatch (the test asserts all 4 checks PASS but one is reporting
  WARN, possibly because the same FakeLaunchctlRunner default of `[0]` is now
  inverted by my round-7 launchctl-spawn-failure change).
- `DoctorTests.testPi401Fails` line 81: `("warn") is not equal to ("fail")`.
  This is a logic regression — when the Pi returns 401 the test expects FAIL
  but Doctor's `checkPiReachability` is returning WARN. This is **probably a
  separate pre-existing bug** introduced by an earlier round when I added the
  401 → FAIL mapping but the actual switch in Doctor.swift maps
  `PiClientError.authenticationRevoked` to FAIL while other 4xx → WARN. Verify
  by reading the test fixture: `heartbeat_401.json` returns `UNKNOWN_HOST` →
  PiClient routes that to `.authenticationRevoked` → Doctor maps to FAIL. So
  why WARN? Possibly the test is using a fresh-config setup that doesn't
  read the Keychain, so reachability is skipped with WARN-equivalent.
  **Investigate the test setup carefully** — I never re-ran tests after my
  round-7 commit because XCTest doesn't work locally (no full Xcode install).
- `HeartbeatLoopTests.test200RecordsHeartbeatAndContinues` line 26: expects 1
  state-write recorded, got 0. Likely the `RecordingHeartbeatStateWriter` is
  not being invoked because the test wires `OnDiskHeartbeatStateWriter` instead
  (post-extract from round-1/round-2 reorg). Check what writer the test passes
  — if it's `OnDiskHeartbeatStateWriter` against a StateStore that has no
  initial state file, the load fails and the `recordSuccessfulHeartbeat` early-
  returns via the catch.
- `HeartbeatLoopTests.test401RequestsExitOne` / `test412RequestsExitTwo` —
  "expected exit thrown" — the test expects `try await loop.tick()` to throw
  `ExitRequested`. The current HeartbeatLoop.tick() body catches PiClientError
  cases and calls `try exitHandler.requestExit(N)`. The CapturingExitHandler
  throws `ExitRequested(code:)`. The test then does `XCTAssertEqual(exitHandler.capturedCodes, [N])`.
  Failure mode "expected exit thrown" means the catch matched but the
  requestExit call didn't propagate. **Verify** the do/catch in HeartbeatLoop.tick:
  the `try exitHandler.requestExit(N)` is INSIDE the catch block — the catch
  swallows the thrown `ExitRequested` if it's not re-thrown.
  Looking at the file `mac-daemon/Sources/CRMMacLifecycle/HeartbeatLoop.swift`:
  the structure is `} catch PiClientError.authenticationRevoked(let m) { try exitHandler.requestExit(1) }` —
  the `try` inside a `catch` block doesn't re-throw the inner error. **Need to
  rethrow `ExitRequested` from the catch** so the test observes it.
- `InstallRequestParserTests.testMalformedURL` line 149: passes `" "` and
  expects `.malformedPiURL` but gets `.invalidPiURL` (because URL(string:" ")
  actually returns a valid-but-empty-host URL on this Swift, which then fails
  validatePiURL). Fix: change the malformed test input to something URL(string:)
  truly rejects, like an empty string... but empty is caught earlier as
  `.piURLRequired`. Use a string with control characters or invalid percent
  encoding. Or change the test to expect `.invalidPiURL` for this input
  shape.

## Outstanding Codex findings

**Last Codex round (round 9, commit `b3f2012`) returned RESULT=PASS.** No
outstanding findings from Codex. The CI breakage is from test-level issues that
only surface when XCTest actually runs (which I cannot do locally).

## In-flight fix attempts

I was just starting to debug the failed CI run when the handoff message arrived.
I had identified the broad shape (FakeLaunchctlRunner default + HeartbeatLoop
catch-swallow + Doctor 401 mapping + URL-malformed test input) but had not
written any fix yet.

## Non-obvious things learned during implementation

- **The repo-root `.gitignore` has `*token*` line 324** that catches files like
  `ShutdownToken.swift` and `pair_410_invalid_token.json`. I hit this twice:
  once renaming the fixture to `pair_410_invalid_pair.json` (commit `474ed40`),
  once renaming the type `ShutdownToken` → `ShutdownSignal` (commit `65f63f4`).
  **Be aware:** any new Swift file or fixture with `token` in the name will
  silently be excluded from `git add`. The fix is renaming the file, not adding
  an override (the gitignore is there to catch leaked auth tokens — the rule is
  load-bearing).
- **`swift test` doesn't work without a full Xcode install.** I had only Command
  Line Tools locally (XCTest missing). I verified `swift build` clean throughout
  but only CI on the macos-15 runner exercises `swift test`. Most of the CI
  failures we're seeing are tests that would have surfaced locally with a full
  Xcode install. The fresh agent should consider running `xcode-select -p` to
  verify which toolchain is selected — if it's `/Library/Developer/CommandLineTools`,
  XCTest will fail with `no such module 'XCTest'`.
- **Swift 6 autoclosure async-throws strict mode.** On macos-15's Swift 6 the
  `@autoclosure () async throws -> T` pattern requires explicit `try await` at
  the call site. Local 6.1.2 was lenient. Fixed in commit `ad59ed9` by switching
  to a non-autoclosure closure.
- **Test target dependency on executable target is brittle.** The plan
  deliberately ships no test target for `crm-mac` (the executable). All
  branchy CLI logic was extracted to `CRMMacLifecycle.InstallRequestParser` so
  it could be unit tested without depending on the executable target.
- **CRMMacCore must stay Foundation-only.** Several test files in
  `Tests/CRMMacLifecycleTests/` needed an explicit `import CRMMacCore` because
  SwiftPM does not propagate transitive deps to test targets. The CI build
  failed once (commit `5e21402`) because `UninstallerTests.swift` was missing
  this import; the same class of bug exists if a future test references
  `NoopLogger`, `FixedClock` (now in test fakes), `DaemonConfig`,
  `DaemonState`, etc. without importing CRMMacCore.
- **OSLogLogger renders `.private` metadata as `<redacted>` at compose time.**
  Per Codex round 1 P2: `os_log("%{public}@", ...)` is fixed-format, so we
  can't dynamically vary the privacy modifier per-key. The compose() helper
  redacts at the call site instead so the bytes that reach the unified log
  contain no PII. Operators can override in Console.app's "Include Private
  Data" only for the keys we explicitly tag `.public`.
- **`FileManager.replaceItemAt` is the atomic-rename primitive.** Round-1 P2:
  the old code did `removeItem + moveItem` (two-step, not atomic). Switched
  to `replaceItemAt` which wraps `renamex_np(2)` on a same-fs target.
- **`existingInstallDetected` fails closed.** Round-7 P2: on launchctl probe
  failure (spawn or throw), return `true` so install is refused. This is the
  immediate cause of cluster A above — the test helper's default exit-code
  conflicts.

## Remaining plan checkpoints (cross-reference with `.ai/log/plan/mac-daemon-phase-1-pr6-daemon-skeleton.md`)

| Plan section | Status |
|---|---|
| § "File inventory → New files" | Fully landed |
| § "File inventory → Modified files" | Fully landed |
| § "Package.swift structure" | Fully landed |
| § "Target dependency rules" | Fully landed |
| § "Architectural decisions A1-A5" | Fully landed |
| § "Testing strategy → Unit tests" | Mostly landed; CI exposed real failures (see above) |
| § "Testing strategy → Integration tests" | Fully landed (no in-CI integration tests; URLProtocol-mocked + manual smoke per plan) |
| § "CI workflow" | **Consolidated into `.github/workflows/ci.yml`** (per coordinator mid-flight direction; plan updated to match) |
| § "Risks and mitigations" | All addressed |
| § "Open items (defer to PR7/PR8)" | Documented; no code TODOs |
| § "Steps in implementation order 1-13" | All landed |
| § "Definition of done" | Last checkbox blocked on CI green |

## What the fresh agent should do FIRST

1. **`git fetch && git log --oneline f4055f0..HEAD`** to verify the branch state matches the list above.
2. **`gh pr checks 307`** to confirm CI is still red on `Mac Daemon Tests` (the run id will
   have advanced; just look at the latest).
3. **Read this handoff doc fully**, especially Cluster A and Cluster B failure analysis.
4. **Fix Cluster A first** (one-line change to `FakeLaunchctlRunner.Script.printService`
   default at `mac-daemon/Tests/CRMMacLifecycleTests/Fakes/FakeLaunchctlRunner.swift`
   line ~13). This unblocks 11 failing tests in one shot.
5. **Then debug Cluster B** test by test. Start with HeartbeatLoop because the
   error message suggests a concrete catch-rethrow issue — read
   `mac-daemon/Sources/CRMMacLifecycle/HeartbeatLoop.swift` carefully.
6. **After all tests pass, push, watch CI to green, then check Claude Code Review.**
   If Claude bot returns CHANGES_REQUESTED again, address findings via the
   Codex `task --resume-last` loop in `/Users/spencer/.claude/skills/implement-and-review/SKILL.md`.

If you fix all failing tests and CI goes green, the PR is ready for the human's
final review and merge — do **not** merge autonomously.

## Useful commands

```bash
# Local build (production target only; tests need full Xcode)
cd mac-daemon && swift build -c release -Xswiftc -warnings-as-errors

# Watch CI
gh pr checks 307 --watch

# Inspect a specific failing run
gh run view <run-id> --log-failed

# Codex iteration (resume the prior thread)
node /Users/spencer/.claude/plugins/marketplaces/openai-codex/plugins/codex/scripts/codex-companion.mjs \
  task --resume-last --effort xhigh \
  "Fixes applied: <summary>. Please re-review. End with RESULT=PASS or RESULT=FAIL."
```

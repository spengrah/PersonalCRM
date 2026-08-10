#!/usr/bin/env bash
# qa-nightly-round.sh — PURE, STATELESS, PARAMETERIZED orchestrator for ONE nightly
# QA judge round. It sequences the in-repo QA pipeline (which versions in lockstep
# with this script) and emits a machine-readable decision for the host wrapper to act
# on. It NEVER checks out, NEVER reseeds/deploys by itself, NEVER persists a watermark
# or any other state, and NEVER reads host facts directly — every host-specific is
# INJECTED as env/args. The wrapper (personal-ops, host-entangled) owns the checkout,
# supplying the reseed/deployed-sha commands + creds, and persisting the watermark
# from the `advance=`/`target=` decision this script emits.
#
# INJECTED INPUTS (env):
#   QA_BASE_SHA          last-round deployed sha        -> cadence gate <BASE>
#   QA_HEAD_SHA          currently-deployed staging sha -> cadence gate <HEAD>, the
#                        pre-tours pin, and (when the round is clean) the advance target
#   QA_DAYS_SINCE        whole days since the last round -> cadence gate <DAYS_SINCE>
#   QA_JUDGE_TRACE       FRESH per-round JSONL path the wrapper supplies (the judge
#                        appends spans here; the whole file is exported). REQUIRED on a
#                        run; a reused path only self-warns (export's sidecar guard).
#   QA_RESEED_CMD        shell command that reseeds staging (optional; abort-on-fail).
#                        When set, tours run with TOURS_SKIP_RESET=1 so tours does not
#                        double-reseed. When unset (local/dev), tours reset themselves.
#   QA_DEPLOYED_SHA_CMD  shell command that PRINTS the current deployed staging sha,
#                        RE-READ after tours so a mid-round redeploy is caught.
#   LANGFUSE_*           export creds (passed through the environment to qa-export).
#   TOURS_*              tours env (TOURS_BASE_URL/…); passed through to `make tours`.
#   QA_SALT_PASSES       optional; passed through to qa-export.
#   QA_MAKE              test seam: the `make` to invoke (default `make`).
#
# THE PIPELINE IS THREE DISTINCT TARGETS (not one) — reflected in this sequence:
#   `make tours`             run-tours.sh: (skippable) reseed + Playwright tours ->
#                            captures + manifest.json in frontend/tests/tours/.runs/<id>/
#   `make qa-report JUDGE=1` judge + TRAP self-test; its EXIT CODE is the hard trap
#                            signal (non-zero = a missed/non-executable trap, e.g. a
#                            tour failure left a behavior uncaptured). Writes QA_JUDGE_TRACE.
#                            It does NOT export.
#   `make qa-export`         ships the spans and PRINTS the RESULT counts (traces /
#                            screenshots / observations / FAILED / enqueue
#                            attempted-ok-skipped-failed);
#                            exits 1 iff a trace failed to ship. Run as an INDEPENDENT
#                            step (`;`, never `&&`) so a trap-miss trace STILL ships its
#                            diagnostic — the round's clean/incomplete grade is computed
#                            afterward, it never gates the ship.
#
# SEQUENCE:
#   1. cadence gate -> run_round. Emit the gate's decision.
#   2. run_round=false -> emit round=skipped, advance=false, exit 0. (Nothing else runs.)
#   3. run_round=true:
#      a. pre-tours pin: assert the checkout HEAD == QA_HEAD_SHA (the wrapper is
#         assumed to have checked it out; this PROVES it). Fail -> round=aborted, exit≠0.
#      b. atomic run-dir reservation, then price apply (fail-open; converges Langfuse's
#         project-scoped model prices to the checked-in file — see the load-bearing
#         position note at its call site), then reseed via QA_RESEED_CMD (if set);
#         reseed non-zero -> round=aborted, exit≠0.
#      c. tours (fresh TOURS_RUN_ID we mint = the run dir + the QA_RUN_ID provenance) ->
#         qa-report JUDGE=1 (capture the trap exit) ->  ; qa-export (capture RESULT
#         counts) -> cost assertion (fail-open; warns without gating — see its call site).
#      d. re-read the deployed sha; post-tours provenance assert vs <RUNDIR>/manifest.json
#         (a mid-round redeploy makes CURRENT != HEAD -> assert fails -> do not advance).
#      e. CLEAN-ROUND PREDICATE (conservative; ANY ambiguity -> advance=false, the round
#         re-runs next cadence). advance=true ONLY if the round is provably complete.
#      f. clean -> advance=true target=QA_HEAD_SHA; else advance=false + why.
#   4. EMIT advance=true|false + target=<sha>. This script NEVER writes a watermark.
#
# CLEAN-ROUND PREDICATE — computed over the signals that EXIST TODAY:
#   tours exit == 0 (a tour failure / incomplete coverage must NOT advance)
#   AND qa-report exit == 0 (no missed/non-executable trap)
#   AND qa-export exit == 0 AND shipped-FAILED == 0 (every trace shipped)
#   AND traces > 0 (something shipped; a missing-creds no-op ships nothing -> no advance)
#   AND enqueue-failed == 0 (no dropped triage POST)
#   AND enqueued + already-queued >= 1 (the mandatory fail/trap enqueue actually landed;
#       a silently-skipped enqueue pass, e.g. an unresolved queue, is a partial-enrichment
#       loss and must NOT advance)
#   AND the post-tours provenance assert passed.
#
# A direct EXPECTED-vs-EXECUTED capture signal does not exist yet: render.ts renders
# behavior coverage from collected grades + a static skip-list (it does not prove every
# scheduled capture executed), and manifest.json holds provenance, not an
# expected/executed set. So this predicate cannot verify "all scheduled captures present"
# directly; it approximates completeness via the trap self-test exit (a trap on an
# uncaptured behavior goes non-executable -> non-zero) + the tours exit, and stays
# CONSERVATIVE — any ambiguity degrades to advance=false and the round simply re-runs.
# A missing signal is never fabricated; it degrades to not advancing.
#
# KNOWN RESIDUALS (need signals/isolation outside this orchestrator; tracked for the follow-up
# wrapper/exporter work — the gate still FAILS SAFE around them):
#   - Per-trace media completeness: the export summary reports only the TOTAL screenshot count,
#     so an all-media-failed round is caught but a per-trace partial loss is not (needs the
#     expected-vs-executed capture signal).
#   - Judge-span error status: an LLM judge call that ERRORS into `unsure` without failing a
#     trap is not visible from the export counts (needs the exporter to surface a judge-error
#     count); the trap self-test exit is the current best judge-health proxy.
#   - Judge env isolation: `env -u` strips the injected command vars + the pipeline's API creds
#     from the qa-report child, but the CRM tooling (bun/next) reloads frontend/.env.local, which
#     can re-introduce app env (e.g. NEXT_PUBLIC_* — public by convention). Fully isolating the
#     judge's Codex subprocess from .env.local is a broader concern than this orchestrator.
#
# EXIT CONTRACT: a round that RAN and produced a decision — clean OR incomplete — exits 0
# with advance= reflecting cleanliness, so the wrapper reads the decision from the emitted
# key=values. Non-zero means the decision is NOT a normal round outcome:
#   - round=aborted (via abort(), exit 1 — except a cadence-gate failure propagates the
#     gate's own exit code): could not resolve the script/repo root, the cadence-gate
#     script is missing, the cadence gate itself failed, QA_JUDGE_TRACE is unset, the
#     pre-tours pin failed, the reseed failed, or the run dir could not be reserved (a
#     run-id collision OR a filesystem/permission error creating it).
#   - exit 3 from emit(): a $GITHUB_ENV write failed, so a decision (of any kind) did not
#     propagate — surfaced loudly rather than a silent exit 0.
# In every case the wrapper must NOT advance the watermark on a non-zero exit.

set -uo pipefail   # NO -e: a failed sub-step must be graded (degrade), not abort silently.

# Sanitize the repo's standard git-location isolation set so nothing this script (or a
# child it spawns — make/tours/qa-report, which run git) inherits from a poisoned caller
# env (e.g. a git hook) resolves the wrong repo. The sibling gate/assert scripts also
# unset these internally; clearing here keeps a leaked var from ever reaching `git`.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY
# Also clear an INHERITED TOURS_SKIP_RESET: this orchestrator decides reset/reseed itself
# (skip only when it reseeded via QA_RESEED_CMD). If the caller's env already carried
# TOURS_SKIP_RESET=1, an un-reseeded round would run tours against stale staging yet still
# be graded clean — so start from a known-clear state and set it explicitly only when reseeding.
unset TOURS_SKIP_RESET

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." 2>/dev/null && pwd)"
CADENCE_GATE="$SCRIPT_DIR/qa-round-cadence-gate.sh"
SHA_ASSERT="$SCRIPT_DIR/qa-round-deployed-sha-assert.sh"
MAKE="${QA_MAKE:-make}"
RUNS_ROOT="$REPO_ROOT/frontend/tests/tours/.runs"

emit() {
  # emit <key> <value>: stdout for humans/tests + $GITHUB_ENV for the wrapper/CI. A lost
  # $GITHUB_ENV append means the decision did not propagate -> fail VISIBLY (non-zero +
  # stderr) rather than exit 0 having written only to stdout (the sibling gate's contract).
  # The value is normalized to a single line first: the wrapper parses key=value lines, so
  # a newline/CR embedded in a value (e.g. reached from raw external command output) must
  # never inject a spurious extra key=value line into stdout or $GITHUB_ENV.
  local v="$2"
  v="${v//$'\n'/ }"
  v="${v//$'\r'/ }"
  printf '%s=%s\n' "$1" "$v"
  if [ -n "${GITHUB_ENV:-}" ]; then
    if ! printf '%s=%s\n' "$1" "$v" >> "$GITHUB_ENV"; then
      printf 'qa-nightly-round: failed to write %s to GITHUB_ENV (%s)\n' "$1" "$GITHUB_ENV" >&2
      exit 3
    fi
  fi
}

# abort <reason> [code]: the round could not validly start. Emit the aborted decision
# and exit non-zero (the wrapper must NOT advance and should surface the failure).
abort() {
  printf 'qa-nightly-round: ABORT — %s\n' "$1" >&2
  emit round aborted
  emit advance false
  emit target ""
  emit reason "$1"
  exit "${2:-1}"
}

QA_BASE_SHA="${QA_BASE_SHA:-}"
QA_HEAD_SHA="${QA_HEAD_SHA:-}"
QA_DAYS_SINCE="${QA_DAYS_SINCE:-}"

[ -n "$SCRIPT_DIR" ] && [ -n "$REPO_ROOT" ] || abort "could not resolve script/repo root"

# ---------------------------------------------------------------------------
# 1. Cadence gate — decides whether to run at all. It emits its own four-flag tuple
#    (run_round/judge_relevant_change/base_known/changed_groups) to stdout AND, since it
#    inherits $GITHUB_ENV, appends them there itself. We re-print its stdout (visibility)
#    and read run_round to branch. A non-zero gate rc is its lost-decision signal (a
#    failed $GITHUB_ENV write) — propagate it rather than proceed on an untrusted tuple.
# ---------------------------------------------------------------------------
[ -x "$CADENCE_GATE" ] || [ -f "$CADENCE_GATE" ] || abort "cadence gate not found at $CADENCE_GATE"
gate_out="$(bash "$CADENCE_GATE" "$QA_BASE_SHA" "$QA_HEAD_SHA" "$QA_DAYS_SINCE")"
gate_rc=$?
printf '%s\n' "$gate_out"
if [ "$gate_rc" -ne 0 ]; then
  abort "cadence gate exited $gate_rc (decision not trustworthy)" "$gate_rc"
fi
run_round="$(printf '%s\n' "$gate_out" | grep -E '^run_round=' | head -1 | cut -d= -f2-)"

# ---------------------------------------------------------------------------
# 2. Skip — nothing else runs.
# ---------------------------------------------------------------------------
if [ "$run_round" != "true" ]; then
  emit round skipped
  emit advance false
  emit target ""
  emit reason "cadence gate: run_round=${run_round:-<unset>}"
  exit 0
fi

# ---------------------------------------------------------------------------
# 3. Run the round.
# ---------------------------------------------------------------------------

# The judge trace path is mandatory on a run — the judge writes it and qa-export ships
# it. A run without it cannot produce a gradeable round, so this is a start-time abort.
[ -n "${QA_JUDGE_TRACE:-}" ] || abort "QA_JUDGE_TRACE unset — the wrapper must supply a fresh per-round trace path"
# The judge APPENDS spans to QA_JUDGE_TRACE and the exporter ships the WHOLE file, so a path
# reused across rounds would mix stale spans into this round's export (and the exporter's
# stale-round WARNING only fires when a provenance sidecar exists to disagree). Guarantee
# freshness here by TRUNCATING the trace to empty at the start of the round — the wrapper
# should also hand a fresh path, but this makes the round self-contained regardless.
: > "$QA_JUDGE_TRACE" 2>/dev/null || abort "cannot initialize (truncate) trace file $QA_JUDGE_TRACE"

# 3a. Pre-tours pin: prove the checkout the tours will run FROM is the deployed sha.
if ! bash "$SHA_ASSERT" "$QA_HEAD_SHA"; then
  abort "pre-tours deployed-sha assert failed — checkout not pinned to $QA_HEAD_SHA"
fi

# 3b. Reserve the run dir + validate the run id BEFORE the destructive reseed, so an
#     invalid id / collision / filesystem error aborts WITHOUT having reseeded staging.
# The run id doubles as the run-dir name (globalSetup honors a pre-set TOURS_RUN_ID) AND
# the QA_RUN_ID export provenance (its %Y%m%dT%H%M%SZ form is a valid run id). QA_GIT_SHA
# = the deployed HEAD. run-tours.sh derives manifest.gitSha from the pinned checkout HEAD.
# QA_RUN_ID_OVERRIDE is a test seam (fix the id so a fixture can construct a collision);
# unset in production, so a fresh second-granular UTC id is minted each round.
RUN_ID="${QA_RUN_ID_OVERRIDE:-$(date -u +%Y%m%dT%H%M%SZ)}"
# Validate the run id HERE, before it flows into RUNDIR_ABS + the mkdir reservation below:
# a traversal value (e.g. an `../..`-bearing QA_RUN_ID_OVERRIDE) would otherwise create a
# directory OUTSIDE .runs at the reservation step, before run-tours.sh's own check runs. The
# date-minted default always matches; only a bad override is rejected.
[[ "$RUN_ID" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || abort "invalid run id ($RUN_ID) — must be a UTC run-id timestamp (YYYYMMDDTHHMMSSZ)"
RUNDIR_ABS="$RUNS_ROOT/$RUN_ID"
RUNDIR_REL="tests/tours/.runs/$RUN_ID"   # relative to frontend/, as qa-report expects

# The run id is only second-granular, so two rounds in the same second (a rapid retry or
# a concurrent invocation) could collide on the run dir; globalSetup mkdir -p's it without
# rejecting a pre-existing dir, which would silently POOL two rounds' captures and break
# fresh-per-round. RESERVE the dir ATOMICALLY: a non-recursive `mkdir` (no -p) succeeds
# only if it did not already exist, so it is a race-free claim — a check-then-act test
# would let two concurrent rounds both see it absent and then share it. mkdir -p on the
# parent first (that is not the raced resource); the leaf mkdir is the reservation. tours'
# own `mkdir -p` is then a harmless no-op on the dir we pre-created.
mkdir -p "$RUNS_ROOT" 2>/dev/null
if ! mkdir "$RUNDIR_ABS" 2>/dev/null; then
  # mkdir can fail because the dir already existed (a real run-id collision) OR for an
  # I/O / permission / missing-parent error — distinguish so the reason is accurate.
  if [ -e "$RUNDIR_ABS" ]; then
    abort "run dir already exists ($RUNDIR_ABS) — run-id collision; refusing to reuse it (fresh-per-round)"
  else
    abort "could not create run dir ($RUNDIR_ABS) — filesystem/permission error"
  fi
fi

# 3b-post. Model-price apply (fail-open). Stale prices make cost APPROXIMATE; a
# skipped round makes the night's QA ABSENT — so a failure here logs loudly, records
# a manifest field, and CONTINUES.
#
# WHERE THIS CALL SITS IS LOAD-BEARING — three constraints, none of them obvious.
# The order is: preflight -> run reservation -> THIS -> reseed -> tours -> export.
#   1. AFTER the preflight (trace path + the pre-tours deployed-sha pin). This is the
#      round's first EXTERNAL mutation, and creating or deleting model definitions
#      from a checkout that then turns out not to be the deployed sha — or for a round
#      that aborts a line later on a missing trace path — changes pricing state for a
#      round that never ran.
#   2. AFTER the atomic run-dir reservation. The run id is only second-granular, so
#      two invocations can race; the reservation is what picks a winner. Applying
#      first lets BOTH interleave delete-then-create against shared Langfuse pricing
#      state, and lets the loser mutate prices (or make the winner report a failed
#      apply) before aborting on the collision.
#   3. BEFORE tours and the export, because Langfuse resolves a model's price when the
#      observation is INGESTED. That is the only one of the three that is about
#      correctness of the cost numbers; the other two are about not mutating shared
#      state on behalf of a round that will not happen.
# Do not reorder without re-satisfying all three.
# ROUND_START_ISO is captured here (before reseed/tours) so the post-export cost
# assertion below can scope its query to exactly this round's observations.
# Same env discipline as every other make child: the injected QA_*_CMD vars can embed
# secrets and apply consumes none of them, nor the tours API key. It DOES need
# LANGFUSE_* — those are its whole purpose — so those stay.
ROUND_START_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
price_apply_out="$( cd "$REPO_ROOT" && env -u QA_RESEED_CMD -u QA_DEPLOYED_SHA_CMD \
  -u QA_DEPLOYED_GEN_CMD -u TOURS_API_KEY "$MAKE" model-prices-apply 2>&1 )" \
  && price_apply_rc=0 || price_apply_rc=$?
printf '%s\n' "$price_apply_out"
if [ "$price_apply_rc" -ne 0 ]; then
  price_apply_status=failed
  emit price_apply_reason "model-prices-apply exited $price_apply_rc"
else
  price_apply_status=ok
fi
emit price_apply "$price_apply_status"

# has_nul <file>: true (0) if the file contains a NUL byte. bash $()/$(<) SILENTLY STRIP
# NUL bytes, so an injected reader returning e.g. "A\0B" would compare EQUAL to "AB" and
# could wrongly permit advance; we compare raw byte counts to reject/flag such reads.
has_nul() { [ "$(wc -c < "$1" 2>/dev/null)" != "$(tr -d '\000' < "$1" 2>/dev/null | wc -c)" ]; }

# Optional monotonic deploy-GENERATION seam. The post-tours sha-equality pin catches a
# deploy that STAYS on a different sha, but NOT an A->B->A redeploy+rollback WITHIN the
# round: staging moves to B during tours then rolls back to the target before the post
# read, so every sha (reader / checkout / manifest) reads the target while the captures
# are mixed-version — sha alone cannot see it. If the wrapper injects QA_DEPLOYED_GEN_CMD
# (a command printing a monotonic deploy generation — a counter or deploy timestamp), we
# capture it BEFORE the reseed+tours window and compare AFTER: any redeploy bumps the generation even on a
# rollback to the same sha -> detected -> no advance. When UNSET, that case is UNDETECTABLE
# by sha alone and is a known best-effort limitation until the wrapper supplies a
# generation source (a deploy-system concern, same shape as the expected-vs-executed gap).
# The emitted deploy_gen_checked records which mode ran. gen values are opaque host text —
# they are compared, never emitted, so they cannot leak into the decision output.
gen_pre=""
gen_pre_ok=true
if [ -n "${QA_DEPLOYED_GEN_CMD:-}" ]; then
  gen_pre_file="$RUNDIR_ABS/.gen-pre-read"
  bash -c "$QA_DEPLOYED_GEN_CMD" >"$gen_pre_file" 2>/dev/null
  gen_pre_rc=$?
  gen_pre="$(cat "$gen_pre_file" 2>/dev/null)"
  # A NUL in the raw pre-read makes the (NUL-stripped) string compare unreliable -> untrusted.
  ! has_nul "$gen_pre_file" || gen_pre_ok=false
  rm -f "$gen_pre_file"
  # Only the pre-read EXIT STATUS is checked here (an errored read is untrustworthy even if
  # it printed a value). An empty-but-rc0 pre-read is deliberately NOT rejected here: the
  # post-read guard below already refuses to advance on an empty gen_post (and a persistently
  # empty reader yields gen_post="" too), so a separate pre-empty check would be redundant
  # and untestable in isolation.
  [ "$gen_pre_rc" -eq 0 ] || gen_pre_ok=false
fi

# 3c. Reseed (abort-on-fail). When we reseed here, tours skip their own reset.
# QA_RESEED_CMD is arbitrary host-supplied text that can embed credentials / private
# hostnames / paths, so on failure the emitted reason carries ONLY a fixed message + the
# exit code — never the command contents (the repo's never-echo-secrets rule).
skip_reset=()
if [ -n "${QA_RESEED_CMD:-}" ]; then
  # DISCARD (do not inherit) the reseed command's stdout+stderr: QA_RESEED_CMD is arbitrary
  # host text that can PRINT secret-bearing content (credentials, private hostnames) or emit
  # a shell diagnostic naming them, so its output must never reach the round's output. On
  # failure the abort below carries ONLY a fixed message + the exit code.
  ( cd "$REPO_ROOT" && bash -c "$QA_RESEED_CMD" ) >/dev/null 2>&1
  reseed_rc=$?
  if [ "$reseed_rc" -ne 0 ]; then
    abort "reseed command failed (exit $reseed_rc)"
  fi
  skip_reset=(TOURS_SKIP_RESET=1)
fi



# Tours are NON-FATAL here: a tour failure still yields a diagnostic-bearing round (the
# trap self-test + export surface it). We capture the exit for the clean-round predicate
# — a failed/incomplete tours run must not advance — but do not abort on it.
# The ${arr[@]+"${arr[@]}"} form expands to nothing (no unbound error) when skip_reset
# is empty — required under `set -u` on bash 3.2 (the macOS judge host's default bash).
# The injected QA_*_CMD vars can embed secrets; strip them from every make child
# (qa-report spawns the Codex judge, which would otherwise inherit them) — the
# orchestrator consumes those commands itself, the children never need them.
( cd "$REPO_ROOT" && env -u QA_RESEED_CMD -u QA_DEPLOYED_SHA_CMD -u QA_DEPLOYED_GEN_CMD TOURS_RUN_ID="$RUN_ID" ${skip_reset[@]+"${skip_reset[@]}"} "$MAKE" tours )
tours_rc=$?

# Judge + trap self-test. Its EXIT CODE is the hard trap signal (0 = all traps caught).
# qa-report runs the LLM JUDGE (a Codex subprocess that reads untrusted tour captures). It
# needs none of the pipeline's API secrets — tours owns TOURS_*; export owns LANGFUSE_* — so
# strip them from the judge's environment too, alongside the injected command vars, so a
# prompt-injected or misbehaving judge cannot read/exfiltrate a credential it never needs.
( cd "$REPO_ROOT" && env -u QA_RESEED_CMD -u QA_DEPLOYED_SHA_CMD -u QA_DEPLOYED_GEN_CMD \
    -u TOURS_API_KEY -u LANGFUSE_SECRET_KEY -u LANGFUSE_PUBLIC_KEY -u LANGFUSE_HOST \
    QA_JUDGE_TRACE="$QA_JUDGE_TRACE" "$MAKE" qa-report JUDGE=1 RUNDIR="$RUNDIR_REL" )
report_rc=$?

# Export — INDEPENDENT `;` step (INV-B: a trap-miss trace still ships). Capture the RESULT
# summary line + exit. Provenance (QA_RUN_ID/QA_GIT_SHA) rides the environment.
export_out="$( cd "$REPO_ROOT" && env -u QA_RESEED_CMD -u QA_DEPLOYED_SHA_CMD -u QA_DEPLOYED_GEN_CMD QA_RUN_ID="$RUN_ID" QA_GIT_SHA="$QA_HEAD_SHA" "$MAKE" qa-export TRACE="$QA_JUDGE_TRACE" 2>&1 )"
export_rc=$?
printf '%s\n' "$export_out"

# Cost assertion (warn-only, fail-open): confirms the round's own freshly-exported
# generation observations carry non-zero cost, scoped to this round via
# FROM=ROUND_START_ISO (captured before reseed/tours, above). Failure feeds `notes`
# (visibility), never `reasons` — it must never gate advancement, matching the
# round's existing degradation channel.
cost_check_out="$( cd "$REPO_ROOT" && env -u QA_RESEED_CMD -u QA_DEPLOYED_SHA_CMD \
  -u QA_DEPLOYED_GEN_CMD -u TOURS_API_KEY "$MAKE" qa-cost-assert FROM="$ROUND_START_ISO" 2>&1 )" \
  && cost_check_rc=0 || cost_check_rc=$?
printf '%s\n' "$cost_check_out"
if [ "$cost_check_rc" -ne 0 ]; then
  cost_check_status=failed
else
  cost_check_status=ok
fi
emit cost_check "$cost_check_status"

# Parse the RESULT counts from qa-export's ONE canonical summary line. run.ts prints
# EXACTLY (langfuse.ts ExportResult):
#   "qa-export: N trace(s), M screenshot(s)[, O observation(s)][, J observation(s) failed][, K observation(s) skipped][, F FAILED]; enqueued E/A[, S already queued][, X enqueue-failed]"
# The observation field is matched OPTIONALLY so the orchestrator is not order-coupled to
# a cosmetic change in run.ts and still parses an older exporter's line.
# We do NOT scan the merged stdout+stderr for field fragments: the exporter also logs a
# pre-summary "enqueued E/A" line and can embed up to ~200 chars of an external HTTP error
# body (which could itself contain a string like "enqueued 1/1") — a fragment scan could
# pick either over the real final counts and manufacture a false-clean. Instead, match the
# WHOLE line against an anchored regex and require EXACTLY ONE such line; a
# missing/duplicate/malformed summary is treated as no-data (parsed as zeros -> no advance,
# plus an explicit duplicate reason). Fields are then read from that single clean line only.
SUMMARY_RE='^qa-export: [0-9]{1,9} trace\(s\), [0-9]{1,9} screenshot\(s\)(, [0-9]{1,9} observation\(s\))?(, [0-9]{1,9} observation\(s\) failed)?(, [0-9]{1,9} observation\(s\) skipped)?(, [0-9]{1,9} FAILED)?; enqueued [0-9]{1,9}/[0-9]{1,9}(, [0-9]{1,9} already queued)?(, [0-9]{1,9} enqueue-failed)?$'
summary_count="$(printf '%s\n' "$export_out" | grep -cE "$SUMMARY_RE")"
summary_line="$(printf '%s\n' "$export_out" | grep -E "$SUMMARY_RE" | tail -1)"

field_num() {
  # field_num <ERE-labeled-count> <default> — extract ONE count from the single summary line.
  local m
  m="$(printf '%s' "$summary_line" | grep -oE "$1" | grep -oE '[0-9]+' | head -1)"
  printf '%s' "${m:-$2}"
}
if [ "$summary_count" -eq 1 ]; then
  traces="$(field_num '[0-9]+ trace\(s\)' 0)"
  screenshots="$(field_num '[0-9]+ screenshot\(s\)' 0)"
  observations="$(field_num '[0-9]+ observation\(s\)' 0)"
  # Anchored on the trailing word, so the leading observations count cannot match it.
  observations_failed="$(field_num '[0-9]+ observation\(s\) failed' 0)"
  observations_skipped="$(field_num '[0-9]+ observation\(s\) skipped' 0)"
  ship_failed="$(field_num '[0-9]+ FAILED' 0)"
  enq_skipped="$(field_num '[0-9]+ already queued' 0)"
  enq_failed="$(field_num '[0-9]+ enqueue-failed' 0)"
  enq_pair="$(printf '%s' "$summary_line" | grep -oE 'enqueued [0-9]+/[0-9]+')"
  enq_ok="$(printf '%s' "$enq_pair" | grep -oE '[0-9]+' | head -1)"; enq_ok="${enq_ok:-0}"
  enq_attempted="$(printf '%s' "$enq_pair" | grep -oE '[0-9]+' | tail -1)"; enq_attempted="${enq_attempted:-0}"
else
  # No summary (missing creds / no spans) OR more than one (malformed/duplicate) -> no
  # trustworthy counts. Zero everything so the predicate degrades to advance=false.
  traces=0; screenshots=0; observations=0; observations_failed=0; observations_skipped=0; ship_failed=0; enq_skipped=0; enq_failed=0; enq_ok=0; enq_attempted=0
fi

# 3d. Post-tours provenance assert: re-read the deployed sha and prove the round both
# STARTED and ENDED on the sha we are about to advance the watermark to (QA_HEAD_SHA).
# This is a fail-closed integrity gate, so "cannot confirm" == "do not advance":
#   - QA_DEPLOYED_SHA_CMD unset -> cannot verify.
#   - the reader command EXITS NON-ZERO -> cannot trust its stdout (a command that prints
#     the expected sha then fails must NOT pass; its stdout is discarded).
#   - the reader's output is not EXACTLY one 40-hex sha (empty / multiline / malformed) ->
#     cannot verify (and must never reach the emit protocol as raw text).
#   - the re-read sha != QA_HEAD_SHA -> a mid-round redeploy moved staging off the round's
#     target. Without this, a deployment + checkout + manifest that ALL move to sha B
#     mid-round would agree with each other (assert passes) while we advance to A — pinning
#     the watermark to a sha the round did not run against. The invariant: end on the target.
#   - then the assert ties the checkout HEAD + the run manifest to that same sha.
# Only a clean, single 40-hex read equal to QA_HEAD_SHA is handed to the assert. post_reason
# is a FIXED string, never interpolated with the raw command output.
is_hex40() { case "$1" in ""|*[!0-9a-f]*) return 1;; esac; [ "${#1}" -eq 40 ]; }
post_ok=true
post_reason=""
if [ -z "${QA_DEPLOYED_SHA_CMD:-}" ]; then
  post_ok=false
  post_reason="QA_DEPLOYED_SHA_CMD unset — cannot verify post-tours provenance"
else
  # Capture the reader's RAW bytes to a file: bash $()/$(…) SILENTLY STRIP NUL bytes, so a
  # malformed "<40hex>\0" response would otherwise be accepted as a clean 40-hex sha on this
  # fail-closed integrity path. We detect a NUL in the raw bytes (a tr-stripped copy differs
  # in size) and reject. The file lives in the run dir we reserved; stderr is discarded so a
  # secret-bearing reader diagnostic cannot leak.
  sha_raw="$RUNDIR_ABS/.deployed-sha-read"
  bash -c "$QA_DEPLOYED_SHA_CMD" >"$sha_raw" 2>/dev/null
  deployed_cmd_rc=$?
  CURRENT="$(cat "$sha_raw" 2>/dev/null)"
  deployed_has_nul=false
  has_nul "$sha_raw" && deployed_has_nul=true
  rm -f "$sha_raw"
  if [ "$deployed_cmd_rc" -ne 0 ]; then
    post_ok=false
    post_reason="deployed-sha reader exited $deployed_cmd_rc — cannot confirm post-tours provenance"
  elif [ "$deployed_has_nul" = true ]; then
    post_ok=false
    post_reason="deployed-sha reader emitted NUL byte(s) (malformed) — cannot confirm post-tours provenance"
  elif ! is_hex40 "$CURRENT"; then
    post_ok=false
    post_reason="deployed-sha reader did not return a single 40-hex sha — cannot confirm post-tours provenance"
  elif [ "$CURRENT" != "$QA_HEAD_SHA" ]; then
    post_ok=false
    post_reason="deployed sha moved mid-round (re-read != the round's target) — cannot advance"
  elif ! bash "$SHA_ASSERT" "$CURRENT" "$RUNDIR_ABS/manifest.json"; then
    post_ok=false
    post_reason="post-tours provenance assert failed (checkout or manifest mismatch)"
  fi
fi

# Deploy-generation check (paired with the pre-tours capture): a changed generation means
# a redeploy happened mid-round even if the sha rolled back to the target. Only evaluated
# when the sha checks passed (one advance=false reason suffices) and the seam is provided.
if [ "$post_ok" = true ] && [ -n "${QA_DEPLOYED_GEN_CMD:-}" ]; then
  if [ "$gen_pre_ok" != true ]; then
    post_ok=false
    post_reason="deploy-generation reader failed pre-tours — cannot confirm no mid-round redeploy"
  else
    gen_post_file="$RUNDIR_ABS/.gen-post-read"
    bash -c "$QA_DEPLOYED_GEN_CMD" >"$gen_post_file" 2>/dev/null
    gen_post_rc=$?
    gen_post="$(cat "$gen_post_file" 2>/dev/null)"
    gen_post_nul=false; has_nul "$gen_post_file" && gen_post_nul=true
    rm -f "$gen_post_file"
    if [ "$gen_post_rc" -ne 0 ] || [ -z "$gen_post" ] || [ "$gen_post_nul" = true ]; then
      post_ok=false
      post_reason="deploy-generation reader failed post-tours — cannot confirm no mid-round redeploy"
    elif [ "$gen_post" != "$gen_pre" ]; then
      post_ok=false
      post_reason="deploy generation changed mid-round (redeploy/rollback detected) — cannot advance"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# 3e. Clean-round predicate — conservative; degrade to NOT advancing on any ambiguity.
# ---------------------------------------------------------------------------
reasons=()
[ "$tours_rc" -eq 0 ]  || reasons+=("tours failed/incomplete (make tours exit $tours_rc)")
[ "$report_rc" -eq 0 ] || reasons+=("trap self-test / judge failed (qa-report exit $report_rc)")
{ [ "$export_rc" -eq 0 ] && [ "$ship_failed" -eq 0 ]; } || reasons+=("trace export failed (exit $export_rc, $ship_failed failed)")
[ "$summary_count" -le 1 ] || reasons+=("ambiguous qa-export summary ($summary_count matching lines)")
[ "$traces" -gt 0 ]    || reasons+=("no traces shipped (nothing to grade / missing creds)")
# Media completeness (coarse): every judged tour trace carries screenshot evidence (the triage
# UI renders it), so traces shipped WITH ZERO total screenshots means the media uploads failed
# (best-effort in the exporter, so export still exits 0) — a degraded round that must not advance.
# RESIDUAL: the summary reports only the TOTAL screenshot count, so this catches an all-media-
# failed round but NOT a per-trace partial loss (trace A uploads, trace B's media fails while
# total stays > 0). Per-trace media accounting needs the expected-vs-executed capture signal that
# does not exist yet (documented above); until it does, this coarse guard is the best available
# and the gate stays conservative elsewhere.
{ [ "$traces" -eq 0 ] || [ "$screenshots" -gt 0 ]; } || reasons+=("traces shipped but 0 screenshots — media upload failed (degraded round)")
[ "$enq_failed" -eq 0 ] || reasons+=("$enq_failed triage enqueue POST(s) failed")
# enqueued + failed must account for EVERY attempted POST. The exporter partitions attempted
# into enqueued + failed, so a summary like "enqueued 2/3" WITHOUT a matching "1 enqueue-failed"
# suffix is truncated/inconsistent — do not treat it as clean even though enq_failed defaults 0.
[ "$((enq_ok + enq_failed))" -eq "$enq_attempted" ] || reasons+=("inconsistent enqueue summary (enqueued $enq_ok + failed $enq_failed != attempted $enq_attempted)")
[ "$((enq_ok + enq_skipped))" -ge 1 ] || reasons+=("mandatory triage enqueue landed 0 items")
# The exporter WARNS (still shipping) on degraded/uncertain provenance — a reused trace path
# relabeling stale spans, an unreadable/inaccessible stale-round sidecar, or unlabelable traces.
# A clean summary can follow such a warning, so the counts alone look fine; treat ANY exporter
# warning as mixed/untrustworthy evidence and do not advance.
if grep -qE '^qa-export: WARNING' <<<"$export_out"; then
  reasons+=("exporter emitted a degradation/uncertainty warning (stale or degraded provenance)")
fi
$post_ok || reasons+=("$post_reason")

# NOTES — degradations that are VISIBLE but deliberately NOT part of the clean-round
# predicate. `reasons` drives advance=false, so anything appended there changes
# whether the watermark moves; a note records the degradation without making that
# call. The observation breaker is the first: a round whose cost data is mostly
# missing is worth seeing in the round's own output, but whether that should also
# block advancement is a semantics change that belongs in its own change.
notes=()
observations_missing=$((observations_failed + observations_skipped))
[ "$observations_missing" -eq 0 ] || notes+=("$observations_missing observation(s) MISSING ($observations_failed failed, $observations_skipped skipped by the export's timeout breaker) — cost data for those spans is absent (visibility only; does not gate advancement)")
[ "$cost_check_status" = ok ] || notes+=("cost assertion failed (make qa-cost-assert exited $cost_check_rc) — cost data may be stale or absent for this round's observations (visibility only; does not gate advancement)")
if [ "${#notes[@]}" -eq 0 ]; then
  emit notes ""
else
  joined_notes="$(printf '%s; ' "${notes[@]}")"
  emit notes "${joined_notes%; }"
fi

if [ "${#reasons[@]}" -eq 0 ]; then
  emit round clean
  emit advance true
  emit target "$QA_HEAD_SHA"
  emit reason "clean round"
else
  # Join reasons into a single newline-free line.
  joined="$(printf '%s; ' "${reasons[@]}")"
  emit round incomplete
  emit advance false
  emit target ""
  emit reason "${joined%; }"
fi

# Observability: the raw signals the decision was computed from.
emit tours_exit "$tours_rc"
emit report_exit "$report_rc"
emit export_exit "$export_rc"
emit export_summary_lines "$summary_count"
emit traces "$traces"
emit observations "$observations"
emit observations_failed "$observations_failed"
emit observations_skipped "$observations_skipped"
emit ship_failed "$ship_failed"
emit enqueue_attempted "$enq_attempted"
emit enqueue_ok "$enq_ok"
emit enqueue_skipped_existing "$enq_skipped"
emit enqueue_failed "$enq_failed"
# Records whether the A->B->A redeploy-rollback guard actually ran this round (the deploy-
# generation seam was provided) or degraded to the sha-only guard (its residual blind spot).
if [ -n "${QA_DEPLOYED_GEN_CMD:-}" ]; then emit deploy_gen_checked true; else emit deploy_gen_checked false; fi
exit 0

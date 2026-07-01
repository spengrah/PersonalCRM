#!/usr/bin/env bash
# Tests for the develop->staging deploy plumbing. Two concerns:
#
#   1. deploy-staging.sh WRAPPER behavior (the env-trust seam): it forces
#      CRM_USER=staging + CRM_HOME=/var/lib/staging, forwards the SHA verbatim,
#      does NOT export DEPLOY_ENV_FILE/NTFY_ENV_FILE, and survives a no-arg call
#      (set -u + "${1:-}") by reaching deploy-artifact.sh with an empty arg.
#
#   2. A SEMANTIC static guard over .github/workflows/deploy-staging.yml — the
#      load-bearing env-trust + three-way-gate invariants live in YAML, so a
#      syntax check is insufficient. Pure bash/grep (no YAML/Python parser:
#      make test-deploy-scripts runs on ubuntu-latest with only bash + stdlib;
#      PyYAML is not provisioned and there is no actionlint). YAML validity is
#      therefore out of scope for this guard — GitHub's on-push validation is the
#      runtime backstop.
#
# All checks run anywhere (no Pi/podman/root). The wrapper hardcodes the absolute
# /usr/local/sbin/deploy-artifact.sh path (that IS the seam), so the test rewrites
# that one path to a recording stub via sed-to-stdout (portable; no sed -i) and
# runs the rewritten copy — the committed wrapper stays byte-pure.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="$REPO_ROOT/scripts/deploy-staging.sh"
WORKFLOW="$REPO_ROOT/.github/workflows/deploy-staging.yml"
VALID_SHA="abcdef0123456789abcdef0123456789abcdef01"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Wrapper sandbox: a stub deploy-artifact.sh that records its inherited env +
# argv, and a copy of the wrapper with its hardcoded exec path rewritten to it.
# ---------------------------------------------------------------------------
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    cat > "$SANDBOX/bin/deploy-artifact.sh" <<EOF
#!/usr/bin/env bash
echo "argc=\$#" >> "$CALL_LOG"
echo "arg1=[\${1-<unset>}]" >> "$CALL_LOG"
echo "env CRM_USER=\${CRM_USER:-<unset>} CRM_HOME=\${CRM_HOME:-<unset>} DEPLOY_ENV_FILE=\${DEPLOY_ENV_FILE:-<unset>} NTFY_ENV_FILE=\${NTFY_ENV_FILE:-<unset>}" >> "$CALL_LOG"
exit 0
EOF
    chmod +x "$SANDBOX/bin/deploy-artifact.sh"

    # Rewrite ONLY the hardcoded exec path to the stub (sed to stdout; '#'
    # delimiter so the path slashes need no escaping). The committed wrapper is
    # unchanged on disk.
    sed "s#/usr/local/sbin/deploy-artifact.sh#$SANDBOX/bin/deploy-artifact.sh#" \
        "$WRAPPER" > "$SANDBOX/deploy-staging.sh"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_wrapper [args...] : run the rewritten wrapper; sets RC. Any env the caller
# exports is visible to `bash` here (in prod sudo would have reset it first).
run_wrapper() {
    bash "$SANDBOX/deploy-staging.sh" "$@" >/dev/null 2>&1
    RC=$?
}

# ===========================================================================
# Wrapper behavior
# ===========================================================================

test_wrapper_forces_tenant_and_forwards_sha() {
    echo "test: wrapper forces CRM_USER/CRM_HOME=staging and forwards the SHA verbatim"
    make_sandbox
    run_wrapper "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "wrapper should exit 0 on a valid SHA, got $RC"; fi
    if grep -qF "env CRM_USER=staging CRM_HOME=/var/lib/staging " "$CALL_LOG"; then ok
    else fail "wrapper must export CRM_USER=staging + CRM_HOME=/var/lib/staging"; fi
    if grep -qF "arg1=[$VALID_SHA]" "$CALL_LOG"; then ok; else fail "wrapper must forward the SHA verbatim"; fi
    if grep -qF "argc=1" "$CALL_LOG"; then ok; else fail "wrapper must pass exactly one arg"; fi
    cleanup_sandbox
}

test_wrapper_overrides_caller_supplied_tenant() {
    echo "test: wrapper OVERRIDES a caller-supplied CRM_USER/CRM_HOME (defense-in-depth)"
    make_sandbox
    # Even if the caller tries to inject a tenant, the wrapper's hardcoded exports win.
    CRM_USER=attacker CRM_HOME=/evil run_wrapper "$VALID_SHA"
    if grep -qF "env CRM_USER=staging CRM_HOME=/var/lib/staging " "$CALL_LOG"; then ok
    else fail "wrapper must override a caller-supplied tenant with staging"; fi
    if grep -qF "CRM_USER=attacker" "$CALL_LOG"; then fail "caller tenant leaked through the wrapper"; else ok; fi
    cleanup_sandbox
}

test_wrapper_does_not_export_env_or_ntfy_file() {
    echo "test: wrapper does NOT export DEPLOY_ENV_FILE / NTFY_ENV_FILE"
    make_sandbox
    run_wrapper "$VALID_SHA"
    if grep -qF "DEPLOY_ENV_FILE=<unset> NTFY_ENV_FILE=<unset>" "$CALL_LOG"; then ok
    else fail "wrapper must not invent DEPLOY_ENV_FILE/NTFY_ENV_FILE"; fi
    cleanup_sandbox
}

test_wrapper_no_arg_reaches_deploy_artifact() {
    echo "test: no-arg wrapper (set -u + \${1:-}) reaches deploy-artifact with an empty arg"
    make_sandbox
    run_wrapper
    # The wrapper must NOT abort on an unbound \$1 before exec; deploy-artifact.sh
    # owns the empty-arg rejection, so the stub MUST have been invoked.
    if grep -qF "argc=1" "$CALL_LOG"; then ok; else fail "no-arg wrapper must still exec deploy-artifact.sh once"; fi
    if grep -qF "arg1=[]" "$CALL_LOG"; then ok; else fail "no-arg wrapper must forward an empty arg"; fi
    cleanup_sandbox
}

test_wrapper_uses_default_empty_expansion() {
    echo "test: committed wrapper uses \"\${1:-}\" (not a bare \"\$1\") under set -u"
    if grep -qE 'exec[[:space:]]+/usr/local/sbin/deploy-artifact\.sh[[:space:]]+"\$\{1:-\}"' "$WRAPPER"; then ok
    else fail "wrapper must exec with \"\${1:-}\" so a no-arg call does not abort on set -u"; fi
    if grep -qE 'deploy-artifact\.sh[[:space:]]+"\$1"' "$WRAPPER"; then fail "wrapper must not use a bare \"\$1\""; else ok; fi
}

# ===========================================================================
# Semantic static guard over deploy-staging.yml
# ===========================================================================

wf_has()   { grep -qF -- "$1" "$WORKFLOW"; }
wf_lacks() { ! grep -qF -- "$1" "$WORKFLOW"; }

test_workflow_trigger() {
    echo "test: workflow triggers on workflow_run, NOT push"
    if wf_has "workflow_run:"; then ok; else fail "workflow must trigger on workflow_run"; fi
    # No real push trigger KEY (anchored — comments that mention 'push:' don't count).
    if grep -qE '^[[:space:]]*push:' "$WORKFLOW"; then fail "workflow must NOT have a push: trigger"; else ok; fi
}

test_workflow_deploy_target() {
    echo "test: deploy step invokes the root-owned wrapper, never deploy-artifact.sh"
    if wf_has "/usr/local/sbin/deploy-staging.sh"; then ok; else fail "deploy step must call deploy-staging.sh"; fi
    if wf_lacks "deploy-artifact.sh"; then ok; else fail "workflow must NOT call deploy-artifact.sh directly"; fi
}

test_workflow_no_env_passthrough() {
    echo "test: workflow does not preserve/pass env into sudo (the catch-all breach)"
    for forbidden in "sudo -E" "--preserve-env" "env_keep" "SETENV"; do
        if wf_lacks "$forbidden"; then ok; else fail "workflow must not use '$forbidden'"; fi
    done
}

test_workflow_no_trusted_knob() {
    echo "test: workflow sets NO deploy-artifact.sh trusted-knob (any occurrence is a regression)"
    local knob
    for knob in CRM_USER CRM_HOME QUADLET_DIR BACKEND_UNIT FRONTEND_UNIT \
                BACKEND_REPO FRONTEND_REPO DEPLOY_ENV_FILE PODMAN_NETWORK \
                DEPLOY_MIGRATIONS_PATH BACKUP_SCRIPT RESTORE_SCRIPT \
                NTFY_ENV_FILE HEALTH_RETRIES; do
        if wf_lacks "$knob"; then ok; else fail "workflow leaks trusted knob '$knob'"; fi
    done
}

test_workflow_three_way_gate_and_job_split() {
    echo "test: three-way-gate + job-split invariants"
    # The deploy-prod-style completed-only filter was deliberately dropped.
    if wf_lacks "status=completed"; then ok; else fail "workflow must NOT use the status=completed filter"; fi
    # Gate exposes a deploy_ready output; deploy job gates on it.
    if wf_has "deploy_ready:"; then ok; else fail "gate job must expose a deploy_ready output"; fi
    if wf_has "needs.gate.outputs.deploy_ready == 'true'"; then ok
    else fail "deploy job must gate on needs.gate.outputs.deploy_ready == 'true'"; fi
    # environment: staging attaches to exactly one job (the deploy job), so it is
    # recorded only for a real staging mutation (a code deploy or a manual reseed) —
    # never for a green-skip. A force_reseed=false dispatch never starts the deploy
    # job, so no phantom Environment deployment is recorded. The exactly-once count
    # assertion below stays green (no second environment: is added).
    if wf_has "environment: staging"; then ok; else fail "deploy job must set environment: staging"; fi
    local n_env
    n_env=$(grep -cE '^[[:space:]]*environment:' "$WORKFLOW")
    if [ "$n_env" -eq 1 ]; then ok; else fail "environment: must appear on exactly one job, found $n_env"; fi
    # Gate filtering is branch-only: no job if: gates on the upstream conclusion.
    if wf_lacks "conclusion == 'success'"; then ok; else fail "no job if: may gate on conclusion == 'success'"; fi
    # Stale-SHA guard: the gate deploys only the current develop tip (an older
    # push whose upstreams finish after a newer push deployed must not roll back).
    if wf_has "commits/develop"; then ok; else fail "gate must query the current develop tip"; fi
    if wf_has "tip_sha"; then ok; else fail "gate must compare head_sha against the develop tip"; fi
}

test_workflow_ci_gate_qualifier() {
    echo "test: CI gate query is qualified to the develop-push run"
    if wf_has "event=push"; then ok; else fail "CI gate query must include event=push"; fi
    if wf_has "branch=develop"; then ok; else fail "CI gate query must include branch=develop"; fi
}

# ===========================================================================
# Auto-reseed wiring (D2 + D5): ordered deploy-job steps over deploy-staging.yml
# ===========================================================================

test_workflow_reseed_steps_exist() {
    echo "test: the capture/checkout/decision/reseed/breadcrumb/nudge steps all exist"
    if wf_has "staging-deployed-sha.sh"; then ok; else fail "missing the host-pinned base capture step"; fi
    if wf_has "uses: actions/checkout@v4"; then ok; else fail "missing the post-deploy checkout step"; fi
    if wf_has "staging-reseed-decision.sh"; then ok; else fail "missing the reseed-decision step"; fi
    if wf_has "/usr/local/sbin/staging-reseed.sh"; then ok; else fail "missing the reseed step"; fi
    if wf_has "skipping auto-reseed"; then ok; else fail "missing the OAuth-skip breadcrumb (greps the stable marker)"; fi
    if wf_has "GITHUB_STEP_SUMMARY"; then ok; else fail "nudges must write to GITHUB_STEP_SUMMARY"; fi
    if wf_has "make staging-reset"; then ok; else fail "summaries must point at the make staging-reset escape hatch"; fi
}

test_workflow_reseed_step_order() {
    echo "test: load-bearing step ORDER — capture < deploy < checkout < decision < reseed"
    local cap dep chk dec res
    cap=$(grep -n 'staging-deployed-sha.sh'            "$WORKFLOW" | head -1 | cut -d: -f1)
    dep=$(grep -n '/usr/local/sbin/deploy-staging.sh'  "$WORKFLOW" | head -1 | cut -d: -f1)
    chk=$(grep -n 'uses: actions/checkout@v4'          "$WORKFLOW" | head -1 | cut -d: -f1)
    dec=$(grep -n 'staging-reseed-decision.sh'         "$WORKFLOW" | head -1 | cut -d: -f1)
    res=$(grep -n '/usr/local/sbin/staging-reseed.sh'  "$WORKFLOW" | head -1 | cut -d: -f1)
    if [ -n "$cap" ] && [ -n "$dep" ] && [ -n "$chk" ] && [ -n "$dec" ] && [ -n "$res" ]; then ok
    else fail "a step marker is missing (cap=$cap dep=$dep chk=$chk dec=$dec res=$res)"; fi
    if [ -n "$cap" ] && [ -n "$dep" ] && [ "$cap" -lt "$dep" ]; then ok; else fail "capture must precede deploy (cap=$cap dep=$dep)"; fi
    if [ -n "$dep" ] && [ -n "$chk" ] && [ "$dep" -lt "$chk" ]; then ok; else fail "deploy must precede checkout (dep=$dep chk=$chk)"; fi
    if [ -n "$chk" ] && [ -n "$dec" ] && [ "$chk" -lt "$dec" ]; then ok; else fail "checkout must precede decision (chk=$chk dec=$dec)"; fi
    if [ -n "$dec" ] && [ -n "$res" ] && [ "$dec" -lt "$res" ]; then ok; else fail "decision must precede reseed (dec=$dec res=$res)"; fi
}

test_workflow_reseed_step_target_and_condition() {
    echo "test: reseed gated on seed_changed, calls the wrapper (never reset/artifact directly), PIPESTATUS"
    if wf_has "env.seed_changed == 'true'"; then ok; else fail "reseed must be gated on env.seed_changed == 'true'"; fi
    if wf_has "PIPESTATUS"; then ok; else fail "reseed must propagate PIPESTATUS through the tee"; fi
    # The workflow only ever calls the root-owned wrappers, never the reset/artifact
    # scripts directly. ('make staging-reset' has no '.sh', so it does not match.)
    if wf_lacks "staging-reset.sh"; then ok; else fail "workflow must NOT call staging-reset.sh directly (use the wrapper)"; fi
    if wf_lacks "deploy-artifact.sh"; then ok; else fail "workflow must NOT call deploy-artifact.sh directly"; fi
}

test_workflow_checkout_resilient() {
    echo "test: post-deploy checkout is resilient (fetch-depth: 0 + continue-on-error)"
    if wf_has "fetch-depth: 0"; then ok; else fail "checkout must use fetch-depth: 0 (base + HEAD present for git diff)"; fi
    if wf_has "continue-on-error: true"; then ok; else fail "checkout must be continue-on-error: true (a flake costs only the reseed)"; fi
}

test_workflow_decision_inline_fallback() {
    echo "test: decision degrades to no-reseed on a non-success checkout OR a missing script (stale-checkout + flake safe)"
    # Gate on the checkout OUTCOME, not just script presence: a failed
    # continue-on-error checkout on a persistent self-hosted runner can leave a
    # STALE older checkout whose script still exists — so existence alone would
    # decide from stale path-filters.yml/script contents.
    if wf_has "id: checkout"; then ok; else fail "checkout step must have an id for outcome gating"; fi
    if wf_has "steps.checkout.outcome"; then ok; else fail "decision must gate on steps.checkout.outcome"; fi
    if wf_has "CHECKOUT_OUTCOME" && grep -qF 'CHECKOUT_OUTCOME" = "success"' "$WORKFLOW"; then ok
    else fail "decision must run the real script only when CHECKOUT_OUTCOME == success"; fi
    if wf_has "[ -x scripts/ci/staging-reseed-decision.sh ]"; then ok; else fail "decision must also guard on the script being present (else a flaked checkout 127-reds the job)"; fi
    if wf_has "seed_changed=false"; then ok; else fail "fallback must default seed_changed=false"; fi
    if wf_has "migrations_changed=false"; then ok; else fail "fallback must default migrations_changed=false"; fi
    if wf_has "base_known=false"; then ok; else fail "fallback must default base_known=false"; fi
}

test_workflow_single_job_no_decision_job_no_deployments() {
    echo "test: single deploy job (needs: gate only); no reseed_decision job; no deployments permission"
    if wf_has "needs: gate"; then ok; else fail "deploy job must keep needs: gate"; fi
    if wf_lacks "reseed_decision"; then ok; else fail "must NOT introduce a separate reseed_decision job"; fi
    if wf_lacks "deployments:"; then ok; else fail "must NOT add a deployments: permission"; fi
}

test_workflow_nudge_conditions() {
    echo "test: migration-nudge + no-base-nudge conditions are exact (no double-nudge)"
    if wf_has "env.migrations_changed == 'true' && env.seed_changed != 'true'"; then ok
    else fail "migration nudge must fire only when migrations changed AND seed did not"; fi
    if wf_has "env.base_known == 'false'"; then ok; else fail "no-base nudge must fire on base_known == 'false'"; fi
}

# ===========================================================================
# D6: workflow_dispatch force_reseed (manual, reseed-only, develop-ref-only)
# ===========================================================================

test_workflow_has_dispatch_trigger() {
    echo "test: workflow declares a workflow_dispatch trigger with a boolean force_reseed input"
    if wf_has "workflow_dispatch:"; then ok; else fail "workflow must declare workflow_dispatch:"; fi
    if wf_has "force_reseed:"; then ok; else fail "workflow_dispatch must expose a force_reseed input"; fi
    if wf_has "type: boolean"; then ok; else fail "force_reseed input must be type: boolean"; fi
}

test_gate_handles_dispatch() {
    echo "test: gate job allows the dispatch event and the gate step branches on it"
    if wf_has "github.event_name == 'workflow_dispatch'"; then ok; else fail "gate if: must allow the workflow_dispatch event"; fi
    if grep -qF 'if [ "${{ github.event_name }}" = "workflow_dispatch" ]' "$WORKFLOW"; then ok
    else fail "gate step must branch at the top on the workflow_dispatch event"; fi
}

test_dispatch_is_develop_only() {
    echo "test: a workflow_dispatch is develop-ref-only (gate if: conjoins the ref check)"
    if grep -qF "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/develop'" "$WORKFLOW"; then ok
    else fail "gate if: must require github.ref == 'refs/heads/develop' on the dispatch path"; fi
}

test_dispatch_false_is_noop() {
    echo "test: gate ties deploy_ready to force_reseed so an unchecked box is a true no-op"
    if grep -qF 'if [ "${{ github.event.inputs.force_reseed }}" = "true" ]' "$WORKFLOW"; then ok
    else fail "gate step must branch deploy_ready on github.event.inputs.force_reseed"; fi
    # Both deploy_ready values must be reachable from the dispatch branch.
    if wf_has 'echo "deploy_ready=true"'; then ok; else fail "dispatch branch must be able to set deploy_ready=true"; fi
    if wf_has 'echo "deploy_ready=false"'; then ok; else fail "dispatch branch must be able to set deploy_ready=false"; fi
}

test_deploy_steps_skip_on_dispatch() {
    echo "test: capture/deploy/checkout/decide steps skip on a workflow_dispatch"
    local n
    n=$(grep -cF "github.event_name != 'workflow_dispatch'" "$WORKFLOW")
    if [ "$n" -ge 4 ]; then ok; else fail "expected >=4 deploy-only step-skip guards, got $n"; fi
}

test_reseed_fires_on_force() {
    echo "test: the Auto-reseed step fires on force_reseed==true (in addition to seed_changed)"
    if wf_has "force_reseed == 'true'"; then ok; else fail "Auto-reseed if: must include force_reseed == 'true'"; fi
    if wf_has "env.seed_changed == 'true'"; then ok; else fail "Auto-reseed if: must keep seed_changed == 'true'"; fi
}

test_oauth_breadcrumb_covers_force() {
    echo "test: the OAuth-skip breadcrumb also fires on a forced reseed"
    if grep -qF "always() && (env.seed_changed == 'true' || (github.event_name == 'workflow_dispatch' && github.event.inputs.force_reseed == 'true'))" "$WORKFLOW"; then ok
    else fail "OAuth-skip if: must broaden to cover the force_reseed dispatch path"; fi
}

# ---------------------------------------------------------------------------
main() {
    test_wrapper_forces_tenant_and_forwards_sha
    test_wrapper_overrides_caller_supplied_tenant
    test_wrapper_does_not_export_env_or_ntfy_file
    test_wrapper_no_arg_reaches_deploy_artifact
    test_wrapper_uses_default_empty_expansion
    test_workflow_trigger
    test_workflow_deploy_target
    test_workflow_no_env_passthrough
    test_workflow_no_trusted_knob
    test_workflow_three_way_gate_and_job_split
    test_workflow_ci_gate_qualifier
    test_workflow_reseed_steps_exist
    test_workflow_reseed_step_order
    test_workflow_reseed_step_target_and_condition
    test_workflow_checkout_resilient
    test_workflow_decision_inline_fallback
    test_workflow_single_job_no_decision_job_no_deployments
    test_workflow_nudge_conditions
    test_workflow_has_dispatch_trigger
    test_gate_handles_dispatch
    test_dispatch_is_develop_only
    test_dispatch_false_is_noop
    test_deploy_steps_skip_on_dispatch
    test_reseed_fires_on_force
    test_oauth_breadcrumb_covers_force

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

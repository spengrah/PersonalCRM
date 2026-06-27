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
    # environment: staging attaches to exactly one job (the deploy job), so a
    # green-skipping gate can't record a phantom Environment deployment.
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

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

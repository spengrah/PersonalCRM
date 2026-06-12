#!/usr/bin/env bash
# Tests for setup-mac-deploy.sh (the one-time Mac deploy setup).
#
# These run anywhere (no Mac, no network). PATH is shadowed with stub
# `git`/`gh`/`launchctl`/`plutil`/`id` and the real `sed`/`shasum`/`install`/
# `cat` are used (they exist on the Ubuntu CI runner). Each test drives a
# scenario via env vars + a sandbox deploy-root and asserts on the recorded
# calls + on-disk state. The real-tool path (a true launchctl bootstrap) is the
# Bucket-B supervised dry-run, not this suite — every macOS-only binary
# (plutil/launchctl) is reached ONLY through a stub so the suite is green on
# Ubuntu CI too.
#
# Asserts the load-bearing correctness points from plan §4.5:
#   - re-run does NOT re-clone (fetch instead) and does NOT overwrite deploy.env.
#   - the timer plist is rendered with __INSTALL_PREFIX__ substituted.
#   - the committed template's content hash is recorded.
#   - the timer is NOT bootstrapped on an empty OR partially-filled deploy.env,
#     and IS bootstrapped only once all three vars are set.
#   - a clean first run (no $DEPLOY_ROOT) creates the skeleton before clone/install.
#   - the script invokes `plutil -lint` on the rendered plist (behavioral).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/setup-mac-deploy.sh"
TIMER_TEMPLATE_PATH="infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template"
# A real copy of the committed template, fed to the git-clone stub so render +
# hash operate on the same bytes the production clone would carry.
REAL_TEMPLATE="$REPO_ROOT/$TIMER_TEMPLATE_PATH"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Test sandbox: a fresh tmp dir per scenario with a stub bin/, a fake clone
# source, a LaunchAgents dir, and a call log. Sets globals SANDBOX, CALL_LOG,
# DEPLOY_ROOT, CLONE_DIR, LAUNCH_AGENT_DIR, RENDERED_PLIST, TEMPLATE_HASH_FILE.
#
# Optional arg $1 = "fresh" to leave $DEPLOY_ROOT NON-EXISTENT (clean first run);
# default pre-creates it.
# ---------------------------------------------------------------------------
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    DEPLOY_ROOT="$SANDBOX/deploy"
    CLONE_DIR="$DEPLOY_ROOT/repo"
    INSTALL_BIN_DIR="$DEPLOY_ROOT/bin"
    DEPLOY_ENV_FILE="$DEPLOY_ROOT/deploy.env"
    TEMPLATE_HASH_FILE="$DEPLOY_ROOT/.installed-template-hash"
    LAUNCH_AGENT_DIR="$SANDBOX/LaunchAgents"
    RENDERED_PLIST="$LAUNCH_AGENT_DIR/xyz.spengrah.crm-mac-deploy.plist"

    # The "clone payload" the git-clone stub materializes: a scripts/ dir with
    # the two deploy scripts + the committed timer template. We model `git clone`
    # by copying this payload into $CLONE_DIR/.git + the tracked files.
    CLONE_PAYLOAD="$SANDBOX/clone-payload"
    mkdir -p "$CLONE_PAYLOAD/scripts" "$CLONE_PAYLOAD/$(dirname "$TIMER_TEMPLATE_PATH")"
    printf '#!/bin/bash\n# stub reconcile\n' > "$CLONE_PAYLOAD/scripts/reconcile-mac-daemon.sh"
    printf '#!/bin/bash\n# stub delegate\n'  > "$CLONE_PAYLOAD/scripts/deploy-mac-daemon.sh"
    cp "$REAL_TEMPLATE" "$CLONE_PAYLOAD/$TIMER_TEMPLATE_PATH"

    if [ "${1:-pre}" != "fresh" ]; then
        mkdir -p "$DEPLOY_ROOT" "$INSTALL_BIN_DIR"
    fi
    mkdir -p "$LAUNCH_AGENT_DIR"

    # --- stub: git ---------------------------------------------------------
    #   clone <url> <dir>     -> materialize the payload into <dir> (+ .git).
    #   -C <dir> fetch ...    -> record + exit 0 (no re-clone).
    #   -C <dir> show ref     -> emit the committed template bytes (for the hash).
    #   -C <dir> remote ...   -> echo a fixed origin URL.
    cat > "$SANDBOX/bin/git" <<EOF
#!/usr/bin/env bash
echo "git \$*" >> "$CALL_LOG"
payload="$CLONE_PAYLOAD"
if [ "\$1" = "clone" ]; then
  # last arg is the dest dir.
  dest="\${@: -1}"
  mkdir -p "\$dest/.git"
  cp -R "\$payload/." "\$dest/"
  exit 0
fi
if [ "\$1" = "-C" ]; then
  dir="\$2"; shift 2
  case "\$1" in
    fetch)  exit "\${STUB_FETCH_RC:-0}" ;;
    show)
      # Ref-aware: the production hash path MUST read the remote-tracking ref
      # \`origin/main:<template>\` (the same ref reconcile's drift check reads).
      # Honor ONLY that ref so a wrong-ref regression (e.g. HEAD:/bare main:)
      # makes \`git show\` fail -> empty hash -> the hash test reds the suite.
      case "\$2" in
        origin/main:*) cat "\$payload/$TIMER_TEMPLATE_PATH" ;;
        *)             exit 1 ;;
      esac ;;
    remote) echo "https://github.com/spengrah/PersonalCRM.git" ;;
    *)      exit 0 ;;
  esac
  exit 0
fi
case "\$1" in
  remote) echo "https://github.com/spengrah/PersonalCRM.git" ;;
esac
exit 0
EOF

    # --- stub: plutil (macOS-only; record the lint call) -------------------
    cat > "$SANDBOX/bin/plutil" <<EOF
#!/usr/bin/env bash
echo "plutil \$*" >> "$CALL_LOG"
exit 0
EOF

    # --- stub: launchctl (must never reach a real one on Ubuntu CI) -------
    #   print gui/<uid>   -> exit ${STUB_LAUNCHCTL_PRINT_RC:-0} (gui domain check)
    #   bootout/bootstrap -> record + exit 0
    cat > "$SANDBOX/bin/launchctl" <<EOF
#!/usr/bin/env bash
echo "launchctl \$*" >> "$CALL_LOG"
if [ "\$1" = "print" ]; then exit "\${STUB_LAUNCHCTL_PRINT_RC:-0}"; fi
exit 0
EOF

    # --- stub: gh (auth status) -------------------------------------------
    cat > "$SANDBOX/bin/gh" <<EOF
#!/usr/bin/env bash
echo "gh \$*" >> "$CALL_LOG"
exit "\${STUB_GH_AUTH_RC:-0}"
EOF

    # --- stub: id (stable uid without depending on the host) --------------
    cat > "$SANDBOX/bin/id" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-u" ]; then echo 4242; exit 0; fi
exec /usr/bin/id "$@"
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# seed_existing_clone : materialize an EXISTING clone (the fetch path). Mirrors
# what `git clone` would have left behind: a .git dir + the tracked scripts/
# template, so `install` + the template render find their inputs.
seed_existing_clone() {
    mkdir -p "$CLONE_DIR/.git"
    cp -R "$CLONE_PAYLOAD/." "$CLONE_DIR/"
}

# Write a deploy.env into the sandbox. Each arg is a KEY=value pair.
write_deploy_env() {
    : > "$DEPLOY_ENV_FILE"
    local pair key val
    for pair in "$@"; do
        key="${pair%%=*}"
        val="${pair#*=}"
        printf '%s="%s"\n' "$key" "$val" >> "$DEPLOY_ENV_FILE"
    done
}

# run_setup : run setup-mac-deploy.sh in the sandbox; sets RC + OUT. Honors any
# STUB_* env the caller exported.
run_setup() {
    OUT="$(
        PATH="$SANDBOX/bin:$PATH" \
        CRM_MAC_DEPLOY_ROOT="$DEPLOY_ROOT" \
        CRM_MAC_CLONE_DIR="$CLONE_DIR" \
        CRM_MAC_INSTALL_BIN_DIR="$INSTALL_BIN_DIR" \
        CRM_MAC_DEPLOY_ENV_FILE="$DEPLOY_ENV_FILE" \
        CRM_MAC_INSTALLED_TEMPLATE_HASH_FILE="$TEMPLATE_HASH_FILE" \
        CRM_MAC_TIMER_TEMPLATE_PATH="$TIMER_TEMPLATE_PATH" \
        CRM_MAC_LAUNCH_AGENT_DIR="$LAUNCH_AGENT_DIR" \
        CRM_MAC_RENDERED_PLIST="$RENDERED_PLIST" \
        CRM_MAC_ORIGIN_URL="https://github.com/spengrah/PersonalCRM.git" \
        bash "$SCRIPT" 2>&1
    )"
    RC=$?
}

log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }

# ===========================================================================
# Tests
# ===========================================================================

test_fresh_first_run_creates_skeleton() {
    echo "test: clean first run (no \$DEPLOY_ROOT) creates skeleton + clones + renders"
    make_sandbox fresh
    if [ -d "$DEPLOY_ROOT" ]; then fail "DEPLOY_ROOT must NOT pre-exist for this case"; else ok; fi
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "fresh first run should exit 0, got $RC ($OUT)"; fi
    if [ -d "$INSTALL_BIN_DIR" ]; then ok; else fail "bin dir must be created"; fi
    if log_has "git clone"; then ok; else fail "fresh run must clone (no existing .git)"; fi
    if [ -x "$INSTALL_BIN_DIR/reconcile-mac-daemon.sh" ]; then ok; else fail "reconcile script must be installed executable"; fi
    if [ -x "$INSTALL_BIN_DIR/deploy-mac-daemon.sh" ]; then ok; else fail "delegate must be installed executable"; fi
    if [ -f "$RENDERED_PLIST" ]; then ok; else fail "timer plist must be rendered"; fi
    cleanup_sandbox
}

test_rerun_does_not_reclone() {
    echo "test: re-run does NOT re-clone (fetches the existing clone instead)"
    make_sandbox
    # Pre-create an existing clone (a .git dir) so the script takes the fetch path.
    seed_existing_clone
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "re-run should exit 0, got $RC ($OUT)"; fi
    if log_lacks "git clone"; then ok; else fail "existing clone must NOT be re-cloned"; fi
    if log_has "fetch --quiet origin"; then ok; else fail "existing clone must be fetched"; fi
    cleanup_sandbox
}

test_does_not_overwrite_deploy_env() {
    echo "test: re-run does NOT overwrite an existing deploy.env"
    make_sandbox
    seed_existing_clone
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    local before
    before="$(cat "$DEPLOY_ENV_FILE")"
    run_setup
    if [ "$(cat "$DEPLOY_ENV_FILE")" = "$before" ]; then ok; else fail "existing deploy.env must be left untouched"; fi
    cleanup_sandbox
}

test_scaffolds_deploy_env_when_absent() {
    echo "test: scaffolds deploy.env (chmod 600) when absent, with all three keys"
    make_sandbox
    seed_existing_clone
    run_setup
    if [ -f "$DEPLOY_ENV_FILE" ]; then ok; else fail "deploy.env must be scaffolded"; fi
    if grep -q '^CRM_MAC_CODESIGN_IDENTITY=' "$DEPLOY_ENV_FILE"; then ok; else fail "scaffold must include CRM_MAC_CODESIGN_IDENTITY"; fi
    if grep -q '^NTFY_URL=' "$DEPLOY_ENV_FILE"; then ok; else fail "scaffold must include NTFY_URL"; fi
    if grep -q '^NTFY_TOPIC=' "$DEPLOY_ENV_FILE"; then ok; else fail "scaffold must include NTFY_TOPIC"; fi
    # chmod 600 = only-owner perms. Check the mode digits portably.
    local mode
    mode="$(stat -f '%Lp' "$DEPLOY_ENV_FILE" 2>/dev/null || stat -c '%a' "$DEPLOY_ENV_FILE" 2>/dev/null)"
    if [ "$mode" = "600" ]; then ok; else fail "scaffolded deploy.env must be chmod 600, got $mode"; fi
    cleanup_sandbox
}

test_renders_placeholder_substituted() {
    echo "test: rendered plist substitutes __INSTALL_PREFIX__ -> the deploy root"
    make_sandbox
    seed_existing_clone
    run_setup
    if [ -f "$RENDERED_PLIST" ]; then ok; else fail "plist must be rendered"; fi
    if ! grep -qF '__INSTALL_PREFIX__' "$RENDERED_PLIST"; then ok; else fail "rendered plist must NOT still contain the placeholder"; fi
    if grep -qF "$DEPLOY_ROOT/bin/reconcile-mac-daemon.sh" "$RENDERED_PLIST"; then ok; else fail "rendered plist must point ProgramArguments at the installed reconcile path"; fi
    # The script lints the rendered plist via plutil (behavioral assertion).
    if log_has "plutil -lint"; then ok; else fail "script must invoke plutil -lint on the rendered plist"; fi
    cleanup_sandbox
}

test_records_template_hash() {
    echo "test: records the committed template content hash for drift detection"
    make_sandbox
    seed_existing_clone
    run_setup
    if [ -f "$TEMPLATE_HASH_FILE" ]; then ok; else fail "template hash file must be written"; fi
    # The recorded hash must equal shasum over the SAME bytes reconcile reads:
    # `git show origin/main:<template>` captured in $(...) (which strips trailing
    # newlines) then `printf '%s'`. Both setup AND reconcile hash this way, so the
    # expectation must mirror the $(...)-strip + printf '%s' too, not a raw `cat`.
    local expected actual template_bytes
    template_bytes="$(cat "$REAL_TEMPLATE")"
    expected="$(printf '%s' "$template_bytes" | shasum -a 256 | awk '{print $1}')"
    actual="$(cat "$TEMPLATE_HASH_FILE")"
    if [ "$actual" = "$expected" ]; then ok; else fail "recorded hash must match the committed template's hash"; fi
    cleanup_sandbox
}

test_timer_not_loaded_when_scaffold_empty() {
    echo "test: timer NOT bootstrapped when deploy.env is an empty scaffold"
    make_sandbox
    seed_existing_clone
    # No deploy.env -> the script scaffolds an EMPTY one -> must not bootstrap.
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "empty scaffold run should exit 0, got $RC ($OUT)"; fi
    if log_lacks "launchctl bootstrap"; then ok; else fail "empty scaffold must NOT bootstrap the timer"; fi
    cleanup_sandbox
}

test_timer_not_loaded_when_partial() {
    echo "test: timer NOT bootstrapped when deploy.env is only partially filled"
    make_sandbox
    seed_existing_clone
    # Identity set but ntfy empty -> still must NOT bootstrap (all-three rule).
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=" "NTFY_TOPIC="
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "partial run should exit 0, got $RC ($OUT)"; fi
    if log_lacks "launchctl bootstrap"; then ok; else fail "partial deploy.env must NOT bootstrap (needs all three vars)"; fi
    cleanup_sandbox
}

test_timer_not_loaded_when_identity_empty() {
    echo "test: timer NOT bootstrapped when identity is empty but ntfy IS set"
    make_sandbox
    seed_existing_clone
    # The inverse of the partial case: ntfy fully set, identity EMPTY. This is
    # the safety-critical sub-rule of the all-three guard -- an empty
    # CRM_MAC_CODESIGN_IDENTITY must never let the RunAtLoad timer bootstrap (it
    # would immediately fire a deploy that ad-hoc-signs and resets FDA). Without
    # this case, dropping the identity check from the guard slips through green.
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "identity-empty run should exit 0, got $RC ($OUT)"; fi
    if log_lacks "launchctl bootstrap"; then ok; else fail "empty identity must NOT bootstrap (even with ntfy set)"; fi
    cleanup_sandbox
}

test_timer_loaded_when_fully_configured() {
    echo "test: timer IS bootstrapped once all three vars are set"
    make_sandbox
    seed_existing_clone
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "fully-configured run should exit 0, got $RC ($OUT)"; fi
    if log_has "launchctl bootstrap"; then ok; else fail "fully-configured deploy.env must bootstrap the timer"; fi
    # bootout-then-bootstrap = idempotent reload.
    if log_has "launchctl bootout"; then ok; else fail "load must bootout before bootstrap (idempotent reload)"; fi
    cleanup_sandbox
}

test_timer_not_loaded_in_non_gui_context() {
    echo "test: non-login (no gui domain) context warns instead of bootstrapping"
    make_sandbox
    seed_existing_clone
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # launchctl print gui/<uid> fails -> no gui domain -> must NOT bootstrap.
    STUB_LAUNCHCTL_PRINT_RC=1 run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "non-gui context should still exit 0, got $RC ($OUT)"; fi
    if log_lacks "launchctl bootstrap"; then ok; else fail "non-gui context must NOT bootstrap"; fi
    cleanup_sandbox
}

test_offline_rerun_still_completes_local_steps() {
    echo "test: offline re-run (fetch fails) still installs + renders + loads from the existing clone"
    make_sandbox
    seed_existing_clone
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Fetch fails (offline), but the script must soft-skip and complete the
    # local-only steps rather than aborting under set -e.
    STUB_FETCH_RC=1 run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "offline re-run should still exit 0, got $RC ($OUT)"; fi
    if [ -x "$INSTALL_BIN_DIR/reconcile-mac-daemon.sh" ]; then ok; else fail "offline re-run must still install the reconcile script"; fi
    if [ -f "$RENDERED_PLIST" ]; then ok; else fail "offline re-run must still render the timer plist"; fi
    if log_has "launchctl bootstrap"; then ok; else fail "offline re-run with a full deploy.env must still load the timer"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_fresh_first_run_creates_skeleton
    test_rerun_does_not_reclone
    test_does_not_overwrite_deploy_env
    test_scaffolds_deploy_env_when_absent
    test_renders_placeholder_substituted
    test_records_template_hash
    test_timer_not_loaded_when_scaffold_empty
    test_timer_not_loaded_when_partial
    test_timer_not_loaded_when_identity_empty
    test_timer_loaded_when_fully_configured
    test_timer_not_loaded_in_non_gui_context
    test_offline_rerun_still_completes_local_steps

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

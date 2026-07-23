#!/usr/bin/env bash
# Tests for reconcile-mac-daemon.sh (the Mac deploy reconcile orchestrator).
#
# These run anywhere (no Mac, no real build, no network). PATH is shadowed with
# stub `git`/`gh`/`plutil`/`crm-mac`/`curl`/`launchctl` and a stub
# `deploy-mac-daemon.sh` that records its argv to a per-test call log. Each test
# drives a scenario via env vars (CI conclusion, gh exit code, diff result,
# doctor output, lock state) and asserts on the recorded calls + the script exit
# code. The real-tool path is the Bucket-B supervised dry-run, not this suite --
# every macOS-only binary (plutil/launchctl) is reached ONLY through a stub so
# the suite is green on the Ubuntu CI backend runner too.
#
# Asserts the load-bearing correctness points from plan §3.2:
#   - relevance gate: no-op when mac-daemon/ unchanged; deploy when changed.
#   - CI gate: fail-closed on failure; soft-skip on missing; structural failure
#     (empty repo / 401|403|404) -> informational notice (not silent); transient
#     -> soft-skip; never invokes `gh auth status` (precheck removed).
#   - fetch advances origin/main (not just FETCH_HEAD).
#   - empty codesign identity: loud abort (ntfy when configured, else red exit).
#   - tooling refresh runs even on a no-op + preserves the executable bit.
#   - timer-template drift -> informational notice; unchanged -> none; ambiguous
#     -> silent.
#   - health gate parses the agent_service line by CONTENT, not exit code.
#   - Contacts-pending informational ntfy on success.
#   - mkdir lock: live holder defers; stale (dead PID / TTL) reclaims; loser does
#     not tear down the winner's worktree.
#   - path-with-spaces deploy path.
#   - worktree cleanup on success + on a mid-run failure.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/reconcile-mac-daemon.sh"
TARGET_SHA="abcdef0123456789abcdef0123456789abcdef01"
INSTALLED_SHA="1111111111111111111111111111111111111111"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Test sandbox: a fresh tmp dir per scenario with a stub bin/, a clone dir, a
# fake installed bundle, and a call log. Sets globals SANDBOX, CALL_LOG,
# DEPLOY_ROOT, CLONE_DIR, INSTALL_BUNDLE, INSTALL_BIN_DIR.
# ---------------------------------------------------------------------------
make_sandbox() {
    # Optional arg $1 = a deploy-root suffix so a test can force a path WITH a
    # space (the real default lives under "Application Support").
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    local root_name="${1:-deploy}"
    DEPLOY_ROOT="$SANDBOX/$root_name"
    CLONE_DIR="$DEPLOY_ROOT/repo"
    WORKTREE_DIR="$DEPLOY_ROOT/worktree"
    INSTALL_BUNDLE="$DEPLOY_ROOT/installed/crm-mac.app"
    INSTALL_BIN_DIR="$DEPLOY_ROOT/bin"
    LOCK_DIR="$DEPLOY_ROOT/reconcile.lock"
    TEMPLATE_HASH_FILE="$DEPLOY_ROOT/.installed-template-hash"
    mkdir -p "$CLONE_DIR" "$INSTALL_BUNDLE/Contents" "$DEPLOY_ROOT/installed"

    # --- stub: git ---------------------------------------------------------
    # Branches on the subcommand. Records every invocation.
    #   fetch            -> exit ${STUB_FETCH_RC:-0}
    #   rev-parse origin/main -> echo the fetched SHA (STUB_TARGET_SHA)
    #   remote get-url   -> echo a fixed origin URL
    #   diff --quiet     -> exit ${STUB_DIFF_RC:-1}  (default 1 = "changed")
    #   show origin/main:scripts/<f> -> emit fake script body (records refresh)
    #   show origin/main:<template>  -> emit ${STUB_TEMPLATE_CONTENT}
    #   worktree         -> exit 0 (records add/remove)
    cat > "$SANDBOX/bin/git" <<EOF
#!/usr/bin/env bash
echo "git \$*" >> "$CALL_LOG"
# Drop a leading "-C <dir>" so we can match on the subcommand.
if [ "\$1" = "-C" ]; then shift 2; fi
case "\$1" in
  fetch)     exit "\${STUB_FETCH_RC:-0}" ;;
  rev-parse) echo "\${STUB_TARGET_SHA:-$TARGET_SHA}"; exit 0 ;;
  remote)    echo "https://github.com/spengrah/PersonalCRM.git"; exit 0 ;;
  diff)      exit "\${STUB_DIFF_RC:-1}" ;;
  show)
    ref="\$2"
    case "\$ref" in
      *scripts/*) echo "#!/bin/bash"; echo "# refreshed \$ref"; exit 0 ;;
      *)
        # Template ref. STUB_TEMPLATE_ABSENT=1 models a ref that does not exist
        # at origin/main yet (the real case until PR3 lands the template): git
        # show emits NOTHING and exits non-zero.
        if [ "\${STUB_TEMPLATE_ABSENT:-0}" = "1" ]; then exit 128; fi
        printf '%s' "\${STUB_TEMPLATE_CONTENT:-}"; exit "\${STUB_TEMPLATE_SHOW_RC:-0}" ;;
    esac ;;
  worktree)  exit 0 ;;
  *)         exit 0 ;;
esac
EOF

    # --- stub: gh ----------------------------------------------------------
    # auth status -> exit ${STUB_GH_AUTH_RC:-0}
    # repo view   -> echo ${STUB_REPO:-spengrah/PersonalCRM} (empty models a
    #                structural resolution failure)
    # api ... --jq -> echo ${STUB_CI_CONCLUSION:-success}; exit ${STUB_GH_API_RC:-0}
    # api -i ...   -> emit a fake HTTP status line ${STUB_GH_HTTP_STATUS:-200},
    #                then exit NON-ZERO when the main api call failed (faithful:
    #                real `gh api -i` prints the status line on stdout AND exits
    #                non-zero on 4xx/5xx). Defaults to STUB_GH_API_RC so the
    #                structural/transient cases drive a non-zero -i exit, which
    #                exercises the script's `|| true` guard against set -e
    #                aborting the status-code probe.
    cat > "$SANDBOX/bin/gh" <<EOF
#!/usr/bin/env bash
echo "gh \$*" >> "$CALL_LOG"
case "\$1" in
  auth)  exit "\${STUB_GH_AUTH_RC:-0}" ;;
  repo)  printf '%s' "\${STUB_REPO-spengrah/PersonalCRM}"; exit 0 ;;
  api)
    if [ "\$2" = "-i" ]; then
      echo "HTTP/2 \${STUB_GH_HTTP_STATUS:-200}"
      exit "\${STUB_GH_API_I_RC:-\${STUB_GH_API_RC:-0}}"
    fi
    echo "\${STUB_CI_CONCLUSION:-success}"
    exit "\${STUB_GH_API_RC:-0}" ;;
  *) exit 0 ;;
esac
EOF

    # --- stub: plutil ------------------------------------------------------
    # -extract CRMBuildSHA raw <plist> -> echo ${STUB_INSTALLED_SHA}; or exit 1
    # to model a missing key (bootstrap path).
    cat > "$SANDBOX/bin/plutil" <<EOF
#!/usr/bin/env bash
echo "plutil \$*" >> "$CALL_LOG"
if [ "\$1" = "-extract" ] && [ "\$2" = "CRMBuildSHA" ]; then
  if [ "\${STUB_INSTALLED_SHA_MISSING:-0}" = "1" ]; then exit 1; fi
  echo "\${STUB_INSTALLED_SHA:-$INSTALLED_SHA}"
  exit 0
fi
exit 0
EOF

    # --- stub: crm-mac (doctor) -------------------------------------------
    # Emits ${STUB_DOCTOR_OUTPUT} and exits the FAIL-count (faithfully models the
    # real exit-code-equals-FAIL-count behavior).
    cat > "$SANDBOX/bin/crm-mac" <<EOF
#!/usr/bin/env bash
echo "crm-mac \$*" >> "$CALL_LOG"
out="\${STUB_DOCTOR_OUTPUT:-PASS  agent_service: registered (enabled)}"
printf '%s\n' "\$out"
# exit code = number of FAIL lines.
fails=\$(printf '%s\n' "\$out" | grep -cE '^FAIL' || true)
exit "\$fails"
EOF

    # --- stub: deploy-mac-daemon.sh (the build+install delegate) ----------
    # Placed at BOTH the worktree path the script invokes and recorded here. The
    # real script invokes "\$WORKTREE_DIR/scripts/deploy-mac-daemon.sh"; the git
    # `worktree add` is stubbed (no real checkout), so we pre-create that path.
    mkdir -p "$WORKTREE_DIR/scripts"
    cat > "$WORKTREE_DIR/scripts/deploy-mac-daemon.sh" <<EOF
#!/usr/bin/env bash
echo "deploy-mac-daemon.sh identity=\${CRM_MAC_CODESIGN_IDENTITY:-<unset>}" >> "$CALL_LOG"
exit "\${STUB_BUILD_RC:-0}"
EOF
    chmod +x "$WORKTREE_DIR/scripts/deploy-mac-daemon.sh"

    # --- stub: curl (ntfy) -------------------------------------------------
    cat > "$SANDBOX/bin/curl" <<EOF
#!/usr/bin/env bash
echo "curl \$*" >> "$CALL_LOG"
exit 0
EOF

    # --- stub: launchctl (must never reach a real one on Ubuntu CI) -------
    cat > "$SANDBOX/bin/launchctl" <<EOF
#!/usr/bin/env bash
echo "launchctl \$*" >> "$CALL_LOG"
exit 0
EOF

    # --- stub: stat (lock-dir mtime) --------------------------------------
    # The lock-mtime path calls `stat -f %m DIR` (BSD) then `stat -c %Y DIR`
    # (GNU). Stub it so the missing-owner TTL branch is HERMETIC -- never
    # dependent on the host's real stat (which differs BSD vs GNU and leaked the
    # GNU `-f`=--file-system "  File: ..." block into arithmetic on Linux CI).
    #   default                 -> emit ${STUB_STAT_MTIME:-1000} for -f %m / -c %Y
    #   STUB_STAT_GARBAGE=1      -> model GNU `stat -f %m DIR` (=--file-system):
    #                              a multi-line block whose first line is "  File:"
    #                              and exit 0, so the script must validate + not crash
    cat > "$SANDBOX/bin/stat" <<EOF
#!/usr/bin/env bash
echo "stat \$*" >> "$CALL_LOG"
if [ "\${STUB_STAT_GARBAGE:-0}" = "1" ]; then
  # GNU \`stat -f %m DIR\` prints a filesystem block (NOT a number) and exits 0.
  if [ "\$1" = "-f" ]; then
    printf '  File: "%s"\n    ID: 0 Namelen: 255 Type: apfs\n' "\${@: -1}"
    exit 0
  fi
  # GNU \`-c %Y\` would still work, but model the worst case: also non-numeric.
  echo "garbage"
  exit 0
fi
# Hermetic numeric mtime for both BSD (-f %m) and GNU (-c %Y) forms.
echo "\${STUB_STAT_MTIME:-1000}"
exit 0
EOF

    # --- stub: sleep (no-op so retry loops run instantly) -----------------
    cat > "$SANDBOX/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_reconcile : run reconcile-mac-daemon.sh in the sandbox; sets RC + OUT.
# Honors any STUB_* env the caller exported. The script's stable-bin refresh
# target ($INSTALL_BIN_DIR) is inside the sandbox so the refresh never touches
# the running repo script.
run_reconcile() {
    OUT="$(
        PATH="$SANDBOX/bin:$PATH" \
        CRM_MAC_DEPLOY_ROOT="$DEPLOY_ROOT" \
        CRM_MAC_CLONE_DIR="$CLONE_DIR" \
        CRM_MAC_WORKTREE_DIR="$WORKTREE_DIR" \
        CRM_MAC_DEPLOY_ENV_FILE="${DEPLOY_ENV_FILE_OVERRIDE:-$DEPLOY_ROOT/missing-deploy.env}" \
        CRM_MAC_INSTALL_BUNDLE="$INSTALL_BUNDLE" \
        CRM_MAC_INSTALL_BIN_DIR="$INSTALL_BIN_DIR" \
        CRM_MAC_LOCK_DIR="$LOCK_DIR" \
        CRM_MAC_INSTALLED_TEMPLATE_HASH_FILE="$TEMPLATE_HASH_FILE" \
        CRM_MAC_BIN="$SANDBOX/bin/crm-mac" \
        CRM_MAC_LOCK_STALE_SECS="${LOCK_STALE_SECS_OVERRIDE:-3600}" \
        CRM_MAC_GH_API_RETRIES="${GH_API_RETRIES_OVERRIDE:-2}" \
        bash "$SCRIPT" 2>&1
    )"
    RC=$?
}

# Write a deploy.env into the sandbox + point the override at it. Each arg is a
# KEY=value pair; the value is double-quoted in the written file (mirroring a real
# env file) so values containing spaces (e.g. a codesign identity "My Cert") are
# sourced as a single value, not word-split.
write_deploy_env() {
    DEPLOY_ENV_FILE_OVERRIDE="$DEPLOY_ROOT/deploy.env"
    : > "$DEPLOY_ENV_FILE_OVERRIDE"
    local pair key val
    for pair in "$@"; do
        key="${pair%%=*}"
        val="${pair#*=}"
        printf '%s="%s"\n' "$key" "$val" >> "$DEPLOY_ENV_FILE_OVERRIDE"
    done
}

# assert helpers operating on $CALL_LOG / $OUT.
log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }
# Match against a here-string, NOT `printf ... | grep`: under `set -o pipefail` a
# `grep -q` that matches an early line exits and closes the pipe before printf
# drains, so printf takes SIGPIPE (141) and pipefail reports the pipeline as
# failed even though the match succeeded — a load-dependent false negative.
out_has()   { grep -qF -- "$1" <<<"$OUT"; }
# count ntfy POSTs (curl invocations). grep -c prints 0 and exits 1 on no match,
# so swallow the exit without a second echo (which would print "0\n0").
ntfy_count() { grep -cE '^curl ' "$CALL_LOG" || true; }

# ===========================================================================
# Tests
# ===========================================================================

test_noop_when_unchanged() {
    echo "test: no-op when mac-daemon/ unchanged vs the bundle-reported SHA"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "no-op should exit 0, got $RC ($OUT)"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "no-op must NOT build"; fi
    # No ntfy POST on a no-op.
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "no-op must not POST ntfy"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_deploys_when_changed() {
    echo "test: deploys when mac-daemon/ changed"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "deploy happy path should exit 0, got $RC ($OUT)"; fi
    if log_has "deploy-mac-daemon.sh identity=My Cert"; then ok; else fail "build must run WITH the codesign identity"; fi
    if log_has "worktree add --detach"; then ok; else fail "worktree must be added at the target SHA"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_fail_closed_on_ci_failure() {
    echo "test: fail-closed on CI failure"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=failure run_reconcile
    if [ "$RC" -eq 1 ]; then ok; else fail "CI failure should exit 1, got $RC"; fi
    if log_has "Title: Mac deploy FAILED"; then ok; else fail "CI failure must ntfy 'Mac deploy FAILED'"; fi
    if log_has "Priority: max"; then ok; else fail "CI failure ntfy must be max priority"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "CI failure must NOT build"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_soft_skip_on_ci_missing() {
    echo "test: soft-skip on CI missing (in-progress / not found)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=missing run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "CI missing should soft-skip exit 0, got $RC"; fi
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "CI missing must not POST ntfy"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "CI missing must NOT build"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_transient_gh_failure_soft_skips() {
    echo "test: transient gh api failure -> soft-skip (no ntfy)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # auth OK, api exits non-zero, HTTP status 503 (transient/transport) across retries.
    STUB_GH_AUTH_RC=0 STUB_GH_API_RC=1 STUB_GH_HTTP_STATUS=503 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "transient gh failure should soft-skip exit 0, got $RC"; fi
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "transient gh failure must NOT ntfy"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "transient gh failure must NOT build"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_never_invokes_gh_auth_status() {
    echo "test: the CI gate NEVER invokes \`gh auth status\` (precheck removed)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # A happy-path run: the precheck having been removed, the script must reach the
    # gh repo view / gh api query WITHOUT ever calling `gh auth status`. This guards
    # against a future re-introduction of the false-failing multi-account precheck.
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "happy path should exit 0, got $RC ($OUT)"; fi
    if log_lacks "gh auth"; then ok; else fail "the CI gate must NOT invoke gh auth status"; fi
    if log_has "gh repo view"; then ok; else fail "the CI gate must resolve the repo via gh repo view"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_structural_gh_failure_informational() {
    echo "test: structural gh failures (empty REPO / 401 / 404 / 403) -> informational notice"
    # Sub-case 1: empty REPO (gh repo view yields nothing).
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_GH_AUTH_RC=0 STUB_REPO="" run_reconcile
    if [ "$RC" -eq 0 ] && log_has "Title: Mac deploy: CI gate could not be queried" && log_lacks "deploy-mac-daemon.sh identity="; then ok
    else fail "empty REPO must give the informational notice + no build (rc=$RC)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE

    # Sub-case 2: 404 (repo/workflow not found).
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_GH_AUTH_RC=0 STUB_GH_API_RC=1 STUB_GH_HTTP_STATUS=404 run_reconcile
    if [ "$RC" -eq 0 ] && log_has "Title: Mac deploy: CI gate could not be queried" && log_lacks "deploy-mac-daemon.sh identity="; then ok
    else fail "404 must give the informational notice + no build (rc=$RC)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE

    # Sub-case 3: 403 (token lacks scope).
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_GH_AUTH_RC=0 STUB_GH_API_RC=1 STUB_GH_HTTP_STATUS=403 run_reconcile
    if [ "$RC" -eq 0 ] && log_has "Title: Mac deploy: CI gate could not be queried" && log_lacks "deploy-mac-daemon.sh identity="; then ok
    else fail "403 must give the informational notice + no build (rc=$RC)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE

    # Sub-case 4: 401 (unauthenticated / expired token). Load-bearing after the
    # precheck removal — the downstream HTTP-status classification is now the SOLE
    # auth-failure surface, and 401 is the most likely real auth failure, so it
    # MUST route to ghfailure (informational notice, no build), not a silent skip.
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_GH_AUTH_RC=0 STUB_GH_API_RC=1 STUB_GH_HTTP_STATUS=401 run_reconcile
    if [ "$RC" -eq 0 ] && log_has "Title: Mac deploy: CI gate could not be queried" && log_lacks "deploy-mac-daemon.sh identity="; then ok
    else fail "401 must give the informational notice + no build (rc=$RC)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_soft_skip_on_fetch_failure() {
    echo "test: soft-skip on git fetch failure"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_FETCH_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "fetch failure should soft-skip exit 0, got $RC"; fi
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "fetch failure must not ntfy"; fi
    if log_lacks "gh api"; then ok; else fail "fetch failure must abort before the CI gate"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_fetch_advances_tracking_ref() {
    echo "test: fetch advances origin/main tracking ref, target from rev-parse origin/main"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    local new_sha="cafebabecafebabecafebabecafebabecafebabe"
    # The fetch must use the explicit refspec form (updates refs/remotes/origin/main).
    STUB_CI_CONCLUSION=success STUB_TARGET_SHA="$new_sha" STUB_DIFF_RC=1 run_reconcile
    if log_has "fetch --quiet origin main:refs/remotes/origin/main"; then ok
    else fail "fetch must use the explicit origin/main refspec (not FETCH_HEAD-only)"; fi
    if log_has "rev-parse origin/main"; then ok; else fail "target SHA must come from rev-parse origin/main"; fi
    # The new SHA must flow into the build (worktree add at the new SHA).
    # Capture first, then match a here-string: a piped `grep | grep -q` can
    # SIGPIPE the first grep under pipefail once the second exits early.
    worktree_adds="$(grep -F 'worktree add' "$CALL_LOG")"
    if grep -qF -- "$new_sha" <<<"$worktree_adds"; then ok
    else fail "deploy must target the NEW SHA the fetch advanced to"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_empty_identity_with_ntfy_fires() {
    echo "test: empty identity WITH ntfy configured -> ntfy fired (no unbound-var crash)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    # Must reach the upgrade decision, find the identity empty, ntfy, and NOT crash.
    if log_has "Title: Mac deploy: deploy.env not configured"; then ok; else fail "empty identity must ntfy the deploy.env-not-configured notice"; fi
    if out_has "unbound variable"; then fail "empty identity must NOT crash with an unbound variable"; else ok; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "empty identity must NOT build"; fi
    if [ "$RC" -ne 0 ]; then ok; else fail "empty identity must exit non-zero, got $RC"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_empty_identity_absent_env_red_exit() {
    echo "test: wholly absent deploy.env -> red exit + stderr, NO ntfy attempted"
    make_sandbox
    # No deploy.env at all -> NTFY disabled. Identity is therefore empty too.
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -ne 0 ]; then ok; else fail "absent deploy.env empty-identity must exit non-zero, got $RC"; fi
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "absent deploy.env: no ntfy POST possible (config in the missing file)"; fi
    if out_has "CRM_MAC_CODESIGN_IDENTITY empty"; then ok; else fail "absent deploy.env must log a clear stderr message"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "empty identity must NOT build"; fi
    cleanup_sandbox
}

test_tooling_refresh_on_noop() {
    echo "test: tooling refresh happens even on a daemon no-op"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "no-op should exit 0, got $RC"; fi
    # The refresh reads all three scripts from origin/main BEFORE the relevance gate.
    if log_has "show origin/main:scripts/reconcile-mac-daemon.sh"; then ok; else fail "refresh must read reconcile-mac-daemon.sh from origin/main"; fi
    if log_has "show origin/main:scripts/deploy-mac-daemon.sh"; then ok; else fail "refresh must read deploy-mac-daemon.sh from origin/main"; fi
    if log_has "show origin/main:scripts/trigger-mac-deploy.sh"; then ok; else fail "refresh must read trigger-mac-deploy.sh from origin/main"; fi
    # The refreshed files land at the stable bin dir.
    if [ -f "$INSTALL_BIN_DIR/reconcile-mac-daemon.sh" ]; then ok; else fail "refreshed reconcile script must be installed to the stable bin dir"; fi
    if [ -f "$INSTALL_BIN_DIR/trigger-mac-deploy.sh" ]; then ok; else fail "refreshed trigger script must be installed to the stable bin dir"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_tooling_refresh_preserves_exec_bit() {
    echo "test: tooling refresh preserves the executable bit (0755)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 run_reconcile
    for f in reconcile-mac-daemon.sh deploy-mac-daemon.sh trigger-mac-deploy.sh; do
        if [ -x "$INSTALL_BIN_DIR/$f" ]; then ok; else fail "$f must be executable after refresh"; fi
    done
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_timer_drift_notifies() {
    echo "test: timer-template drift -> informational notice (changed)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Record a stored hash that differs from the committed template's hash.
    echo "0000000000000000000000000000000000000000000000000000000000000000" > "$TEMPLATE_HASH_FILE"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 \
        STUB_TEMPLATE_CONTENT="new template body" run_reconcile
    if log_has "Title: Mac deploy: timer template changed"; then ok; else fail "drift must fire the timer-template-changed notice"; fi
    if log_lacks "launchctl"; then ok; else fail "drift must NOT auto-reload launchd"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_timer_drift_unchanged_no_notice() {
    echo "test: timer template unchanged -> no notice"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Compute the committed template's real hash and store it -> equal -> no notice.
    local content="stable template body"
    printf '%s' "$content" | shasum -a 256 | awk '{print $1}' > "$TEMPLATE_HASH_FILE"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 \
        STUB_TEMPLATE_CONTENT="$content" run_reconcile
    if log_lacks "Title: Mac deploy: timer template changed"; then ok; else fail "unchanged template must NOT notify"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_timer_drift_ambiguous_silent() {
    echo "test: timer-drift ambiguous (no recorded hash) -> silent, no crash"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # No TEMPLATE_HASH_FILE written.
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 \
        STUB_TEMPLATE_CONTENT="some body" run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "ambiguous drift must not crash, got $RC ($OUT)"; fi
    if log_lacks "Title: Mac deploy: timer template changed"; then ok; else fail "ambiguous drift must NOT spam a notice"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_timer_drift_template_absent_no_notice() {
    echo "test: timer template absent at origin/main (pre-PR3) -> no spurious notice"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # A stored hash EXISTS but the committed template does NOT (git show fails).
    # Guards against hashing empty stdin to a non-empty digest and firing a false
    # drift notice every run before PR3 lands the template.
    echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" > "$TEMPLATE_HASH_FILE"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=0 \
        STUB_TEMPLATE_ABSENT=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "absent template must not crash, got $RC ($OUT)"; fi
    if log_lacks "Title: Mac deploy: timer template changed"; then ok; else fail "absent template must NOT fire a drift notice"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_health_fail_max_ntfy() {
    echo "test: max-priority ntfy on health failure"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 \
        STUB_DOCTOR_OUTPUT="FAIL  agent_service: not registered" run_reconcile
    if [ "$RC" -eq 1 ]; then ok; else fail "health failure should exit 1, got $RC"; fi
    if log_has "Title: Mac deploy FAILED"; then ok; else fail "health failure must ntfy 'Mac deploy FAILED'"; fi
    if log_has "Priority: max"; then ok; else fail "health failure ntfy must be max priority"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_health_gate_parses_by_content() {
    echo "test: health gate parses by content, not exit code"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # agent_service PASSES but an unrelated pi_reachability FAILS -> doctor exits
    # non-zero. The deploy must still be declared HEALTHY (parse by content).
    local doctor="PASS  agent_service: registered (enabled)
FAIL  pi_reachability: 401 from Pi"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 \
        STUB_DOCTOR_OUTPUT="$doctor" run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "an unrelated FAIL must NOT false-fail the gate, got $RC ($OUT)"; fi
    if log_lacks "Title: Mac deploy FAILED"; then ok; else fail "a healthy agent_service must NOT fire a FAILED ntfy"; fi
    if log_has "Title: Mac deploy OK"; then ok; else fail "a healthy agent_service must report success"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_contacts_pending_ntfy_on_success() {
    echo "test: informational Contacts-pending ntfy on success"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "success should exit 0, got $RC"; fi
    if log_has "Title: Mac deploy OK -- Contacts re-approval needed"; then ok; else fail "success must fire the combined Contacts re-approval notice"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_build_failure_fail_closed() {
    echo "test: build (deploy-mac-daemon.sh) failure -> fail-closed"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 STUB_BUILD_RC=1 run_reconcile
    if [ "$RC" -eq 1 ]; then ok; else fail "build failure should exit 1, got $RC"; fi
    if log_has "Title: Mac deploy FAILED"; then ok; else fail "build failure must ntfy 'Mac deploy FAILED'"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_lock_live_holder_defers() {
    echo "test: mkdir lock prevents concurrent runs (LIVE holder)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Pre-create the lock with an owner naming a LIVE PID ($$ of this test) + a
    # fresh timestamp.
    mkdir -p "$LOCK_DIR"
    echo "$$ $(date +%s)" > "$LOCK_DIR/owner"
    STUB_CI_CONCLUSION=success run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "live-holder lock should exit 0, got $RC"; fi
    if log_lacks "fetch --quiet origin"; then ok; else fail "live-holder lock must NOT fetch/build"; fi
    if log_lacks "deploy-mac-daemon.sh identity="; then ok; else fail "live-holder lock must NOT build"; fi
    # The loser's trap must NOT remove the lock it did not create.
    if [ -d "$LOCK_DIR" ]; then ok; else fail "loser must NOT remove the winner's lock dir"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_lock_loser_does_not_touch_worktree() {
    echo "test: loser does NOT remove the winner's worktree"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    mkdir -p "$LOCK_DIR"
    echo "$$ $(date +%s)" > "$LOCK_DIR/owner"
    STUB_CI_CONCLUSION=success run_reconcile
    # The loser's cleanup trap must fire NO git worktree remove.
    if log_lacks "worktree remove"; then ok; else fail "loser must NOT tear down the winner's worktree"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_stale_lock_dead_pid_reclaims() {
    echo "test: stale-lock recovery (dead PID)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Owner names a PID guaranteed dead (a very high, unlikely PID) + fresh ts.
    mkdir -p "$LOCK_DIR"
    echo "999999 $(date +%s)" > "$LOCK_DIR/owner"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "dead-PID lock should be reclaimed -> exit 0, got $RC ($OUT)"; fi
    if log_has "fetch --quiet origin"; then ok; else fail "after reclaim, reconcile must proceed to fetch"; fi
    if log_has "deploy-mac-daemon.sh identity="; then ok; else fail "after reclaim, reconcile must build"; fi
    # On exit, the reclaimed lock is released (this invocation created it).
    if [ ! -d "$LOCK_DIR" ]; then ok; else fail "reclaiming invocation must release the lock on exit"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_stale_lock_ttl_no_owner_file() {
    echo "test: stale-lock TTL fallback (missing owner file)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Lock dir exists but NO owner file. The `stat` stub returns a fixed numeric
    # mtime (1000); LOCK_STALE_SECS=0 forces the age>=TTL reclaim path. Hermetic:
    # never touches the host's real stat (BSD vs GNU output differs).
    mkdir -p "$LOCK_DIR"
    LOCK_STALE_SECS_OVERRIDE=0 STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "missing-owner TTL-stale lock should be reclaimed -> exit 0, got $RC ($OUT)"; fi
    if log_has "fetch --quiet origin"; then ok; else fail "after TTL reclaim, reconcile must proceed"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_stale_lock_garbage_stat_does_not_crash() {
    echo "test: missing owner + non-numeric stat output -> safe reclaim, NO crash"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # Lock dir exists, NO owner file, and `stat -f %m` emits a non-numeric
    # filesystem block (the GNU `stat -f`=--file-system "  File: ..." output that
    # crashed Linux CI: `File: unbound variable` under set -u). The mtime must be
    # rejected as unparseable and the lock RECLAIMED, never an arithmetic crash.
    mkdir -p "$LOCK_DIR"
    STUB_STAT_GARBAGE=1 STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "garbage stat output must reclaim safely -> exit 0, got $RC ($OUT)"; fi
    if out_has "unbound variable"; then fail "garbage stat output must NOT crash with an unbound variable"; else ok; fi
    if out_has "arithmetic"; then fail "garbage stat output must NOT hit an arithmetic error"; else ok; fi
    if log_has "fetch --quiet origin"; then ok; else fail "after safe reclaim, reconcile must proceed"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_lock_released_on_signal() {
    echo "test: lock + worktree cleanup trap is registered for INT TERM HUP, not EXIT alone"
    # Structural assertion: the script registers the trap for INT TERM HUP EXIT.
    if grep -qE 'trap cleanup EXIT INT TERM HUP' "$SCRIPT"; then ok
    else fail "cleanup trap must cover INT TERM HUP (not EXIT alone)"; fi
}

test_path_with_spaces() {
    echo "test: deploy path with a SPACE in the deploy root"
    make_sandbox "deploy root with spaces"
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "space-bearing path deploy should exit 0, got $RC ($OUT)"; fi
    if log_has "deploy-mac-daemon.sh identity=My Cert"; then ok; else fail "build must run from the space-bearing path"; fi
    if log_has "worktree add --detach"; then ok; else fail "worktree add must succeed under a space-bearing path"; fi
    if [ -f "$INSTALL_BIN_DIR/reconcile-mac-daemon.sh" ]; then ok; else fail "tooling refresh must work under a space-bearing path"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_worktree_cleanup_on_success() {
    echo "test: worktree removed on the success path"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 run_reconcile
    # The cleanup trap (lock_created=1 on a real run) removes the worktree on exit.
    if log_has "worktree remove"; then ok; else fail "success path must clean up the worktree (trap)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_worktree_cleanup_on_failure() {
    echo "test: worktree removed on a mid-run (build) failure"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA="$INSTALLED_SHA" STUB_DIFF_RC=1 STUB_BUILD_RC=1 run_reconcile
    if [ "$RC" -eq 1 ]; then ok; else fail "build failure should exit 1, got $RC"; fi
    if log_has "worktree remove"; then ok; else fail "failure path must clean up the worktree (trap)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_bootstrap_missing_stamp_deploys() {
    echo "test: missing CRMBuildSHA stamp -> must deploy (bootstrap path)"
    make_sandbox
    write_deploy_env "CRM_MAC_CODESIGN_IDENTITY=My Cert" "NTFY_URL=https://ntfy.example" "NTFY_TOPIC=tok"
    # plutil -extract exits 1 (no stamp). The relevance gate must fall through to
    # the upgrade regardless of the diff result.
    STUB_CI_CONCLUSION=success STUB_INSTALLED_SHA_MISSING=1 STUB_DIFF_RC=0 run_reconcile
    if [ "$RC" -eq 0 ]; then ok; else fail "bootstrap (no stamp) should deploy + exit 0, got $RC ($OUT)"; fi
    if log_has "deploy-mac-daemon.sh identity="; then ok; else fail "missing stamp must force a deploy (no short-circuit)"; fi
    cleanup_sandbox
    unset DEPLOY_ENV_FILE_OVERRIDE
}

test_ntfy_degrade_open() {
    echo "test: ntfy absent deploy.env -> a forced fail path logs but POSTs no ntfy"
    make_sandbox
    # No deploy.env -> NTFY disabled. Force a CI failure (would normally ntfy).
    STUB_CI_CONCLUSION=failure run_reconcile
    if [ "$RC" -eq 1 ]; then ok; else fail "CI failure should still exit 1 with ntfy disabled, got $RC"; fi
    if [ "$(ntfy_count)" -eq 0 ]; then ok; else fail "no ntfy POST may be attempted when deploy.env is absent"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_noop_when_unchanged
    test_deploys_when_changed
    test_fail_closed_on_ci_failure
    test_soft_skip_on_ci_missing
    test_transient_gh_failure_soft_skips
    test_never_invokes_gh_auth_status
    test_structural_gh_failure_informational
    test_soft_skip_on_fetch_failure
    test_fetch_advances_tracking_ref
    test_empty_identity_with_ntfy_fires
    test_empty_identity_absent_env_red_exit
    test_tooling_refresh_on_noop
    test_tooling_refresh_preserves_exec_bit
    test_timer_drift_notifies
    test_timer_drift_unchanged_no_notice
    test_timer_drift_ambiguous_silent
    test_timer_drift_template_absent_no_notice
    test_health_fail_max_ntfy
    test_health_gate_parses_by_content
    test_contacts_pending_ntfy_on_success
    test_build_failure_fail_closed
    test_lock_live_holder_defers
    test_lock_loser_does_not_touch_worktree
    test_stale_lock_dead_pid_reclaims
    test_stale_lock_ttl_no_owner_file
    test_stale_lock_garbage_stat_does_not_crash
    test_lock_released_on_signal
    test_path_with_spaces
    test_worktree_cleanup_on_success
    test_worktree_cleanup_on_failure
    test_bootstrap_missing_stamp_deploys
    test_ntfy_degrade_open

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

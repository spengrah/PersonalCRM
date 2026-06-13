#!/usr/bin/env bash
# PersonalCRM Mac deploy TRIGGER (the runner-side thin trigger).
#
# Invoked by .github/workflows/deploy-mac.yml on the [self-hosted, mac] runner.
# It does NOT build, sign, or install anything. It tells launchd to run the
# already-loaded login-session reconcile TIMER immediately, via
# `launchctl kickstart`. The timer's reconcile does all the real work (fetch ->
# CI gate -> relevance gate -> build -> codesign -> install -> health -> ntfy)
# in the user's LOGIN session, where codesign can reach the login Keychain.
#
# Why a trigger and not a direct reconcile invocation: a GitHub Actions
# self-hosted-runner job runs inside an isolated security session
# (SessionCreate=true on the runner's LaunchAgent) that cannot reach the user's
# login Keychain, so `codesign` against the local self-signed identity fails with
# errSecInternalComponent. The login-session timer CAN reach it. The design's
# central bet is that kickstarting the timer makes launchd run the reconcile in
# the TIMER's context (login session) — NOT as a child of the runner's isolated
# session — so codesign is EXPECTED to work. That runner-session->login-session
# crossing is validated by the Bucket-B supervised dry-run, not proven here.
#
# Fire-and-forget: `launchctl kickstart` returns as soon as launchd ACCEPTS the
# start request — it does NOT wait for reconcile to finish. So a green exit from
# this script means "the trigger was SENT", NOT "the deploy succeeded". The real
# deploy result is reported by reconcile's own channels (ntfy + the timer's
# reconcile-stdout.log / reconcile-stderr.log).
#
# Exit codes:
#   0  trigger sent (launchctl kickstart accepted, including the benign
#      already-running overlap, which empirically returns 0).
#   non-zero  the timer is not loaded (no kickstart target), or launchd REFUSED
#      the start (disabled service / domain mismatch / launchd error). Either is
#      a real misconfiguration the runner job should show red — the deploy is not
#      lost (the timer's RunAtLoad / StartCalendarInterval catch-up still
#      converges to origin/main), but the red surfaces the problem loudly.
#
# Userland: no sudo, launchctl-only. Targets gui/$(id -u)/<label> — no PII, no
# literal /Users/ paths.

set -euo pipefail

LABEL="${CRM_MAC_LAUNCH_AGENT_LABEL:-xyz.spengrah.crm-mac-deploy}"
TARGET="gui/$(id -u)/$LABEL"

log() { echo "[trigger-mac-deploy] $*" >&2; }

# Step 1: probe — is the timer loaded? `launchctl print <target>` exits 0 for a
# loaded service and non-zero (error 113, "Could not find specified service")
# when the label is not loaded or there is no gui domain (e.g. logged out). A
# not-loaded timer is a real misconfiguration: the on-promote deploy has no
# trigger target. Fail RED with a remediation hint. (The deploy is not lost — the
# timer's RunAtLoad fires on the next login and converges to origin/main.)
if ! launchctl print "$TARGET" >/dev/null 2>&1; then
    log "FAILED: timer '$LABEL' is not loaded in $TARGET (or no gui session)."
    log "Run \`make setup-mac-deploy\` in your login session to load the timer."
    log "(The deploy is not lost: the timer's RunAtLoad will converge on next login.)"
    exit 1
fi

# Step 2: kickstart the loaded timer. Plain kickstart (NO -k): -k would kill an
# in-flight instance, which could interrupt a build mid-codesign. An overlap with
# a scheduled fire is benign — launchd coalesces/accepts the start and returns 0
# (probed empirically), and reconcile's own mkdir lock serializes concurrent
# runs. So a NON-ZERO kickstart here is NOT a benign overlap; it means launchd
# REFUSED the start (disabled / domain mismatch / launchd error) — fail RED with
# the captured stderr. The `|| kickstart_rc=$?` keeps `set -e` from aborting
# before we can classify the non-zero rc.
kickstart_rc=0
kickstart_stderr="$(launchctl kickstart "$TARGET" 2>&1 1>/dev/null)" || kickstart_rc=$?
if [ "$kickstart_rc" -ne 0 ]; then
    log "FAILED: launchctl kickstart $TARGET exited $kickstart_rc."
    if [ -n "$kickstart_stderr" ]; then
        log "launchctl stderr: $kickstart_stderr"
    fi
    log "launchd refused the start (disabled service / domain mismatch / launchd error)."
    log "(The deploy is not lost: the timer's scheduled catch-up still converges to origin/main.)"
    exit 1
fi

log "kickstarted timer '$LABEL'. This green check means the trigger was SENT,"
log "NOT that the deploy succeeded — watch ntfy + reconcile-stderr.log for the real result."
exit 0

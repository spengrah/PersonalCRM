#!/usr/bin/env bash
# Tests for the dev-stack environment knobs the local seeding rehearsal depends
# on: dev-seed.sh's CRM_ENV precedence + DEV_SEED_RESET switch, and
# start-backend.sh's CRM_ENV precedence at BOTH of its pin sites.
#
# Both scripts `set -a; source .env`, which exports every line in that file — so
# a CRM_ENV line there beats a naive `${CRM_ENV:-testing}` default that is
# evaluated afterwards. That is the exact failure this suite exists to catch, and
# it is why the caller-wins case is asserted with a CONFLICTING .env value rather
# than an empty one.
#
# PATH-shadowed stubs (go), fixture .env, call-log assertions. No database, no
# compiler, no network.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok() { PASS=$((PASS + 1)); }

# make_sandbox <env_crm_env|__none__> : a fake project root with .env + stub go.
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : >"$CALL_LOG"
    mkdir -p "$SANDBOX/scripts" "$SANDBOX/backend" "$SANDBOX/logs" "$SANDBOX/bin"

    {
        printf 'POSTGRES_USER=u\nPOSTGRES_PASSWORD=p\nPOSTGRES_DB=d\nPOSTGRES_PORT=5432\n'
        printf 'API_KEY=k\nSESSION_SECRET=s\n'
        if [ "$1" != "__none__" ]; then printf 'CRM_ENV=%s\n' "$1"; fi
    } >"$SANDBOX/.env"

    # A `go` that records the CRM_ENV it was handed and the arguments it got,
    # then exits 0 without building anything.
    cat >"$SANDBOX/bin/go" <<EOF
#!/usr/bin/env bash
echo "go CRM_ENV=\${CRM_ENV:-__unset__} args=\$*" >> "$CALL_LOG"
exit 0
EOF
    chmod +x "$SANDBOX/bin/go"

    cp "$REPO_ROOT/scripts/dev-seed.sh" "$SANDBOX/scripts/dev-seed.sh"
    cp "$REPO_ROOT/scripts/start-backend.sh" "$SANDBOX/scripts/start-backend.sh"
}

cleanup_sandbox() { rm -rf "$SANDBOX"; }

# run_seed [VAR=VAL ...] : run dev-seed.sh in the sandbox.
run_seed() {
    env "$@" PATH="$SANDBOX/bin:$PATH" bash "$SANDBOX/scripts/dev-seed.sh" \
        >"$SANDBOX/stdout" 2>"$SANDBOX/stderr"
    RC=$?
}

# run_backend [VAR=VAL ...] : run start-backend.sh in the sandbox.
run_backend() {
    env "$@" PATH="$SANDBOX/bin:$PATH" bash "$SANDBOX/scripts/start-backend.sh" \
        >"$SANDBOX/stdout" 2>"$SANDBOX/stderr"
    RC=$?
}

seed_crm_env() { sed -n 's/^go CRM_ENV=\([^ ]*\) .*/\1/p' "$CALL_LOG" | head -1; }

assert_seed_env() {
    local want="$1" got
    got="$(seed_crm_env)"
    if [ "$got" = "$want" ]; then ok; else fail "$2: seed ran under CRM_ENV=$got, want $want"; fi
}

# ---------------------------------------------------------------------------

test_seed_default_is_preserved() {
    echo "test: dev-seed.sh with nothing set still seeds under testing (default preserved)"
    make_sandbox __none__
    run_seed
    if [ "$RC" -eq 0 ]; then ok; else fail "expected exit 0, got $RC: $(cat "$SANDBOX/stderr")"; fi
    assert_seed_env testing "no override"
    if grep -q -- '--seed --profile standard --yes' "$CALL_LOG"; then ok; else fail "default must be the ADDITIVE --seed of the standard profile: $(cat "$CALL_LOG")"; fi
    cleanup_sandbox
}

test_seed_dotenv_beats_the_default() {
    echo "test: dev-seed.sh honors a CRM_ENV line in .env when the caller sets nothing"
    make_sandbox accelerated
    run_seed
    assert_seed_env accelerated ".env value"
    cleanup_sandbox
}

test_seed_caller_beats_dotenv() {
    echo "test: dev-seed.sh CALLER's CRM_ENV beats a CONFLICTING .env line"
    make_sandbox testing
    run_seed CRM_ENV=staging
    assert_seed_env staging "caller override against a conflicting .env"
    cleanup_sandbox
}

test_seed_reset_switch() {
    echo "test: DEV_SEED_RESET=1 selects --reset-and-seed"
    make_sandbox __none__
    run_seed DEV_SEED_RESET=1 DEV_SEED_PROFILE=standard
    if grep -q -- '--reset-and-seed --profile standard --yes' "$CALL_LOG"; then ok; else fail "reset switch did not select --reset-and-seed: $(cat "$CALL_LOG")"; fi
    if grep -q -- ' --seed ' "$CALL_LOG"; then fail "the additive --seed must not also run"; else ok; fi
    cleanup_sandbox
}

test_seed_reset_off_by_default() {
    echo "test: DEV_SEED_RESET unset leaves the additive --seed (a wipe must be asked for)"
    make_sandbox __none__
    run_seed DEV_SEED_PROFILE=standard
    if grep -q -- '--seed --profile standard --yes' "$CALL_LOG"; then ok; else fail "unset DEV_SEED_RESET must stay additive: $(cat "$CALL_LOG")"; fi
    if grep -q -- '--reset-and-seed' "$CALL_LOG"; then fail "a wipe must never be the default"; else ok; fi
    cleanup_sandbox
}

test_backend_default_is_preserved() {
    echo "test: start-backend.sh with nothing set still starts under testing"
    make_sandbox __none__
    run_backend
    if [ "$RC" -eq 0 ]; then ok; else fail "expected exit 0, got $RC: $(cat "$SANDBOX/stderr")"; fi
    assert_seed_env testing "no override"
    cleanup_sandbox
}

test_backend_caller_beats_dotenv_at_both_pin_sites() {
    echo "test: start-backend.sh CALLER's CRM_ENV beats a conflicting .env, at the nohup env pin too"
    make_sandbox testing
    run_backend CRM_ENV=staging
    # The stub `go` is launched through `nohup env CRM_ENV=... go run`, so the
    # value it records IS the second pin site. A fix applied only to the export
    # above would show `testing` here.
    assert_seed_env staging "the detached process's env"
    cleanup_sandbox
}

test_backend_dotenv_beats_the_default() {
    echo "test: start-backend.sh honors a CRM_ENV line in .env when the caller sets nothing"
    make_sandbox accelerated
    run_backend
    assert_seed_env accelerated ".env value"
    cleanup_sandbox
}

main() {
    test_seed_default_is_preserved
    test_seed_dotenv_beats_the_default
    test_seed_caller_beats_dotenv
    test_seed_reset_switch
    test_seed_reset_off_by_default
    test_backend_default_is_preserved
    test_backend_caller_beats_dotenv_at_both_pin_sites
    test_backend_dotenv_beats_the_default

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

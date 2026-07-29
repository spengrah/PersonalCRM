#!/usr/bin/env bash
# Falsification suite for check-tour-markers.sh.
#
# A gate that cannot fail is indistinguishable from one that always passes, so
# every rejection this script exists to make is driven here against a stubbed
# API: a missing marker, a duplicated marker, an HTTP failure, a garbage body,
# and an SSOT the extraction cannot read. Exit 0 is asserted only for the
# all-ones case.
#
# The stub is a PATH-shadowed `curl`, so the real script runs unmodified —
# including its marker extraction, its match predicate and its exit codes.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/check-tour-markers.sh"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok() { PASS=$((PASS + 1)); }

# make_sandbox : a fixture SSOT with three markers + a stub curl driven by
# per-marker response files.
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/responses"

    printf 'const (\n\tFixtureMarkerAlpha = "fxalpha"\n\tFixtureMarkerBeta = "fxbeta"\n\tFixtureMarkerGamma = "fxgamma"\n)\n' \
        >"$SANDBOX/fixtures.go"

    cat >"$SANDBOX/bin/curl" <<EOF
#!/usr/bin/env bash
# Stub curl: the last argument is the URL; the marker is its search= param.
url="\${@: -1}"
marker="\${url##*search=}"
if [ -f "$SANDBOX/responses/\$marker.fail" ]; then
    echo "stub curl: simulated HTTP failure" >&2
    exit 22
fi
if [ -f "$SANDBOX/responses/\$marker.json" ]; then
    cat "$SANDBOX/responses/\$marker.json"
    exit 0
fi
echo '{"data":[]}'
exit 0
EOF
    chmod +x "$SANDBOX/bin/curl"

    # Default: every marker resolves to exactly one contact.
    for m in fxalpha fxbeta fxgamma; do
        printf '{"data":[{"id":"1","full_name":"synth-ns-Zeta Testwell %s"}]}\n' "$m" >"$SANDBOX/responses/$m.json"
    done
}

cleanup_sandbox() { rm -rf "$SANDBOX"; }

# run_check [FIXTURES_GO_OVERRIDE] : run the script; sets RC and OUT.
run_check() {
    local fixtures="${1:-$SANDBOX/fixtures.go}"
    OUT="$(PATH="$SANDBOX/bin:$PATH" FIXTURES_GO="$fixtures" TOURS_API_URL=http://stub.invalid \
        TOURS_API_KEY=stub-key bash "$SCRIPT" 2>&1)"
    RC=$?
}

assert_rc() {
    if [ "$RC" -eq "$1" ]; then ok; else fail "$2: exit $RC, want $1 (output: $OUT)"; fi
}

assert_names() {
    if printf '%s' "$OUT" | grep -q "$1"; then ok; else fail "$2: output did not name '$1': $OUT"; fi
}

# ---------------------------------------------------------------------------

test_all_ones_passes() {
    echo "test: exit 0 only when every marker resolves to exactly one contact"
    make_sandbox
    run_check
    assert_rc 0 "all-ones"
    assert_names "all 3 markers" "all-ones"
    cleanup_sandbox
}

test_missing_marker_fails() {
    echo "test: exit 1 when a marker resolves to zero contacts"
    make_sandbox
    printf '{"data":[]}\n' >"$SANDBOX/responses/fxbeta.json"
    run_check
    assert_rc 1 "missing marker"
    assert_names "fxbeta" "missing marker"
    assert_names "0 contacts" "missing marker"
    cleanup_sandbox
}

test_duplicate_marker_fails() {
    echo "test: exit 1 when a marker resolves to two contacts"
    make_sandbox
    printf '{"data":[{"id":"1","full_name":"a fxgamma"},{"id":"2","full_name":"b fxgamma"}]}\n' \
        >"$SANDBOX/responses/fxgamma.json"
    run_check
    assert_rc 1 "duplicate marker"
    assert_names "fxgamma" "duplicate marker"
    assert_names "2 contacts" "duplicate marker"
    cleanup_sandbox
}

test_http_failure_fails() {
    echo "test: exit 1 on an HTTP failure (never a silent pass)"
    make_sandbox
    touch "$SANDBOX/responses/fxalpha.fail"
    run_check
    assert_rc 1 "http failure"
    assert_names "HTTP failure" "http failure"
    cleanup_sandbox
}

test_garbage_body_fails() {
    echo "test: exit 1 on an unreadable response body"
    make_sandbox
    printf 'not json at all\n' >"$SANDBOX/responses/fxalpha.json"
    run_check
    assert_rc 1 "garbage body"
    assert_names "unreadable response body" "garbage body"
    cleanup_sandbox
}

test_unreadable_ssot_fails() {
    echo "test: exit 1 when the marker SSOT is missing or yields no markers"
    make_sandbox
    run_check "$SANDBOX/does-not-exist.go"
    assert_rc 1 "missing SSOT"
    assert_names "cannot read the marker SSOT" "missing SSOT"

    printf 'package synthetic\n// no markers here\n' >"$SANDBOX/empty.go"
    run_check "$SANDBOX/empty.go"
    assert_rc 1 "empty SSOT"
    assert_names "found NO marker constants" "empty SSOT"
    cleanup_sandbox
}

test_neighbour_rows_are_ignored() {
    echo "test: search neighbours that do not carry the marker do not count"
    make_sandbox
    # Full-text search ranks and returns neighbours; only the marker decides.
    printf '{"data":[{"id":"1","full_name":"synth-ns-Vex Mockford"},{"id":"2","full_name":"synth-ns-Zeta Testwell fxbeta"}]}\n' \
        >"$SANDBOX/responses/fxbeta.json"
    run_check
    assert_rc 0 "neighbour rows"
    cleanup_sandbox
}

test_reads_the_real_ssot() {
    echo "test: the extraction finds the REAL marker constants (the regex is load-bearing)"
    make_sandbox
    # Point at the committed SSOT but keep the stub curl, which answers every
    # unknown marker with an empty list — so this must FAIL, and the failure count
    # tells us how many markers the extraction actually found.
    run_check "$REPO_ROOT/backend/internal/synthetic/fixtures.go"
    assert_rc 1 "real SSOT with an empty world"
    if printf '%s' "$OUT" | grep -qE 'of 11 markers did not resolve uniquely'; then ok
    else fail "extraction did not find the expected 11 markers in the committed SSOT: $OUT"; fi
    cleanup_sandbox
}

main() {
    test_all_ones_passes
    test_missing_marker_fails
    test_duplicate_marker_fails
    test_http_failure_fails
    test_garbage_body_fails
    test_unreadable_ssot_fails
    test_neighbour_rows_are_ignored
    test_reads_the_real_ssot

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

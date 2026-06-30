#!/usr/bin/env bash
# Tests for staging-deployed-sha.sh — the host-pinned base reader.
#
# Writes a fixture backend Quadlet unit with various Image= refs and asserts the
# script prints the 40-hex git sha ONLY for a :<40-hex> pin (digest / :latest /
# missing unit -> empty + nonzero). PATH-shadows `sudo` so `sudo -u staging sed`
# runs the real sed against the fixture. No Pi/podman/root.
#
# Portability: no BSD-only stat -f / date -r / sed -i.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/staging-deployed-sha.sh"
SHA="1111111111111111111111111111111111111111"
DIGEST="sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
REPO="ghcr.io/spengrah/personalcrm-backend"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    mkdir -p "$SANDBOX/bin"
    UNIT="$SANDBOX/backend.container"
    # Minimal sudo stub: skip -u <user>, -n, and leading KEY=val env tokens until
    # the first non-option (the command), then exec the rest verbatim.
    cat > "$SANDBOX/bin/sudo" <<'EOF'
#!/usr/bin/env bash
args=("$@"); out=(); i=0; opts=1
while [ $i -lt ${#args[@]} ]; do
    a="${args[$i]}"
    if [ "$opts" = "1" ]; then
        if [ "$a" = "-u" ]; then i=$((i + 2)); continue; fi
        if [ "$a" = "-n" ]; then i=$((i + 1)); continue; fi
        if [[ "$a" == *=* ]]; then i=$((i + 1)); continue; fi
        opts=0
    fi
    out+=("$a"); i=$((i + 1))
done
exec "${out[@]}"
EOF
    chmod +x "$SANDBOX/bin/sudo"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

write_unit() { printf '[Container]\nContainerName=crm-backend\nImage=%s\nNetwork=crm.network\n' "$1" > "$UNIT"; }

# run_reader : run the script with the fixture unit; sets RC + OUT (stdout).
run_reader() {
    OUT="$(PATH="$SANDBOX/bin:$PATH" STAGING_BACKEND_UNIT="$UNIT" bash "$SCRIPT" 2>/dev/null)"
    RC=$?
}

# ===========================================================================
test_sha_tag_prints() {
    echo "test: Image=repo:<40-hex> -> prints the sha, exit 0"
    make_sandbox
    write_unit "$REPO:$SHA"
    run_reader
    if [ "$RC" -eq 0 ]; then ok; else fail "sha pin must exit 0, got $RC"; fi
    if [ "$OUT" = "$SHA" ]; then ok; else fail "must print the sha, got '$OUT'"; fi
    cleanup_sandbox
}

test_digest_empty() {
    echo "test: Image=repo@sha256:<digest> -> empty + nonzero (64-hex != 40)"
    make_sandbox
    write_unit "$REPO@$DIGEST"
    run_reader
    if [ "$RC" -ne 0 ]; then ok; else fail "digest must exit nonzero, got $RC"; fi
    if [ -z "$OUT" ]; then ok; else fail "digest must print nothing, got '$OUT'"; fi
    cleanup_sandbox
}

test_latest_empty() {
    echo "test: Image=repo:latest -> empty + nonzero"
    make_sandbox
    write_unit "$REPO:latest"
    run_reader
    if [ "$RC" -ne 0 ]; then ok; else fail "latest must exit nonzero, got $RC"; fi
    if [ -z "$OUT" ]; then ok; else fail "latest must print nothing, got '$OUT'"; fi
    cleanup_sandbox
}

test_missing_unit_empty() {
    echo "test: missing unit -> empty + nonzero"
    make_sandbox
    rm -f "$UNIT"
    run_reader
    if [ "$RC" -ne 0 ]; then ok; else fail "missing unit must exit nonzero, got $RC"; fi
    if [ -z "$OUT" ]; then ok; else fail "missing unit must print nothing, got '$OUT'"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_sha_tag_prints
    test_digest_empty
    test_latest_empty
    test_missing_unit_empty

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

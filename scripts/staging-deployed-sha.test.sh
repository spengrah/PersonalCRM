#!/usr/bin/env bash
# Tests for staging-deployed-sha.sh — the host-pinned base reader.
#
# Writes a fixture backend Quadlet unit with various Image= refs and asserts the
# script prints the 40-hex git sha ONLY for a :<40-hex> pin (digest / :latest /
# missing unit -> empty + nonzero). The tenant identity + unit path are HARDCODED
# in the script (the env-trust seam), so the PATH-shadowed `sudo` stub rewrites the
# hardcoded staging unit path to the test fixture (no /var/lib/staging touch) and
# logs calls — and an override-rejection test confirms a caller-supplied
# CRM_USER/CRM_HOME/STAGING_BACKEND_UNIT cannot redirect the read. No Pi/podman/root.
#
# Portability: no BSD-only stat -f / date -r / sed -i.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/staging-deployed-sha.sh"
SHA="1111111111111111111111111111111111111111"
EVIL_SHA="2222222222222222222222222222222222222222"
DIGEST="sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
REPO="ghcr.io/spengrah/personalcrm-backend"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"
    UNIT="$SANDBOX/backend.container"
    # sudo stub: log the full call, skip -u <user>/-n/leading KEY=val tokens until
    # the command, then rewrite the script's HARDCODED staging unit path to the test
    # fixture (so the read is observable without touching /var/lib/staging) and exec.
    # A NON-staging path (e.g. an injected override) is NOT rewritten, so an
    # override-honoring bug would read the wrong file and surface a different sha.
    cat > "$SANDBOX/bin/sudo" <<EOF
#!/usr/bin/env bash
echo "sudo \$*" >> "$CALL_LOG"
args=("\$@"); out=(); i=0; opts=1
while [ \$i -lt \${#args[@]} ]; do
    a="\${args[\$i]}"
    if [ "\$opts" = "1" ]; then
        if [ "\$a" = "-u" ]; then i=\$((i + 2)); continue; fi
        if [ "\$a" = "-n" ]; then i=\$((i + 1)); continue; fi
        if [[ "\$a" == *=* ]]; then i=\$((i + 1)); continue; fi
        opts=0
    fi
    case "\$a" in
        */personalcrm-backend.container) out+=("$UNIT") ;;
        *) out+=("\$a") ;;
    esac
    i=\$((i + 1))
done
exec "\${out[@]}"
EOF
    chmod +x "$SANDBOX/bin/sudo"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

write_unit() { printf '[Container]\nContainerName=crm-backend\nImage=%s\nNetwork=crm.network\n' "$1" > "$UNIT"; }

# run_reader : run the script; sets RC + OUT (stdout). No env passed — the script
# hardcodes its tenant identity + unit path.
run_reader() {
    OUT="$(PATH="$SANDBOX/bin:$PATH" bash "$SCRIPT" 2>/dev/null)"
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

test_override_rejection() {
    echo "test: caller CRM_USER/CRM_HOME/STAGING_BACKEND_UNIT overrides are IGNORED (hardcoded seam)"
    make_sandbox
    write_unit "$REPO:$SHA"                                  # the real staging unit -> good sha
    printf '[Container]\nImage=%s\n' "$REPO:$EVIL_SHA" > "$SANDBOX/evil.container"
    OUT="$(PATH="$SANDBOX/bin:$PATH" CRM_USER=attacker CRM_HOME=/evil STAGING_BACKEND_UNIT="$SANDBOX/evil.container" bash "$SCRIPT" 2>/dev/null)"
    RC=$?
    if [ "$RC" -eq 0 ]; then ok; else fail "override run should still read staging + succeed, got $RC"; fi
    if [ "$OUT" = "$SHA" ]; then ok; else fail "must read the HARDCODED staging unit (got '$OUT')"; fi
    if [ "$OUT" = "$EVIL_SHA" ]; then fail "override redirected the read to the attacker unit"; else ok; fi
    if grep -qF -- '-u staging' "$CALL_LOG"; then ok; else fail "must sudo to the staging tenant"; fi
    if grep -qF -- '-u attacker' "$CALL_LOG"; then fail "must NOT sudo to a caller-supplied tenant"; else ok; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_sha_tag_prints
    test_digest_empty
    test_latest_empty
    test_missing_unit_empty
    test_override_rejection

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

#!/bin/bash
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP_SCRIPT="$REPO_ROOT/scripts/backup-offsite.sh"
VERIFY_SCRIPT="$REPO_ROOT/scripts/verify-offsite-backup.sh"
RESTORE_SCRIPT="$REPO_ROOT/scripts/restore-offsite.sh"
ORIGINAL_PATH="$PATH"

PASS=0
FAIL=0

fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok() { PASS=$((PASS + 1)); }

require_backup_tools() {
    local missing="" tool
    for tool in age rclone zstd; do
        if ! command -v "$tool" >/dev/null 2>&1; then
            missing="$missing $tool"
        fi
    done
    if [ -n "$missing" ]; then
        echo "FAIL: missing backup pipeline tool(s):$missing. Install age, rclone, and zstd before running this test." >&2
        return 1
    fi
}

# The round-trip assertions are meaningless without the real format and remote
# implementations, so missing dependencies stop the harness instead of skipping.
if ! require_backup_tools; then
    exit 1
fi

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    RCLONE_CONFIG="$SANDBOX/rclone.conf"
    REMOTE_PATH="local:$SANDBOX/remote"
    FIXTURE_SQL="$SANDBOX/fixture.sql"
    FIXTURE_ENV="$SANDBOX/fixture.env"
    AGE_IDENTITY="$SANDBOX/age.key"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/remote" "$SANDBOX/tmp"

    printf '[local]\ntype = local\n' > "$RCLONE_CONFIG"
    {
        printf 'CREATE TABLE contact (id integer, deleted_at timestamp, label text);\n'
        printf 'CREATE TABLE schema_migrations (version integer);\n'
        i=1
        while [ "$i" -le 180 ]; do
            printf 'INSERT INTO contact VALUES (%s, NULL, '\''fixture-%03d'\'');\n' "$i" "$i"
            i=$((i + 1))
        done
    } > "$FIXTURE_SQL"
    printf 'DATABASE_URL=<database-url>\nCRM_FIXTURE=round-trip\n' > "$FIXTURE_ENV"

    age-keygen -o "$AGE_IDENTITY" > "$SANDBOX/age-keygen.out" 2>&1
    AGE_RECIPIENT="$(age-keygen -y "$AGE_IDENTITY")"

    cat > "$SANDBOX/bin/pgstub" <<EOF
#!/bin/bash
{
    printf 'pgstub'
    for arg in "\$@"; do printf ' [%s]' "\$arg"; done
    printf '\\n'
} >> "$CALL_LOG"

command_name="\${1:-}"
if [ "\$command_name" = "pg_dump" ]; then
    if [ -n "\${STUB_PG_DUMP_EXIT:-}" ]; then
        exit "\$STUB_PG_DUMP_EXIT"
    fi
    cat "$FIXTURE_SQL"
    exit 0
fi
if [ "\$command_name" != "psql" ]; then
    echo "unexpected pgstub subcommand: \$command_name" >&2
    exit 2
fi

shift
db=""
query=""
args=("\$@")
i=0
while [ "\$i" -lt "\${#args[@]}" ]; do
    case "\${args[\$i]}" in
        -d) i=\$((i + 1)); db="\${args[\$i]}" ;;
        -c|-qAtc) i=\$((i + 1)); query="\${args[\$i]}" ;;
    esac
    i=\$((i + 1))
done

count_file="$SANDBOX/psql-count"
if [ -f "\$count_file" ]; then
    n="\$(cat "\$count_file")"
else
    n=0
fi
n=\$((n + 1))
printf '%s\\n' "\$n" > "\$count_file"
if [ -z "\$query" ]; then
    cat > "$SANDBOX/psql-stdin.\$n"
else
    : > "$SANDBOX/psql-stdin.\$n"
fi

case "\$query" in
    *schema_migrations*)
        case "\$db" in
            personal_crm) printf '%s\\n' "\${STUB_MIGRATION_VERSION_personal_crm:-81}" ;;
            personal_crm_verify) printf '%s\\n' "\${STUB_MIGRATION_VERSION_personal_crm_verify:-81}" ;;
        esac
        ;;
    *'count(*)'*)
        case "\$db" in
            personal_crm) printf '%s\\n' "\${STUB_CONTACT_COUNT_personal_crm:-500}" ;;
            personal_crm_verify) printf '%s\\n' "\${STUB_CONTACT_COUNT_personal_crm_verify:-500}" ;;
            personal_crm_restore) printf '%s\\n' "\${STUB_CONTACT_COUNT_personal_crm_restore:-500}" ;;
        esac
        ;;
    *pg_database*) printf '%s\\n' "\${STUB_DB_EXISTS:-}" ;;
esac
EOF
    chmod +x "$SANDBOX/bin/pgstub"

    STUB_PG_DUMP_EXIT=""
    STUB_DB_EXISTS=""
    STUB_MIGRATION_VERSION_personal_crm=81
    STUB_MIGRATION_VERSION_personal_crm_verify=81
    STUB_CONTACT_COUNT_personal_crm=500
    STUB_CONTACT_COUNT_personal_crm_verify=500
}

cleanup_sandbox() {
    [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"
}

run_backup() {
    OUT="$(
        PATH="$SANDBOX/bin:$ORIGINAL_PATH" \
        RCLONE_CONFIG="$RCLONE_CONFIG" \
        BACKUP_REMOTE=local BACKUP_BUCKET="$SANDBOX/remote" \
        AGE_RECIPIENT="$AGE_RECIPIENT" CRM_ENV_FILE="$FIXTURE_ENV" \
        PG_EXEC="$SANDBOX/bin/pgstub" TMPDIR="$SANDBOX/tmp" \
        STUB_PG_DUMP_EXIT="${STUB_PG_DUMP_EXIT:-}" \
        bash "$BACKUP_SCRIPT" 2>"$SANDBOX/stderr"
    )"
    RC=$?
}

run_verify() {
    OUT="$(
        PATH="$SANDBOX/bin:$ORIGINAL_PATH" \
        RCLONE_CONFIG="$RCLONE_CONFIG" \
        BACKUP_REMOTE=local BACKUP_BUCKET="$SANDBOX/remote" \
        AGE_IDENTITY_FILE="$AGE_IDENTITY" PG_EXEC="$SANDBOX/bin/pgstub" \
        TMPDIR="$SANDBOX/tmp" PG_USER=crm_user PG_DB=personal_crm \
        VERIFY_DB=personal_crm_verify \
        BACKUP_MAX_AGE_HOURS=36 VERIFY_CONTACT_TOLERANCE_PCT=5 \
        STUB_MIGRATION_VERSION_personal_crm="$STUB_MIGRATION_VERSION_personal_crm" \
        STUB_MIGRATION_VERSION_personal_crm_verify="$STUB_MIGRATION_VERSION_personal_crm_verify" \
        STUB_CONTACT_COUNT_personal_crm="$STUB_CONTACT_COUNT_personal_crm" \
        STUB_CONTACT_COUNT_personal_crm_verify="$STUB_CONTACT_COUNT_personal_crm_verify" \
        STUB_DB_EXISTS="${STUB_DB_EXISTS:-}" \
        bash "$VERIFY_SCRIPT" 2>"$SANDBOX/stderr"
    )"
    RC=$?
}

run_restore() {
    local object="$1" target="${2:-}"
    OUT="$(
        PATH="$SANDBOX/bin:$ORIGINAL_PATH" \
        RCLONE_CONFIG="$RCLONE_CONFIG" \
        BACKUP_REMOTE=local BACKUP_BUCKET="$SANDBOX/remote" \
        AGE_IDENTITY_FILE="$AGE_IDENTITY" PG_EXEC="$SANDBOX/bin/pgstub" \
        TMPDIR="$SANDBOX/tmp" PG_USER=crm_user \
        STUB_DB_EXISTS="${STUB_DB_EXISTS:-}" \
        bash "$RESTORE_SCRIPT" "$object" ${target:+"$target"} 2>"$SANDBOX/stderr"
    )"
    RC=$?
}

list_db_objects() {
    RCLONE_CONFIG="$RCLONE_CONFIG" rclone lsf "$REMOTE_PATH" --files-only --include 'personal_crm-*.sql.zst.age'
}

list_env_objects() {
    RCLONE_CONFIG="$RCLONE_CONFIG" rclone lsf "$REMOTE_PATH" --files-only --include 'personalcrm-env-*.age'
}

newest_db_object() {
    list_db_objects | LC_ALL=C sort | tail -n 1
}

write_db_object() {
    local object="$1"
    TMPDIR="$SANDBOX/tmp" RCLONE_CONFIG="$RCLONE_CONFIG" \
        sh -c 'cat "$1" | zstd -q -3 | age -r "$2" | rclone rcat "$3/$4"' \
        sh "$FIXTURE_SQL" "$AGE_RECIPIENT" "$REMOTE_PATH" "$object"
}

find_payload() {
    local file
    for file in "$SANDBOX"/psql-stdin.*; do
        if [ -s "$file" ]; then
            printf '%s\n' "$file"
            return 0
        fi
    done
    return 1
}

assert_tmp_empty() {
    if [ -z "$(find "$SANDBOX/tmp" -mindepth 1 -print -quit)" ]; then
        ok
    else
        fail "TMPDIR was not empty after the script run"
    fi
}

test_tool_preflight() {
    echo "test: required backup pipeline tools resolve before scenarios"
    if require_backup_tools; then ok; else fail "tool preflight should pass in the test environment"; fi
}

test_backup_happy_path() {
    echo "test: backup round trip creates encrypted database and environment objects"
    make_sandbox
    run_backup
    if [ "$RC" -eq 0 ]; then ok; else fail "backup should exit 0, got $RC"; fi

    db_object="$(list_db_objects)"
    env_object="$(list_env_objects)"
    if [[ "$db_object" =~ ^personal_crm-[0-9]{8}T[0-9]{6}Z\.sql\.zst\.age$ ]]; then ok
    else fail "database object name has the wrong pattern: $db_object"; fi
    if [[ "$env_object" =~ ^personalcrm-env-[0-9]{8}T[0-9]{6}Z\.age$ ]]; then ok
    else fail "environment object name has the wrong pattern: $env_object"; fi

    if RCLONE_CONFIG="$RCLONE_CONFIG" rclone cat "$REMOTE_PATH/$db_object" \
        | age -d -i "$AGE_IDENTITY" | zstd -d -q > "$SANDBOX/db-round-trip.sql"; then
        if cmp -s "$FIXTURE_SQL" "$SANDBOX/db-round-trip.sql"; then ok
        else fail "decrypted database object differs from the fixture"; fi
    else fail "database object could not be decrypted and decompressed"; fi
    if RCLONE_CONFIG="$RCLONE_CONFIG" rclone cat "$REMOTE_PATH/$env_object" \
        | age -d -i "$AGE_IDENTITY" > "$SANDBOX/env-round-trip"; then
        if cmp -s "$FIXTURE_ENV" "$SANDBOX/env-round-trip"; then ok
        else fail "decrypted environment object differs from the fixture"; fi
    else fail "environment object could not be decrypted"; fi
    if grep -qE "$db_object [1-9][0-9]* bytes" <<< "$OUT"; then ok
    else fail "backup summary did not report a non-zero database size"; fi
    if grep -qE "$env_object [1-9][0-9]* bytes" <<< "$OUT"; then ok
    else fail "backup summary did not report a non-zero environment size"; fi
    assert_tmp_empty
    cleanup_sandbox
}

test_backup_dump_failure_cleans_partial() {
    echo "test: pg_dump failure removes the partial database object and skips env upload"
    make_sandbox
    STUB_PG_DUMP_EXIT=3
    run_backup
    if [ "$RC" -ne 0 ]; then ok; else fail "failed pg_dump should make backup exit non-zero"; fi
    if grep -qi 'database backup pipeline failed' "$SANDBOX/stderr"; then ok
    else fail "backup stderr did not identify the failed database pipeline"; fi
    if [ -z "$(list_db_objects)" ]; then ok; else fail "partial database object remained"; fi
    if [ -z "$(list_env_objects)" ]; then ok; else fail "environment object was uploaded after database failure"; fi
    assert_tmp_empty
    cleanup_sandbox
}

prepare_fresh_backup() {
    STUB_PG_DUMP_EXIT=""
    run_backup
    if [ "$RC" -ne 0 ]; then
        fail "fixture backup failed while preparing a scenario"
        return 1
    fi
    return 0
}

test_verify_happy_path() {
    echo "test: verify restores the newest object and drops its scratch database"
    make_sandbox
    if prepare_fresh_backup; then
        run_verify
        if [ "$RC" -eq 0 ]; then ok; else fail "verify should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
        payload="$(find_payload || true)"
        if [ -n "$payload" ] && cmp -s "$FIXTURE_SQL" "$payload"; then ok
        else fail "verify psql stdin was not byte-identical to the fixture"; fi
        create_line="$(grep -n 'CREATE DATABASE personal_crm_verify' "$CALL_LOG" | head -n 1 | cut -d: -f1)"
        final_drop_line="$(grep -n 'DROP DATABASE IF EXISTS personal_crm_verify' "$CALL_LOG" | tail -n 1 | cut -d: -f1)"
        if [ -n "$create_line" ] && [ -n "$final_drop_line" ] && [ "$create_line" -lt "$final_drop_line" ]; then ok
        else fail "verify did not create before its final scratch database drop"; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

test_verify_stale_before_database() {
    echo "test: stale newest backup fails before any database call"
    make_sandbox
    old_ts="$(date -u -d '48 hours ago' +%Y%m%dT%H%M%SZ 2>/dev/null || date -u -v-48H +%Y%m%dT%H%M%SZ)"
    old_object="personal_crm-$old_ts.sql.zst.age"
    write_db_object "$old_object"
    run_verify
    if [ "$RC" -ne 0 ]; then ok; else fail "stale backup should make verify exit non-zero"; fi
    if grep -qi stale "$SANDBOX/stderr"; then ok; else fail "stale verify failure did not say stale"; fi
    if [ ! -s "$CALL_LOG" ]; then ok; else fail "stale verify made a database call"; fi
    assert_tmp_empty
    cleanup_sandbox
}

test_verify_migration_mismatch() {
    echo "test: migration mismatch fails and the trap drops the scratch database"
    make_sandbox
    STUB_MIGRATION_VERSION_personal_crm=81
    STUB_MIGRATION_VERSION_personal_crm_verify=80
    if prepare_fresh_backup; then
        run_verify
        if [ "$RC" -ne 0 ]; then ok; else fail "migration mismatch should make verify exit non-zero"; fi
        if grep -q 'live=81 restored=80' "$SANDBOX/stderr"; then ok
        else fail "migration mismatch did not name both versions"; fi
        if grep -q 'DROP DATABASE IF EXISTS personal_crm_verify' "$CALL_LOG"; then ok
        else fail "migration mismatch did not run the cleanup drop"; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

test_verify_contact_tolerance() {
    echo "test: contact count outside tolerance fails"
    make_sandbox
    STUB_CONTACT_COUNT_personal_crm=500
    STUB_CONTACT_COUNT_personal_crm_verify=400
    if prepare_fresh_backup; then
        run_verify
        if [ "$RC" -ne 0 ]; then ok; else fail "out-of-tolerance contacts should make verify exit non-zero"; fi
        if grep -q 'live=500 restored=400' "$SANDBOX/stderr"; then ok
        else fail "contact tolerance failure did not name both counts"; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

test_verify_corruption() {
    echo "test: corruption in the newest object fails and still drops the scratch database"
    make_sandbox
    if prepare_fresh_backup; then
        db_object="$(newest_db_object)"
        db_path="$SANDBOX/remote/$db_object"
        size="$(wc -c < "$db_path")"
        offset=$((size / 2))
        dd if=/dev/zero of="$db_path" bs=1 count=1 seek="$offset" conv=notrunc >/dev/null 2>&1
        run_verify
        if [ "$RC" -ne 0 ]; then ok; else fail "corrupt backup should make verify exit non-zero"; fi
        if grep -q 'DROP DATABASE IF EXISTS personal_crm_verify' "$CALL_LOG"; then ok
        else fail "corruption failure did not run the cleanup drop"; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

test_restore_targets() {
    echo "test: restore creates a missing default target and preserves an existing target"
    make_sandbox
    if prepare_fresh_backup; then
        db_object="$(newest_db_object)"
        STUB_DB_EXISTS=""
        run_restore "$db_object"
        if [ "$RC" -eq 0 ]; then ok; else fail "default restore should exit 0, got $RC"; fi
        if grep -q '\[-d\] \[personal_crm_restore\]' "$CALL_LOG"; then ok
        else fail "default restore did not target personal_crm_restore"; fi
        if grep -q 'CREATE DATABASE personal_crm_restore' "$CALL_LOG"; then ok
        else fail "default restore did not create a missing database"; fi
        payload="$(find_payload || true)"
        if [ -n "$payload" ] && cmp -s "$FIXTURE_SQL" "$payload"; then ok
        else fail "default restore stdin was not byte-identical to the fixture"; fi

        : > "$CALL_LOG"
        STUB_DB_EXISTS=1
        run_restore "$db_object"
        if [ "$RC" -eq 0 ]; then ok; else fail "existing-target restore should exit 0, got $RC"; fi
        if grep -q 'CREATE DATABASE personal_crm_restore' "$CALL_LOG"; then fail "existing target was recreated"; else ok; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

test_restore_explicit_live_name_never_drops() {
    echo "test: explicit live target is allowed but restore never drops it"
    make_sandbox
    if prepare_fresh_backup; then
        db_object="$(newest_db_object)"
        STUB_DB_EXISTS=1
        run_restore "$db_object" personal_crm
        if [ "$RC" -eq 0 ]; then ok; else fail "explicit target restore should exit 0, got $RC"; fi
        if grep -q '\[-d\] \[personal_crm\]' "$CALL_LOG"; then ok
        else fail "explicit restore did not target personal_crm"; fi
        if grep -qi 'DROP' "$CALL_LOG"; then fail "restore must never issue a DROP"; else ok; fi
    fi
    assert_tmp_empty
    cleanup_sandbox
}

main() {
    test_tool_preflight
    test_backup_happy_path
    test_backup_dump_failure_cleans_partial
    test_verify_happy_path
    test_verify_stale_before_database
    test_verify_migration_mismatch
    test_verify_contact_tolerance
    test_verify_corruption
    test_restore_targets
    test_restore_explicit_live_name_never_drops

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"

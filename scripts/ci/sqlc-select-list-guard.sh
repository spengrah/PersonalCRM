#!/usr/bin/env bash
# sqlc-select-list-guard.sh — grep/awk guard for the duplicated-SELECT-list
# invariant (#348).
#
# Full-row reads must use SELECT * (sqlc expands it at codegen time, so adding
# a column needs no query edit). A repeated explicit multi-column projection of
# the same table is the duplication smell to avoid — it forces lockstep edits
# of parallel column lists when a column is added.
#
# This guard scans ONLY the SOURCE query files in
# backend/internal/db/queries/*.sql — never the generated *.sql.go (those are
# full of identical column lists that sqlc auto-expands from SELECT *, which is
# harmless). It flags any identical explicit (non-*) SELECT projection of >= 3
# columns that appears in 2+ queries and is not allowlisted.
#
# This is the reviewer-visible CI signal; the authoritative check is the Go
# test backend/tests/sqlc_select_list_static_test.go (it parses SQL more
# robustly). Mirrors scripts/ci/crm-marker-construction-guard.sh and
# scripts/ci/followup-sole-writer-guard.sh.

set -euo pipefail

cd "$(dirname "$0")/../.."

QUERIES_DIR="backend/internal/db/queries"

# Allowlist of normalized fingerprints (table|sorted-lowercased-columns) that
# legitimately repeat. Keep in sync with allowedDuplicateProjections in the Go
# test.
#   oauth_credential status projection: intentionally omits encrypted-token
#   columns; SELECT * would leak secrets.
ALLOWLIST=(
  "oauth_credential|account_id, account_name, created_at, expires_at, id, provider, scopes, updated_at"
)

is_allowlisted() {
  local fp="$1"
  local a
  for a in "${ALLOWLIST[@]}"; do
    [[ "$fp" == "$a" ]] && return 0
  done
  return 1
}

# Emit one line "<table>|<sorted col list>\t<file>:<line>:<query>" per
# qualifying query (line = the -- name: marker line). The awk script
# accumulates each query's full text (queries are delimited by "-- name:"
# markers), then extracts the outermost top-level SELECT...FROM projection and
# fingerprints it. Skips SELECT *, DISTINCT, aggregate-only, and SELECT 1 — the
# same shapes the Go test skips — and like the Go test reaches the outer SELECT
# of a WITH (CTE) query rather than the CTE's inner SELECT.
extract_one_file() {
  awk -v fname="$1" '
    function trim(s) { gsub(/^[ \t\n]+|[ \t\n]+$/, "", s); return s }
    function emit(   text, sp, fp, after, table, proj, depth, i, ch, cur, n, parts, c, cols, k, key, j, out, ncols, hasplain, selpos) {
      text = buf
      buf = ""
      if (qname == "") return
      # Find the OUTERMOST SELECT — the first SELECT at paren-depth 0. CTEs
      # (WITH ... AS (SELECT ...)) and subqueries nest their SELECT inside
      # parentheses (depth >= 1), so depth-0 scoping skips them and lands on
      # the main outer SELECT. Mirrors findTopLevelKeyword in the Go test.
      depth = 0; selpos = 0
      for (i = 1; i <= length(text); i++) {
        ch = substr(text, i, 1)
        if (ch == "(") { depth++; continue }
        if (ch == ")") { if (depth > 0) depth--; continue }
        if (depth == 0 && toupper(substr(text, i, 6)) == "SELECT" \
            && (i == 1 || substr(text, i-1, 1) ~ /[^a-zA-Z0-9_]/) \
            && (substr(text, i+6, 1) ~ /[ \t\n]/)) {
          selpos = i + 6
          break
        }
      }
      if (selpos == 0) return
      text = substr(text, selpos)
      # Walk to the first top-level FROM, accumulating the projection.
      depth = 0; proj = ""; table = ""; i = 1
      while (i <= length(text)) {
        # Top-level FROM keyword bounded by non-word chars.
        if (depth == 0 && toupper(substr(text, i, 4)) == "FROM" \
            && (i == 1 || substr(text, i-1, 1) ~ /[^a-zA-Z0-9_]/) \
            && (substr(text, i+4, 1) ~ /[ \t\n]/)) {
          after = trim(substr(text, i + 4))
          if (match(after, /^[a-zA-Z_][a-zA-Z0-9_]*/))
            table = tolower(substr(after, RSTART, RLENGTH))
          break
        }
        ch = substr(text, i, 1)
        if (ch == "(") depth++
        else if (ch == ")") { if (depth > 0) depth-- }
        proj = proj ch
        i++
      }
      proj = trim(proj)
      if (proj == "" || table == "") return
      if (proj == "*") return
      if (tolower(proj) ~ /^distinct/) return
      if (proj == "1") return
      # Split projection on top-level commas.
      depth = 0; cur = ""; n = 0
      for (i = 1; i <= length(proj); i++) {
        ch = substr(proj, i, 1)
        if (ch == "(") depth++
        else if (ch == ")") { if (depth > 0) depth-- }
        if (ch == "," && depth == 0) { parts[++n] = cur; cur = "" }
        else cur = cur ch
      }
      parts[++n] = cur
      # Keep ALL top-level terms (columns AND expressions) with whitespace
      # collapsed — matching the Go test, which fingerprints the full term
      # list. Track whether at least one term is a bare identifier (the
      # hasPlainColumn gate); a projection of only expressions/aggregates is
      # not a hand-maintained column list and is skipped.
      k = 0; hasplain = 0
      for (i = 1; i <= n; i++) {
        c = tolower(trim(parts[i]))
        gsub(/[ \t]+/, " ", c)
        if (c == "" || c == "*") continue
        cols[++k] = c
        if (index(c, "(") == 0 && index(c, " as ") == 0 && c !~ /[ \t]/) hasplain = 1
      }
      ncols = k
      delete parts
      if (hasplain == 0 || ncols < 3) { delete cols; return }
      # Insertion-sort columns for an order-independent fingerprint.
      for (i = 2; i <= ncols; i++) {
        key = cols[i]; j = i - 1
        while (j >= 1 && cols[j] > key) { cols[j+1] = cols[j]; j-- }
        cols[j+1] = key
      }
      out = cols[1]
      for (i = 2; i <= ncols; i++) out = out ", " cols[i]
      delete cols
      printf "%s|%s\t%s:%d:%s\n", table, out, fname, qline, qname
    }
    /^--[ \t]*name:/ {
      emit()
      qname = $3
      qline = FNR
      next
    }
    {
      line = $0
      sub(/--.*/, "", line)   # strip line comments
      buf = buf " " line
    }
    END { emit() }
  ' "$1"
}

# Collect "<fingerprint>\t<file>:<line>:<query>" lines across all source files.
# Avoids bash-4 associative arrays for portability to the system bash 3.2.
ALL=""
while IFS= read -r f; do
  ALL+="$(extract_one_file "$f")"$'\n'
done < <(find "$QUERIES_DIR" -name '*.sql' | sort)

# Fingerprints (field 1) that occur 2+ times across distinct queries.
DUP_FINGERPRINTS=$(
  printf '%s' "$ALL" \
    | grep -v '^[[:space:]]*$' \
    | cut -d$'\t' -f1 \
    | sort \
    | uniq -c \
    | awk '$1 >= 2 { $1=""; sub(/^ /, ""); print }'
)

violation_count=0
# Feed the loop via process substitution (a pipe/FD, NOT a here-document temp
# file) so the guard cannot fail open if /tmp is read-only — a here-string
# would silently skip the loop and still exit 0. The loop runs in the current
# shell (not a pipe subshell) so violation_count survives.
while IFS= read -r fp; do
  [[ -z "$fp" ]] && continue
  if is_allowlisted "$fp"; then
    continue
  fi
  table="${fp%%|*}"
  cols="${fp#*|}"
  # Gather the query locations sharing this fingerprint.
  locs=$(
    printf '%s' "$ALL" \
      | grep -F "$fp"$'\t' \
      | cut -d$'\t' -f2 \
      | paste -sd, - \
      | sed 's/,/, /g'
  )
  echo "❌ sqlc SELECT-list guard: identical explicit projection of table '$table' in 2+ queries:" >&2
  echo "      queries: $locs" >&2
  echo "      columns: $cols" >&2
  violation_count=$((violation_count + 1))
done < <(printf '%s\n' "$DUP_FINGERPRINTS")

if (( violation_count > 0 )); then
  echo >&2
  echo "Fix: use SELECT * for full-row reads (sqlc expands it; adding a column" >&2
  echo "needs no query edit), or add a justified allowlist entry for a genuine" >&2
  echo "narrow projection. See backend/tests/sqlc_select_list_static_test.go." >&2
  exit 1
fi

echo "✓ sqlc SELECT-list guard: no un-allowlisted duplicated explicit SELECT column lists"

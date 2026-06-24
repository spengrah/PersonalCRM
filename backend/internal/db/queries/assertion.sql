-- Assertion store queries (graph foundation).
--
-- The assertion has no nullable vector column, so the full-row reads use
-- SELECT * (sqlc expands it at codegen, so adding a column needs no query edit).
-- The write API (a later layer) drives these; this file is the sqlc surface it
-- calls, including the advisory-lock query (so no raw pg_advisory_* SQL ever
-- appears inline in Go).

-- name: InsertAssertion :one
-- Inserts a new assertion with the write-API-computed proposition_key and the
-- full bi-temporal envelope. knowledge_from is the learned-at clock; valid_from
-- is set only from content evidence (NULL = open-ended), never defaulted to now.
INSERT INTO assertion (
    subject_node_id, predicate_key, object_node_id,
    value_text, value_num, value_date, value_bool,
    valid_from, valid_to, knowledge_from, knowledge_to,
    confidence, salience, status, closure_reason, superseded_by, trust_tier,
    proposition_key
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18
)
RETURNING *;

-- name: GetAssertion :one
SELECT * FROM assertion WHERE id = $1;

-- name: GetAssertionForUpdate :one
-- Row-locking read for the lifecycle transitions (Accept/Reject/Retract): the
-- caller locks the row FOR UPDATE so the status precondition check + the status
-- update are atomic within the tx (a concurrent Accept/Reject on the same row
-- blocks until commit, so the from-status guard cannot be raced).
SELECT * FROM assertion WHERE id = $1 FOR UPDATE;

-- name: FindLiveProposition :one
-- The dedup lookup: the single LIVE assertion (proposed or accepted, not yet
-- knowledge-closed) for a proposition_key. Backed by idx_assertion_live_proposition.
SELECT * FROM assertion
WHERE proposition_key = $1
  AND status IN ('proposed', 'accepted')
  AND knowledge_to IS NULL;

-- name: FindAcceptedForSlot :many
-- Single-cardinality conflict check for an ASYMMETRIC predicate: the accepted,
-- knowledge-open assertion(s) for (subject, predicate) whose valid-time window
-- OVERLAPS the new one's effective window. The new-side lower bound is
-- effective_from (= COALESCE(new.valid_from, now), computed by the caller), so a
-- NULL new.valid_from probes as [now, new.valid_to) and conflicts only with the
-- currently-open accepted row. FOR UPDATE locks any found row as the second belt
-- behind the advisory lock. The ::timestamptz casts pin the probe-range param
-- types (sqlc cannot infer the type of a bare arg inside tstzrange()).
SELECT * FROM assertion
WHERE status = 'accepted'
  AND knowledge_to IS NULL
  AND subject_node_id = sqlc.arg(subject_node_id)
  AND predicate_key = sqlc.arg(predicate_key)
  AND tstzrange(valid_from, valid_to, '[)')
   && tstzrange(sqlc.arg(effective_from)::timestamptz, sqlc.arg(new_valid_to)::timestamptz, '[)')
FOR UPDATE;

-- name: FindAcceptedForSlotSymmetric :many
-- Single-cardinality conflict check for a SYMMETRIC predicate: the single-current
-- invariant is PER-PARTICIPANT, so the slot is any accepted edge where EITHER new
-- participant (participant_a / participant_b) appears in EITHER position. So
-- partner_of(B,C) finds the existing partner_of(A,B) via B. Same valid-time
-- overlap + FOR UPDATE as the asymmetric variant.
SELECT * FROM assertion
WHERE status = 'accepted'
  AND knowledge_to IS NULL
  AND predicate_key = sqlc.arg(predicate_key)
  AND (
        sqlc.arg(participant_a) IN (subject_node_id, object_node_id)
     OR sqlc.arg(participant_b) IN (subject_node_id, object_node_id)
      )
  AND tstzrange(valid_from, valid_to, '[)')
   && tstzrange(sqlc.arg(effective_from)::timestamptz, sqlc.arg(new_valid_to)::timestamptz, '[)')
FOR UPDATE;

-- name: CloseAssertion :exec
-- Terminalizes an assertion: the present-successor, closure-only, retract, and
-- rollover paths all use this (set valid_to/status/closure_reason/superseded_by/
-- knowledge_to). The caller supplies the new values; superseded_by is nullable
-- (a closure-only / retract has no successor).
UPDATE assertion
SET valid_to = $2,
    status = $3,
    closure_reason = $4,
    superseded_by = $5,
    knowledge_to = $6
WHERE id = $1;

-- name: BoundPendingSuccessor :exec
-- The future-successor branch: bound the prior's valid_to and point it at the
-- pending successor, but KEEP status='accepted' / knowledge_to=NULL (the prior
-- stays current until the future date). The rollover job terminalizes it later.
UPDATE assertion
SET valid_to = $2,
    superseded_by = $3
WHERE id = $1;

-- name: WidenAssertionValidity :exec
-- The same-value re-affirmation branch: extend valid_to / lower valid_from to
-- cover new corroborating evidence, and recompute proposition_key from the new
-- (widened) valid_from bucket so the key keeps representing the row's interval.
UPDATE assertion
SET valid_from = $2,
    valid_to = $3,
    proposition_key = $4
WHERE id = $1;

-- name: RolloverDueBoundedSuccessors :many
-- The rollover job: terminalize the bounded-with-pending-successor rows whose
-- bound has been reached. Scoped TIGHT — superseded_by IS NOT NULL excludes
-- successor-less historical accepted facts (which simply aren't current). Sets
-- status='superseded', closure_reason='superseded'. knowledge_to is
-- GREATEST(now, knowledge_from) so a row whose knowledge_from was set in the
-- future via a KnowledgeFromOverride does not violate the assertion_knowledge_range
-- CHECK and abort the whole sweep. Returns the updated rows so the caller can emit
-- one assertion.superseded event per row.
UPDATE assertion
SET status = 'superseded',
    closure_reason = 'superseded',
    knowledge_to = GREATEST(sqlc.arg(now)::timestamptz, knowledge_from)
WHERE status = 'accepted'
  AND knowledge_to IS NULL
  AND superseded_by IS NOT NULL
  AND valid_to IS NOT NULL
  AND valid_to <= sqlc.arg(now)
RETURNING *;

-- name: TransitionStatus :exec
-- The accept/reject/retract status transition: set status and (for terminal
-- transitions) knowledge_to + closure_reason. The terminal-knowledge_to CHECK
-- enforces the iff at the schema level.
UPDATE assertion
SET status = $2,
    knowledge_to = $3,
    closure_reason = $4
WHERE id = $1;

-- name: GetCurrentAccepted :one
-- The current-accepted value for a slot: accepted, knowledge-open, and its
-- valid-time window contains the now arg. For a single-cardinality slot this
-- returns the one live row; for multi it returns the first by created_at (callers
-- needing all live rows use ListLiveEdgesForNode / ListAssertionsBySubject). All
-- params are named (the now arg appears twice; mixing positional + named is
-- disallowed by sqlc, so the whole query uses sqlc.arg()).
SELECT * FROM assertion
WHERE subject_node_id = sqlc.arg(subject_node_id)
  AND predicate_key = sqlc.arg(predicate_key)
  AND status = 'accepted'
  AND knowledge_to IS NULL
  AND (valid_from IS NULL OR valid_from <= sqlc.arg(now))
  AND (valid_to IS NULL OR valid_to > sqlc.arg(now))
ORDER BY created_at
LIMIT 1;

-- name: ListAssertionsBySubject :many
-- All assertions for a subject node (any status), newest first — the review /
-- history surface.
SELECT * FROM assertion
WHERE subject_node_id = $1
ORDER BY created_at DESC;

-- name: ListLiveEdgesForNode :many
-- Live edges of a predicate touching a node in EITHER orientation (the symmetric
-- two-direction read): a node may be subject or object of a stored edge. Returns
-- proposed + accepted, knowledge-open rows.
SELECT * FROM assertion
WHERE predicate_key = $2
  AND (subject_node_id = $1 OR object_node_id = $1)
  AND status IN ('proposed', 'accepted')
  AND knowledge_to IS NULL
ORDER BY created_at;

-- name: RepointAssertionSubject :exec
-- Merge primitive (a later layer): repoint a loser assertion's subject to the
-- winner and set the recomputed proposition_key. Called only by the merge
-- procedure, never the normal write path.
UPDATE assertion
SET subject_node_id = $2,
    proposition_key = $3
WHERE id = $1;

-- name: RepointAssertionObject :exec
-- Merge primitive (a later layer): repoint a loser assertion's object to the
-- winner and set the recomputed proposition_key.
UPDATE assertion
SET object_node_id = $2,
    proposition_key = $3
WHERE id = $1;

-- name: UpdateAssertionConfidenceTrust :exec
-- Dedup re-aggregate: on a corroborating write, raise confidence (SP1 rule:
-- max(existing, incoming)) and recompute trust_tier. trust_tier is nullable.
UPDATE assertion
SET confidence = $2,
    trust_tier = $3
WHERE id = $1;

-- name: AcquirePropositionSlotLock :exec
-- The transaction-scoped advisory lock guarding the single-cardinality conflict
-- check. The caller passes the Go-computed int64 slot key (e.g.
-- hashtextextended of the slot identity); the lock auto-releases at tx end. This
-- is the sqlc surface for the lock so no raw pg_advisory_* SQL appears in Go.
SELECT pg_advisory_xact_lock($1);

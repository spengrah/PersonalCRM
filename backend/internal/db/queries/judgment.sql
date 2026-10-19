-- Judgment store queries (migrations 092-094, 099). Predicate keys are arguments, so
-- the judgment package's constants stay their one definition.

-- name: CreateJudgmentRun :one
INSERT INTO judgment_run (kind, method_key, method_version, wording_version, window_from, window_to, started_at)
VALUES (sqlc.arg('kind'), sqlc.arg('method_key'), sqlc.arg('method_version'), sqlc.arg('wording_version'),
        sqlc.narg('window_from'), sqlc.narg('window_to'), sqlc.arg('started_at'))
RETURNING *;

-- name: GetJudgmentRun :one
SELECT * FROM judgment_run WHERE id = sqlc.arg('id');

-- name: FinishJudgmentRun :execrows
UPDATE judgment_run SET finished_at = sqlc.arg('finished_at')
WHERE id = sqlc.arg('id') AND finished_at IS NULL;

-- name: ListJudgmentRunSummaries :many
-- Every run with its judged count (reused appearances included), the effects
-- its appearances changed, and the cost summed over the calls it made (its
-- fresh appearances); cost_reported is false when none of them reported one.
SELECT
    r.*,
    (SELECT count(*) FROM judgment_appearance a WHERE a.run_id = r.id)::int AS judged_count,
    (SELECT COALESCE(sum(a.obligations_opened), 0) FROM judgment_appearance a WHERE a.run_id = r.id)::int AS obligations_opened,
    (SELECT COALESCE(sum(a.flags_raised), 0) FROM judgment_appearance a WHERE a.run_id = r.id)::int AS flags_raised,
    (SELECT COALESCE(sum(a.claims_raised), 0) FROM judgment_appearance a WHERE a.run_id = r.id)::int AS claims_raised,
    (SELECT COALESCE(sum(j.cost_usd), 0) FROM judgment_appearance a JOIN judgment j ON j.id = a.judgment_id
      WHERE a.run_id = r.id AND a.fresh)::float8 AS cost_usd,
    EXISTS (SELECT 1 FROM judgment_appearance a JOIN judgment j ON j.id = a.judgment_id
      WHERE a.run_id = r.id AND a.fresh AND j.cost_usd IS NOT NULL) AS cost_reported
FROM judgment_run r
WHERE (NOT sqlc.arg('finished_only')::boolean OR r.finished_at IS NOT NULL)
ORDER BY r.started_at, r.id;

-- name: CreateJudgmentUnjudged :exec
-- Records the messages no judgment of the run answered at a cut-off, each with
-- its reason; recording a message again keeps one record with the newer reason.
INSERT INTO judgment_unjudged (run_id, thread_id, cut_off, message_key, reason)
SELECT sqlc.arg('run_id')::uuid, sqlc.arg('thread_id')::uuid, sqlc.arg('cut_off')::timestamptz,
       unnest(sqlc.arg('message_keys')::text[])::text,
       unnest(sqlc.arg('reasons')::text[])::text
ON CONFLICT (run_id, thread_id, cut_off, message_key) DO UPDATE SET reason = EXCLUDED.reason;

-- name: ListJudgmentUnjudged :many
SELECT * FROM judgment_unjudged ORDER BY run_id, cut_off, thread_id, message_key;

-- name: CreateJudgmentIncomplete :exec
INSERT INTO judgment_incomplete (run_id, thread_id, cut_off, reason)
VALUES (sqlc.arg('run_id'), sqlc.arg('thread_id'), sqlc.arg('cut_off'), sqlc.arg('reason'))
ON CONFLICT (run_id, thread_id, cut_off) DO UPDATE SET reason = EXCLUDED.reason;

-- name: ListJudgmentIncomplete :many
SELECT * FROM judgment_incomplete ORDER BY run_id, cut_off, thread_id;

-- name: EnsureJudgmentExample :one
-- The one issuer of example IDs: returns the example of (thread, input hash),
-- creating it with a random ID the first time.
INSERT INTO judgment_example (thread_id, input_hash)
VALUES (sqlc.arg('thread_id'), sqlc.arg('input_hash'))
ON CONFLICT (thread_id, input_hash) DO UPDATE SET input_hash = EXCLUDED.input_hash
RETURNING *;

-- name: FindJudgment :one
SELECT j.id
FROM judgment j
JOIN judgment_example e ON e.id = j.example_id
WHERE j.method_key = sqlc.arg('method_key')
  AND j.method_version = sqlc.arg('method_version')
  AND j.wording_version = sqlc.arg('wording_version')
  AND e.thread_id = sqlc.arg('thread_id')
  AND e.input_hash = sqlc.arg('input_hash')
  AND j.read_digest = sqlc.arg('read_digest');

-- name: CreateJudgment :one
INSERT INTO judgment (
    method_key, method_version, wording_version, example_id, context_kind,
    cut_off, judged_at, context_tokens, question_count, billed_tokens,
    cost_usd, latency_ns, questions, window_start, new_messages, read_digest
) VALUES (
    sqlc.arg('method_key'), sqlc.arg('method_version'), sqlc.arg('wording_version'),
    sqlc.arg('example_id'), sqlc.arg('context_kind'), sqlc.arg('cut_off'),
    sqlc.arg('judged_at'), sqlc.arg('context_tokens'), sqlc.arg('question_count'),
    sqlc.narg('billed_tokens'), sqlc.narg('cost_usd'), sqlc.arg('latency_ns'),
    sqlc.arg('questions'), sqlc.narg('window_start'), sqlc.arg('new_messages')::text[],
    sqlc.arg('read_digest')
)
RETURNING id;

-- name: CreateJudgmentFlags :exec
-- Records a judgment's flag decisions: one message key, kind and probability
-- per position of the three arrays.
INSERT INTO judgment_flag (judgment_id, message_key, kind, probability)
SELECT sqlc.arg('judgment_id')::uuid,
       unnest(sqlc.arg('message_keys')::text[])::text,
       unnest(sqlc.arg('kinds')::text[])::text,
       unnest(sqlc.arg('probabilities')::float8[])::float8;

-- name: CreateJudgmentClearings :exec
-- Records a judgment's clearing decisions: one commitment's message key and
-- earlier judgment, the answer and its probability of done per position of the
-- four arrays.
INSERT INTO judgment_clearing (judgment_id, commitment_message_key, commitment_judgment_id, done, probability)
SELECT sqlc.arg('judgment_id')::uuid,
       unnest(sqlc.arg('message_keys')::text[])::text,
       unnest(sqlc.arg('commitment_judgment_ids')::uuid[])::uuid,
       unnest(sqlc.arg('dones')::boolean[])::boolean,
       unnest(sqlc.arg('probabilities')::float8[])::float8;

-- name: IsEarlierCommitment :one
-- Whether the judgment holds a commitment flag decision for the message and
-- belongs to the thread with a cut-off before the given one.
SELECT EXISTS (
    SELECT 1
    FROM judgment_flag f
    JOIN judgment j ON j.id = f.judgment_id
    JOIN judgment_example e ON e.id = j.example_id
    WHERE f.judgment_id = sqlc.arg('judgment_id')::uuid
      AND f.message_key = sqlc.arg('message_key')::text
      AND f.kind = sqlc.arg('commitment_kind')::text
      AND e.thread_id = sqlc.arg('thread_id')::uuid
      AND j.cut_off < sqlc.arg('cut_off')::timestamptz);

-- name: ListJudgmentFlags :many
SELECT * FROM judgment_flag
WHERE judgment_id = ANY(sqlc.arg('judgment_ids')::uuid[])
ORDER BY judgment_id, message_key, kind;

-- name: ListJudgmentClearings :many
SELECT * FROM judgment_clearing
WHERE judgment_id = ANY(sqlc.arg('judgment_ids')::uuid[])
ORDER BY judgment_id, commitment_message_key;

-- name: ListJudgmentFirstMessages :many
-- The key of the first message of the thread context each given judgment read
-- when it read the whole context (a judgment before migration 099): the
-- thread's first message at the judgment's cut-off. A judgment whose thread
-- held no content then has no row.
SELECT j.id AS judgment_id, first.message_key
FROM judgment j
JOIN judgment_example e ON e.id = j.example_id
CROSS JOIN LATERAL (
    SELECT tc.message_key
    FROM thread_content tc
    WHERE tc.venue_id = e.thread_id AND tc.sent_at <= j.cut_off
    ORDER BY tc.sent_at, tc.message_key
    LIMIT 1
) first
WHERE j.id = ANY(sqlc.arg('judgment_ids')::uuid[]);

-- name: GetFlagDisplayThreshold :one
SELECT display_threshold FROM judgment_flag_threshold
WHERE method_key = sqlc.arg('method_key') AND kind = sqlc.arg('kind');

-- name: GetFlagClearingFloor :one
SELECT clearing_floor FROM judgment_flag_method
WHERE method_key = sqlc.arg('method_key');

-- name: ListJudgmentRows :many
-- Judgments with their example, by ID, or for a thread, a run or both (a run
-- matches every judgment appearing in it).
SELECT j.*, e.thread_id, e.input_hash
FROM judgment j
JOIN judgment_example e ON e.id = j.example_id
WHERE (cardinality(sqlc.arg('ids')::uuid[]) = 0 OR j.id = ANY(sqlc.arg('ids')::uuid[]))
  AND (sqlc.narg('thread_id')::uuid IS NULL OR e.thread_id = sqlc.narg('thread_id')::uuid)
  AND (sqlc.narg('run_id')::uuid IS NULL OR EXISTS (
        SELECT 1 FROM judgment_appearance a
        WHERE a.judgment_id = j.id AND a.run_id = sqlc.narg('run_id')::uuid))
  AND (cardinality(sqlc.arg('example_ids')::text[]) = 0 OR j.example_id = ANY(sqlc.arg('example_ids')::text[]))
  AND (sqlc.narg('method_key')::text IS NULL OR (
        j.method_key = sqlc.narg('method_key')::text
        AND j.method_version = sqlc.narg('method_version')::text
        AND j.wording_version = sqlc.narg('wording_version')::text))
ORDER BY j.cut_off, j.created_at, j.id;

-- name: CreateJudgmentAppearance :execrows
INSERT INTO judgment_appearance (run_id, judgment_id, cut_off, fresh)
VALUES (sqlc.arg('run_id'), sqlc.arg('judgment_id'), sqlc.arg('cut_off'), sqlc.arg('fresh'))
ON CONFLICT (run_id, judgment_id, cut_off) DO NOTHING;

-- name: SetJudgmentAppearanceEffects :exec
UPDATE judgment_appearance
SET obligations_opened = sqlc.arg('obligations_opened'),
    flags_raised = sqlc.arg('flags_raised'),
    claims_raised = sqlc.arg('claims_raised')
WHERE run_id = sqlc.arg('run_id') AND judgment_id = sqlc.arg('judgment_id') AND cut_off = sqlc.arg('cut_off');

-- name: ListJudgmentAppearances :many
-- The appearances of the given judgments, or of every judgment in a run, or
-- of every judgment; fresh_only keeps only the appearance whose call produced
-- each judgment. Oldest recorded first.
SELECT a.run_id, a.judgment_id, a.cut_off, e.thread_id, e.id AS example_id
FROM judgment_appearance a
JOIN judgment j ON j.id = a.judgment_id
JOIN judgment_example e ON e.id = j.example_id
WHERE (cardinality(sqlc.arg('judgment_ids')::uuid[]) = 0 OR a.judgment_id = ANY(sqlc.arg('judgment_ids')::uuid[]))
  AND (sqlc.narg('run_id')::uuid IS NULL OR a.run_id = sqlc.narg('run_id')::uuid)
  AND (NOT sqlc.arg('fresh_only')::boolean OR a.fresh)
ORDER BY a.created_at, a.cut_off, a.run_id;

-- name: CreateJudgmentOutcome :one
-- Creates an outcome's node and its judgment_outcome row together.
WITH outcome_node AS (
    INSERT INTO node (id, type, canonical_label)
    VALUES (uuid_generate_v4(), 'judgment_outcome', '')
    RETURNING id
)
INSERT INTO judgment_outcome (node_id, judgment_id, question, question_id, contact_id, due_date, confidence)
SELECT outcome_node.id, sqlc.arg('judgment_id'), sqlc.arg('question'), sqlc.narg('question_id'),
       sqlc.narg('contact_id'), sqlc.narg('due_date'), sqlc.narg('confidence')
FROM outcome_node
RETURNING *;

-- name: ListJudgmentOutcomes :many
-- The outcomes of the given judgments, or the given outcomes, each with the
-- example its judgment judged.
SELECT o.*, j.example_id
FROM judgment_outcome o
JOIN judgment j ON j.id = o.judgment_id
WHERE o.judgment_id = ANY(sqlc.arg('judgment_ids')::uuid[])
   OR o.node_id = ANY(sqlc.arg('outcome_ids')::uuid[])
ORDER BY o.judgment_id, o.question, o.question_id NULLS LAST, o.contact_id, o.node_id;

-- name: ResolveContactMerges :many
-- Each given contact with the contact it now is: the end of its chain of
-- contact merges (node.merged_into), the contact itself when never merged. A
-- contact without a node has no row.
WITH RECURSIVE chain(contact_id, node_id, merged_into) AS (
    SELECT n.id, n.id, n.merged_into
    FROM node n
    WHERE n.id = ANY(sqlc.arg('contact_ids')::uuid[])
  UNION
    SELECT c.contact_id, n.id, n.merged_into
    FROM chain c
    JOIN node n ON n.id = c.merged_into
)
SELECT chain.contact_id, chain.node_id AS live_contact_id
FROM chain
WHERE chain.merged_into IS NULL
ORDER BY chain.contact_id;

-- name: SetJudgmentOutcomeKeptDueDate :exec
-- Records the date an outcome's obligation held when the user first reviewed
-- the later date the outcome suggests.
UPDATE judgment_outcome
SET kept_due_date = sqlc.narg('kept_due_date')
WHERE node_id = sqlc.arg('node_id');

-- name: SetJudgmentOutcomeObligation :exec
-- Records the obligation task an outcome's live effect is; NULL when its
-- effect is no obligation.
UPDATE judgment_outcome
SET obligation_task_id = sqlc.narg('obligation_task_id')
WHERE node_id = sqlc.arg('node_id');

-- name: ListOutcomeAssertions :many
-- The assertions about the given outcome nodes under the given predicates,
-- oldest first.
SELECT * FROM assertion
WHERE subject_node_id = ANY(sqlc.arg('outcome_ids')::uuid[])
  AND predicate_key = ANY(sqlc.arg('predicates')::text[])
ORDER BY created_at, id;

-- name: ListLatestExampleReviews :many
-- The user's latest review of each question and contact on each example,
-- whichever method's outcome it reviewed, ordered by example, question and
-- contact: the one definition of a decision and its label. keeps_date is
-- whether that review kept an open commitment's current date. cut_off is the
-- example's earliest judgment cut-off. Restricted to the given examples when
-- any are named.
SELECT DISTINCT ON (e.id, o.question, o.contact_id)
    e.id AS example_id, e.thread_id, e.input_hash,
    (SELECT min(j2.cut_off) FROM judgment j2 WHERE j2.example_id = e.id)::timestamptz AS cut_off,
    o.question, o.contact_id,
    a.value_text AS right_outcome,
    COALESCE(kd.value_bool, FALSE)::boolean AS keeps_date
FROM assertion a
JOIN judgment_outcome o ON o.node_id = a.subject_node_id
JOIN judgment j ON j.id = o.judgment_id
JOIN judgment_example e ON e.id = j.example_id
LEFT JOIN LATERAL (
    SELECT x.value_bool FROM assertion x
    WHERE x.subject_node_id = o.node_id AND x.predicate_key = sqlc.arg('keeps_date_predicate')::text
      AND x.status = 'accepted'
    ORDER BY x.created_at DESC LIMIT 1
) kd ON TRUE
WHERE a.predicate_key = sqlc.arg('review_predicate')::text
  AND a.status = 'accepted'
  AND (cardinality(sqlc.arg('example_ids')::text[]) = 0 OR e.id = ANY(sqlc.arg('example_ids')::text[]))
ORDER BY e.id, o.question, o.contact_id, a.created_at DESC, a.id DESC;

-- name: ListReviewRecords :many
-- Every live review with the reviewed outcome's judgment, for the export.
SELECT
    e.id AS example_id,
    j.method_key, j.method_version, j.wording_version,
    o.question, o.question_id,
    COALESCE(ans.value_text, cls.value_text, '')::text AS judged_value,
    a.value_text AS right_outcome,
    COALESCE(kd.value_bool, FALSE)::boolean AS keeps_date,
    a.knowledge_from AS reviewed_at
FROM assertion a
JOIN judgment_outcome o ON o.node_id = a.subject_node_id
JOIN judgment j ON j.id = o.judgment_id
JOIN judgment_example e ON e.id = j.example_id
LEFT JOIN LATERAL (
    SELECT x.value_text FROM assertion x
    WHERE x.subject_node_id = o.node_id AND x.predicate_key = sqlc.arg('answer_predicate')::text
      AND x.status = 'accepted'
    ORDER BY x.created_at DESC LIMIT 1
) ans ON TRUE
LEFT JOIN LATERAL (
    SELECT x.value_text FROM assertion x
    WHERE x.subject_node_id = o.node_id AND x.predicate_key = ANY(sqlc.arg('class_predicates')::text[])
    ORDER BY x.created_at DESC LIMIT 1
) cls ON TRUE
LEFT JOIN LATERAL (
    SELECT x.value_bool FROM assertion x
    WHERE x.subject_node_id = o.node_id AND x.predicate_key = sqlc.arg('keeps_date_predicate')::text
      AND x.status = 'accepted'
    ORDER BY x.created_at DESC LIMIT 1
) kd ON TRUE
WHERE a.predicate_key = sqlc.arg('review_predicate')::text
  AND a.status = 'accepted'
ORDER BY a.knowledge_from, a.id;

-- name: FindLiveVenueFlag :one
SELECT id FROM assertion
WHERE subject_node_id = sqlc.arg('venue_id')
  AND predicate_key = sqlc.arg('predicate_key')::text
  AND value_text = sqlc.arg('value')::text
  AND status IN ('proposed', 'accepted')
ORDER BY created_at DESC
LIMIT 1;

-- name: InteractionStartedAwaitingWait :one
-- Whether the contact's outreach that started its awaiting-reply wait (the
-- outbound at last_outreach_at) happened on the venue.
SELECT EXISTS (
    SELECT 1
    FROM interaction i
    JOIN contact c ON c.id = i.contact_id
    WHERE i.contact_id = sqlc.arg('contact_id')
      AND i.venue_id = sqlc.arg('venue_id')
      AND i.deleted_at IS NULL
      AND i.direction <> 'inbound'
      AND i.occurred_at = c.last_outreach_at
)::boolean AS started;

-- name: GetTodoistTaskSettings :one
-- The Todoist settings a new task's metadata snapshots, from the most recently
-- updated Todoist sync state.
SELECT metadata FROM external_sync_state
WHERE source = 'todoist'
ORDER BY updated_at DESC
LIMIT 1;

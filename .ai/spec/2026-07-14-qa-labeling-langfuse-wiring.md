# QA labeling & Langfuse wiring — design sketch

Status: DRAFT (high-level; capture-so-we-don't-forget). Owner: maintainer. Date: 2026-07-14.

## Decision context

The seed-fidelity arc is complete: staging (and the corpus world) are causally seeded, the judge's prod-impossible false positives are gone, and the first post-arc label session is underway. The maintainer's direction (2026-07-14): **Langfuse is the source of truth for judge-related items** — draft labels are authored into it, human review happens in its annotation UI, and the git label files become synced snapshots rather than the primary artifact. This effectively settles Langfuse-vs-Phoenix for the QA lane (extraction/#379 remains a separate decision). Verifier-lane items never go to Langfuse — they are deterministic and migrate to E2E per the standing direction.

## What exists today (post this session)

- **Live judge loop (staging):** `make tours` (reseed + 4 tours) → `make qa-report JUDGE=1` with `QA_JUDGE_TRACE=<file>` → `make qa-export TRACE=<file>` ships one GenAI trace per LLM judge call (intents + judge-residue behavior items) with screenshots to Langfuse. Proven end-to-end 2026-07-14: 12 traces, 66 screenshots.
- **Corpus factory (local only):** committed corpus fixtures are cut from a LOCAL `prod-shaped` sweep — `CRM_ENV=testing`, `TIME_ACCELERATION=60` **and** `TIME_BASE=$(unix seconds)` (both required; missing TIME_BASE silently disables acceleration server-side and renders "Invalid Date" in the UI), seed run with the backend STOPPED (a live backend's River worker races the wipe and strands assertion-derived caches, e.g. birthdays). Regenerated this session from run `local-20260714T151954Z`; PII audit + `qa-eval` green. Curation was a one-off script (scratchpad) — see follow-ups.
- **Draft labels:** `label.ts` (drafter `codex-exec:gpt-5.5`) produced 7 drafts over the new corpus (5 judge-residue behavior items + 2 intent cases).
- **Langfuse label session (manual push, one-off script):** annotation queue `qa-label-session-20260714` on the obs-tenant Langfuse — one trace + queue item per drafted item; trace input = then-clause/intent goal + capture summaries + screenshots (from the sweep run dir); trace output = draft verdict/critique; the human-set categorical `verdict` score (+ comment) is what "human-confirmed" means. Draft verdicts are deliberately NOT pre-filled as scores.
- **Reach:** Langfuse is loopback-only on stovepipes; from the Mac `ssh -N -L 3000:localhost:3000 -L 9090:localhost:9090 stovepipes` (the :9090 MinIO forward is REQUIRED — media presigned URLs point at localhost:9090 and its absence fails every span/screenshot with a generic "Unable to connect").

## Target wiring (the loop we want)

```
                     (per run, exists)
staging tours ──► judge ──► qa-export ──────────────► Langfuse traces (judge calls, advisory)

                     (per corpus regen + label session)
local sweep ──► curate ──► corpus fixtures ──► label.ts drafts
                                                  │  --langfuse sink (NEW: replaces the one-off push script)
                                                  ▼
                                     Langfuse annotation queue  ◄── maintainer reviews here (SoT)
                                                  │
                                       label-sync (NEW: pull human-confirmed
                                       verdict scores + comments)
                                                  ▼
                                     corpus/labels/*.labeled.json (synced snapshot, committed)
                                                  │
                                                  ▼
                              make qa-eval (stays OFFLINE + deterministic; merge gate unchanged)
                              → unlocks: held-out fail-precision bar, luna experiment, PR4 issue-mode
```

Hybrid ground-truth model (decided): Langfuse is where labels are authored and live; git carries pinned snapshots so the eval never takes a network dependency and results stay reproducible per commit.

## Changes to make (high level)

1. **`label.ts --langfuse` sink** — productionize the one-off push: after writing `*.draft.json`, create/reuse the `verdict` score config + a per-session annotation queue, one trace + queue item per drafted item (input: clause/goal + capture summaries + screenshots; output: draft verdict/critique; metadata: sourceRun, corpus file). Env-gated (`LANGFUSE_*`), no-op without it — same opt-in-by-construction pattern as qa-export.
2. **`label-sync` (new small script)** — pull completed queue items' `verdict` scores + comments, write/refresh `*.labeled.json` (`status: human-confirmed`, verdict from score, critique from comment; drop stale labeled files whose case no longer exists). Idempotent; run after a session, commit the diff.
3. **Corpus regen script** — encode this session's hand-run flow as one command (`make qa-corpus-regen`?): checks ports, seeds with backend stopped, starts backend with the full accelerated env contract, sweeps, curates (the scratchpad curate.ts logic: filename-preserving copy, screenshot strip, the two documented body drops, scrub, stale-file removal), refreshes PROVENANCE sourceRun, runs pii-audit + qa-eval. Kills the "re-learn the env contract by failing twice" tax.
4. **Obs-tenant hardening (prerequisite for SoT):** backup.sh on a systemd timer (labels become primary data the moment a session completes in Langfuse); graduate the obs tenant into personal-ops `setup-vps.sh` TENANTS; durable reach (Tailscale Serve or Caddy loopback) to retire the SSH forwards, including a same-origin answer for the MinIO media URLs (the :9090 coupling breaks any reach story that only fronts :3000).
5. **Doctored-mutation liveness guard** — regen can silently invalidate a doctored case (found live 2026-07-14: the corpus regen shrank a capture's response list and `DSH-004-doctored-stale-reason`'s `itemIndex: 4` went out of range; `set_json_field` no-ops silently and the verifier-only gate cannot catch a judge-layer no-op, so the drafter graded clean evidence). Add a mechanical check to `qa-eval`: applying a doctored case's mutation MUST change the evidence (deep-inequality vs the base fixtures), else fail loudly. Related labeling-UI caveat now stamped on doctored traces: screenshots always show the undoctored world (mutations touch only aria/API JSON, which is all the drafter grades).
6. **Luna experiment (unblocked by the label session):** per `judge/DEFERRED.md` — pin/upgrade codex CLI, run the eval with `QA_INTENT_MODEL=gpt-5.6-luna` vs the `gpt-5.5` baseline over the labeled held-out set, compare intent fail-precision + verdict agreement + `--repeat` self-consistency; flip `DEFAULT_INTENT_MODEL` only if non-inferior on fail-precision. Also a deliberate stress-test of Langfuse as the labeling/comparison surface.

## Judge-as-tenant on stovepipes (maintainer's question — sketch only)

What it might look like: a fourth tenant (`qa`, own uid/slice like staging/sandbox/obs) owning the scheduled loop `reseed staging → tours → judge → qa-export`, so runs stop depending on the Mac. The plumbing mostly exists: the sandbox observe path (Caddy :80 via 10.100.0.1, `qa-staging` forced-command reseed) is the network model; Langfuse is host-local. Open problems, in rough order of hardness:

- **Codex auth on a headless VPS:** the judge + drafter ride the ChatGPT-subscription quota with interactive login + self-rotating refresh tokens. The transport (CLI vs the deferred `codex-sdk`) is orthogonal — the SDK uses the same auth store, so subscription auth works through either; the blocker is purely the auth lifecycle. Syncing `auth.json` from the Mac is a trap: two consumers of one rotating refresh token produce the known "refresh token already used" burn. The clean shape is a **dedicated account/auth slot for the tenant** — one interactive `codex login` at provision time (device-code or forwarded browser), after which rotation continues tenant-side and stays valid as long as only the tenant uses it. Fallbacks: API-key auth (metered), or Mac-only model stages with tours+export on the tenant.
- **Browser runtime:** Playwright + chromium on ARM Debian for the tours — sized/cached per tenant; straightforward but not free (the vps-dev-sandbox contents work overlaps here).
- **Secrets:** TOURS_API_KEY + LANGFUSE keys need tenant-local provisioning (the /var/lib/sandbox/secrets/crm pattern already exists for the observe path).
- **Trigger:** systemd timer for a nightly advisory run + a manual trigger for post-deploy runs; report lands as an artifact (and later, PR4 issue-mode).

Recommendation: don't block the labeling loop on this; sequence it after items 1–4, and only if the codex-auth question has an acceptable answer. Revisit alongside the vps-dev-sandbox contents work (shared container/tooling patterns).

## Sequencing

1. (now) Maintainer completes `qa-label-session-20260714` in Langfuse.
2. `label-sync` (hand-run acceptable for the first pass) → commit corpus regen + drafts + labeled files as one PR.
3. Luna experiment over the fresh ground truth.
4. `label.ts --langfuse` sink + corpus regen script (turn this session's one-offs into repo tooling).
5. Obs-tenant hardening (backup timer first — cheap and now-relevant).
6. Judge-as-tenant design decision (separate doc if pursued).

## Session learnings worth keeping (operational)

- Seed with the backend STOPPED — assertion-driven caches (birthday etc.) silently vanish under the River race, and the seed still exits 0.
- `TIME_ACCELERATION` without `TIME_BASE` is a silent no-op server-side but reports `is_accelerated: true` with empty `base_time`, which the frontend renders as "Invalid Date" — set both, base as Unix seconds.
- The corpus is local-only by design: accelerated-frame clauses (e.g. CON-045[4]) are only provable in the testing frame, and the PII guarantee is structural in a `--reset-and-seed` world; staging serves the live judge, not the corpus.
- Langfuse media = MinIO presigned URLs on :9090; any reach/tunnel story must cover it or every media op fails with an unhelpful connection error.
- The local dev port 3000 collides with the habitual Langfuse forward — the regen script should use a non-3000 frontend port.
- Langfuse ingestion dedups by EVENT id (not trace id): re-submitting a trace-create with a previously-used event id is silently dropped — updates appear to succeed but change nothing. Any writer that upserts traces (the `label.ts --langfuse` sink) must generate unique event ids per submission.
- Label-trace contract (learned from the first review session): the trace input must carry the full scenario (`behavior_title`/`given`/`when`/`then_text`/`all_then` or the intent goal), the `doctored` mutation, a `screenshot_caveat` on doctored cases (screenshots always show the undoctored world), and `graded_evidence` — the mutation-applied captures in the prompt's own CAPTURE[n] structure, each entry carrying its `capture_file` name and its own `screenshot` media token (the media gallery does NOT preserve order, so a flat screenshots array is unattributable) — so critiques are verifiable without leaving Langfuse. Ingestion is processed async by a worker: verification reads need a settle delay.

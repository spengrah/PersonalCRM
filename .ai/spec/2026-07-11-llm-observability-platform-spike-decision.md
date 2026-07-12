# Self-hosted LLM observability spike — decision (Langfuse vs Phoenix vs git-committed media)

**Issue:** #635 (shared by #380/#606 QA harness + #379 extraction program) · **Spike:** 2026-07-11/12 · **Tree:** `495416a8` · **Status:** decided

**Companions:** [field catalog](./2026-07-12-observability-platform-catalog.md) (is there anything better out there? — no) · [Phoenix revival runbook](./2026-07-12-phoenix-revival-runbook.md) (how to bring the fallback back) · [QA architecture restructuring](./2026-07-12-qa-architecture-restructuring.md) (what the harness should actually be grading) · [Langfuse as QA SSOT](./2026-07-12-langfuse-as-qa-ssot-plan.md) (the corpus/label/prompt migration)

## Decision

**Langfuse, self-hosted on the VPS.** Both candidates were stood up side by side on the VPS and seeded with the *same* real data (both judge surfaces, the human-confirmed labels, real screenshots from a live tour run). The maintainer then ran an actual label pass in **both** UIs and preferred Langfuse's. That preference is the decisive input, and it is reinforced by one hard capability gap: **Phoenix has no object store, so it structurally cannot hold the screenshots the intent judge actually sees.**

Phoenix remains a credible fallback on weight alone (515 MB, one container, all-SQL — 6× lighter), and the OTel seam means switching costs little. But it cannot serve the QA harness's media requirement, and the intent pass — the screenshot-consuming surface — is also the expensive one.

**Host: the VPS, not the Pi.** Langfuse idles at ~3 GB across 6 containers (ClickHouse + MinIO + Redis + Postgres + web + worker). The Pi has **~4.2 GB available** next to *prod* (whose whole CRM stack is a featherweight ~350 MB) on 4 shared cores. Putting a non-critical observability stack there would let it threaten the one thing that must stay up. Phoenix *would* have fit the Pi comfortably — that was the real tension, and it is resolved below rather than by moving Langfuse.

## How the PII constraint is satisfied without the Pi

The maintainer's condition: real content on the rented VPS is acceptable **only if what lands there is a small, de-identified slice** — not a de-facto duplicate of prod message content. That is a *logging choice*, and the design makes it one:

- **Do not log raw inputs for #379.** Store the extraction **output candidates + model/tokens/cost + a `contact_id`/`message_id` reference** (UUIDs, never names). The trace becomes a thin index *over* prod, not a copy *of* it.
- **Correction is CRM-native.** The per-contact "view extracted features → confirm/correct" surface is the #379 trust/review pillar and belongs in the CRM, which already holds the content. Because the human reviews content *there*, the platform never needs raw content to support labeling — it only needs the **outcome keyed by `trace_id`**.
- **These reinforce each other**: CRM-side review is exactly what *permits* the platform to stay content-light. The QA half is unaffected — its corpus is synthetic, so log it in full.
- **Residual**: offline experiment reruns (e.g. the luna comparison) do want inputs. Handle by re-feeding content from the CRM at experiment time, or by keeping a small, deliberately curated held-out set — a bounded slice, consciously accepted, not "everything."

Net: the Pi stays the sole home of durable real content; the VPS holds outputs + references. The Pi option is not foreclosed — if Langfuse's ops burden disappoints, Phoenix-on-Pi is the pre-analyzed fallback.

**Empirical support (found the hard way):** an early seeding pass sent the judge's `QA_JUDGE_TRACE` spans as-is — which are *metrics-only* (model/tokens, no prompt). The annotation queue was then **impossible to use**: there was nothing to read to make the call. That is the exact failure mode of over-minimizing, and it is why extraction labeling *must* be CRM-side if extraction traces are content-light. You cannot label what you didn't log.

## What was built (live)

- **Tenant:** isolated `obs` tenant on the VPS (`<vps-host>`) — uid 1998, `/var/lib/obs`, linger, subuid `500000:65536`, `user-1998.slice` MemoryMax=6G/CPUQuota=300%. Mirrors `setup-vps.sh`'s tenant pattern; staging/sandbox/nftables untouched. **Not yet in `personal-ops` IaC.**
- **Langfuse v3** (v3.212.0): upstream compose translated to sequenced rootless `podman run` on a user network (`up.sh`). Loopback-only; reached over an SSH forward. Headless `LANGFUSE_INIT_*` bootstrap (org/project/user/keys). `TELEMETRY_ENABLED=false`.
- **Phoenix** (`arizephoenix/phoenix`, 515 MB) alongside, for the comparison. **Auth-off by default** — would need Tailscale Serve/Caddy or `PHOENIX_ENABLE_AUTH`.
- **Backups tested** (`backup.sh`): postgres `pg_dump` (276 K) + minio volume export + clickhouse volume export (12 M). ClickHouse *native* `BACKUP` needs a `backups.allowed_path` config — volume export used instead.
- Artifacts: `/var/lib/obs/langfuse/{.env,up.sh,backup.sh}`; clients in `.ai/log/plan/llm-observability-spike/` (gitignored).

## Proofs (deliverable 2) — all against real data

Seeded from the **7 label files** (6 human-confirmed from the 2026-07-10 session + 1 draft), across **both judge surfaces** — behavior residue (`classification.ts`) *and* the intent layer (#622/#623) — with **50 real screenshots** from a live `make tours` run (`20260712T201849Z`).

**(a) Judge spans → Langfuse OTLP.** A dependency-free exporter reads the `QA_JUDGE_TRACE` GenAI JSONL and POSTs OTLP/JSON to `/api/public/otel/v1/traces`. Model recognized, tokens captured, **cost computed automatically** — the per-run cost #635 mined by hand from codex logs becomes a dashboard number. The same seam is #379's Venice exporter unchanged. (Phoenix needs OTLP **protobuf** + OpenInference attributes — JSON returns 415.)

**(b) Corpus as a dataset with real media.** Dataset `qa-corpus-495416a8`: **39 items** (37 behavior + 2 intent cases), **every one carrying real screenshots** (4.2 MB in the MinIO object store). Media flow is register → presigned PUT → **PATCH finalize** (skipping the PATCH leaves media 404 despite bytes being stored). Note: Langfuse reserves dataset-item IDs **project-wide even after deletion**, so a clean reseed needs fresh IDs.

**(c) Annotation queue → git round-trip.** Score config + queue over the labeled set; the 6 human-confirmed labels loaded with their real maintainer critiques (incl. the DSH-010 override: *"PASS (maintainer override of the codex draft's fail)… part of the draft's grounding was an aria-cap truncation artifact, not UI"*). The maintainer then **labeled the remaining draft (DSH-004-doctored-stale-reason → `fail`) in the Langfuse UI**, and it was **exported back to git** as `DSH-004-doctored-stale-reason.labeled.json` (draft removed, per the correct-in-place convention). The corpus is now **7 labeled / 0 draft**. That is the full loop — and git remains the SSOT, as #635 requires.

## Ops burden, as lived

1. **No compose on podman-static** — compose translated to hand-maintained `podman run`.
2. **podman-static healthchecks are inert** — containers sit `(starting)` forever; `up.sh` uses active `exec` probes.
3. **Port collisions** with staging's `:5432` — internal stores kept off the host.
4. **Media needs the PATCH finalize** (silent 404 otherwise).
5. **ClickHouse native BACKUP needs config**; volume export used.
6. **~3 GB idle / 6 containers** to observe a trickle of traces. The standing argument for Phoenix, and the reason this lives on the VPS not the Pi.
7. **Upgrades untested** — `:3` pinned; ClickHouse migrations run on web boot.

## Candidates

- **Langfuse (chosen).** Preferred labeling UX (maintainer-verified in a real head-to-head), annotation queues, score configs, **object-store media**, native cost, mature feedback-by-`trace_id` API (the #379 CRM-correction loop). Cost: the 6-service stack.
- **Phoenix (fallback).** 515 MB, one container, OTEL-native, datasets/annotations. **No object store → cannot hold the screenshots.** Thinner annotation setup (config must be created *and* `PUT`-attached to the project). Auth-off by default. Keep as the escape hatch, especially if the Pi ever becomes the required host. **Torn down 2026-07-12** — container, volume, and image all removed; it left nothing on the host, so the [revival runbook](./2026-07-12-phoenix-revival-runbook.md) is the sole record of how it was wired.
- **Git-committed screenshots — rejected as the shared answer.** Serves neither #379 tracing nor cost/labeling. Survives only as a QA-media-only stopgap *if* Phoenix were chosen.

## Action items

1. **Graduate the `obs` tenant into `personal-ops` IaC** (`setup-vps.sh` TENANTS: uid 1998, 6 G ceiling); decide Quadlets vs the committed `up.sh`.
2. **Durable reach** — replace the SSH forward with Tailscale Serve / Caddy loopback.
3. **Second project `extraction`** (#379) with its own key + RBAC **before any real-content trace is sent**, and with input-minimization on from day one.
4. **`trace_id` on assertion provenance** + a **platform-agnostic feedback adapter** (`recordVerdict(traceId, verdict, correctedValue)`) — the two prerequisites that keep Langfuse/Phoenix and VPS/Pi swappable.
5. **Backups on a timer** + offsite copy.
6. **Promote the OTLP exporter** into the harness behind an env flag.
7. **Upgrade runbook** before the first version bump.

## What the spike surfaced beyond the decision

Seeding a real observability platform with the harness's real judge calls forced a hard look at *what is actually being judged* — and the answer was uncomfortable. Of the ~60 spec then-items the tours grader owns, ~58 are graded by a deterministic **verifier**: "the Previous button is disabled at the first position", "the URL gains `?sort=name`", "the row count drops by one". Those are assertions about **deterministic CRM application behavior**, which is what a Playwright E2E test is for — and several are *already asserted*, verbatim, in `frontend/tests/e2e/`. The tours harness was re-deriving them from captured aria snapshots.

That is a category error, and it has a cost: those items dominate the corpus that must be curated, PII-audited, and committed, while contributing nothing an E2E test does not already contribute more cheaply and more precisely. Meanwhile the part of the harness that *only* an LLM can do — the **intent** layer, judged UX goals over screenshots — covers 3 of 12 domains and 41% of `ux` behaviors, and the largest UX surface in the app (`settings`, 10 `ux` behaviors) has no tour at all.

The corrective is in [`2026-07-12-qa-architecture-restructuring.md`](./2026-07-12-qa-architecture-restructuring.md): deterministic assertions migrate to cited E2E specs, and the agentic layer **grows** to fill the surface it was always meant to cover. That restructuring is also what unblocks [`2026-07-12-langfuse-as-qa-ssot-plan.md`](./2026-07-12-langfuse-as-qa-ssot-plan.md) — the only reason the corpus had to stay in git was that `make qa-eval` is a merge gate, and that gate exists to protect the verifier lane that is leaving.

## Postscript — a process failure worth recording

The first pass of this spike was conducted against a **14-commit-stale checkout**, and produced confidently wrong claims: that the corpus had no labels, and that the judge surface was 3 calls. In truth #634 had landed the label-session batch, #627 had re-audited the judge's ownership, #629 had regenerated the corpus, and #624 had added the intent pass's screenshots. The maintainer caught it. **Check sync against origin before deriving facts from the tree** — and prefer the repo's own canonical code paths (`resolveCaseCaptures`, `buildJudgeInput`, `judgeItemsFor`) over re-deriving semantics by hand.

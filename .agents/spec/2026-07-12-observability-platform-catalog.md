# Self-hostable LLM observability / eval / prompt-management — field catalog

**Issue:** #635 (shared by #380/#606 QA harness + #379 extraction program) · **Date:** 2026-07-12 · **Status:** research complete · **Companion to:** [`2026-07-11-llm-observability-platform-spike-decision.md`](./2026-07-11-llm-observability-platform-spike-decision.md)

## Purpose

The spike decision doc evaluated Langfuse (stood up, chosen) against Phoenix (fallback). This doc answers the separate question it did not: **is there anything else out there we missed?** It is a discovery catalog of the wider field, not a re-litigation of the decision. Conclusion up front: **no. Nothing in the field beats Langfuse on the combination both programs need, and the search materially sharpens the Langfuse-vs-Phoenix trade-off.**

## TL;DR — three findings that change the framing

1. **The "lighter alternative" hope is a dead end.** Every candidate carrying the full feature set (media + annotation queues + datasets + prompt versioning + cost) depends on **ClickHouse**. The only genuinely light platform found (OpenLIT, 2 containers) is light *precisely because* it has no datasets, no annotation queues, and no media. There is no lighter Langfuse-equivalent; going lighter means giving up capability, not just ops weight.
2. **Opik (Comet) is the one serious candidate we had not evaluated — and it is HEAVIER than Langfuse, not lighter.** It matches Langfuse's completeness and adds a *first-party* MCP server, but its self-host stack is ~8 long-running containers (MySQL + Redis + ClickHouse + **Zookeeper** + MinIO + a **JVM** backend + a Python backend + frontend). It is a feature upgrade at a footprint cost — which inverts the original motivation for looking.
3. **The Langfuse-vs-Phoenix decision is not "heavy vs light" — it is "complete vs light."** Phoenix's lightness costs exactly the features the #380 QA judge requires (screenshots-in-datasets, annotation/labeling queues). That reframes Phoenix from "cheaper option" to "capability downgrade."

## Method + confidence

Two research passes. A broad discovery sweep (5 search angles → 21 sources → 84 claims → 25 adversarially verified, 23 confirmed / 2 refuted), then a targeted primary-doc verification of the three most promising net-new candidates (Opik, OpenLIT, Lunary), each dive independently re-verified against the projects' actual `docker-compose` files. Findings below are marked **[verified]** (primary docs, adversarially checked this pass) or **[unverified]** (general knowledge / secondary sources, listed for completeness — confirm before relying on).

## The net-new candidates, verified

### Opik (Comet) — the real find; complete, but heaviest **[verified]**

Apache-2.0, fully self-hostable, nothing gated that matters for a single user (only multi-user/SSO/RBAC is enterprise).

- **Footprint (CONFIRMED heavier than Langfuse).** Read from `deployment/docker-compose/docker-compose.yaml` on main: the base datastores carry **no compose profile**, so they are unconditional — MySQL 8.4 + Redis + ClickHouse + **Zookeeper** (ClickHouse coordination) + MinIO, plus `opik-backend` (**JVM / Java 25**), `opik-python-backend`, and the frontend. **~8 long-running containers, no minimal mode** — ClickHouse and Zookeeper cannot be dropped. Expect meaningfully **>3 GB**. This is Langfuse's ClickHouse+Redis+MinIO cost *plus* Zookeeper *plus* a full MySQL *plus* a JVM service.
- **Media (CONFIRMED supported self-hosted).** The self-host architecture doc lists MinIO as a core datastore for "attachments, artifacts, file uploads"; base64 payloads >250 KB auto-extract to object storage; images render in the **tracing, datasets, and experiment** views; the SDK `Attachment` type logs images/video/audio. LLM-as-judge supports vision eval. **Residual gap:** image rendering *inside the annotation-queue review UI specifically* is likely but not doc-confirmed.
- **Must-haves + workflow:** cost/token ✅ · prompt versioning ✅ · annotation queues ✅ · datasets ✅ · LLM-as-judge ✅ · first-party TS SDK (fits the Playwright judge) ✅.
- **MCP:** ✅ **first-party** — `comet-ml/opik-mcp` (traces, prompts, projects, metrics into Claude Code/Cursor). Two tools (`ask_ollie`, `run_experiment`) are Comet-Cloud-only and fail on self-host; read/list/write retained. This is the one capability Langfuse has only via a *community* server (`avivsinai/langfuse-mcp`, MIT, ~48 tools).
- **Health:** very strong — ~20.6k stars, 517 releases, latest v2.1.22 dated 2026-07-10.
- **Verdict:** the only platform matching Langfuse's completeness *and* adding first-party MCP — but you pay **more** ops weight, not less. Not worth +2 containers and a JVM for the MCP alone.

### OpenLIT — genuinely light, but cannot be the shared layer **[verified]**

Apache-2.0, nothing gated.

- **Footprint (CONFIRMED light).** The real root `docker-compose.yml` is **2 containers**: ClickHouse + the OpenLIT app. The OTLP gRPC/HTTP receivers are **embedded in the app image** (docs' "3 components" is conceptual, not a third service), and app metadata lives in an **embedded SQLite**. No Redis, no object store, no separate worker or Postgres.
- **Media (CONFIRMED unsupported)** — and the gap is wider than media: OpenLIT has **no datasets feature and no annotation/labeling queue at all**. It is OTel-span/text observability (span trees with token/cost metadata).
- **Must-haves:** cost/token ✅ · prompt versioning ✅ (Prompt Hub, semantic major/minor/patch, SDK version-fetch). Datasets ❌ · annotation queues ❌ · media ❌.
- **MCP:** ❌ none exposing its own data. (It can *instrument* MCP servers as an observability target — a different thing.)
- **Verdict:** a clean fit for **#379 extraction only** (OTLP + cost + prompt versioning, light, Apache-2.0, private content stays local). It **fails every #380 QA-judge requirement**, so it cannot serve as the shared instrumentation layer.

### Lunary — effectively disqualified; the OSS project is gone **[verified]**

- The public source repo `github.com/lunary-ai/lunary` returns **HTTP 404 — deleted** (~Dec 2025). The Python SDK is **archived**.
- The pricing page still advertises a free "Community Edition," but **both** self-hosting doc pages state the Docker *and* Docker-Compose setups are **"available only with Lunary Enterprise Edition"** (requires `docker login -u lunarycustomer` + a paid `LICENSE_KEY`). **There is no working free self-host path** — no public source to build, and the images are paywalled.
- Its one genuine upside — **Postgres-only, no ClickHouse** (~5 app containers) — is moot given the above. Media ❌, annotation queues ❌, product MCP ❌ (only a docs-search MCP).
- **Verdict:** dead end. Recorded so it is not chased again.

## Comparison — the platforms that matter

| Platform | License / self-host | Footprint | Cost/token | Prompt mgmt | Annotation queues | Datasets + **media** | MCP |
|---|---|---|---|---|---|---|---|
| **Langfuse** (chosen) | MIT core; EE only peripheral (RBAC/SCIM/audit — irrelevant single-user) | 6 svc, ~3 GB (ClickHouse **mandatory**, no alternative OLAP) | ✅ | ✅ | ✅ | ✅ **proven in spike** | community (MIT, ~48 tools) |
| **Opik** | Apache-2.0; only SSO/RBAC gated | **8 svc, >3 GB** (+ Zookeeper, MySQL, JVM) | ✅ | ✅ | ✅ | ✅ confirmed | ✅ **first-party** |
| **Phoenix** (fallback) | **Elastic License 2.0** — source-available, *not* MIT/Apache | ✅ light (single process, no ClickHouse) | ✅ | ⚠️ thin | ⚠️ thin | ❌ **weak media** | ~ |
| **OpenLIT** | Apache-2.0 | ✅ **2 svc** | ✅ | ✅ | ❌ | ❌ | ❌ |
| **Laminar (lmnr)** | Apache-2.0 (open-core) | ❌ ClickHouse — no lighter than Langfuse | ✅ | ⚠️ | ⚠️ | ⚠️ | ✅ native (`query_laminar_sql`) |
| **Lunary** | OSS repo **deleted**; self-host images **EE-paywalled** | ~5 svc, Postgres-only | ✅ | ✅ | ❌ | ❌ | ❌ |

**[unverified — listed for completeness, confirm before relying on]:** Helicone (Apache-2.0 core, proxy/gateway model), Langtrace (OTel-native), Portkey (OSS gateway; in-path), Pezzo (prompt-first; maintenance questionable), Agenta, Literal AI. **Not viable:** LangSmith (self-host is paid enterprise), Braintrust / HoneyHive / Parea / Athina / PromptLayer (cloud-first or enterprise-gated self-host), Datadog / Dynatrace LLM observability (no OSS self-host). **General APMs (SigNoz, Grafana LGTM):** will ingest OTel GenAI spans and do cost from attributes, but have **no** prompt management, annotation queues, datasets, or media — not candidates for the shared layer.

## Eval frameworks + local viewers (companions, not replacements)

- **Inspect AI (UK AISI)** **[verified]** — MIT, local pip/uv, native LLM-as-judge (`model_graded_qa`/`model_graded_fact`), agent-eval first-class, no hosted sink. The strongest pure eval framework found; a natural companion to whichever platform is the sink.
- **Promptfoo** **[verified]** — MIT, runs **fully local** (satisfies the privacy rule), declarative CI-friendly configs. Confirms the harness design's existing Tier-2 candidate.
- **[unverified]:** DeepEval (MIT, pytest-style, G-Eval), Ragas (RAG-centric), OpenAI Evals, Evidently, TruLens, DeepChecks, Giskard.
- **Raindrop Workshop** **[verified]** — MIT, single binary + SQLite (no server, no Docker), live trace streaming, native `workshop` MCP. Confirms the extraction spec's framing: a **dev-loop debugger** for extractor iteration, **not** a platform (its cost/prompt/annotation completeness is unresolved and it lacks the platform workflow).

## Consequences for the open decision

1. **Keep-Langfuse is ratified by the field survey.** Nothing beats it on the *combination* both programs need. The only platform matching its completeness (Opik) costs more weight, not less.
2. **Reframe Langfuse-vs-Phoenix.** It is not heavy-vs-light; it is **complete-vs-light**. Phoenix's lightness costs precisely the two capabilities the #380 QA judge depends on — **screenshots in datasets** and **annotation/labeling queues** — both of which the spike proved working on Langfuse. Phoenix is therefore a *capability downgrade*, not merely a cheaper option; choosing it means changing the QA harness's labeling and media design, not just its sink. (Also note Phoenix is **ELv2**, source-available rather than permissive OSS — the only non-MIT/Apache option in the shortlist.)
3. **The "split" option is now well-evidenced but probably worse.** OpenLIT (2 containers) could serve #379 alone, with #380's labeling handled elsewhere — but that means operating two systems, which likely costs more than the one heavy Langfuse it was meant to avoid.
4. **Langfuse's weight is inherent, not tunable.** Confirmed from primary docs: ClickHouse "is currently a required component… There is no alternative OLAP database supported at this time." (ClickHouse Inc. acquired Langfuse in Jan 2026 — the dependence is now structural.) There is no lighter Langfuse configuration to find.
5. **If agent-queryable observability becomes a priority**, the gap to close is MCP: Langfuse's path is the community `langfuse-mcp` server (MIT, ~48 tools covering traces, prompts, datasets, annotation queues, scores) rather than a first-party one. That is the single capability where Opik is genuinely ahead — but not worth its footprint on its own.

## Open questions not closed by this pass

- Whether Langfuse's **annotation-queue UI** renders media inline as well as the datasets view does (the spike proved media in dataset items; Opik has the same unconfirmed corner).
- Actual measured RAM for Opik/OpenLIT on **ARM under rootless Podman** — footprints above are read from compose topology, not benchmarked on the box.
- Whether the community `langfuse-mcp` server is robust enough to depend on (it is the incumbent's only MCP path).

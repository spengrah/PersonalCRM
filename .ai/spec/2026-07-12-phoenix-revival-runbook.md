# Phoenix — revival runbook (the fallback we chose not to take)

**Issue:** #635 · **Date:** 2026-07-12 · **Status:** torn down, revivable · **Companion to:** [`2026-07-11-llm-observability-platform-spike-decision.md`](./2026-07-11-llm-observability-platform-spike-decision.md)

Arize Phoenix was stood up on the `obs` tenant alongside Langfuse, seeded with the *same* real data, and used for a real side-by-side label pass. Langfuse won on labeling UX and on one hard capability (object-store media). Phoenix was then **fully removed** — container, volume, and image. It left nothing on the host, so this doc is the only record of how it was wired. Everything below was executed and verified during the spike; it is a replay script, not a design sketch.

## When you would revive it

Any one of these:

- **The Pi becomes the required host.** Phoenix is ~515 MB in one container against Langfuse's ~3 GB idle across six. That is the whole reason it stayed a live option — the Pi has ~4.2 GB free next to prod, which fits Phoenix comfortably and Langfuse not at all. If PII policy ever forces observability onto owned hardware, this is the path.
- **Langfuse's ops burden disappoints.** The seven frictions in the decision doc's "Ops burden, as lived" are the concrete watchlist; a failed upgrade or a ClickHouse incident is the trigger.
- **The screenshot requirement goes away.** The media gap is Phoenix's disqualifier. If the intent judge stopped consuming screenshots (it will not — see the restructuring doc, which *expands* the screenshot-consuming layer), Phoenix would be strictly the better-weighted choice.

## Stand it back up

The `obs` tenant already exists (uid 1998, `/var/lib/obs`, linger on, `user-1998.slice` MemoryMax=6G / CPUQuota=300%). Phoenix needs nothing from it but the podman socket. The `crm-rootless`-style prefix is required — rootless podman cannot chdir into the ssh user's home.

```bash
ssh <vps-host>
cd /tmp && sudo -n -u obs HOME=/var/lib/obs XDG_RUNTIME_DIR=/run/user/1998 \
  podman run -d --name phoenix --restart=always \
    -p 127.0.0.1:6006:6006 \
    -e PHOENIX_WORKING_DIR=/mnt/data \
    -v phoenix_data:/mnt/data \
    docker.io/arizephoenix/phoenix:latest
```

That is the entire deployment. SQLite lives in the `phoenix_data` volume; there is no separate database, cache, or object store to run.

**Reach it** the same way Langfuse was reached — loopback-only on the VPS, forwarded over SSH:

```bash
ssh -N -L 6006:127.0.0.1:6006 <vps-host>
# then http://localhost:6006
```

**Auth is OFF by default.** Loopback-only + SSH forward is what made that acceptable during the spike. Any durable exposure (Tailscale Serve, Caddy) must first set `PHOENIX_ENABLE_AUTH=true` plus `PHOENIX_SECRET` — do not put an unauthenticated Phoenix behind a tailnet hostname and call it private.

**Do not expect a healthcheck.** As with Langfuse on this host, podman-static never runs Quadlet/`HealthCmd` probes, so the container reports `(starting)` forever regardless of actual health. Probe it actively: `curl -sf http://127.0.0.1:6006/v1/projects`.

## Wire the harness to it

Three things differ from Langfuse, and all three cost real time to rediscover.

### 1. Traces: OTLP **protobuf**, OpenInference attributes

Phoenix's collector rejects OTLP/JSON with **415 Unsupported Media Type**. The dependency-free JSON exporter that works against Langfuse (`/api/public/otel/v1/traces`) will not work here. You need the OTel SDK and the protobuf exporter, and spans must carry **OpenInference** semantic conventions (not the bare OTel GenAI attributes) or the UI will not recognize them as LLM spans and will show no input/output.

```python
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from openinference.semconv.trace import SpanAttributes, OpenInferenceSpanKindValues

provider = TracerProvider(resource=Resource.create({"openinference.project.name": "qa-harness"}))
provider.add_span_processor(SimpleSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:6006/v1/traces")))
tracer = provider.get_tracer("qa-judge-harness")

span = tracer.start_span("judge CON-044-baseline", attributes={
    SpanAttributes.OPENINFERENCE_SPAN_KIND: OpenInferenceSpanKindValues.LLM.value,
    SpanAttributes.LLM_MODEL_NAME: model,
    SpanAttributes.LLM_TOKEN_COUNT_PROMPT: prompt_tokens,
    SpanAttributes.LLM_TOKEN_COUNT_COMPLETION: completion_tokens,
    SpanAttributes.INPUT_VALUE: prompt,      # the FULL judge prompt — see the warning below
    SpanAttributes.OUTPUT_VALUE: response,
    SpanAttributes.METADATA: json.dumps({...}),
})
span.end()
provider.force_flush()
```

Deps: `opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-http`, `openinference-semantic-conventions`.

**Log the prompt.** The QA corpus is synthetic — there is nothing to protect, and a trace without `INPUT_VALUE` is unlabelable. During the spike I stubbed the prompt out of the Phoenix spans by reflex, over-applying the #379 real-content minimization to data that has no PII in it, and the annotation UI became useless. Extraction traces (#379) are the *only* place minimization applies.

### 2. Annotations: create the config, then **attach it to the project**

Two steps, and the second is easy to miss — without the `PUT`, the config exists but no annotation controls appear in the project's UI.

```python
# create (once, global)
cid = POST /v1/annotation_configs {
  "name": "verdict", "type": "CATEGORICAL", "optimization_direction": "MAXIMIZE",
  "values": [{"label": "pass", "score": 1.0}, {"label": "unsure", "score": 0.5}, {"label": "fail", "score": 0.0}],
}
# attach (per project) — THIS is what turns the UI on
PUT /v1/projects/{project_id}/annotation_configs/{cid}
```

Apply a label with `POST /v1/span_annotations` (`{span_id, name, annotator_kind: "HUMAN", result: {label, score, explanation}}`). Span IDs come from `GET /v1/projects/{pid}/spans`; project IDs from `GET /v1/projects` (which also serves `DELETE` for a clean reset).

Phoenix has **no annotation queue** in Langfuse's sense — no worklist, no COMPLETED status, no "N items pending." You annotate spans from the trace table. That thinness is a real part of why the head-to-head went the way it did.

### 3. Datasets: multipart CSV, not JSON

`POST /v1/datasets/upload?sync=true` takes `multipart/form-data` with a CSV `file` part plus repeated `input_keys[]` / `output_keys[]` / `metadata_keys[]` parts naming the columns. There is no per-item JSON endpoint equivalent to Langfuse's `/api/public/dataset-items`.

```
--boundary
Content-Disposition: form-data; name="action"      -> create
Content-Disposition: form-data; name="name"        -> qa-corpus
Content-Disposition: form-data; name="input_keys[]"  -> behavior_id   (repeat per key)
Content-Disposition: form-data; name="output_keys[]" -> expected
Content-Disposition: form-data; name="metadata_keys[]"-> case_id
Content-Disposition: form-data; name="file"; filename="corpus.csv"; Content-Type: text/csv
```

## The gap that decided it

**Phoenix has no object store.** There is no media API, no presigned upload, no way to attach a PNG to a span or a dataset item. Screenshots can only be *referenced* — a run-dir-relative path in metadata, pointing at bytes that live in a gitignored `.runs/` directory on a laptop.

That is fatal for this harness specifically, because the **intent** judge (#622/#623/#624) *takes screenshots as evidence* — and per the restructuring plan the intent layer is the part that grows. A reviewer adjudicating an intent verdict must see what the judge saw. In Phoenix they cannot.

If Phoenix is ever revived, that gap must be closed out-of-band, and there are only two honest options: commit the screenshots to git (rejected in the decision doc — it serves neither program), or stand up a separate object store and put URLs in the span metadata, which quietly re-adds the second service Phoenix's lightness was supposed to save.

## Repo changes a switch would require

The OTel seam keeps this small, which is the point of having built it that way.

- `frontend/tests/tours/judge/adapter/span.ts` — the `QA_JUDGE_TRACE` emission point. Today it emits **metrics-only** GenAI spans (model, tokens, finish reason, *no content*). Either platform needs it enriched with the prompt/response; that change is a prerequisite for *both*, not a Phoenix tax.
- The exporter — swap OTLP/JSON for OTLP/protobuf + OpenInference attribute names. One file.
- The corpus/dataset push — CSV multipart instead of JSON items.
- The label round-trip — `span_annotations` instead of `scores` + queue items, and you lose the queue's "what's left to label" affordance.
- Screenshots — the unsolved one. See above.

Nothing in the harness or in #379's extractor is Langfuse-shaped by construction; the `recordVerdict(traceId, verdict, correctedValue)` adapter in the decision doc's action items exists precisely to keep this swap cheap.

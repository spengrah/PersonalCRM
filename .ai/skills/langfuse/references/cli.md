---
name: langfuse-cli
description: Langfuse CLI usage reference — install, resource/action discovery, credentials, and common usage tips for `langfuse-cli`. Use for further tips on using the Langfuse CLI.
metadata:
  required_access:
    - LANGFUSE_PROJECT_INTERFACE
---

# Langfuse CLI Reference

Documentation: https://langfuse.com/docs/api-and-data-platform/features/cli

## Install

```bash
# Run directly (recommended)
npx langfuse-cli api <resource> <action>
bunx langfuse-cli api <resource> <action>

# Or install globally
npm i -g langfuse-cli
langfuse api <resource> <action>
```

## Discovery

```bash
# List all resources and auth info
langfuse api __schema

# List actions for a resource
langfuse api <resource> --help

# Show args/options for a specific action
langfuse api <resource> <action> --help

# Preview the curl command without executing
langfuse api <resource> <action> --curl
```

## Credentials

Set environment variables:

```bash
export LANGFUSE_PUBLIC_KEY=pk-lf-...
export LANGFUSE_SECRET_KEY=sk-lf-...
export LANGFUSE_BASE_URL=https://cloud.langfuse.com  
```

## Tips

- Use `--json` for machine-readable JSON output
- Use `--curl` to preview the HTTP request without executing
- All list commands support filtering — check `<resource> <action> --help` for available options
- Use `observations` (v2), never `legacy-observations-v1s` — `observations` is the modern high-performance endpoint (cursor pagination, selective field groups); `legacy-observations-v1s` is the deprecated v1, and it returns 404 on a Langfuse v4 instance whose write mode is `events_only`
- Prefer `metrics` over `legacy-metrics-v1s` for the same reason
- Prefer `scores` over `legacy-score-v1s` for list/get operations
- Read traces through their root observations, not `traces list`/`traces get`: the legacy trace endpoints return 404 on a v4 instance in `events_only`, and `traces list` can time out on Langfuse Cloud. `observations list` (`GET /api/public/v2/observations`) with `isRootObservation=true` and `fields=core,io,trace_context` returns one root row per trace carrying `traceId`, `traceName`, the trace's `tags`, and `input`/`output`. Use `--trace-id` when traversing from a known trace. See the [Observations API docs](https://langfuse.com/docs/api-and-data-platform/features/observations-api) for the v1 → v2 mapping.
  - Filter by trace tags with `filter=[{"type":"arrayOptions","column":"traceTags","operator":"all of","value":["<tag>", ...]}]`; `all of` matches every listed tag.
  - The trace is the row's `traceId`, never its observation `id`. Score joins, annotation-queue items (`objectType: TRACE`, `objectId`) and `/project/<projectId>/traces/<traceId>` links all use `traceId`; a root backfilled from v3 has the id `t-<traceId>`, and an OTLP root has its own span id.
  - `input` and `output` come back as raw strings; decode JSON on the client.
  - Rows are unique by `(traceId, id)`, not by `id`: span ids are scoped to a trace, and a re-sent root can appear twice until Langfuse merges it.
- Pagination: legacy v1 endpoints use `--limit` and `--page`; modern endpoints (`observations`, `metrics`, `scores`) use cursor-based pagination — pass `--limit`, then thread `meta.cursor` from the response into the next request's `--cursor`. Observations v2 never returns a `limit` in `meta`, and its last page is `meta: {}` with no cursor. A cursor you have already followed means the read is broken: stop with an error rather than loop.

# Personal CRM Makefile

.PHONY: help setup dev dev-seed staging-reset tours build crm-admin mac-daemon test test-daemon-local clean docker-up docker-down docker-reset test-cadence-ultra test-cadence-fast qa-report qa-export qa-obs-smoke qa-model-prices qa-langfuse-setup qa-fn-backfill prod staging accelerated testing start start-local stop restart reload status dev-stop dev-restart dev-api-stop dev-api-start dev-api-restart ci-build-backend ci-build-frontend ci-build ci-test test-e2e test-e2e-local test-e2e-diff e2e-db deploy-mac promote setup-pi setup-mac-deploy dev-native postgres-native sqlc smoke-test test-deploy-scripts worktree-env worktree-deps test-integration-fast test-integration-slow test-clean-clones worktree-test-pg-ensure test-pg-stop test-pg-teardown test-pg-reap test-pg-smoke check-cadence-sole-writer check-followup-sole-writer check-rematch-sole-dispatcher check-crm-marker-construction check-sqlc-select-lists lint-ingest-registry spec-lint spec-coverage spec-drift api-types api-types-check api-docs api-docs-check

# Repo root (supports running make from subdirectories).
REPO_ROOT := $(shell git rev-parse --show-toplevel)

# Playwright browsers on the workspace volume, NOT the default
# ~/.cache/ms-playwright home-layer cache — a container rebuild wipes that even
# when the workspace persists, breaking every E2E test at browserType.launch.
# Anchored on the git COMMON dir (the main repo's .git, identical for every
# linked worktree) so all worktrees + the main checkout SHARE one browser cache
# (Playwright namespaces by version, so this is safe across version bumps).
# Falls back to the worktree toplevel if git resolution fails. Exported so every
# e2e recipe and `playwright install` resolve the same path.
MAIN_REPO_ROOT := $(shell cd "$$(dirname "$$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)")" 2>/dev/null && pwd || git rev-parse --show-toplevel 2>/dev/null)
PLAYWRIGHT_BROWSERS_PATH := $(MAIN_REPO_ROOT)/.playwright-browsers
export PLAYWRIGHT_BROWSERS_PATH

# Go build cache (workspace-local by default; override via env).
GOCACHE ?= $(REPO_ROOT)/.gocache
export GOCACHE

# Build stamping — mirrors build-images.yml; vars live in backend/internal/health/health.go.
# The $(shell ...) probes evaluate at Makefile parse on every `make` (three fast
# subshells); the `|| echo` fallbacks keep them safe outside a git checkout.
STAMP_VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
STAMP_GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
STAMP_BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
STAMP_LDFLAGS := -X personal-crm/backend/internal/health.Version=$(STAMP_VERSION) -X personal-crm/backend/internal/health.GitCommit=$(STAMP_GIT_COMMIT) -X personal-crm/backend/internal/health.BuildTime=$(STAMP_BUILD_TIME)

# Per-worktree test Postgres (gh #433). In a linked git worktree,
# scripts/worktree-test-pg.sh url emits a per-worktree TEST_DATABASE_URL (an
# isolated cluster on a derived port); in the main checkout / CI / opt-out it
# emits nothing. The `url` subcommand is side-effect-FREE + render-safe (it
# NEVER starts a server and NEVER warns) so it is safe to expand here and during
# `make -n`. Lazy `=` + `$(eval ...)` memo so the script runs at most ONCE per
# `make`, and ONLY when a default that references it is expanded — never on
# `make help`/`make build`. Provisioning (the side-effecting part) is the
# worktree-test-pg-ensure prerequisite below, not this variable.
WORKTREE_TEST_DB_URL = $(eval WORKTREE_TEST_DB_URL := $(shell bash scripts/worktree-test-pg.sh url))$(WORKTREE_TEST_DB_URL)

# The provisioning command for the worktree-test-pg-ensure prereq, computed
# lazily + memoized. It is EMPTY (so `make -n` prints NO ensure line and the
# main-checkout / forced-shared / CI render stays byte-identical) unless ALL of:
# this is an active linked worktree (resolver `active` echoes 1; render-safe,
# silent), AND TEST_DATABASE_URL is NOT an explicit env/CLI override (an explicit
# override wins and provisions nothing — Make's $(origin) distinguishes a `?=`
# default from an override). When inactive the recipe is a bare `@` (no-op, no
# printed command). `active` short-circuits to empty under CRM_WORKTREE_PG=0 and
# GITHUB_ACTIONS=true, so this is also empty in CI.
# $(origin TEST_DATABASE_URL) is "file" or "default" for the unoverridden `?=`
# default, and "environment"/"command line" for an explicit override. We want
# the ensure command ONLY in the non-override case, so match origin against the
# single-word tokens "file"/"default" via $(filter ...).
WORKTREE_PG_ENSURE_CMD = $(eval WORKTREE_PG_ENSURE_CMD := $(if $(filter file default,$(origin TEST_DATABASE_URL)),$(if $(shell bash scripts/worktree-test-pg.sh active),bash scripts/worktree-test-pg.sh ensure,),))$(WORKTREE_PG_ENSURE_CMD)

# CI sets GITHUB_ACTIONS=true AND an explicit TEST_DATABASE_URL, so the resolver
# must be a no-op there. The CI branch is a pure constant that never references
# the resolver (byte-for-byte unchanged). The local branch prefers the
# per-worktree URL when present, else the shared :5432 literal. TEST_DATABASE_URL
# stays `?=` so an explicit env/CLI override still wins everywhere.
ifeq ($(GITHUB_ACTIONS),true)
  TEST_DATABASE_URL ?= postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable
else
  TEST_DATABASE_URL ?= $(or $(WORKTREE_TEST_DB_URL),postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable)
endif

# Adaptive LOCAL -p/-parallel for the integration recipes. The formula lives
# ONLY in scripts/test-parallelism.sh; the Makefile just calls it. Lazy `=` +
# `$(eval ...)` memo so the script runs at most ONCE per `make`, and ONLY when
# an integration recipe expands $(TEST_P) — never on `make help`/`make build`.
# TEST_DATABASE_URL/PER_BINARY_CONN_EST are passed explicitly because $(shell)
# does NOT inherit Make variables as env vars automatically.
ADAPTIVE_P = $(eval ADAPTIVE_P := $(shell TEST_DATABASE_URL='$(TEST_DATABASE_URL)' PER_BINARY_CONN_EST='$(PER_BINARY_CONN_EST)' bash scripts/test-parallelism.sh))$(ADAPTIVE_P)

# CI pins the historical 4/4 (the CI Postgres + go-build concurrency are tuned
# for it; raising it there is out of scope). Gate on GITHUB_ACTIONS (GHA sets it
# "true" in every job), NOT bare CI (a local tool could export CI). The CI
# branch is a `:=` constant — no probe runs in CI.
ifeq ($(GITHUB_ACTIONS),true)
  TEST_P := 4
  TEST_PARALLEL := 4
else
  TEST_P = $(ADAPTIVE_P)
  TEST_PARALLEL = $(ADAPTIVE_P)
endif

E2E_DATABASE_NAME ?= personal_crm_test
E2E_DATABASE_URL ?= postgres://crm_user:crm_password@localhost:5432/$(E2E_DATABASE_NAME)?sslmode=disable
E2E_FRONTEND_PORT ?= 3000
E2E_BACKEND_PORT ?= 8080
BACKEND_SLOW_TESTS_REGEX := TestSyncWorker_LoadNoDuplicateConcurrentSyncs|TestPeriodicTick_FiresOnStart|TestSyncWorker_RescueOnCrash|TestSynthetic|TestTestdb

# Verbosity for `go test`. Defaults to -v for local readability; CI overrides
# to empty (GOTEST_VERBOSE=) to cut ~23k log lines. Failures still print.
GOTEST_VERBOSE ?= -v

# Optional integration-suite selectors, used by the per-worktree-pg smoke
# (scripts/test/smoke-worktree-test-pg.sh) to drive ONE collation-sensitive
# test through the REAL recipe. Both default to today's exact behavior so the
# render guard's byte-identical assertions are unaffected when unset:
#   INTEGRATION_RUN  -> appends -run '<regex>' when non-empty (else no -run).
#   INTEGRATION_PKGS -> the package list (default = today's full list).
INTEGRATION_RUN ?=
INTEGRATION_PKGS ?= ./tests/... ./internal/todoist/... ./internal/google/... ./internal/testdb/... ./cmd/crm-admin/... ./cmd/crm-api/...
# The leading space is embedded ONLY when non-empty so the recipe is
# byte-identical to today when the knob is unset (no trailing/double space).
INTEGRATION_RUN_FLAG := $(if $(INTEGRATION_RUN), -run '$(INTEGRATION_RUN)')

# Default target
# NOTE: When adding or removing make targets, update this help section to match
help:
	@echo "Available targets:"
	@echo ""
	@echo "🔧 Setup:"
	@echo "  setup       - Setup development environment (install deps + git hooks)"
	@echo ""
	@echo "🚀 Production Commands:"
	@echo "  start       - Start Personal CRM (production mode on port 3001)"
	@echo "  start-local - Start with .env.local (preserves your production secrets)"
	@echo "  stop        - Stop Personal CRM"
	@echo "  restart     - Restart Personal CRM (full stop/start)"
	@echo "  reload      - Rebuild and restart apps (keeps database running)"
	@echo "  status      - Check CRM status"
	@echo ""
	@echo "Environment Management:"
	@echo "  testing     - Switch to testing environment (ultra-fast cadences)"
	@echo "  staging     - Switch to staging environment (production cadence durations)" 
	@echo "  prod        - Switch to production environment (real cadences)"
	@echo ""
	@echo "Development:"
	@echo "  dev          - Start development servers (uses Docker for PostgreSQL)"
	@echo "  dev-seed     - Seed the dev synthetic world, then start dev servers (opt-in; dev is unchanged)"
	@echo "  dev-native   - Start dev servers with native PostgreSQL (no Docker)"
	@echo "  worktree-env - Symlink the main checkout's gitignored env files into this worktree"
	@echo "  worktree-deps - Install per-worktree frontend deps (node_modules) into this worktree"
	@echo "  staging-reset - HARD reset + reseed STAGING with the prod-shaped synthetic world — the manual force path / escape hatch (full wipe regardless of oauth; deploy-staging.yml auto-reseeds on seed-surface changes). Fail-closed: refuses a production-alias/empty CRM_ENV"
	@echo "  build       - Build both frontend and backend"
	@echo "  crm-admin   - Build the operator-only admin CLI (backend/crm-admin)"
	@echo "  mac-daemon  - Build the macOS daemon app bundle (optionally set CRM_MAC_CODESIGN_IDENTITY)"
	@echo "  sqlc        - Regenerate sqlc code from SQL queries"
	@echo "  api-types   - Regenerate frontend API types from Go wire structs"
	@echo "  api-types-check - Fail if generated API types drifted (non-mutating)"
	@echo "  api-docs    - Regenerate the Swagger spec from Go annotations"
	@echo "  api-docs-check - Fail if the generated Swagger spec drifted (non-mutating)"
	@echo "  lint        - Run all linters (backend + frontend)"
	@echo "  spec-lint   - Lint the behavior spec corpus (spec/*.yaml)"
	@echo "  spec-coverage - Report per-then-item coverage: ui behaviors (E2E), api behaviors (Go)"
	@echo "  spec-drift  - Warn when a behavior's assertions changed but no citing test was touched"
	@echo "  clean       - Clean build artifacts"
	@echo ""
	@echo "Testing:"
	@echo "  test                  - Run all backend tests (unit + integration, includes slow opt-in tests)"
	@echo "  test-unit             - Run backend unit tests only"
	@echo "  test-integration      - Run all backend integration tests"
	@echo "  test-integration-fast - Run backend integration tests without LONG_TESTS"
	@echo "  test-integration-slow - Run only LONG_TESTS-gated backend integration tests"
	@echo "  test-clean-clones     - Drop leaked clone and stale template databases"
	@echo "  test-pg-stop          - Stop this worktree's per-worktree test Postgres (keep data dir)"
	@echo "  test-pg-teardown      - Stop + delete this worktree's per-worktree test Postgres data dir"
	@echo "  test-pg-reap          - Prune per-worktree test Postgres instances whose worktree is gone"
	@echo "  test-pg-smoke         - Real-cluster smoke for the per-worktree Postgres mechanism"
	@echo "  test-frontend         - Run frontend unit tests"
	@echo "  test-e2e              - Run Playwright E2E tests"
	@echo "  test-e2e-local        - Run Playwright E2E tests (honors PLAYWRIGHT_GREP)"
	@echo "  test-e2e-diff         - Run diff-selected E2E tests (core + impacted)"
	@echo "  test-api              - Run API endpoint tests"
	@echo "  test-deploy-scripts   - Run the mocked deploy-script shell suites"
	@echo "  smoke-test            - Full system verification (restart + test)"
	@echo ""
	@echo "Docker:"
	@echo "  docker-up   - Start Docker Compose services"
	@echo "  docker-down - Stop Docker Compose services"
	@echo "  docker-reset- Reset Docker volumes and restart"
	@echo ""
	@echo "Cadence Testing:"
	@echo "  test-cadence-ultra - Test all cadences in minutes (testing env)"
	@echo "  test-cadence-fast  - Test all cadences in hours (accelerated env)"
	@echo ""
	@echo "Deployment:"
	@echo "  setup-pi         - One-time Pi setup (create user, directories)"
	@echo "  setup-mac-deploy - One-time Mac deploy setup (clone + reconcile + timer)"
	@echo "  promote          - Fast-forward main to develop (triggers prod deploy)"
	@echo "  deploy-mac       - Build and install the Mac daemon (requires CRM_MAC_CODESIGN_IDENTITY)"

# Setup development environment (installs all dev dependencies)
# Run this first when setting up a new development environment
setup:
	@bash scripts/setup-dev.sh

# Create logs directory
logs:
	@mkdir -p logs

# Development
dev:
	@echo "Starting development environment..."
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend server..."
	@bash scripts/start-backend.sh
	@echo "✅ Backend server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo "Starting frontend development server..."
	@bash scripts/start-frontend-dev.sh
	@echo "✅ Frontend dev server started (logs: logs/frontend-dev.log, PID: $$(cat logs/frontend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo ""
	@echo "🌐 Frontend: http://localhost:3000"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo ""
	@echo "💡 Both servers are running detached and will continue after you close this terminal"
	@echo "   Use 'make dev-stop' to stop both servers"
	@echo ""
	@echo "📋 To view logs:"
	@echo "   tail -f logs/backend-dev.log"
	@echo "   tail -f logs/frontend-dev.log"
	@echo ""
	@echo "Press Ctrl+C to exit (servers will keep running)"
	@tail -f logs/frontend-dev.log logs/backend-dev.log 2>/dev/null || sleep infinity

dev-seed: ## Seed the `dev` synthetic world into local Postgres, then start dev servers (opt-in; `make dev` is unchanged)
	@echo "Seeding dev synthetic world, then starting development environment..."
	@make dev-api-stop                        # stop any detached backend so the seed harness owns the River queue
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@bash scripts/dev-seed.sh                 # exports DATABASE_URL, migrates + crm-admin --seed --profile dev --yes (backend NOT running)
	@echo "Starting backend server..."
	@bash scripts/start-backend.sh
	@echo "✅ Backend server started (logs: logs/backend-dev.log)"
	@echo "Starting frontend development server..."
	@bash scripts/start-frontend-dev.sh
	@echo "✅ Frontend dev server started (logs: logs/frontend-dev.log)"
	@echo ""
	@echo "🌐 Frontend: http://localhost:3000"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo ""
	@echo "Press Ctrl+C to exit (servers will keep running)"
	@tail -f logs/frontend-dev.log logs/backend-dev.log 2>/dev/null || sleep infinity

# Development helpers
dev-stop:
	@echo "Stopping development servers (backend and frontend dev)..."
	@# Kill backend by port. `go run ./cmd/crm-api` names the child binary
	@# 'crm-api' (so `pkill -f crm-api` reaches it), but port-kill is the
	@# reliable mechanism regardless of process name.
	@lsof -ti:8080 | xargs kill -9 2>/dev/null || true
	@pkill -f "next dev" || true
	@pkill -f "node.*next" || true
	@if [ -f logs/frontend-dev.pid ]; then kill $$(cat logs/frontend-dev.pid) 2>/dev/null || true; fi
	@if [ -f logs/backend-dev.pid ]; then kill $$(cat logs/backend-dev.pid) 2>/dev/null || true; fi
	@echo "✅ Dev servers stopped (if they were running)"

dev-restart:
	@echo "🔄 Restarting development environment..."
	@make dev-stop
	@sleep 1
	@make dev

dev-api-stop:
	@echo "Stopping backend dev server..."
	@pkill -f crm-api || true
	@# `go run ./cmd/crm-api` execs a child binary named `crm-api`, so the
	@# pkill above reaches it. Kill by listening port 8080 too, then wait for
	@# the port to be released — `make dev-seed` + the seed CLI rely on the
	@# backend's River workers being genuinely gone.
	@lsof -ti tcp:8080 | xargs kill -9 2>/dev/null || true
	@for i in 1 2 3 4 5; do \
	  if lsof -ti tcp:8080 >/dev/null 2>&1; then \
	    sleep 0.4; \
	  else \
	    break; \
	  fi; \
	done
	@# Fail loudly if the port is still bound — make dev-seed relies on the
	@# backend being genuinely gone before it seeds (a live backend would race
	@# the seed's River client). A misleading "freed" message would hide that.
	@if lsof -ti tcp:8080 >/dev/null 2>&1; then \
	  echo "❌ Backend dev server still bound on port 8080 after kill — refusing to report stopped"; \
	  exit 1; \
	fi
	@echo "✅ Backend dev server stopped (if it was running) and port freed"

dev-api-start:
	@echo "Starting backend dev server..."
	@make docker-up
	@make logs
	@bash scripts/start-backend.sh
	@echo "✅ Backend dev server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"

dev-api-restart:
	@echo "🔄 Restarting backend dev server..."
	@make dev-api-stop
	@sleep 1
	@make dev-api-start

staging-reset: ## HARD reset + reseed STAGING — manual force / escape hatch (full wipe regardless of oauth; deploy-staging.yml auto-reseeds on seed-surface changes; fail-closed production refuse; STAGING-only)
	@bash scripts/staging-reset.sh   # ssh STAGING_HOST -> refuse if CRM_ENV is a production alias or empty -> stop backend -> ephemeral crm-admin --reset-and-seed --profile prod-shaped --yes (deployed image) -> start backend

tours: ## Reset staging + run the agentic UX QA tours. Config from env only: TOURS_BASE_URL, TOURS_API_KEY, TOURS_API_URL. Captures land in frontend/tests/tours/.runs/ (gitignored). TOURS_SKIP_RESET=1 skips the reset.
	@scripts/run-tours.sh

qa-report: ## Render the advisory report over a tours run dir (RUNDIR relative to frontend/, or absolute). JUDGE=1 adds the live judge over residue items (codex quota) AND the trap detection self-test — a missed/non-executable trap sets a non-zero EXIT (a hard signal, distinct from the advisory verdicts, which never gate). OUT=<file> writes to a file instead of stdout. To ship the doctored trap trace when a trap MISSES, run `make qa-export` as an INDEPENDENT step (`;` — never `&&`, which would skip the export on the very miss it must diagnose). Set QA_JUDGE_TRACE=<file> to capture the spans. (The deterministic verifier merge gate retired with the verifier lane — its coverage lives in the cited Playwright E2E specs.)
	@cd frontend && bun run tests/tours/judge/report/render.ts "$(RUNDIR)" $(if $(OUT),"$(OUT)",) $(if $(filter 1,$(JUDGE)),--judge,)

qa-export: ## Ship a judge run's GenAI spans (TRACE=<trace.jsonl>, written when the judge runs with QA_JUDGE_TRACE set) to Langfuse, screenshots included. No-op without LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY. QA_RUN_ID/QA_GIT_SHA stamp per-round provenance tags + a session; QA_SALT_PASSES (default 3) sets how many passing traces are salted into the triage queue. The round wrapper sets QA_RUN_ID/QA_GIT_SHA from manifest.json; each is validated + applied independently (an invalid one is dropped, never fail-closed). The recipe inherits the environment (no env -i), so these pass through.
	@cd frontend && bun run tests/tours/judge/export/run.ts "$(TRACE)"

qa-obs-smoke: ## Live-verify the usage generation observation end-to-end against a WRITABLE Langfuse. Ships ONE synthetic span (behavior SMOKE-OBS, per-invocation ids stable within the run, fixed token counts, near-present span instants) twice, reads the observation back from the observations endpoint, and asserts observations non-empty, usageDetails echoed exactly, the observation's own costDetails total == the hand-computed figure, startTime from the span (not export time), and — after re-exporting the same span — still exactly ONE observation at the same id and cost (a re-export does not duplicate; it also does not update). Cross-checks the trace detail endpoint and names any disagreement between the two views. Requires LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY with WRITE access. Non-zero exit on any failed assertion.
	@cd frontend && bun run tests/tours/judge/export/obs-smoke.ts

qa-model-prices: ## Sync this project's Langfuse model prices for the judge's ACTIVE models (QA_JUDGE_MODEL + QA_INTENT_MODEL, or MODELS=a,b) against upstream's default-model-prices.json. RECONCILES: zero drift = zero writes, and an override is DELETED once the managed row catches up. DRY_RUN=1 prints intended changes without writing; FORCE=1 overrides the >5x implausible-delta guard; STRICT=1 exits non-zero on failure (default is fail-open, for the nightly); UPSTREAM=<file> reads the payload from a local file instead of fetching (test/smoke seam); RESET=<model> deletes this project's override for <model> and exits. Requires LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY with WRITE access.
	@cd frontend && bun run tests/tours/judge/export/model-prices.ts \
	  $(if $(MODELS),--models "$(MODELS)",) $(if $(filter 1,$(DRY_RUN)),--dry-run,) \
	  $(if $(filter 1,$(FORCE)),--force,) $(if $(filter 1,$(STRICT)),--strict,) \
	  $(if $(UPSTREAM),--upstream "$(UPSTREAM)",) $(if $(RESET),--reset "$(RESET)",)

qa-langfuse-setup: ## Idempotently provision the standing QA triage queue (ground_truth+disposition dims) + verdict/ground_truth/disposition score configs in Langfuse (obs). Re-runnable; reconciles desired state. Requires LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY (errors non-zero without them).
	@cd frontend && bun run tests/tours/judge/export/setup.ts

qa-fn-backfill: ## False-negative recall helper. BEHAVIOR=<id> lists covering-PASS candidate traces (deep-links + cite/critique); add ROUND=<runId|gitSha> to narrow. BEHAVIOR=<id> TRACE=<traceId> enqueues that proven candidate into the qa-triage queue for should_fail scoring. Fail-closed (non-zero on any error); reports, never mutates, an already-triaged item. Requires LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY; LANGFUSE_PROJECT_ID resolves deep-links (else the key's sole project).
	@if [ -n "$(TRACE)" ] && [ -n "$(ROUND)" ]; then echo "qa-fn-backfill: TRACE (enqueue) and ROUND (list narrow) are mutually exclusive — pass only one." >&2; exit 2; fi
	@cd frontend && bun run tests/tours/judge/export/backfill.ts "$(BEHAVIOR)" $(if $(TRACE),"$(TRACE)",$(if $(ROUND),--round "$(ROUND)",))

# Native PostgreSQL (for containerized development without Docker-in-Docker)
# Symlink the main checkout's gitignored env files (.env, frontend/.env.local,
# ...) into the current worktree. Normally automatic via the post-checkout git
# hook on `git worktree add`; run this manually if a worktree was created before
# the hook existed, or to re-link after rotating a secret. No-op in the main
# checkout.
worktree-env:
	@WORKTREE_ENV_VERBOSE=1 bash scripts/link-worktree-env.sh

# Install per-worktree dependencies that can't be symlinked (frontend
# node_modules is branch-specific, tied to this branch's lockfile). Normally
# automatic via the post-checkout git hook on `git worktree add`; run this
# manually if a worktree was created before the hook existed. No-op in the main
# checkout (use `make setup` there). Go deps need nothing (global modcache).
worktree-deps:
	@WORKTREE_DEPS_VERBOSE=1 bash scripts/install-worktree-deps.sh

postgres-native:
	@bash scripts/start-postgres-native.sh

# Development with native PostgreSQL (no Docker required)
# Use this when running inside a container where Docker is not available
dev-native: postgres-native
	@echo "Starting development environment (native PostgreSQL)..."
	@make logs
	@echo "Starting backend server..."
	@bash scripts/start-backend.sh
	@echo "✅ Backend server started (logs: logs/backend-dev.log, PID: $$(cat logs/backend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo "Starting frontend development server..."
	@bash scripts/start-frontend-dev.sh
	@echo "✅ Frontend dev server started (logs: logs/frontend-dev.log, PID: $$(cat logs/frontend-dev.pid 2>/dev/null || echo 'unknown'))"
	@echo ""
	@echo "🌐 Frontend: http://localhost:3000"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo ""
	@echo "💡 Both servers are running detached and will continue after you close this terminal"
	@echo "   Use 'make dev-stop' to stop both servers"
	@echo ""
	@echo "📋 To view logs:"
	@echo "   tail -f logs/backend-dev.log"
	@echo "   tail -f logs/frontend-dev.log"
	@echo ""
	@echo "Press Ctrl+C to exit (servers will keep running)"
	@tail -f logs/frontend-dev.log logs/backend-dev.log 2>/dev/null || sleep infinity

test-e2e: e2e-db
	@echo "Cleaning up any conflicting processes..."
	@-lsof -ti:$(E2E_FRONTEND_PORT) | xargs -r kill -9 2>/dev/null || true
	@-lsof -ti:$(E2E_BACKEND_PORT) | xargs -r kill -9 2>/dev/null || true
	@sleep 1
	@echo "Running Playwright E2E tests..."
	@ENV_FILE=$${ENV_FILE:-$(REPO_ROOT)/.env.example.testing}; \
	if [ ! -f "$$ENV_FILE" ]; then echo "❌ ENV file not found: $$ENV_FILE"; exit 1; fi; \
	set -a; . "$$ENV_FILE"; set +a; \
	export DATABASE_URL="$(E2E_DATABASE_URL)"; \
	if [ -f "$(REPO_ROOT)/frontend/.env.local" ]; then mv "$(REPO_ROOT)/frontend/.env.local" "$(REPO_ROOT)/frontend/.env.local.bak"; fi; \
	echo "NEXT_PUBLIC_API_KEY=$$API_KEY" > "$(REPO_ROOT)/frontend/.env.local"; \
	echo "NEXT_PUBLIC_API_URL=http://localhost:$(E2E_BACKEND_PORT)" >> "$(REPO_ROOT)/frontend/.env.local"; \
	cd "$(REPO_ROOT)/frontend" && DATABASE_URL="$$DATABASE_URL" API_KEY=$$API_KEY NEXT_PUBLIC_API_KEY=$$API_KEY NEXT_PUBLIC_API_URL=http://localhost:$(E2E_BACKEND_PORT) E2E_FRONTEND_PORT="$(E2E_FRONTEND_PORT)" E2E_BACKEND_PORT="$(E2E_BACKEND_PORT)" ./node_modules/.bin/playwright test --project=chromium; \
	EXIT_CODE=$$?; \
	rm -f "$(REPO_ROOT)/frontend/.env.local"; \
	if [ -f "$(REPO_ROOT)/frontend/.env.local.bak" ]; then mv "$(REPO_ROOT)/frontend/.env.local.bak" "$(REPO_ROOT)/frontend/.env.local"; fi; \
	exit $$EXIT_CODE

test-e2e-local: e2e-db
	@echo "Cleaning up any conflicting processes..."
	@-lsof -ti:$(E2E_FRONTEND_PORT) | xargs -r kill -9 2>/dev/null || true
	@-lsof -ti:$(E2E_BACKEND_PORT) | xargs -r kill -9 2>/dev/null || true
	@sleep 1
	@echo "Running Playwright E2E tests (local selection)..."
	@ENV_FILE=$${ENV_FILE:-$(REPO_ROOT)/.env.example.testing}; \
	if [ ! -f "$$ENV_FILE" ]; then echo "❌ ENV file not found: $$ENV_FILE"; exit 1; fi; \
	set -a; . "$$ENV_FILE"; set +a; \
	export DATABASE_URL="$(E2E_DATABASE_URL)"; \
	if [ -f "$(REPO_ROOT)/frontend/.env.local" ]; then mv "$(REPO_ROOT)/frontend/.env.local" "$(REPO_ROOT)/frontend/.env.local.bak"; fi; \
	echo "NEXT_PUBLIC_API_KEY=$$API_KEY" > "$(REPO_ROOT)/frontend/.env.local"; \
	echo "NEXT_PUBLIC_API_URL=http://localhost:$(E2E_BACKEND_PORT)" >> "$(REPO_ROOT)/frontend/.env.local"; \
	GREP_ARGS=""; \
	if [ -n "$$PLAYWRIGHT_GREP" ]; then GREP_ARGS="--grep $$PLAYWRIGHT_GREP"; fi; \
	cd "$(REPO_ROOT)/frontend" && DATABASE_URL="$$DATABASE_URL" API_KEY=$$API_KEY NEXT_PUBLIC_API_KEY=$$API_KEY NEXT_PUBLIC_API_URL=http://localhost:$(E2E_BACKEND_PORT) E2E_FRONTEND_PORT="$(E2E_FRONTEND_PORT)" E2E_BACKEND_PORT="$(E2E_BACKEND_PORT)" ./node_modules/.bin/playwright test --project=chromium $$GREP_ARGS; \
	EXIT_CODE=$$?; \
	rm -f "$(REPO_ROOT)/frontend/.env.local"; \
	if [ -f "$(REPO_ROOT)/frontend/.env.local.bak" ]; then mv "$(REPO_ROOT)/frontend/.env.local.bak" "$(REPO_ROOT)/frontend/.env.local"; fi; \
	exit $$EXIT_CODE

test-e2e-diff: e2e-db
	@PLAYWRIGHT_WORKERS=1 node "$(REPO_ROOT)/scripts/run-e2e-local.mjs"

e2e-db:
	@bash "$(REPO_ROOT)/scripts/ensure-postgres-for-tests.sh"
	@echo "Setting up isolated E2E test database ($(E2E_DATABASE_NAME))..."
	@E2E_DATABASE_NAME="$(E2E_DATABASE_NAME)" bash "$(REPO_ROOT)/scripts/e2e-db-reset.sh"
	@echo "✓ E2E test database ready"

# Build
build:
	@echo "Building backend..."
	@cd backend && go build -ldflags "$(STAMP_LDFLAGS)" -o bin/crm-api ./cmd/crm-api
	@echo "Building frontend..."
	@cd frontend && bun run build

# Operator-only admin binary. NOT wired into CI; build on demand on
# the Pi when a one-shot maintenance task is needed (e.g.,
# `./crm-admin --messages-rematch-stranded`).
crm-admin:
	@echo "Building crm-admin..."
	@cd backend && go build -ldflags "$(STAMP_LDFLAGS)" -o crm-admin ./cmd/crm-admin
	@echo "✓ crm-admin built at backend/crm-admin"

# Mac daemon. Built locally on a Mac; not wired into `make build`
# because the Pi-side build pipeline has no Swift toolchain.
# Produces a signed crm-mac.app bundle at mac-daemon/.build/release/crm-mac.app
# (per the SMAppService architecture in
# .ai/log/plan/mac-daemon-app-bundle-rewrite.md). Set
# CRM_MAC_CODESIGN_IDENTITY to use a stable local codesigning identity;
# otherwise the bundle is ad-hoc signed.
# Bundle assembly is delegated to Scripts/assemble_bundle.sh — only
# Command Line Tools (no full Xcode) are required.
mac-daemon:
	@echo "Building crm-mac (release)..."
	@cd mac-daemon && swift build -c release
	@CRM_BUILD_SHA="$$(git rev-parse HEAD)" bash mac-daemon/Scripts/assemble_bundle.sh \
		mac-daemon/.build/release/crm-mac \
		mac-daemon/.build/release/crm-mac.app \
		mac-daemon/Sources/crm-mac/Info.plist
	@echo "✓ crm-mac.app built at mac-daemon/.build/release/crm-mac.app"

# Run Mac daemon Swift tests locally. Requires Xcode 16 (Swift 6 toolchain).
# A CI-skipped chat.db smoke test reads ~/Library/Messages/chat.db; that
# test only runs without the CI=true env var (i.e. interactive dev runs).
test-daemon-local:
	@echo "Running Mac daemon Swift tests..."
	@cd mac-daemon && swift test

# Tests
test: test-unit test-integration test-frontend

test-unit:
	@echo "Running backend unit tests..."
	@cd backend && go test ./tests/... ./internal/matching/... ./internal/events/... ./internal/service/... ./internal/contacttask/... ./internal/synthetic ./internal/synthetic/factory/... ./internal/synthetic/replay/... ./internal/spec/... ./cmd/spec-lint/... ./cmd/spec-coverage/... ./cmd/spec-drift/... $(GOTEST_VERBOSE) -short

# Provisions the per-worktree Postgres instance BEFORE the integration recipes
# expand $(TEST_DATABASE_URL) (gh #433). As a prerequisite it runs to
# completion — including before the dependent recipe's variable expansion — so
# on a cold first run the server is up and `url` resolves to the per-worktree
# URL (cold-run ordering). The recipe is the lazily-computed
# $(WORKTREE_PG_ENSURE_CMD), which is EMPTY for the main checkout / CRM_WORKTREE_PG=0
# / CI / an explicit TEST_DATABASE_URL override — so `make -n` prints NO line in
# those cases and the render stays byte-for-byte identical to today. When active
# it is `bash scripts/worktree-test-pg.sh ensure`; on failure that warns on
# stderr and (non-strict) exits 0, degrading to the shared instance, while
# CRM_WORKTREE_PG=strict makes it fail the target.
worktree-test-pg-ensure:
	@$(WORKTREE_PG_ENSURE_CMD)

test-integration-fast: worktree-test-pg-ensure
	@echo "Running backend integration tests (default set)..."
	@cd backend && DATABASE_URL="$(TEST_DATABASE_URL)" go test -tags integration_testdb -count=1 -parallel $(TEST_PARALLEL) -p $(TEST_P) $(INTEGRATION_PKGS) $(GOTEST_VERBOSE)$(INTEGRATION_RUN_FLAG)

test-integration: worktree-test-pg-ensure
	@echo "Running backend integration tests..."
	@cd backend && DATABASE_URL="$(TEST_DATABASE_URL)" LONG_TESTS=1 go test -tags integration_testdb -count=1 -parallel $(TEST_PARALLEL) -p $(TEST_P) $(INTEGRATION_PKGS) $(GOTEST_VERBOSE)$(INTEGRATION_RUN_FLAG)

test-integration-slow: worktree-test-pg-ensure
	@echo "Running backend slow integration tests..."
	@cd backend && DATABASE_URL="$(TEST_DATABASE_URL)" LONG_TESTS=1 go test -tags integration_testdb -count=1 -parallel $(TEST_PARALLEL) -p $(TEST_P) $(INTEGRATION_PKGS) $(GOTEST_VERBOSE) -run '$(BACKEND_SLOW_TESTS_REGEX)'

# Per-worktree test-Postgres lifecycle (gh #433). All operate ONLY on
# this worktree's own instance under $CRM_WORKTREE_PG_HOME — never the shared
# Docker crm-postgres:5432, never Docker.
test-pg-stop:
	@bash scripts/worktree-test-pg.sh stop

test-pg-teardown:
	@bash scripts/worktree-test-pg.sh teardown

# reap prunes per-worktree instances whose worktree no longer exists (run
# between sessions). Cross-references `git worktree list`; safe to run anytime.
test-pg-reap:
	@bash scripts/worktree-test-pg.sh reap

# Real-cluster smoke for the per-worktree mechanism (NOT pre-push: it owns a DB
# and binds a port). Set CRM_PG_SMOKE_REQUIRED=1 to make a missing pg16
# toolchain a hard failure instead of a clean skip.
test-pg-smoke:
	@bash scripts/test/smoke-worktree-test-pg.sh

# Sweep leaked clone databases (personal_crm_test_clone_*) AND stale
# per-migration-set template databases (personal_crm_test_template_<hash>).
# Crashed processes can leak clones; templates accumulate as migration sets
# change over time (branch switches, divergent worktrees). Run ONLY when no
# integration tests are in flight: the template drop pass holds the build
# advisory lock so it never drops a template mid-CREATE...TEMPLATE, but the
# lock is released between a running test process's operations, so a different
# worktree's still-running process could still clone from a template later
# (stronger cross-worktree-concurrent safety is out of scope — see #424).
# Implemented in the Go harness (not raw psql) so it shares the same name
# guards — every drop passes assertDroppableTestDBName, the current run's
# template is kept warm, and the base is never touched. Uses `go run` on a
# package main (NOT `go test`) so the sweep can never execute during the normal
# `go test ./internal/testdb/...` integration run.
test-clean-clones:
	@echo "Sweeping leaked clone and stale template databases..."
	@cd backend && DATABASE_URL="$(TEST_DATABASE_URL)" go run -tags integration_testdb ./internal/testdb/cmd/cleanclones

test-frontend:
	@echo "Running frontend tests..."
	@cd frontend && bun run test

test-api:
	@echo "Running API tests..."
	@cd backend && go test ./tests/... -v

# Mocked shell suites for the deploy orchestrators (Pi + Mac). Pure bash with
# PATH-shadow stubs (no podman/Mac/network), so they run on any CI runner. The
# committed timer template is validated for XML/plist well-formedness with a
# cross-platform python3 plistlib parse (the __INSTALL_PREFIX__ placeholder lives
# inside <string> values, so it parses fine); plutil is macOS-only and not used here.
test-deploy-scripts:
	@echo "Running deploy-script shell tests (parallel)..."
	@tests="scripts/deploy-artifact.test.sh scripts/backup-db.test.sh scripts/restore-db.test.sh scripts/deploy-staging.test.sh scripts/staging-reset.test.sh scripts/ci/staging-reseed-decision.test.sh scripts/ci/ghcr-retention.test.sh scripts/ci/qa-round-cadence-gate.test.sh scripts/ci/qa-nightly-round.test.sh scripts/ci/qa-fn-backfill-guard.test.sh scripts/staging-deployed-sha.test.sh scripts/staging-reseed.test.sh scripts/admin/setup-staging-reseed-host.sh.test.sh scripts/run-tours.test.sh scripts/reconcile-mac-daemon.test.sh scripts/setup-mac-deploy.test.sh scripts/trigger-mac-deploy.test.sh scripts/test/test-promote-preflight.sh"; \
	tmp="$$(mktemp -d)"; \
	for t in $$tests; do \
	  ( bash "$$t" >"$$tmp/$$(echo "$$t" | tr / _).out" 2>&1; echo "$$?" >"$$tmp/$$(echo "$$t" | tr / _).rc" ) & \
	done; \
	wait; \
	fail=0; \
	for t in $$tests; do \
	  b="$$tmp/$$(echo "$$t" | tr / _)"; rc="$$(cat "$$b.rc" 2>/dev/null || echo 1)"; \
	  if [ "$$rc" = 0 ]; then echo "  OK   $$t"; else echo "  FAIL $$t (exit $$rc):"; sed "s/^/      /" "$$b.out"; fail=1; fi; \
	done; \
	rm -rf "$$tmp"; \
	[ "$$fail" = 0 ] || { echo "deploy-script shell tests FAILED"; exit 1; }; \
	echo "All deploy-script shell tests passed."
	@echo "Validating the committed timer template (plistlib parse)..."
	@python3 -c "import plistlib,sys; plistlib.loads(open(sys.argv[1],'rb').read()); print('  timer template OK')" \
		infra/mac-deploy/xyz.spengrah.crm-mac-deploy.plist.template

smoke-test:
	@echo "Running full system smoke test..."
	@./scripts/smoke-test.sh

# CI/CD targets
ci-build-backend:
	@echo "Building backend for ARM64..."
	@cd backend && GOOS=linux GOARCH=arm64 go build -ldflags "$(STAMP_LDFLAGS)" -o bin/crm-api ./cmd/crm-api
	@cd backend && GOOS=linux GOARCH=arm64 go build -ldflags "$(STAMP_LDFLAGS)" -o bin/crm-admin ./cmd/crm-admin

ci-build-frontend:
	@echo "Building frontend..."
	@cd frontend && bun run build

ci-build: ci-build-backend ci-build-frontend

# Linting
GOLANGCI_LINT := $(shell which golangci-lint 2>/dev/null || echo $$(go env GOPATH)/bin/golangci-lint)

lint: lint-ingest-registry
	@echo "Running golangci-lint..."
	@cd backend && $(GOLANGCI_LINT) run ./...

lint-fix:
	@echo "Running golangci-lint with auto-fix..."
	@cd backend && $(GOLANGCI_LINT) run --fix ./...

# Lint the behavior SSOT corpus (spec/*.yaml) against the schema in
# spec/README.md. Standalone target (NOT a `lint` prerequisite): the pre-push
# LINT lane runs it as its own entry, so chaining it into `lint` would
# double-run it in that lane.
spec-lint:
	@cd backend && go run ./cmd/spec-lint $(REPO_ROOT)/spec

# Behavior-SSOT traceability scanner: cross-references // spec: citations in
# test files against spec/*.yaml and reports per-then-item coverage keyed on
# surface — ui behaviors via E2E citations, api behaviors via Go-test
# citations. Warn-only unless a domain lists the surface in its settled list;
# invalid citations and orphans on a settled surface exit non-zero.
spec-coverage:
	@cd backend && go run ./cmd/spec-coverage $(REPO_ROOT)

# Behavior-drift advisory: warns when a behavior's given/when/then/statement
# changed but no citing test file was touched. Warn-only (exit 0); exit 2 on
# git/operational error (never fail-open). Base ref = origin/develop; the CLI
# computes merge-base(HEAD, origin/develop) internally.
spec-drift:
	@cd backend && go run ./cmd/spec-drift $(REPO_ROOT) origin/develop

# Grep guard for #342's descriptor table: fails if the IngestBatch body
# names any event kind (constant or dotted literal) or per-family
# predicate — routing must go through the daemonFamily kindToFamily
# table. See scripts/check-ingest-registry.sh and the agreement test
# backend/internal/service/ingest_registry_test.go.
lint-ingest-registry:
	@$(REPO_ROOT)/scripts/check-ingest-registry.sh

ci-test: lint check-cadence-sole-writer check-followup-sole-writer check-rematch-sole-dispatcher check-crm-marker-construction check-sqlc-select-lists test-unit test-integration-fast test-frontend
	@echo "✅ All CI tests passed"

# Sole-writer guard: verifies only CadenceUpdater calls cadence-writing queries.
# Runs alongside the Go AST test at backend/tests/sole_writer_static_test.go;
# produces reviewer-visible file/line evidence whenever a cadence-writing
# symbol escapes the allowlist. See scripts/check-cadence-sole-writer.sh.
check-cadence-sole-writer:
	@$(REPO_ROOT)/scripts/check-cadence-sole-writer.sh

# Sole-writer guard: verifies only FollowUpManager (consumer/followup_manager.go)
# calls follow-up writer symbols, and that the retired todoist_close_pending
# metadata key is not referenced anywhere. See
# scripts/ci/followup-sole-writer-guard.sh.
check-followup-sole-writer:
	@$(REPO_ROOT)/scripts/ci/followup-sole-writer-guard.sh

# Sole-dispatcher guard: enforces that StartRematchForContact is test-only
# after the PR-10 event-bus cutover (#180). Production rematch dispatch
# flows through events.Bus + RematchDispatcher; any non-test caller
# indicates a partial revert. See scripts/ci/rematch-sole-dispatcher-guard.sh.
check-rematch-sole-dispatcher:
	@$(REPO_ROOT)/scripts/ci/rematch-sole-dispatcher-guard.sh

# CRM-marker construction guard: verifies the Todoist CRM-marker wire format
# is built only in contacttask.EncodeMarker. Runs alongside the Go AST test
# at backend/tests/crm_marker_construction_static_test.go. See
# scripts/ci/crm-marker-construction-guard.sh.
check-crm-marker-construction:
	@$(REPO_ROOT)/scripts/ci/crm-marker-construction-guard.sh

# Duplicated-SELECT-list guard: fails if an identical explicit >=3-column
# SELECT projection of the same table appears in 2+ source queries (use
# SELECT * for full-row reads). Runs alongside the Go test at
# backend/tests/sqlc_select_list_static_test.go (authoritative parser). See
# scripts/ci/sqlc-select-list-guard.sh.
check-sqlc-select-lists:
	@$(REPO_ROOT)/scripts/ci/sqlc-select-list-guard.sh

# Code generation
sqlc:
	@echo "Generating sqlc code from SQL queries..."
	@cd backend && "$$(go env GOPATH)/bin/sqlc" generate
	@echo "✅ sqlc code generated"

# API specific commands
# swag runs via `go tool` (pinned in backend/go.mod alongside tygo) rather than
# a ~/go/bin binary: CI has no such install, and an unpinned local binary can
# generate a different spec than the one the drift check expects.
api-docs:
	@echo "Generating API documentation..."
	@cd backend && go tool swag init -g cmd/crm-api/main.go --output ./docs
	@echo "✅ API docs generated"

# Non-mutating drift check: generates into a temp dir and diffs against the
# committed backend/docs, so it never touches the tree readers are using.
# Also catches outright generation FAILURE, which is how the spec silently went
# stale for three months — a type swag could not resolve aborted the whole run
# and nothing was watching.
# The temp output dir must be named `docs`: swag derives the generated package
# clause from the directory basename, so any other name diffs on `package X`.
api-docs-check:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	mkdir -p "$$tmp/out" && \
	(cd backend && go tool swag init -g cmd/crm-api/main.go --output "$$tmp/out/docs" >/dev/null) && \
	if ! diff -ru backend/docs "$$tmp/out/docs"; then \
		echo "❌ Generated API docs are stale — run 'make api-docs' and commit"; \
		exit 1; \
	fi && \
	echo "✅ Generated API docs are in sync"

# Generate frontend TypeScript API types from the Go wire structs
# (backend/tygo.yaml). CI + pre-push guard drift via api-types-check.
api-types:
	@echo "Generating frontend API types from Go structs..."
	@cd backend && go tool tygo generate
	@echo "✅ API types generated"

# Non-mutating drift check: generates into a temp dir and diffs against the
# committed files, so it is safe to run concurrently with readers of
# frontend/src/types/generated (the pre-push LINT lane runs alongside the
# FRONTEND test lane).
api-types-check:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	sed "s|\.\./frontend/src/types/generated/|$$tmp/out/|g" backend/tygo.yaml > "$$tmp/tygo.yaml" && \
	mkdir -p "$$tmp/out" && \
	(cd backend && go tool tygo generate --config "$$tmp/tygo.yaml") && \
	if ! diff -ru frontend/src/types/generated "$$tmp/out"; then \
		echo "❌ Generated API types are stale — run 'make api-types' and commit"; \
		exit 1; \
	fi && \
	echo "✅ Generated API types are in sync"

api-build:
	@echo "Building API server..."
	@cd backend && go build -ldflags "$(STAMP_LDFLAGS)" -o bin/crm-api ./cmd/crm-api

api-run: api-build
	@echo "Starting API server..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && ./backend/bin/crm-api

# Environment switching
testing:
	@echo "Switching to TESTING environment (ultra-fast cadences)..."
	@cp .env.example.testing .env
	@echo "✅ Testing environment active:"
	@echo "   - Weekly cadence: 2 minutes"
	@echo "   - Monthly cadence: 10 minutes"
	@echo "   - Quarterly cadence: 30 minutes"
	@echo "   - Reminder scheduler: every 30 seconds"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "Use 'make test-cadence-ultra' to validate all cadences quickly"

staging:
	@echo "Switching to STAGING environment (production cadence durations)..."
	@cp .env.example.staging .env
	@echo "✅ Staging environment active:"
	@echo "   - Weekly cadence: 7 days"
	@echo "   - Monthly cadence: 30 days"
	@echo "   - Quarterly cadence: 90 days"
	@echo "   - Reminder scheduler: every 5 minutes"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "Use 'make accelerated' + 'make test-cadence-fast' for hour-scale cadences"

accelerated:
	@echo "Switching to ACCELERATED environment (fast cadences)..."
	@cp .env.example.accelerated .env
	@echo "✅ Accelerated environment active:"
	@echo "   - Weekly cadence: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly cadence: 1 hour (1 month = 1 hour)"
	@echo "   - Quarterly cadence: 3 hours (1 quarter = 3 hours)"
	@echo "   - Reminder scheduler: every 5 minutes"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "Use 'make test-cadence-fast' to validate cadences in hours"

prod:
	@echo "Switching to PRODUCTION environment (real cadences)..."
	@cp .env.example.production .env
	@echo "✅ Production environment active:"
	@echo "   - Weekly cadence: 7 days"
	@echo "   - Monthly cadence: 30 days"
	@echo "   - Quarterly cadence: 90 days"
	@echo "   - Reminder scheduler: daily at 8 AM"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "⚠️  CAUTION: Real-world timing active"

# Cadence testing commands
test-cadence-ultra:
	@echo "🚀 Starting ULTRA-FAST cadence testing..."
	@echo "This will test all reminder cadences in minutes!"
	@echo ""
	@make testing
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend with ultra-fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && nohup go run ./cmd/crm-api > ../logs/backend-testing.log 2>&1 & echo $$! > ../logs/backend-testing.pid && cd ../.. && bash -c "disown %1" 2>/dev/null || true
	@echo ""
	@echo "⏱️  CADENCE TIMING (ultra-fast):"
	@echo "   - Weekly: 2 minutes"
	@echo "   - Monthly: 10 minutes"
	@echo "   - Quarterly: 30 minutes"
	@echo "   - Reminder scheduler: every 30 seconds"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "📋 Logs: logs/backend-testing.log"
	@echo "💡 Add test contacts with different cadences and watch reminders generate!"
	@echo "💡 Process will continue running after you close this terminal"

test-cadence-fast:
	@echo "🏎️  Starting FAST cadence testing..."
	@echo "This will test all reminder cadences in hours!"
	@echo ""
	@make accelerated
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting backend with fast cadences..."
	@set -a && source ./.env && set +a && export DATABASE_URL="postgres://$${POSTGRES_USER}:$${POSTGRES_PASSWORD}@localhost:$${POSTGRES_PORT:-5432}/$${POSTGRES_DB}?sslmode=disable" && cd backend && nohup go run ./cmd/crm-api > ../logs/backend-accelerated.log 2>&1 & echo $$! > ../logs/backend-accelerated.pid && cd ../.. && bash -c "disown %1" 2>/dev/null || true
	@echo ""
	@echo "⏱️  CADENCE TIMING (fast):"
	@echo "   - Weekly: 10 minutes (1 week = 10 min)"
	@echo "   - Monthly: 1 hour (1 month = 1 hour)"
	@echo "   - Quarterly: 3 hours (1 quarter = 3 hours)"
	@echo "   - Reminder scheduler: every 5 minutes"
	@echo "   - External sync scheduler: every hour"
	@echo ""
	@echo "📋 Logs: logs/backend-accelerated.log"
	@echo "💡 Perfect for validating 3+ months of cadence behavior in 3 hours!"
	@echo "💡 Process will continue running after you close this terminal"

# Clean
clean:
	@echo "Cleaning build artifacts..."
	@cd backend && rm -rf bin/
	@cd frontend && rm -rf .next/ out/

clean-logs:
	@echo "Cleaning log files..."
	@rm -rf logs/*.log logs/*.pid
	@echo "✅ Logs cleaned"

# Docker operations
docker-up:
	@echo "Starting Docker services..."
	@cd infra && docker compose up -d

docker-down:
	@echo "Stopping Docker services..."
	@cd infra && docker compose down

docker-reset:
	@echo "Resetting Docker environment..."
	@cd infra && docker compose down -v
	@cd infra && docker compose up -d

# Production Commands
start:
	@echo "🚀 Starting Personal CRM..."
	@make prod
	@make build
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting CRM backend on port 8080..."
	@bash scripts/start-backend-prod.sh
	@echo "Starting CRM frontend on port 3001..."
	@bash scripts/start-frontend-prod.sh
	@sleep 3
	@echo ""
	@echo "✅ Personal CRM is running!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo "📖 API Docs: http://localhost:8080/swagger/index.html"
	@echo ""
	@echo "📋 Logs:"
	@echo "   Backend:  logs/backend.log"
	@echo "   Frontend: logs/frontend.log"
	@echo ""
	@echo "💡 Processes will continue running after you close this terminal"
	@echo "   Use 'make stop' to stop the CRM"

start-local:
	@echo "🚀 Starting Personal CRM with local production config..."
	@if [ ! -f .env.local ]; then \
		echo "❌ Error: .env.local not found!"; \
		echo ""; \
		echo "Create .env.local with your production secrets first:"; \
		echo "  1. Generate secrets: openssl rand -base64 32"; \
		echo "  2. Copy template: cp .env.example.production .env.local"; \
		echo "  3. Edit .env.local with your secrets"; \
		exit 1; \
	fi
	@echo "📋 Using configuration from .env.local"
	@cp .env.local .env
	@make build
	@make docker-up
	@bash scripts/sync-postgres-auth.sh
	@make logs
	@echo "Starting CRM backend on port 8080..."
	@bash scripts/start-backend-prod.sh
	@echo "Starting CRM frontend on port 3001..."
	@bash scripts/start-frontend-prod.sh
	@sleep 3
	@echo ""
	@echo "✅ Personal CRM is running with local production config!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"
	@echo "📖 API Docs: http://localhost:8080/swagger/index.html"
	@echo ""
	@echo "📋 Logs:"
	@echo "   Backend:  logs/backend.log"
	@echo "   Frontend: logs/frontend.log"
	@echo ""
	@echo "💡 Processes will continue running after you close this terminal"
	@echo "   Use 'make stop' to stop the CRM"

stop:
	@echo "🛑 Stopping Personal CRM..."
	@# Kill backend by port and name (prod uses compiled crm-api binary)
	@lsof -ti:8080 | xargs kill -9 2>/dev/null || true
	@pkill -f crm-api || true
	@# Kill frontend by port (process is 'next-server', not 'next start')
	@lsof -ti:3001 | xargs kill -9 2>/dev/null || true
	@make docker-down
	@echo "✅ Personal CRM stopped"

restart:
	@echo "🔄 Restarting Personal CRM..."
	@make stop
	@sleep 2
	@make start

reload:
	@echo "🔄 Rebuilding and reloading Personal CRM..."
	@echo "Building..."
	@make build
	@echo "Restarting backend..."
	@bash scripts/start-backend-prod.sh
	@echo "Restarting frontend..."
	@bash scripts/start-frontend-prod.sh
	@echo ""
	@echo "✅ Personal CRM reloaded!"
	@echo "🌐 Frontend: http://localhost:3001"
	@echo "🔧 Backend:  http://localhost:8080"

status:
	@echo "📊 Personal CRM Status:"
	@echo ""
	@echo "Backend (port 8080):"
	@curl -s http://localhost:8080/health | jq -r '.status' 2>/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Frontend Dev (port 3000):"
	@curl -s http://localhost:3000 >/dev/null 2>&1 && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Frontend Prod (port 3001):"
	@curl -s http://localhost:3001 >/dev/null 2>&1 && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@echo "Database:"
	@docker ps --filter "name=crm-postgres" --format "table {{.Names}}\t{{.Status}}" | grep crm-postgres >/dev/null && echo "  ✅ Running" || echo "  ❌ Not running"
	@echo ""
	@if [ -d logs ]; then \
		echo "📋 Recent Log Files:"; \
		ls -lh logs/*.log 2>/dev/null | tail -5 || echo "  No log files found"; \
	fi

# Deployment
deploy-mac:
	@./scripts/deploy-mac-daemon.sh

# Promote: fast-forward main to develop's HEAD. The deploy runs on the Pi via
# the self-hosted runner (deploy-prod.yml) when main moves. Same SHA = the
# :<sha> image is already built + CI-green; prod just pulls it. A non-fast-forward
# push is rejected by the remote (no --force) — that rejection IS the ff-only
# guarantee; if it fires, main has diverged and must be investigated, not forced.
# Pre-flight refuses to advance main on a stale local ref or a SHA whose prod
# gates are not already green — see scripts/promote-preflight.sh for why this is
# checked locally when deploy-prod.yml checks it again server-side.
# PROMOTE_SKIP_PREFLIGHT=1 is the deliberate escape hatch.
promote:
	@if [ "$(PROMOTE_SKIP_PREFLIGHT)" = "1" ]; then \
		echo "⚠️  promote pre-flight SKIPPED (PROMOTE_SKIP_PREFLIGHT=1)"; \
	else \
		bash scripts/promote-preflight.sh; \
	fi
	@git push origin develop:main

setup-pi:
	@./scripts/setup-pi.sh

# One-time Mac deploy setup (dedicated clone, reconcile install, timer LaunchAgent).
# Operational wiring (runner registration, deploy.env values, codesign key
# pre-auth) is the runbook's job — see infra/mac-runner-installation-runbook.md.
setup-mac-deploy:
	@./scripts/setup-mac-deploy.sh

# shellcheck shell=bash
# Sourced, not run. The one probe the test-Postgres scripts use to ask whether a
# Docker daemon answers: scripts/e2e-pg.sh (local E2E and the verify skill) and
# scripts/worktree-test-pg.sh (whether the main checkout gets its own cluster).
docker_answers() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}

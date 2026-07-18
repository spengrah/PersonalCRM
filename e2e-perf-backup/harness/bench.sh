#!/usr/bin/env bash
# Rigorous E2E benchmark: MODE=dev|standalone WORKERS=N GREP=<optional> REBUILD=0|1
# Emits phase timings + Playwright JSON metrics + /proc/stat CPU utilization.
set -uo pipefail
MODE="${MODE:-dev}"; WORKERS="${WORKERS:-4}"; GREP="${GREP:-}"; REBUILD="${REBUILD:-1}"
ROOT=/home/dev/workspace/PersonalCRM/.claude/worktrees/agent-a2f9da27bd5bdca1c
SB=/tmp/claude-1000/-home-dev-workspace-PersonalCRM/575e4100-b13f-4965-ab74-e34c66b54fce/scratchpad
TAG="${MODE}-w${WORKERS}${GREP:+-grep}"
JSON="$SB/bench-$TAG.json"; RUNLOG="$SB/bench-$TAG.run.log"
NCORES=$(nproc); HZ=$(getconf CLK_TCK)
# TRUE cpu budget = cgroup quota (this container is CFS-capped below nproc)
CPU_QUOTA=$(awk -v n="$NCORES" '{if($1=="max")print n; else printf "%.2f", $1/$2}' /sys/fs/cgroup/cpu.max 2>/dev/null || echo "$NCORES")
cgstat(){ awk -v k="$1" '$1==k{print $2}' /sys/fs/cgroup/cpu.stat 2>/dev/null; }
cd "$ROOT"
set -a; . "$ROOT/.env.example.testing"; set +a
export DATABASE_URL="postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable"
dur(){ awk "BEGIN{printf \"%.1f\", $2-$1}"; }
echo "########## BENCH mode=$MODE workers=$WORKERS grep='${GREP:-<all>}' ncores=$NCORES ##########"

kill_e2e(){ for p in $(pgrep -f "cmd/crm-api|/crm-api|standalone/server.js|bun run dev|next dev|next-server|playwright test|playwright/lib/common/process" 2>/dev/null | grep -vw $$); do kill -9 "$p" 2>/dev/null; done; }
port_free(){ python3 -c "import socket;s=socket.socket()
try: s.bind(('127.0.0.1',$1)); print('y')
except OSError: print('n')
finally: s.close()"; }
wait_ports(){ for i in $(seq 1 60); do [ "$(port_free 3000)" = y ] && [ "$(port_free 8080)" = y ] && return 0; sleep 1; done; return 1; }

kill_e2e; wait_ports || { echo "PORTS STUCK"; exit 1; }

t=$(date +%s.%N); make e2e-db >"$SB/bench-db.log" 2>&1; DB_S=$(dur $t $(date +%s.%N))
echo "phase db_reset_seconds=$DB_S"

t=$(date +%s.%N)
( cd "$ROOT/backend" && DATABASE_URL="$DATABASE_URL" go run ./cmd/crm-api ) >"$SB/bench-backend.log" 2>&1 &
BE=$!
for i in $(seq 1 120); do [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://localhost:8080/health)" = 200 ] && break; sleep 1; done
BE_S=$(dur $t $(date +%s.%N)); echo "phase backend_start_seconds=$BE_S"
[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://localhost:8080/health)" = 200 ] || { echo "BACKEND FAIL"; tail -20 "$SB/bench-backend.log"; kill -9 $BE; exit 1; }

BUILD_S=0
if [ "$MODE" = standalone ]; then
  [ -f "$ROOT/frontend/.env.local" ] && cp "$ROOT/frontend/.env.local" "$SB/.env.local.save"
  { echo "NEXT_PUBLIC_API_KEY=$API_KEY"; echo "NEXT_PUBLIC_API_URL=http://localhost:8080"; } > "$ROOT/frontend/.env.local"
  if [ "$REBUILD" = 1 ]; then
    t=$(date +%s.%N)
    ( cd "$ROOT/frontend" && bun run build && cp -r .next/static .next/standalone/.next/static && cp -r public .next/standalone/public ) >"$SB/bench-build.log" 2>&1 || { echo "BUILD FAIL"; tail -20 "$SB/bench-build.log"; kill -9 $BE; exit 1; }
    BUILD_S=$(dur $t $(date +%s.%N))
  fi
  echo "phase build_seconds=$BUILD_S"
  t=$(date +%s.%N)
  ( cd "$ROOT/frontend" && PORT=3000 HOSTNAME=127.0.0.1 node .next/standalone/server.js ) >"$SB/bench-frontend.log" 2>&1 &
  FE=$!
else
  echo "phase build_seconds=0 (next dev, no build)"
  t=$(date +%s.%N)
  # PORT=3000 REQUIRED: sourcing .env.example.testing exported PORT=8080 (backend);
  # without this override next dev inherits 8080 and collides with the backend.
  ( cd "$ROOT/frontend" && PORT=3000 NEXT_PUBLIC_API_KEY="$API_KEY" NEXT_PUBLIC_API_URL=http://localhost:8080 bun run dev -- --hostname 127.0.0.1 ) >"$SB/bench-frontend.log" 2>&1 &
  FE=$!
fi
for i in $(seq 1 90); do c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:3000/); [ -n "$c" ] && [ "$c" != 000 ] && break; sleep 1; done
FE_S=$(dur $t $(date +%s.%N)); echo "phase frontend_start_seconds=$FE_S (http $c)"

U0=$(cgstat usage_usec); TH0=$(cgstat throttled_usec); NT0=$(cgstat nr_throttled)
t=$(date +%s.%N)
( cd "$ROOT/frontend" && API_KEY="$API_KEY" NEXT_PUBLIC_API_KEY="$API_KEY" NEXT_PUBLIC_API_URL=http://localhost:8080 \
   E2E_FRONTEND_PORT=3000 E2E_BACKEND_PORT=8080 PLAYWRIGHT_JSON_OUTPUT_NAME="$JSON" \
   ./node_modules/.bin/playwright test --project=chromium --workers=$WORKERS --retries=0 \
   ${GREP:+--grep "$GREP"} --reporter=json,list >"$RUNLOG" 2>"$SB/bench-$TAG.err" )
EXEC_S=$(dur $t $(date +%s.%N))
U1=$(cgstat usage_usec); TH1=$(cgstat throttled_usec); NT1=$(cgstat nr_throttled)
CPU_BUSY_S=$(awk "BEGIN{printf \"%.1f\", ($U1-$U0)/1e6}")
THROTTLED_S=$(awk "BEGIN{printf \"%.1f\", ($TH1-$TH0)/1e6}")
echo "phase exec_wall_seconds=$EXEC_S  container_cpu_seconds=$CPU_BUSY_S  throttled_seconds=$THROTTLED_S  nr_throttled=$((NT1-NT0))  cpu_quota=$CPU_QUOTA"

kill -9 $FE $BE 2>/dev/null; kill_e2e
[ -f "$SB/.env.local.save" ] && mv "$SB/.env.local.save" "$ROOT/frontend/.env.local"
echo "=== SUMMARY tag=$TAG ==="
echo "MODE=$MODE WORKERS=$WORKERS  BUILD=${BUILD_S}s BACKEND=${BE_S}s FE=${FE_S}s DB=${DB_S}s EXEC=${EXEC_S}s"
python3 "$SB/parse_pw.py" "$JSON" "$EXEC_S" "$CPU_BUSY_S" "$CPU_QUOTA" 2>&1
echo "throttled_seconds=$THROTTLED_S over exec (CPU the container wanted but was denied by the ${CPU_QUOTA}-CPU cap)"
echo "BENCH_DONE tag=$TAG"

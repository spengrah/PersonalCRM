#!/usr/bin/env bash
# E2E benchmark for macOS. RUN FROM YOUR PersonalCRM REPO ROOT:
#     bash mac-bench.sh
# Measures: build / server-start / db-reset / test-exec wall-clock, and
# Playwright's own per-test timing (effective concurrency) for each
# (server mode) x (worker count). Paste the RESULTS block back.
#
# Override the matrix if you want a quick first pass:
#     MODES="dev" WORKERS_LIST="4" bash mac-bench.sh
set -uo pipefail
MODES="${MODES:-dev standalone}"
WORKERS_LIST="${WORKERS_LIST:-1 2 4}"
ROOT="$(git rev-parse --show-toplevel)"
OUT="$ROOT/mac-bench-results.txt"
NCORES="$(sysctl -n hw.logicalcpu 2>/dev/null || nproc)"
FE_PORT=3000; BE_PORT=8080
now(){ python3 -c 'import time;print(time.time())'; }
dur(){ python3 -c "print(f'{$2-$1:.1f}')"; }
freeport(){ lsof -ti:"$1" 2>/dev/null | xargs -r kill -9 2>/dev/null; }
: > "$OUT"
echo "host: $(uname -srm)  cores=$NCORES  node=$(node -v)" | tee -a "$OUT"

set -a; . "$ROOT/.env.example.testing"; set +a
export DATABASE_URL="${E2E_DATABASE_URL:-postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable}"

# minimal Playwright JSON summarizer (tests, sum-of-durations, effective concurrency)
PARSE=$(mktemp); cat > "$PARSE" <<'PY'
import json,sys
rep=json.load(open(sys.argv[1])); wall=float(sys.argv[2]); tests=[]
def walk(s):
    for sp in s.get('specs',[]):
        for t in sp.get('tests',[]):
            d=sum(r.get('duration',0) for r in t.get('results',[])); tests.append((d,t.get('status')))
    for c in s.get('suites',[]): walk(c)
for s in rep.get('suites',[]): walk(s)
n=len(tests); summ=sum(d for d,_ in tests)/1000
p=sum(1 for _,st in tests if st=='expected'); fl=sum(1 for _,st in tests if st=='flaky'); f=sum(1 for _,st in tests if st=='unexpected')
print(f"tests={n} pass={p} flaky={fl} fail={f}  sum_test_s={summ:.0f}  eff_concurrency={summ/wall:.2f}")
PY

run(){ MODE=$1; W=$2
  freeport $FE_PORT; freeport $BE_PORT; sleep 1
  make e2e-db >/tmp/mb-db.log 2>&1
  t=$(now); ( cd "$ROOT/backend" && DATABASE_URL="$DATABASE_URL" go run ./cmd/crm-api ) >/tmp/mb-be.log 2>&1 & BE=$!
  for i in $(seq 1 120); do [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://localhost:$BE_PORT/health)" = 200 ] && break; sleep 1; done
  BE_S=$(dur $t $(now))
  BUILD_S=0
  if [ "$MODE" = standalone ]; then
    [ -f "$ROOT/frontend/.env.local" ] && cp "$ROOT/frontend/.env.local" /tmp/mb-envlocal.save
    { echo "NEXT_PUBLIC_API_KEY=$API_KEY"; echo "NEXT_PUBLIC_API_URL=http://localhost:$BE_PORT"; } > "$ROOT/frontend/.env.local"
    t=$(now); ( cd "$ROOT/frontend" && bun run build && cp -r .next/static .next/standalone/.next/static && cp -r public .next/standalone/public ) >/tmp/mb-build.log 2>&1; BUILD_S=$(dur $t $(now))
    t=$(now); ( cd "$ROOT/frontend" && PORT=$FE_PORT HOSTNAME=127.0.0.1 node .next/standalone/server.js ) >/tmp/mb-fe.log 2>&1 & FE=$!
  else
    # PORT=$FE_PORT REQUIRED: .env.example.testing exports PORT=8080 (backend);
    # without this override next dev inherits 8080 and collides with the backend.
    t=$(now); ( cd "$ROOT/frontend" && PORT=$FE_PORT NEXT_PUBLIC_API_KEY="$API_KEY" NEXT_PUBLIC_API_URL=http://localhost:$BE_PORT bun run dev -- --hostname 127.0.0.1 ) >/tmp/mb-fe.log 2>&1 & FE=$!
  fi
  for i in $(seq 1 90); do c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:$FE_PORT/); [ -n "$c" ] && [ "$c" != 000 ] && break; sleep 1; done
  FE_S=$(dur $t $(now))
  JSON=$(mktemp)
  t=$(now)
  ( cd "$ROOT/frontend" && API_KEY="$API_KEY" NEXT_PUBLIC_API_KEY="$API_KEY" NEXT_PUBLIC_API_URL=http://localhost:$BE_PORT \
     E2E_FRONTEND_PORT=$FE_PORT E2E_BACKEND_PORT=$BE_PORT PLAYWRIGHT_JSON_OUTPUT_NAME="$JSON" \
     ./node_modules/.bin/playwright test --project=chromium --workers=$W --retries=0 --reporter=json,list ) >/tmp/mb-run.log 2>&1
  EXEC_S=$(dur $t $(now))
  kill -9 $FE $BE 2>/dev/null; freeport $FE_PORT; freeport $BE_PORT
  [ -f /tmp/mb-envlocal.save ] && mv /tmp/mb-envlocal.save "$ROOT/frontend/.env.local"
  echo "--- MODE=$MODE WORKERS=$W  build=${BUILD_S}s backend=${BE_S}s frontend_start=${FE_S}s exec=${EXEC_S}s ---" | tee -a "$OUT"
  python3 "$PARSE" "$JSON" "$EXEC_S" | tee -a "$OUT"
}

for m in $MODES; do for w in $WORKERS_LIST; do run "$m" "$w"; done; done
echo "MAC_BENCH_COMPLETE -> paste $OUT" | tee -a "$OUT"

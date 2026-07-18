#!/usr/bin/env bash
SB=/tmp/claude-1000/-home-dev-workspace-PersonalCRM/575e4100-b13f-4965-ab74-e34c66b54fce/scratchpad
OUT="$SB/MATRIX2-RESULTS.txt"; : > "$OUT"
run(){ echo "===== $* =====" >>"$OUT"
  MODE=$1 WORKERS=$2 REBUILD=$3 GREP="${4:-}" bash "$SB/bench.sh" 2>/dev/null | grep -E "phase |MODE=|tests=|sum_test|effective_conc|worker_busy|cpu_busy|CPU_UTIL|-> |top areas|^   |longest|BENCH_DONE" >>"$OUT"
  echo "[$(date -u +%H:%M:%S)] done $1 w$2 ${4:-full}"; }
# 1-2: same-hardware server-mode delta on a data-driven subset (fast, reliable)
run dev 4 0 '@area:contacts'
run standalone 4 1 '@area:contacts'
# 3-5: sandbox next-dev full-suite scaling (the real bottleneck picture)
run dev 4 0
run dev 2 0
run dev 1 0
echo "MATRIX2_COMPLETE" >>"$OUT"

#!/usr/bin/env python3
"""Parse a Playwright JSON report into bottleneck metrics.
Usage: parse_pw.py <report.json> <exec_wall_seconds> <cpu_busy_seconds> <ncores>
"""
import json, sys, collections, re

rep = json.load(open(sys.argv[1]))
exec_wall = float(sys.argv[2])
cpu_busy = float(sys.argv[3])
ncores = float(sys.argv[4])  # CPU quota (cgroup cap), not host core count

tests = []  # (file, title, duration_ms, workerIndex, status, ok)
def walk(suite, file_hint=None):
    f = suite.get('file', file_hint)
    for sp in suite.get('specs', []):
        title = sp.get('title')
        for t in sp.get('tests', []):
            # last result is the decisive one
            results = t.get('results', [])
            dur = sum(r.get('duration', 0) for r in results)
            wi = results[-1].get('workerIndex') if results else None
            status = t.get('status')  # expected/unexpected/flaky/skipped
            tests.append((f, title, dur, wi, status))
    for s in suite.get('suites', []):
        walk(s, f)
for s in rep.get('suites', []):
    walk(s)

n = len(tests)
sum_ms = sum(t[2] for t in tests)
passed = sum(1 for t in tests if t[4]=='expected')
flaky  = sum(1 for t in tests if t[4]=='flaky')
failed = sum(1 for t in tests if t[4]=='unexpected')
skipped= sum(1 for t in tests if t[4]=='skipped')

# per-worker busy time
byw = collections.defaultdict(float)
for f,ti,d,wi,st in tests:
    if wi is not None: byw[wi]+=d
workers_used = len(byw)
max_worker = max(byw.values())/1000 if byw else 0
min_worker = min(byw.values())/1000 if byw else 0

# per-file (area) sums, top 12
byf = collections.defaultdict(float)
for f,ti,d,wi,st in tests:
    byf[re.sub(r'.*/tests/e2e/','',f or '?')] += d
top_files = sorted(byf.items(), key=lambda x:-x[1])[:12]

# longest individual tests
longest = sorted(tests, key=lambda x:-x[2])[:12]

eff_conc = sum_ms/1000/exec_wall if exec_wall else 0     # effective concurrency achieved
cpu_util = cpu_busy/(exec_wall*ncores) if exec_wall and ncores else 0  # whole-system CPU utilization

print(f"tests={n} passed={passed} flaky={flaky} failed={failed} skipped={skipped}")
print(f"sum_test_seconds={sum_ms/1000:.1f}  exec_wall_seconds={exec_wall:.1f}")
print(f"effective_concurrency(sum/wall)={eff_conc:.2f}   (workers_used={workers_used})")
print(f"worker_busy_seconds: max={max_worker:.1f} min={min_worker:.1f}  (imbalance={max_worker-min_worker:.1f}s)")
print(f"container_cpu_seconds={cpu_busy:.0f}  cpu_quota={ncores}  CPU_UTIL_vs_quota={cpu_util*100:.0f}%")
print(f"  -> {'CPU-BOUND (saturating the quota)' if cpu_util>0.80 else 'NOT cpu-bound (IO/DB/sleep/server-wait)' } at this worker count")
print("top areas by total test-seconds:")
for f,ms in top_files:
    print(f"   {ms/1000:6.1f}s  {f}")
print("longest individual tests:")
for f,ti,d,wi,st in longest:
    print(f"   {d/1000:6.1f}s  [{st}] {re.sub(r'.*/tests/e2e/','',f or '?')} :: {ti}")

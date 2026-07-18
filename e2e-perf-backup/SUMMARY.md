# E2E performance research — session backup (gh #425)

**Purpose:** durable backup before the user rebuilds the sandbox container to raise its CPU limit 2 → 6. Everything here is on GitHub (branch `chore/e2e-perf-research-backup`) so it survives a container wipe. This branch is a BACKUP — do not merge it.

**Session as of 2026-07-18.** Original session id: `575e4100-b13f-4965-ab74-e34c66b54fce`.

---

## 1. Status of #425 — CODE IS SHIPPED

#425 ("E2E test parallelization, sub-project ii") shipped in three PRs, all merged to `develop`:
- **#682** — resolver modal keyed by candidate id (fixed a real wrong-candidate race; retired `navigateModalToCandidate`).
- **#683** — backend-arbitrated global lock (`TestLockService`) + per-worker scoped reads (retired the cross-worker pollution workaround family).
- **#686** — **cgroup-CPU-quota-aware Playwright worker heuristic** in `frontend/playwright.config.ts` (retired the arm64→1-worker pin). Merge commit was `7c08d24`; develop has since advanced.

CI runs the suite green at 4 workers. The naive "unpin to `os.cpus()` formula" plan was **dropped** — measurement proved it regresses CPU-capped containers.

## 2. THE PENDING ACTION (resume here after the rebuild)

The user is raising the sandbox container CPU cap **2 → 6** (requires container rebuild). After the rebuild:

1. **Verify the new cap:** `cat /sys/fs/cgroup/cpu.max` — expect `600000 100000` (= 6 CPUs). `nproc` will still lie (reports host cores, ~10).
2. **Re-run the instrumented matrix at 6 CPUs** to measure the actual wall-clock + scaling, and decide worker-count tuning. Note: the shipped heuristic maps 6 CPUs → **3 workers** (formula steps ≥4→3, ≥8→4). The open question is whether **4 workers at 6 CPUs** beats 3 — the suite's parallelism ceiling is ~4 (Mac hit 3.69 effective concurrency). If 4 wins, tune the formula threshold in `playwright.config.ts` (e.g. `cpuCount >= 6 ? 4`).
3. **Answer the deferred question** the user asked: what is the real wall-clock at 5–6 CPUs / 3 workers? My arithmetic could not narrow it (two methods gave ~100s vs ~370s because throttling and core-speed are entangled in all 2-CPU data). Measurement resolves it.

### Re-measure procedure (harness is in `harness/`)
The harness lives in `e2e-perf-backup/harness/` on this branch. To run (from repo root, on the commit you want to measure — use `3220ad8` for apples-to-apples with the tables below, or current develop which already has the #686 heuristic):

```bash
# copy harness back to a scratch dir and run a matrix (dev mode, worker sweep):
SB=/tmp/e2e-bench; mkdir -p $SB; cp e2e-perf-backup/harness/* $SB/
# single cell:  MODE=dev|standalone  WORKERS=N  GREP=<optional playwright grep>
MODE=dev WORKERS=3 bash $SB/bench.sh          # full suite, 3 workers, next dev
MODE=dev WORKERS=4 bash $SB/bench.sh          # compare 4 workers at 6 CPUs
# or the driver (edit matrix2.sh cell list first):
bash $SB/matrix2.sh; cat $SB/MATRIX2-RESULTS.txt
```

`bench.sh` prints per-phase timing + `parse_pw.py` metrics: exec wall, effective concurrency, **container CPU used + throttled seconds vs the cgroup quota**. At 6 CPUs, expect throttle to drop sharply vs the 2-CPU numbers below.

## 3. Measured data (all on `develop` @ `3220ad8`, full suite ~180 tests, next dev unless noted)

### Cross-environment wall-clock (exec = test run only)
| Env | CPU | mode | workers | exec wall | note |
|---|---|---|---|---|---|
| Mac | 12 | next dev | 4 | 45.4s | clean |
| Mac | 12 | standalone | 4 | 32.2s (+15.8s build) | clean |
| CI | ~4, 2 shards | standalone | 4×2 | ~42s wall (42s/shard) | clean |
| Sandbox | **2 (capped)** | next dev | 2 | **440s (best)** | 1 fail |
| Sandbox | 2 | next dev | 1 | 486s | 1 fail |
| Sandbox | 2 | next dev | 4 | 562s (worst) | 4 fail |

### Sandbox worker scaling at the 2-CPU cap (the decisive curve)
| workers | exec | container CPU-s | throttled CPU-s | util vs 2-CPU |
|---|---|---|---|---|
| 1 | 486s | 802 | 489 | 83% |
| 2 | **440s** | 866 | 1,254 | 98% |
| 4 | 562s | 1,121 | 3,080 | 100% |

### Mac full-suite scaling (12 CPU, unthrottled — the "shape")
| mode | W1 | W2 | W4 | eff-conc @ W4 |
|---|---|---|---|---|
| next dev | 149.1s | 79.2s | 45.4s | 3.69 |
| standalone | 110.0s | 56.5s | 32.2s | 3.47 |

Full raw sandbox matrix output: `sandbox-matrix-raw.txt`.

## 4. Key conclusions (measured, not assumed)
1. **The suite is CPU-bound** and parallelizes near-linearly to ~4 workers *when CPU is available* (Mac). No parallelism defect in the tests.
2. **The sandbox's 2-CPU cgroup cap was the dominant bottleneck** — ~12× slower than the Mac for identical code/workers, purely from CPU starvation (throttled ≫ used). `cpu.max` is read-only inside the container; only a host rebuild changes it.
3. **Oversubscribing workers past the CPU quota is SLOWER** (4 workers on 2 CPUs = 562s vs 440s at 2). A single worker's tree already draws ~1.65 CPUs. Hence the cgroup-aware heuristic.
4. **`next dev` costs ~29% vs standalone**, but standalone adds a ~16s build, so a single full run is a wash; standalone only wins amortized/repeated. Sandbox standalone runs FAILED in the ad-hoc harness (a sandbox-specific issue; not needed for conclusions — Mac + CI give clean standalone numbers).

## 5. Gotchas baked into the harness (don't re-derive)
- **`PORT=8080` leak:** `.env.example.testing` exports `PORT=8080` (backend). `set -a; . env` leaks it into the `next dev` launch → frontend collides on 8080 and never comes up. The harness overrides `PORT=3000` on the frontend launch (both dev and standalone). If you rewrite the launch, keep that override.
- **Port hygiene:** `kill -9` leaves brief `EADDRINUSE`; the harness polls until ports are *bindable* (python `socket.bind`) before starting servers — `ss`/`lsof` are unreliable in this rootless container. Orphaned process name variants to kill: `next-server`, `next build`, `jest-worker`, `bun run dev`, `standalone/server.js` (not just "next dev").
- **CPU measurement:** use cgroup `cpu.stat` `usage_usec`/`throttled_usec` deltas against `cpu.max`, NOT host `/proc/stat` (which reports all host cores + other tenants). The harness reads `/proc/self/cgroup` to find the right cgroup path.
- Sandbox is native Postgres on :5432 (no docker); `make e2e-db` resets `personal_crm_test`.

## 6. Memory files (backed up in `memory/` in case the memory dir is wiped)
- `sandbox-test-environment.md` — has the 2-CPU-cap UPDATE (2026-07-17). **After the rebuild, update this: cap is now 6, verify `cpu.max`.**
- `measure-dont-assume.md` — the standing directive: on empirical questions, instrument and measure; never present extrapolation as fact. (User feedback after repeated unmeasured claims this session.)

## 7. What NOT to do
- Do not merge this backup branch.
- Do not trust `nproc` for the CPU budget — read `cpu.max`.
- Do not present an estimated wall-clock as fact — the 5-CPU/3-worker estimate is unresolved (~100–370s spread); measure it.
- Do not re-run full-suite **standalone** on the sandbox expecting it to pass — it fails in the harness (unresolved, non-blocking).

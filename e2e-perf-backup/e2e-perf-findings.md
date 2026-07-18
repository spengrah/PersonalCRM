# E2E performance findings (gh #425) — measured, not assumed

All runs on `develop` @ `3220ad8`, full Playwright suite (~180 tests, chromium), `--retries=0`, identical bench harness. Instrumentation: Playwright JSON reporter (per-test/per-worker timing) + cgroup `cpu.stat` (container CPU used + throttled) measured against the real `cpu.max` quota.

## Cross-environment wall-clock (exec = test run, excludes build/setup)

| Env | CPU budget | mode | workers | exec wall | result |
|---|---|---|---|---|---|
| Mac | 12 | next dev | 4 | 45.4s | clean |
| Mac | 12 | standalone | 4 | 32.2s (+15.8s build) | clean |
| CI (GitHub) | ~4, 2 shards | standalone | 4×2 | ~42s wall (42s/shard, parallel) | clean |
| Sandbox | **2 (cgroup-capped)** | next dev | 2 (best) | **440s** | 1 fail |
| Sandbox | 2 | next dev | 1 | 486s | 1 fail |
| Sandbox | 2 | next dev | 4 | **562s (worst)** | 4 fail |

Identical suite/code/workers: **sandbox is ~12× slower than the Mac** (562s vs 45s at W4), entirely due to the 2-CPU cap.

## Sandbox worker scaling at the 2-CPU cap (the decisive curve)

| workers | exec | throttled CPU-s | CPU util vs 2-CPU quota |
|---|---|---|---|
| 1 | 486s | 489 | 83% |
| 2 | **440s (optimal)** | 1,254 | 98% |
| 4 | 562s | 3,080 | 100% |

- **4 workers is the *slowest*** — oversubscribing 4 workers onto 2 CPUs adds more throttle/context-switch cost than parallelism buys (28% slower than 2 workers).
- Parallelism barely helps at all (W1→W2 only −10%) because a *single* worker's tree (Playwright + browser + `next dev` compile + backend) already draws ~1.65 CPUs, so 2 workers saturate the cap.

## Conclusions

1. **The bottleneck is CPU, and the sandbox is starved.** At the best config (W2) it is throttled 59% — denied 1,254 of the 2,119 CPU-seconds it wanted. The suite itself is healthy: it parallelizes near-linearly to ~4 effective workers when CPU is available (Mac eff-concurrency 3.69).
2. **`next dev` costs ~29% vs standalone** (steady across worker counts), but standalone adds a ~16s build, so a single full run is a wash; standalone wins only across repeated runs or the full suite amortized. For the pre-push *diff subset* (one-shot, few tests), `next dev` wins.
3. **The original PR3 plan was wrong.** "Unpin → `os.cpus().length` formula" reads `nproc`=10 on the sandbox → picks 4 workers → 562s, the worst case. The arm64→1-worker pin it would remove is conservative-but-near-optimal (1 worker 486s vs optimal 2 worker 440s). Naive unpinning *regresses* constrained environments.

## Recommendations

1. **Infra (the real win): raise the sandbox container's CPU allocation 2 → 4.** Expected ~2× faster E2E (toward ~220–250s) since the suite scales to ~4 workers; beyond ~4–6 CPUs gains taper (suite parallelism ceiling ~4). This is a host/VPS change — `cpu.max` is read-only inside the container. **Re-measure after the bump to confirm the ~2× before finalizing worker logic.**
2. **Code (only worthwhile *after* the CPU bump):** replace the arm64-linux worker proxy with a **cgroup-CPU-quota-aware** heuristic (read `cpu.max`; fall back to `os.cpus()` where absent, e.g. macOS). At 2 CPUs it picks 2 (440s, not 562s); at 4 CPUs it picks 4; CI still overrides via `PLAYWRIGHT_WORKERS`. Its correct shape depends on the post-bump reality, so sequence it after infra.
3. **Flake elimination (the substance of #425) is already shipped** — PR1 #682 (modal-by-id) + PR2 #683 (backend global lock + scoped reads), CI green at 4 workers. Sandbox's residual 1–4 failures are CPU-starvation timeouts, not the pollution class; they should vanish once CPU is adequate.

# Decision models for ball-in-court: session notes

Working notes from one session (2026-10-04 to 2026-10-12): a VPS prefill benchmark of Cloudflare's Clef-Flash, research on ways to run decision models, PII masking, the start of the ball-in-court eval spec, and a dev-workflow prerequisites interlude. Facts marked "reported" come from secondary sources; everything else was read at a primary source or measured.

## Summary

- **The VPS CPU cannot run Clef-Flash usefully.** Prefill measured 8.08 tokens/s at 4 threads (pp512), so a 2,048-token thread takes about 4.3 minutes. The GGUF also lacks Clef's separate decision head.
- **Clef-Flash on Cloudflare Workers AI is the leading production candidate.** It costs $0.09 per million input tokens (about $0.53/month at the expected volume), has a 38.8 ms median latency, and Cloudflare's data-usage policy says it neither trains on nor stores request content.
- **Threads would be masked before any hosted call.** Speaker roles, regexes for formatted identifiers, and the CRM's own contact names as a dictionary remove most identifiers at little expected accuracy cost for ball-in-court.
- **A reusable ball-in-court eval is being specced** through explore-and-spec. The priorities are confirmed; two shape rulings are open (see [Ball-in-court eval spec](#ball-in-court-eval-spec-in-progress)).
- **Decision models return probabilities, not reasons.** That conflicts with the extraction program's "visible why from day one" ruling and must be settled in the ball-in-court slice spec.

## VPS prefill benchmark

The goal was to measure prefill (prompt processing) speed for Clef-Flash at 4-bit on the shared VPS, since Clef runs one forward pass and never generates text.

### Hardware

| Property | Value |
|---|---|
| CPU | 10 × Neoverse-N1 r3p1, 1 thread per core |
| CPU features | asimd, asimddp (DOTPROD), asimdhp; no i8mm, no SVE |
| Memory | 15 GiB total, about 9.5 GiB available; 8 GiB swap |
| OS | Debian 12, kernel 6.1.0-49-arm64, glibc 2.36 |
| llama.cpp backend loaded | `libggml-cpu-armv8.2_2.so` |

### Blockers and workarounds

- The llama.cpp release binaries (build `b11401`, `llama-b11401-bin-ubuntu-arm64.tar.gz`) need glibc 2.38 and `GLIBCXX_3.4.32` (an Ubuntu 24.04 userland) plus `libgomp.so.1`. None is on the VPS, so the binaries do not run.
- The release tarball has no `llama-embedding` binary.
- The repo's "latest release" API points at an unrelated `v0.5.0` tag with no binaries; numbered builds (`bNNNNN`) carry the assets.
- No compiler toolchain is installed (no gcc, g++, make or cmake), so a source build needs package installs.
- The workaround was the official `ghcr.io/ggml-org/llama.cpp:full` image (arm64 variant exists) under rootless Podman with `podman run --rm`.
- Claude Code's auto-mode classifier blocked `podman pull` and `podman run` on the shared VPS ("Modify Shared Resources"). The user ran the pull and launched a prepared script (`run.sh`) that waited for load below 2, ran the sweep, and logged load every 30 s.
- The image has no `/usr/bin/time`, so the script polled the process's `VmHWM` for peak RSS.

### Results

Command: `llama-bench -m Cloudflare_clef-flash-IQ4_XS.gguf -p 512,2048,4096 -n 0 -r 3 -t 4,8,10`, model `bartowski/Cloudflare_clef-flash-GGUF` IQ4_XS (4.86 GiB, 8.95 B params, reported as `qwen35 9B IQ4_XS - 4.25 bpw`).

| Threads | Test | Prefill (tokens/s) |
|---|---|---|
| 4 | pp512 | 8.08 ± 0.05 |
| 4 | pp2048 | 7.97 ± 0.03 |

The run was stopped at the user's request before the pp4096, 8-thread and 10-thread rows, so peak RSS and the step-5 per-token embedding check were never recorded. Load was 1.42 at the start and 5–7 during the run, partly from the benchmark and partly from an active dev-sandbox session. Everything was cleaned up: `~/clef-bench` deleted, the container removed by `--rm`, and the image removed with `podman rmi`.

Process lesson: `pkill -f <pattern>` over SSH matched its own SSH command line and killed the session; kill by PID instead.

## How the benchmark relates to ball-in-court

The benchmark measures the worst case: a cold read of the whole prompt with no reuse. Ball-in-court is an event-triggered background job (run when a message syncs), so latency matters little and daily throughput matters most.

- Real inputs are probably a few hundred tokens for chats; email with quoted history may exceed 2,048. Real thread sizes were not measured.
- Rough throughput: 200 thread updates/day × 600 tokens = 60,000 tokens/day, about 2 hours of 4 busy cores at 8 tokens/s.
- Backfill and eval runs are the expensive part: 200 labeled threads × 600 tokens is about 4 hours at 4 threads.
- Prefix reuse could cut steady-state cost if the head only needs some positions, but Clef's head reads every position (see below), so reuse would also need cached hidden states.
- The current awaiting-reply rule is a timestamp comparison (last outreach strictly later than last response). Ball-in-court corrects cases such as "thanks, sounds good!" closing a loop.

## Research findings

Three Sonnet web-research agents ran in parallel (hosted APIs, self-hosting, rented GPUs), followed by an open-weights check.

### Hosted decision APIs

| Option | Price (input) | Monthly cost estimate | Retention | Notes |
|---|---|---|---|---|
| Clef-Flash on Workers AI (`@cf/cloudflare/clef-flash`) | $0.09/M | ~$0.53 | No training; stored only if you use R2/KV (verified) | 38.8 ms median latency; 65,536-token context; 1–64 typed questions per call |
| Clef on Workers AI (`@cf/cloudflare/clef`) | $0.24/M | ~$1.40 | Same | 209.3 ms median latency |
| Jev (TypeSafe AI) | $0.042/M | ~$0.25 | Not found | Closed weights; via Vercel AI Gateway; direct API waitlisted (reported) |
| OpenAI Decisions API | Not published | Unknown | Unknown | Announced at DevDay 2026-09-29; limited preview; GPT-6 Luna based (all reported) |
| Together Tev1-4B | $0.042/M (reported) | ~$0.25 | Not checked | Picks a lettered option via chat completions; normal LM head |

Monthly estimates assume 200 calls/day × 800 tokens plus one 300-thread × 800-token eval run a week (about 5.84 M tokens/month). Question text and criteria also count as input tokens.

The Jev-style interface, shared by Clef and Kev: send the thread as `state` plus typed questions (`noul` for a yes/no probability, `choice` with per-option criteria, `score`), and receive a probability per option. No hosted API lets you bring your own head; Clef fine-tuning is a hands-on Cloudflare service.

### Self-hosting

- The Clef-Flash release on Hugging Face is a BF16 backbone (about 19 GB) plus `joint_head.safetensors` (244 MB), `joint_head_config.json` and `joint_schema_model.py`. The head is a small transformer that reads the final hidden states of all positions and scores all options of all questions jointly; the backbone is post-trained (rank-256 adapters).
- Documented runtimes are Transformers, vLLM and SGLang. A GGUF route would need the head run separately in Python, and the 4-bit quant shifts the hidden states the head was trained on by an unmeasured amount.
- IQ4_XS is probably a poor CPU prefill choice on ARM: llama.cpp's repacked ARM kernels cover Q4_0 (and IQ4_NL), not IQ4_XS (inference; no direct benchmark found).
- Reference CPU point: Graviton4 runs a 4.6 B model at Q4_0 at 106 tokens/s (pp128, 4 threads), roughly 50 tokens/s scaled to 9 B (inference).
- Apple Silicon: base M1–M4 chips do 118–221 tokens/s pp512 on Llama 7B Q4_0 via Metal, so a 9 B 4-bit model should reach roughly 90–170 tokens/s, 10–20× the VPS (inference).
- vLLM supports prefix caching for Qwen3.5's hybrid layers (`--mamba-cache-mode align`, reported); llama.cpp uses periodic checkpoints for recurrent models.

### Rented GPUs

| Option | Rate | Monthly estimate | Notes |
|---|---|---|---|
| Modal, L4 (24 GB) | ~$0.80/h (reported) | ~$1.50, inside the $30/month free credit | Bring your own container; GPU memory snapshots cut cold start to ~5 s |
| RunPod Serverless Flex, 24 GB | ~$0.69/h (reported) | ~$1.30 | Bills from worker start including model load |
| Hugging Face Inference Endpoints, L4 | $0.80/h | ~$1.50 | Billed per minute |
| Hetzner GEX44 (always on) | €184/month + €79 setup (reported) | €184 | RTX 4000 SFF Ada, 20 GB |
| Vast.ai RTX 3090 (always on) | $0.11–0.25/h (reported) | $110–180 | Third-party hosts |

GPU estimates assume about 2,500 tokens/s prefill on an L4 (the researcher's estimate, not a benchmark). The one-time backfill (about 16 M tokens) costs about $1.40 on either an L4 or Workers AI. Fly.io GPUs were deprecated on 2026-07-31 (reported).

## Decision-model landscape

OpenRouter's decision-model ranking measures request volume, not quality (data through 2026-10-06). Jev handles about 80× the requests of the next model.

| Rank | Model | Maker | Requests | Open weights | License | Size / base | GGUF / MLX |
|---|---|---|---|---|---|---|---|
| 1 | Jev 1.13 | TypeSafe | 982 M | No (reported) | Proprietary | Undisclosed | No |
| 2 | Span-01 Lite | Respan | 12.4 M | Probably no | Not stated | Undisclosed | No |
| 3 | d1 | Liquid | 2.87 M | No (reported) | Proprietary | Undisclosed | No |
| 4 | Kev 4B | Jared Palmer | 2.43 M | Yes | Apache 2.0 | LoRA on Qwen3.5-4B-Base | Both |
| 5 | Clef Flash | Cloudflare | 2.38 M | Yes | Apache 2.0 | ~9.4 B, Qwen3.5-9B | Both |
| 6 | Clef | Cloudflare | 1.8 M | Yes | Apache 2.0 | ~27.4 B, Qwen3.8-27B | Both |
| 7 | Decider V1 27B | Perplexity | 1.27 M | Yes | Apache 2.0 | ~26.1 B, Qwen3.8-27B | Neither (EXL3 only) |
| 8 | Solar Decide | Upstage | 1.21 M | Unknown | Unknown | Solar Mini 4 (reported) | No |
| 9 | Tev1 4B Experimental | Together | 994 K | Downloadable | Unresolved ("being finalized") | ~4.7 B, Qwen3.5-4B | Both |
| 10 | Span-01 | Respan | 961 K | Probably no | Not stated | Undisclosed | No |

Notes for the eval:

- Kev also ships 0.5 B, 0.8 B, 9 B and 27 B sizes, supports MLX, and uses the Jev SDK interface.
- Clef (27 B) fits on the 64 GB Mac via `mlx-community/clef-4bit` (~15 GB) or `-8bit` (~28 GB). Decider V1 27B fits only in BF16 (~52 GB).
- Community GGUF and MLX builds may carry only the backbone; check whether they include the decision head before relying on them.
- Laya (421 M, ModernBERT-based, reported 193–464 ms per question on CPU) appeared in the self-hosting research but not in the ranking.
- None of the "hosted-only" verdicts comes from a vendor source.

## PII masking

Masking identifiers before a hosted call is tractable, and ball-in-court is close to the best case: the decision depends on who spoke last and the message's act (question, request, promise, closer), not on who the people are.

What masking covers:

1. **Structure:** send no names, handles, phone numbers, emails or contact IDs. Label speakers `ME`, `THEM`, and `PERSON_2`… for groups; send relative times ("+3h"). Clef accepts structured JSON state.
2. **Formatted identifiers:** regexes on the Pi for emails, phone numbers, URLs, account and card numbers, street addresses.
3. **Names in message text:** the CRM's contact names and nicknames as a dictionary, with a local named-entity recognizer (Presidio, GLiNER PII models) as a backstop.
4. **Consistent placeholders within a thread,** so references still resolve.

The response is only per-option probabilities, so nothing needs unmasking and the placeholder map never leaves the Pi.

What masking cannot cover: sensitive topics (health, money, relationships) survive masking, though no longer linked to a person. If that exposure is unacceptable, inference must run on hardware the user owns (the Mac).

Expected accuracy impact (reasoning, not measured): role labels are neutral or helpful; bracketed tags like `[PERSON_1]` are a mild distribution shift, so realistic stand-in names are worth testing; entity-dependent cases lose some signal; over-eager recognizers that mask "Thursday" would remove closure cues, so weekday and relative-time words stay.

Measuring the cost without leaking: run Clef-Flash on the Mac over the labeled corpus raw and masked; the difference is the masking cost. Only masked threads go to Cloudflare.

## Ball-in-court eval spec (in progress)

This is an explore-and-spec session for the eval section and likely first slice of the ball-in-court extractor under the extraction program (#379, `.agents/spec/llm-extraction-program.md`). Phase 3 (the summary gate) is confirmed; Phase 4 (shape) awaits two rulings. The full spec has not been written.

### Rulings so far

- Labels are three classes: **I owe**, **they owe**, **nobody** (thread closed), plus a way to mark threads that fit none.
- Raw-content methods run on the user's Mac (64 GB RAM).
- The user will label as many threads as needed (roughly 250, stratified toward cases the current rule likely gets wrong).
- "Reuse" means a scorecard for assessing future methods, rules, prompts, masking and models on this workload, not a merge gate.
- Live corrections and live confirmations in the CRM both become eval material.
- Missed obligations (false negatives on "I owe") are worse than false nags.
- A thread moving on makes the live verdict stale, but the frozen snapshot's label stays valid; a label later judged wrong for the same snapshot is a different case (history vs overwrite still open).
- Retention terms and privacy preservation are a pass/fail criterion for any vendor.
- Standard eval output is content-free; agents may read thread content ad hoc only with the user's approval.
- Overfitting to the corpus is not a concern for now (small output space; new live judgments act as unseen examples).
- The harness should be small and share infrastructure with later extractor evals.
- No persistent tests of the harness itself (throwaway tests are fine); the interactions surface and labeling UX are excluded from that rule.
- The interactions surface (`.agents/spec/contact-interactions-surface.md`) ships first.
- Candidate methods: current rule (baseline), Clef-Flash raw on the Mac, Clef-Flash masked on the Mac, Clef-Flash masked on Workers AI, Jev masked, a generative LLM via Venice, plus optional Kev and masking-style variants.

### Confirmed priorities, in order

1. Missed obligations are the worst outcome; recall of "I owe" is the headline metric.
2. Privacy: no real content at rest off the Pi, masking before hosted calls, verified vendor retention terms, content-free standard output.
3. A reusable, per-example comparison of methods (which examples flipped, in which direction), not only aggregates.
4. One act of judgment in the CRM: labeling, confirming and correcting all become eval material, tied to frozen snapshots.
5. Small, and shared with later extractors.
6. Secondary: cost, latency, calibration, masking cost.

Out of scope: the interactions surface itself, the production extractor (pipeline, auto-apply, the "why" display), choosing the method in advance, persistent harness tests, overfitting machinery, Langfuse datasets and annotation queues.

### Shapes proposed (awaiting rulings)

**Decision 1: where judgments live.**

- **Shape A (recommended):** judgments are SP1 assertions. A live verdict is a `proposed` assertion with provenance (`producer_kind`, version, `input_hash`); confirming accepts it; correcting rejects it and adds a `user` assertion. A new snapshot table holds frozen thread content keyed by `input_hash`. Serves priorities 4 and 5 with one writer; supersession gives label history for free. Costs: a thread needs a node kind (or verdicts hang off the contact node, which breaks for groups), the KNW-033 lift comes forward, and bitemporal semantics are heavier than labels need.
- **Shape B:** a dedicated eval store (snapshots + judgments with a source of corpus, confirmation or correction). Smaller, but once live verdicts are assertions it creates two writers for one fact.

**Decision 2: runner and methods (single proposal).**

- A command run from a git checkout on the Mac pulls snapshots from the Pi at run time and keeps no copies.
- A method = masking variant (or none) + adapter + endpoint + model + question or prompt config, versioned by git SHA and config hash.
- Three adapters: SystemOne format (Clef local, Workers AI, Jev via Vercel, Kev), OpenAI-style chat (Venice), and the in-process current rule (last message from them → I owe; from me → they owe; never "nobody").
- The masked methods use the same masker as production, which argues for writing the runner in Go beside the backend.
- Per-example predictions go to Langfuse keyed by example ID; the terminal report is content-free (recall/precision of "I owe", confusion matrix, calibration, flips against a baseline run); "unclear" examples are counted, not scored.
- A separate `show <example-id>` command prints content, gated by a Claude Code ask-permission rule.

### Open rulings

1. Shape A or Shape B.
2. Whether the masker counts as production code with tests in the suite, while scorers, metrics and the runner get throwaway tests only.

Langfuse's role follows the 2026-07-11 observability decision (`.agents/spec/2026-07-11-llm-observability-platform-spike-decision.md`): a separate `extraction` project, content-free traces, scores keyed by example ID, live precision via `recordVerdict`, nothing load-bearing for correctness.

## Interlude: dev-workflow 0.36.1 prerequisites

Merged as PR #883 (squash `00b122c3`, "chore: arc node CI, interactions arc engines, drop CLAUDE.md symlinks"), with no merge-gate review at the user's direction.

- **CI on arc PRs:** `.github/workflows/ci.yml` `pull_request.branches` is now `[ main, develop, 'arc/**' ]`. No branch protection or required check changed.
- **Engines block:** `.agents/spec/contact-interactions-surface.md` gained `## Engines` with `{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }` and a line preferring no cheaper engine unless it is the best fit, and Opus 5.5 over Fable.
- **CLAUDE.md removal:** the `CLAUDE.md` symlinks (root `.claude/`, `backend/`, `frontend/`, `mac-daemon/`) and the `.gitattributes` export-ignore line were deleted; code comments citing CLAUDE.md now cite `AGENTS.md`, `core.md` rules, or the backend troubleshooting guide.
- **Branch rule:** `core.md` now allows branch names prescribed by the workflow or tool creating the branch, keeping the repo rules agnostic of spec-and-ship.
- **Arc branch prefix:** `arc/<feature>/…` is a spec-and-ship skill convention, not enforced by runner code.
- **Report-only checks:** the coverage gate maps spec items but has no notion of acceptance criteria; trunk is named indirectly (`AGENTS.md` → `core.md`, and GitHub's default branch is `develop`); no in-flight plan used the old `tests` element shape or a stale bindings file.
- **Engine catalog** (machine-wide, outside the repo): Claude `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-fable-5-1`; Codex `gpt-6.1-sol`, `gpt-6-astra`, `gpt-6-luna` on Codex account a; only luna uses priority processing when attended; efforts low through max for both families (ultra excluded). Codex account b is retired.

## Open items

- [ ] Rule on Shape A vs B and on the masker test line, then write the eval spec (Phase 5).
- [ ] Settle the "why" question (decision models return probabilities only) in the ball-in-court slice spec.
- [ ] Verify Jev's (TypeSafe) retention and zero-data-retention terms at the source.
- [ ] Confirm whether community GGUF/MLX builds of Clef include the decision head.
- [ ] Measure real thread sizes from the corpus.
- [ ] Decide whether to clean up the codex-companion failover tool, which still tries to switch to account b, and the sandbox's logged-in b slot.

## Sources

- [Cloudflare: Clef decision models launch blog](https://blog.cloudflare.com/clef-decision-models/)
- [Workers AI: clef-flash model page](https://developers.cloudflare.com/workers-ai/models/clef-flash/)
- [Workers AI pricing](https://developers.cloudflare.com/workers-ai/platform/pricing/)
- [Workers AI data usage](https://developers.cloudflare.com/workers-ai/platform/data-usage/)
- [Hugging Face: Cloudflare/clef-flash](https://huggingface.co/Cloudflare/clef-flash)
- [Hugging Face: Cloudflare/clef](https://huggingface.co/Cloudflare/clef)
- [Hugging Face: perplexity-ai/pplx-decider-v1-27b](https://huggingface.co/perplexity-ai/pplx-decider-v1-27b)
- [Hugging Face: togethercomputer/Tev1-4B-experimental](https://huggingface.co/togethercomputer/Tev1-4B-experimental)
- [GitHub: jaredpalmer/kev](https://github.com/jaredpalmer/kev)
- [TypeSafe AI (Jev)](https://typesafe.ai)
- [Vercel AI Gateway: TypeSafe](https://vercel.com/docs/ai-gateway/sdks-and-apis/typesafe)
- [Simon Willison on Jev](https://simonwillison.net/2026/Sep/21/jev/)
- [OpenRouter decision-model rankings](https://openrouter.ai/rankings/decisions)
- [Modal pricing](https://modal.com/pricing) · [Modal GPU memory snapshots](https://modal.com/blog/gpu-mem-snapshots)
- [RunPod serverless pricing](https://docs.runpod.io/serverless/pricing)
- [Hugging Face Inference Endpoints pricing](https://huggingface.co/docs/inference-endpoints/pricing)
- [Arm: AFM-4.5B on Graviton4 benchmarks](https://learn.arm.com/learning-paths/servers-and-cloud-computing/arcee-foundation-model-on-aws/07_evaluating_the_quantized_models/)
- [llama.cpp Apple Silicon benchmarks (discussion 4167)](https://github.com/ggml-org/llama.cpp/discussions/4167)
- Secondary (reported): [eesel on the OpenAI Decisions API](https://www.eesel.ai/blog/openai-decisions-api-pricing), [datanorth on Liquid d1](https://datanorth.ai/news/liquid-ai-releases-d1), [Respan Span-01 docs](https://respan.ai/docs/documentation/span-01/concept), [LMCache Qwen3.5 recipe](https://docs.lmcache.ai/recipes/qwen3_5.html)

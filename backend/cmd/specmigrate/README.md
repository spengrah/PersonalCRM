# specmigrate — one-shot spec citation key migration (GH #760)

This is a throwaway artifact of the spec-citation-by-key arc's PR2. It ran once, against one corpus, to key the 393 cited-or-waived then-items and rewrite the 616 positional references that addressed them. It is wired into **nothing** — no CI job, no git hook, no pre-push phase — and exactly one `make` target: `test-unit`'s package list, which runs `slug_test.go`. **PR3 deletes this directory and that Makefile entry.** It is committed only so the migration's correctness proof is reproducible by a reviewer instead of resting on pasted numbers.

Re-running `keys` on a migrated corpus is a byte-level no-op: keys are permanent (arc §3.6) and the tool only ever keys items that lack one.

## Subcommands

| Command | What it does |
|---|---|
| `keys -out <dir> [--write] <root>` | Pass A. Mints a key for every cited-or-waived unkeyed then-item and rewrites `      - <text>` into `      - key: <slug>` + `        text: <text>`. Writes `slug-table.txt` (all rows) and `slug-review.txt` (the flagged weak-slug subset). |
| `cite -out <dir> [--write] <root>` | Pass B. Re-reads the **final** corpus, builds one `(ID, index) → key` map, and rewrites the waiver lines, the marker lines in the two scan roots, and the out-of-root markers. Writes `substitutions.txt`, the 616-row ledger. |
| `g1 <base-ref> <root>` | Map faithfulness over all 616 references. `old_index` comes from a materialized checkout of `<base-ref>`, never from the ledger — the ledger is the thing under test. |
| `g2 [-subs <f>] <base> <tool> <passB> <head> <root>` | Diff shape: G2a (scan roots + substitution exactness), G2b (path allowlist + production-untouched), G2c (corpus shape + inverse-transform identity), plus three-way commit disjointness. Four refs because the branch has four boundaries and each clause needs a different pair. |
| `g3 <base-ref> <root>` | Corpus text preservation: then texts, given/when/statement, metadata, and waiver reasons identical before and after. |

Exit codes are meaningful: `0` ok, `1` a violation was found, `2` an operational error.

**Read them from a BUILT BINARY, not from `go run`.** Measured on go1.25: `go run` collapses any non-zero program exit to **1**, so the 1-vs-2 distinction — the one that separates "the migration is not faithful" from "the tool refused to run" — is invisible through it. `make` is worse, normalizing any recipe failure to 2. Build once and invoke the binary:

```
go build -o /tmp/specmigrate ./cmd/specmigrate
/tmp/specmigrate g1 <base> ..; echo $?
```

Never read an exit code through a pipe either — the status is the last stage's.

## Fail-closed rules

- `--write` is opt-in; the default is a dry run that prints the plan and writes only the `-out` artifacts.
- `--write` refuses to run on a dirty tree, which is what makes `git checkout --` an unambiguous rollback for either pass.
- `-out` is required and must resolve **outside** the repository, so no artifact can become an untracked file that breaks the guards' `git status --short` checks.
- Both passes validate the whole corpus before writing any file — a failure on file 7 of 12 must not leave 6 rewritten files on disk.
- `cite` aborts before its first byte if the key map has no entry for a reference it must rewrite, naming every unresolvable one.

## Two passes, and why they are not fused

Keys are permanent. Pass A writes them; a human reviews and hand-edits the weak ones; Pass B then re-reads the final corpus and rewrites the references from it. Fusing the passes would bake 393 unreviewed names into 616 references. The state between the passes is deliberately a valid, fully green tree: keyed items with index-form waivers and index-form citations are all forms the parser accepts.

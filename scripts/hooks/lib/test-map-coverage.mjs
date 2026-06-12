// test-map-coverage.mjs — the matcher behind scripts/hooks/test-map-coverage-check.sh.
//
// Asserts that every E2E spec file passed on argv is matched by at least one
// `pattern` in test-map.json. The matching uses the SAME `new RegExp(pattern).test(file)`
// construction that scripts/run-e2e-local.mjs uses, so the guard's "is this spec
// selected?" answer is byte-identical to what the diff-selector would compute (no
// bash/grep -E vs JS RegExp semantics drift).
//
// Usage:   node test-map-coverage.mjs <map-path> <spec-path...>
// Stdout:  one unmatched spec path per line (the drift offenders).
// Stderr:  the error message on an internal failure.
// Exit:    0 = every spec matched; 1 = >=1 unmatched spec; 2 = internal error
//          (fail-closed: an unreadable/unparseable map, a pattern that throws in
//          new RegExp(), or an empty spec list all exit 2 so the guard blocks the
//          push rather than silently passing on an empty offender list).
import { readFileSync } from 'node:fs'

function main(argv) {
  const [mapPath, ...specPaths] = argv
  if (!mapPath) {
    throw new Error('usage: test-map-coverage.mjs <map-path> <spec-path...>')
  }
  if (specPaths.length === 0) {
    throw new Error('no spec files provided — refusing to pass on an empty spec list')
  }

  const mapping = JSON.parse(readFileSync(mapPath, 'utf8'))
  if (!Array.isArray(mapping)) {
    throw new Error(`${mapPath} did not parse to a JSON array`)
  }

  // Compile every pattern up front so a bad regex fails the run (exit 2) rather
  // than silently never matching (which would look like a drift offender, exit 1).
  const regexes = mapping.map(rule => new RegExp(rule.pattern))

  const offenders = specPaths.filter(spec => !regexes.some(re => re.test(spec)))

  for (const spec of offenders) {
    console.log(spec)
  }
  return offenders.length > 0 ? 1 : 0
}

try {
  process.exit(main(process.argv.slice(2)))
} catch (err) {
  console.error(`test-map-coverage: ${err.message}`)
  process.exit(2)
}

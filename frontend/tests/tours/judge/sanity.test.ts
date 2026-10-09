import { afterEach, expect, it } from 'vitest'
import { spawnSync } from 'child_process'
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from 'fs'
import path from 'path'
import { allIntents } from './intent-catalog'

const scratch = path.resolve(__dirname, '../.runs')
const roots: string[] = []
afterEach(() => roots.splice(0).forEach(root => rmSync(root, { recursive: true, force: true })))

function fixture(options: { verdict?: string; toursFail?: boolean; retired?: boolean } = {}) {
  mkdirSync(scratch, { recursive: true })
  const root = mkdtempSync(path.join(scratch, 'sanity-test-'))
  roots.push(root)
  const judge = path.join(root, 'frontend/tests/tours/judge')
  cpSync(__dirname, judge, { recursive: true, filter: source => !source.endsWith('.test.ts') })
  if (options.retired) {
    writeFileSync(
      path.join(judge, 'intent-catalog.ts'),
      "\nINTENT_CATALOG['DSH-011'].status = 'retired'\n",
      { flag: 'a' }
    )
  }
  const run = path.join(root, 'run')
  const calls = path.join(root, 'calls.jsonl')
  const capture = {
    tour: 'dashboard',
    seq: 1,
    behaviors: ['DSH-010', 'DSH-011', 'DSH-012'],
    note: 'fresh evidence',
    url: 'http://example.invalid',
    aria: { role: 'root', children: [] },
    apiResponses: {},
  }
  const writeCaptures = (dir: string, note: string) => {
    mkdirSync(path.join(dir, 'captures'), { recursive: true })
    writeFileSync(path.join(dir, 'captures/001.json'), JSON.stringify({ ...capture, note }))
  }
  writeCaptures(run, 'fresh evidence')
  writeCaptures(path.join(root, 'frontend/tests/tours/.runs/20000101T000000Z'), 'older evidence')
  mkdirSync(path.join(root, 'scripts'), { recursive: true })
  const fakeTours = path.join(root, 'scripts/fake-tours.ts')
  writeFileSync(
    fakeTours,
    `
import { appendFileSync, mkdirSync, writeFileSync } from 'fs'
import path from 'path'
import { allIntents } from './intent-catalog'
appendFileSync(${JSON.stringify(calls)}, JSON.stringify({ tours: process.env.TOURS_RUN_ID }) + '\\n')
const dir = path.join(${JSON.stringify(root)}, 'frontend/tests/tours/.runs', process.env.TOURS_RUN_ID)
mkdirSync(path.join(dir, 'captures'), { recursive: true })
writeFileSync(path.join(dir, 'captures/001.json'), ${JSON.stringify(JSON.stringify(capture))})
process.exitCode = ${options.toursFail ? 1 : 0}
`
  )
  writeFileSync(path.join(root, 'scripts/run-tours.sh'), `exec bun '${fakeTours}'`)
  const preload = path.join(root, 'fake-boundaries.ts')
  writeFileSync(
    preload,
    `
import { mock } from 'bun:test'
import { appendFileSync, mkdirSync, writeFileSync } from 'fs'
import path from 'path'
import { allIntents } from './intent-catalog'
const log = value => appendFileSync(${JSON.stringify(calls)}, JSON.stringify(value) + '\\n')
globalThis.fetch = async url => { log({ network: String(url) }); throw new Error('unexpected network request') }
mock.module(${JSON.stringify(path.join(judge, 'adapter/codex-sdk.ts'))}, () => ({ makeCodexSdkJudge: () => async input => {
  log({ intent: input.behaviorId, notes: input.captureSections.map(c => c.note) })
  return [{ itemIndex: 0, verdict: ${JSON.stringify(options.verdict ?? 'pass')}, citation: 'CAPTURE[0]: root', critique: 'observed goal critique' }]
} }))
`
  )
  function cli(entry: string, args: string[] = []) {
    const env = { ...process.env, QA_JUDGE: 'codex-sdk' }
    for (const key of Object.keys(env))
      if (key.startsWith('LANGFUSE_')) delete env[key as keyof typeof env]
    const result = spawnSync('bun', ['--preload', preload, path.join(judge, entry), ...args], {
      env,
      encoding: 'utf8',
    })
    return {
      ...result,
      calls: () =>
        readFileSync(calls, 'utf8')
          .trim()
          .split('\n')
          .map(line => JSON.parse(line)),
    }
  }
  return { root, run, cli }
}

it('PR1.c1: a fail is advisory; fresh tours produce a local report without Langfuse', () => {
  const f = fixture({ verdict: 'fail' })
  const result = f.cli('sanity.ts')
  expect(result.status, result.stderr).toBe(0)
  const reportPath = result.stdout.trim().split('\n').at(-1)!
  const report = readFileSync(reportPath, 'utf8')
  expect(report).toContain('fail')
  expect(report).toContain('observed goal critique')
  const calls = result.calls()
  expect(calls[0]).toHaveProperty('tours')
  const modelCalls = calls.filter(call => call.intent)
  expect(modelCalls.map(call => call.intent)).toEqual(['DSH-010', 'DSH-011', 'DSH-012'])
  expect(calls.some(call => call.network)).toBe(false)
  expect(modelCalls.map(call => call.notes)).toEqual([
    ['dashboard#1 — fresh evidence'],
    ['dashboard#1 — fresh evidence'],
    ['dashboard#1 — fresh evidence'],
  ])
  expect(reportPath).toContain(calls[0].tours)
})

it('PR1.c1: failed tours exit non-zero without a verdict report or model call', () => {
  const f = fixture({ toursFail: true })
  const result = f.cli('sanity.ts')
  expect(result.status).not.toBe(0)
  expect(result.stderr).toContain('tours failed')
  expect(result.stdout).toBe('')
  expect(result.calls()).toHaveLength(1)
  expect(result.calls()[0]).toHaveProperty('tours')
  expect(
    readdirSync(path.join(f.root, 'frontend/tests/tours/.runs'), { recursive: true }).some(file =>
      String(file).endsWith('sanity-report.md')
    )
  ).toBe(false)
})

it('PR1.c2: the report CLI covers current and proposed goals, abstains without evidence, and omits retired goals', () => {
  const f = fixture({ retired: true })
  const result = f.cli('report/render.ts', [f.run])
  expect(result.status, result.stderr).toBe(0)
  const report = readFileSync(result.stdout.trim(), 'utf8')
  expect(report).toContain('DSH-010 — pass (current)')
  expect(report).toContain('Critique: observed goal critique')
  expect(report).toContain('DSH-012 — pass (proposed)')
  const noEvidence = report.split('## No evidence')[1]
  expect(noEvidence).toContain('CON-050 (current)')
  expect(noEvidence).toContain('no bound evidence')
  expect(noEvidence).not.toContain('pass')
  expect(report).not.toContain('DSH-011')
  expect(result.calls().map(call => call.intent)).toEqual(['DSH-010', 'DSH-012'])
  for (const intent of allIntents().filter(
    intent => intent.status !== 'retired' && intent.id !== 'DSH-011'
  )) {
    expect(report.match(new RegExp(intent.id, 'g')), intent.id).toHaveLength(1)
  }
  expect(report).not.toMatch(/item.verdict|trap|self.test/i)
})

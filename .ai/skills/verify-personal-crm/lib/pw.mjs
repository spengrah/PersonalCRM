// Playwright session for verify-personal-crm drive scripts. Run scripts through
// bin/run, which sets the VERIFY_* environment this module reads.
//
// Records into VERIFY_EVIDENCE_DIR:
//   NN-<label>.png   screenshots from shot()
//   steps.jsonl      one line per step()/note(): label, url, time, data
//   api.jsonl        every /api/v1 request: method, path, status, request and
//                    response bodies (mutations always; GET bodies too)
//   trace.zip        Playwright trace (open: frontend/node_modules/.bin/playwright show-trace)
//   summary.json     counts, 4xx responses (clientErrors), and problems;
//                    problems make the run exit 1
//
// Problems: uncaught page errors, console errors other than resource-load
// failures, /api/v1 responses with status >= 500, failed check() calls, a
// thrown script, and any request to a localhost port that is not this
// instance's (a frontend compiled against the wrong API URL).

import { createRequire } from 'node:module'
import fs from 'node:fs'
import path from 'node:path'

const env = name => {
  const v = process.env[name]
  if (!v) throw new Error(`${name} unset: run drive scripts through bin/run`)
  return v
}
const BASE_URL = env('VERIFY_BASE_URL')
const API_URL = env('VERIFY_API_URL')
const API_KEY = env('VERIFY_API_KEY')
const EVIDENCE = env('VERIFY_EVIDENCE_DIR')
const OUR_PORTS = new Set([env('VERIFY_FRONTEND_PORT'), env('VERIFY_BACKEND_PORT')])

// Resolve playwright from this checkout's frontend, wherever the script lives.
const require = createRequire(path.join(env('VERIFY_FRONTEND_DIR'), 'package.json'))
const { chromium, devices } = require('playwright')

export async function session({ device } = {}) {
  const browser = await chromium.launch()
  const context = await browser.newContext({
    baseURL: BASE_URL,
    ...(device ? devices[device] : { viewport: { width: 1280, height: 900 } }),
  })
  await context.tracing.start({ screenshots: true, snapshots: true })
  const page = await context.newPage()

  const problems = []
  const log = (file, obj) =>
    fs.appendFileSync(path.join(EVIDENCE, file), JSON.stringify(obj) + '\n')
  let shots = 0
  let apiCalls = 0
  const clientErrors = []

  page.on('pageerror', err => problems.push({ kind: 'pageerror', message: err.message }))
  page.on('console', msg => {
    // A failed fetch also logs "Failed to load resource"; api.jsonl already
    // records its status, and 4xx is often expected (see Gotchas in SKILL.md).
    if (msg.type() === 'error' && !msg.text().startsWith('Failed to load resource')) {
      problems.push({ kind: 'console', message: msg.text() })
    }
  })
  page.on('request', req => {
    const u = new URL(req.url())
    if (['localhost', '127.0.0.1', '[::1]'].includes(u.hostname) && u.port && !OUR_PORTS.has(u.port)) {
      problems.push({ kind: 'foreign-port', message: `${req.method()} ${req.url()}` })
    }
  })
  page.on('response', async res => {
    const u = new URL(res.url())
    if (!u.pathname.startsWith('/api/v1')) return
    const req = res.request()
    apiCalls++
    let body
    try {
      body = await res.json()
    } catch {
      body = null
    }
    log('api.jsonl', {
      time: new Date().toISOString(),
      method: req.method(),
      path: u.pathname + u.search,
      status: res.status(),
      request: req.postDataJSON?.() ?? null,
      response: body,
    })
    if (res.status() >= 400 && res.status() < 500) clientErrors.push(`${req.method()} ${u.pathname} → ${res.status()}`)
    if (res.status() >= 500) problems.push({ kind: 'api', message: `${req.method()} ${u.pathname} → ${res.status()}` })
  })

  const s = {
    page,
    context,
    /** Screenshot the current page as NN-<label>.png. */
    async shot(label, opts = {}) {
      shots++
      const file = `${String(shots).padStart(2, '0')}-${label.replace(/[^a-z0-9-]+/gi, '-')}.png`
      await page.screenshot({ path: path.join(EVIDENCE, file), fullPage: true, ...opts })
      log('steps.jsonl', { time: new Date().toISOString(), shot: file, url: page.url() })
      return file
    },
    /** Run one user action, logging it, then screenshot the result. */
    async step(label, fn) {
      log('steps.jsonl', { time: new Date().toISOString(), step: label, url: page.url() })
      const out = await fn(page)
      await s.shot(label)
      return out
    },
    /** Record an observation (e.g. a DB or API read) next to the steps. */
    note(label, data) {
      log('steps.jsonl', { time: new Date().toISOString(), note: label, data })
    },
    /** Read the backend directly, for observing side effects, never for acting. */
    async apiGet(p) {
      const res = await fetch(API_URL + p, { headers: { 'X-API-Key': API_KEY } })
      const data = await res.json().catch(() => null)
      s.note(`GET ${p} → ${res.status}`, data)
      return { status: res.status, data }
    },
    /** Assert a condition; a failure is recorded and fails the run. */
    check(ok, message) {
      log('steps.jsonl', { time: new Date().toISOString(), check: message, ok: !!ok })
      if (!ok) problems.push({ kind: 'check', message })
      return !!ok
    },
    async close() {
      await context.tracing.stop({ path: path.join(EVIDENCE, 'trace.zip') })
      await browser.close()
      const summary = { baseURL: BASE_URL, shots, apiCalls, clientErrors, problems }
      fs.writeFileSync(path.join(EVIDENCE, 'summary.json'), JSON.stringify(summary, null, 2))
      if (problems.length) {
        console.error(`✗ ${problems.length} problem(s); see summary.json`)
        for (const p of problems) console.error(`  ${p.kind}: ${p.message}`)
        process.exitCode = 1
      } else {
        console.log(`✓ no problems (${shots} screenshots, ${apiCalls} API calls)`)
      }
      if (clientErrors.length) console.log(`ℹ ${clientErrors.length} 4xx API response(s), not failures; see summary.json clientErrors`)
    },
  }
  return s
}

/** Run fn(session) and always close, so a thrown error still leaves evidence. */
export async function drive(fn, opts) {
  const s = await session(opts)
  try {
    await fn(s)
  } catch (err) {
    s.check(false, `script threw: ${err.message}`)
    await s.shot('error').catch(() => {})
  } finally {
    await s.close()
  }
}

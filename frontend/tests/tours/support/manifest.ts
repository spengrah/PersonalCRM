// Run-manifest assembly + host redaction (arc §1b). Pure — no Playwright import.

import { CAPTURE_FORMAT_VERSION, CAPTURE_GENERATOR_VERSION, type Manifest } from './types'

// Redact the staging host: keep the scheme + path shape, replace the host with
// the literal <staging> so the hostname is never committed (arc §4.6). Falls
// back to a fully-opaque value if the URL cannot be parsed.
export function redactHost(baseUrl: string): string {
  try {
    const u = new URL(baseUrl)
    return `${u.protocol}//<staging>${u.pathname === '/' ? '/' : u.pathname}`
  } catch {
    return '<staging>'
  }
}

export interface ManifestEnv {
  runId: string
  gitSha?: string
  stagingImageDigest?: string
  seedProfile?: string
  baseUrl?: string
  timestamp?: string
}

export function buildManifest(env: ManifestEnv): Manifest {
  return {
    captureFormatVersion: CAPTURE_FORMAT_VERSION,
    captureGeneratorVersion: CAPTURE_GENERATOR_VERSION,
    gitSha: env.gitSha || 'unknown',
    stagingImageDigest: env.stagingImageDigest || 'unknown',
    seedProfile: env.seedProfile || 'prod-shaped',
    baseUrl: env.baseUrl ? redactHost(env.baseUrl) : '<staging>',
    timestamp: env.timestamp || new Date().toISOString(),
    runId: env.runId,
  }
}

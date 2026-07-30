// Run-manifest assembly + host redaction. Pure — no Playwright import.

import { CAPTURE_FORMAT_VERSION, CAPTURE_GENERATOR_VERSION, type Manifest } from './types'

// Redact the staging host: keep the scheme + path shape, replace the host with
// the literal <staging> so the hostname is never committed. Falls back to a
// fully-opaque value if the URL cannot be parsed.
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
    // Default to 'unknown', NOT 'standard': an unset profile means the seed
    // is whatever was already on the target (e.g. a TOURS_SKIP_RESET run). The
    // caller (run-tours.sh) declares 'standard' only when it actually
    // established that provenance. Never assert a provenance we did not create.
    seedProfile: env.seedProfile || 'unknown',
    baseUrl: env.baseUrl ? redactHost(env.baseUrl) : '<staging>',
    timestamp: env.timestamp || new Date().toISOString(),
    runId: env.runId,
  }
}

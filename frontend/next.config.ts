import type { NextConfig } from 'next'

const nextConfig: NextConfig = {
  output: 'standalone',
  // `bun run lint` runs ESLint in CI and pre-push. The build's own pass ignores
  // eslint-suppressions.json, so it would fail on the recorded violations.
  eslint: { ignoreDuringBuilds: true },
}

export default nextConfig

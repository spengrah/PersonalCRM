# PersonalCRM frontend image (A0 containerization cutover).
#
# Packages the CI-built Next.js standalone output. node:22 (glibc, bookworm-slim)
# is used over alpine/musl so any linux-arm64 native prebuild would work; recon
# confirms this app has ZERO runtime native addons (no next/image, no sharp), so
# the standalone is pure JS.
#
# IMPORTANT: the standalone is built KEY-LESS and same-origin (empty
# NEXT_PUBLIC_API_KEY and NEXT_PUBLIC_API_URL). The public image must bake no
# secret; the X-API-Key is injected at the Caddy edge (see infra/caddy/Caddyfile).
#
# Build context = repo root:  docker buildx build -f infra/images/frontend.Dockerfile .
FROM node:22-bookworm-slim

WORKDIR /app
ENV NODE_ENV=production \
    HOSTNAME=0.0.0.0 \
    PORT=3001 \
    NEXT_TELEMETRY_DISABLED=1

# Next standalone server + its minimal node_modules, then the static assets and
# public/ dir (standalone output does not include these automatically).
COPY frontend/.next/standalone ./
COPY frontend/.next/static ./.next/static
COPY frontend/public ./public

EXPOSE 3001
USER node
CMD ["node", "server.js"]

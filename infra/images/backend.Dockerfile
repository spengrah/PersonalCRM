# PersonalCRM backend image (A0 containerization cutover).
#
# Thin packaging layer over the CI-cross-compiled, fully static arm64 binaries
# (GOOS=linux GOARCH=arm64 CGO_ENABLED=0 -> statically-linked ELF). No build
# happens here -- the binaries are produced upstream (CI / `make`), so this
# image needs no Go toolchain and no QEMU.
#
# distroless/static: no shell, no package manager, runs as nonroot. Ships CA
# certs (outbound HTTPS to Todoist/Google), tzdata, and /etc/passwd. The binary
# is static so the static base is sufficient.
#
# Bundles BOTH binaries: crm-api (the server, runs golang-migrate on boot) and
# crm-admin (operator CLI; A needs `crm-admin --migrate` / `--seed`). They share
# the baked migration files at /migrations.
#
# Build context = repo root:  docker buildx build -f infra/images/backend.Dockerfile .
FROM gcr.io/distroless/static-debian12:nonroot

COPY backend/migrations /migrations
COPY backend/bin/crm-api-arm64 /usr/local/bin/crm-api
COPY backend/bin/crm-admin-arm64 /usr/local/bin/crm-admin

ENV MIGRATIONS_PATH=/migrations \
    PORT=8080 \
    HOST=0.0.0.0

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/crm-api"]

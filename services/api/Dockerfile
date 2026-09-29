# syntax=docker/dockerfile:1

# ── builder ──────────────────────────────────────────────────────────────────
# The pinned digest determines the Go toolchain compiled into the binary, so a
# stale pin ships stdlib CVEs that trivy flags against usr/local/bin/api. This
# digest is Go 1.25.14; the previous one was 1.25.12, which predates the
# 1.25.13 fixes for CVE-2026-33818, -39821, -56853, -56858, -56859, -56860
# and -56862.
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS builder

WORKDIR /build

# Cache module downloads before copying source.
COPY services/api/go.mod services/api/go.sum ./
RUN go mod download

# Copy source and build the binary.
COPY services/api/ ./

# Build arguments for version information (issue #397).
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-X 'github.com/Depo-dev/trident/services/api/handlers.Version=${VERSION}' \
              -X 'github.com/Depo-dev/trident/services/api/handlers.CommitSHA=${COMMIT_SHA}' \
              -X 'github.com/Depo-dev/trident/services/api/handlers.BuildTime=${BUILD_TIME}'" \
    -o /api .

# ── runtime ──────────────────────────────────────────────────────────────────
# Was pinned to a stale alpine:3.19 digest, so the runtime image kept
# shipping musl (CVE-2025-26519) and libcrypto3 (CVE-2024-6119) versions
# that upstream had already patched. 3.22 clears both (issue #371).
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS runtime

# The digest above is the current alpine:3.22 (3.22.5), so a bump cannot clear
# CVE-2026-14456 — the fixed libcrypto3 (3.5.8-r0) is in the 3.22 package repo
# but not yet in a rebuilt base image. Upgrading installed packages at build
# time picks it up while keeping the base pinned, so the build stays
# reproducible against a known image and still ships patched libraries.
#
# Without this the `SBOM + scan (go-api)` gate fails on a fixable HIGH, which
# blocks every PR in the repo since it is a required check.
RUN apk upgrade --no-cache

RUN adduser -S -u 1001 trident

COPY --from=builder /api /usr/local/bin/api

USER trident

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/api"]

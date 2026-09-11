# Multi-stage build for Go binaries — per-component to minimize CI work.
#
# Each component has its own builder stage that compiles ONLY its binary.
# Building with `--target=<component>` (matrix job in .github/workflows/ci.yaml)
# causes Docker to traverse only the stages on the dependency path for that
# target — base-builder + builder-<component> + <component>. Other components'
# builder stages are skipped entirely.
#
# The shared `base-builder` stage holds the expensive `go mod download` step
# and full source COPY. Its layers are cached/shared across matrix jobs via
# `cache-from: type=gha` in the workflow.

FROM golang:1.25-alpine AS base-builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# --- API Gateway ---
FROM base-builder AS builder-api-gateway
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/api-gateway ./cmd/api-gateway/

FROM alpine:3.19 AS api-gateway
RUN apk add --no-cache ca-certificates tzdata shellcheck
COPY --from=builder-api-gateway /bin/api-gateway /usr/local/bin/api-gateway
USER 65534:65534
ENTRYPOINT ["api-gateway"]

# --- Provision Worker ---
FROM base-builder AS builder-provision-worker
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/provision-worker ./cmd/provision-worker/

FROM alpine:3.19 AS provision-worker
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder-provision-worker /bin/provision-worker /usr/local/bin/provision-worker
USER 65534:65534
ENTRYPOINT ["provision-worker"]

# --- Crucible Engine ---
FROM base-builder AS builder-crucible-engine
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/crucible-engine ./cmd/crucible-engine/

FROM alpine:3.19 AS crucible-engine
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder-crucible-engine /bin/crucible-engine /usr/local/bin/crucible-engine
USER 65534:65534
ENTRYPOINT ["crucible-engine"]

# --- Synthetic API Monitor ---
FROM base-builder AS builder-synthetic-api-monitor
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/synthetic-api-monitor ./cmd/synthetic-api-monitor/

FROM alpine:3.19 AS synthetic-api-monitor
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder-synthetic-api-monitor /bin/synthetic-api-monitor /usr/local/bin/synthetic-api-monitor
USER 65534:65534
ENTRYPOINT ["synthetic-api-monitor"]

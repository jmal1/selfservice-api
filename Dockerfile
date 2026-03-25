# Multi-stage build for Go binaries
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build API gateway
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/api-gateway ./cmd/api-gateway/

# Build provision worker
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/provision-worker ./cmd/provision-worker/

# Build crucible engine
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/crucible-engine ./cmd/crucible-engine/

# --- API Gateway image ---
FROM alpine:3.19 AS api-gateway
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/api-gateway /usr/local/bin/api-gateway
USER 65534:65534
ENTRYPOINT ["api-gateway"]

# --- Provision Worker image ---
FROM alpine:3.19 AS provision-worker
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/provision-worker /usr/local/bin/provision-worker
USER 65534:65534
ENTRYPOINT ["provision-worker"]

# --- Crucible Engine image ---
FROM alpine:3.19 AS crucible-engine
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /bin/crucible-engine /usr/local/bin/crucible-engine
USER 65534:65534
ENTRYPOINT ["crucible-engine"]

# Runner container image
# Ubuntu 24.04 + crucible-runner binary + actions.sh library
# Built separately from the main Crucible images (uses Ubuntu, not Alpine)

# --- Stage 1: Build the Go binary ---
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/crucible-runner ./cmd/crucible-runner/

# --- Stage 2: Runner image ---
FROM ubuntu:24.04

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install assessment tools and dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    # Core utilities
    bash \
    ca-certificates \
    curl \
    wget \
    jq \
    socat \
    iproute2 \
    iputils-ping \
    dnsutils \
    netcat-openbsd \
    # Network assessment tools
    nmap \
    smbclient \
    ssh \
    openssl \
    # Development tools (for workflow scripts)
    git \
    python3 \
    python3-pip \
    # Timezone data
    tzdata \
    && rm -rf /var/lib/apt/lists/*

# Create crucible directories
RUN mkdir -p /opt/crucible/lib /opt/crucible/bin

# Copy the Go runner binary
COPY --from=builder /bin/crucible-runner /opt/crucible/bin/crucible-runner

# Copy the action library
COPY deploy/runner/actions.sh /opt/crucible/lib/actions.sh
RUN chmod +x /opt/crucible/lib/actions.sh

# Add runner binary to PATH
ENV PATH="/opt/crucible/bin:${PATH}"

# Default working directory for workflow execution
WORKDIR /tmp

ENTRYPOINT ["crucible-runner"]

# Runner container image
# Kali Rolling + crucible-runner binary + actions.sh library
# Built separately from the main Crucible images (uses Kali, not Alpine)

# --- Stage 1: Build the Go binary ---
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/crucible-runner ./cmd/crucible-runner/

# --- Stage 2: Kali Linux runtime ---
#
# WHY KALI: Assessment workflows execute offensive-security checks against student
# VMs (port scans, SMB enumeration, credential spraying, web fingerprinting, etc.).
# Ubuntu 24.04 does not ship the required tooling in its default repos. Kali Rolling
# does, with versions maintained by Offensive Security and kept current via rolling
# releases.
#
# WHY THE TOOL LIST IS CURATED: kali-linux-large and kali-linux-everything are
# multi-GB meta-packages that push the image well over 3 GB and exceed the runner
# Job's cold-pull deadline on a fresh node. Only tools actually invoked by current
# assessment workflows are included here.
#
# SIZE TARGET: Keep the final image under 3 GB. Do NOT add kali-linux-large,
# kali-linux-everything, or metasploit-framework -- each adds several GB.
#
# COLD PULL NOTE: kali-rolling is not pre-pulled on runner nodes. The first pull
# after a new digest can take 1-2 minutes on a cold node. Plan maintenance windows
# around image refreshes; consider a pre-pull DaemonSet if cold-start latency
# becomes a problem against Job deadlines.
ARG KALI_BASE=kalilinux/kali-rolling
FROM ${KALI_BASE} AS runtime

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Packages are a HARD requirement. kali-rolling package names do drift, and an
# earlier draft of this file swallowed install failures with a "WARNING: SKIPPED"
# echo so a rename could not break the build. That is the wrong trade: a runner
# image that builds green but silently lacks netexec/hydra does not fail loudly —
# it produces wrong assessment results for students. A broken build is visible and
# takes minutes to fix; a toolless runner is invisible and corrupts grades.
# If CI fails here after a kali-rolling update, fix the package name. Do not
# reintroduce a best-effort loop.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
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
        openssh-client \
        openssl \
        git \
        python3 \
        python3-pip \
        tzdata \
        nmap \
        smbclient \
        netexec \
        hydra \
        enum4linux-ng \
        impacket-scripts \
        seclists \
        nikto \
        whatweb \
        redis-tools \
        ldap-utils \
        smbmap \
    && rm -rf /var/lib/apt/lists/*

# Assert the tools actually resolve on PATH. Installing a package is not the same
# as getting the command: Kali repackages routinely rename wrapper scripts while
# keeping the package name stable, which apt reports as success. This is the check
# that a workflow author actually depends on.
RUN set -eu; \
    missing=""; \
    for cmd in \
        jq socat curl wget openssl ssh git python3 \
        nmap smbclient netexec hydra enum4linux-ng nikto whatweb smbmap \
        redis-cli ldapsearch impacket-secretsdump \
    ; do \
        command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"; \
    done; \
    [ -d /usr/share/seclists ] || missing="$missing seclists(/usr/share/seclists)"; \
    if [ -n "$missing" ]; then \
        echo "FATAL: required runner tooling missing from PATH:$missing" >&2; \
        exit 1; \
    fi; \
    echo "runner tooling verified"

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
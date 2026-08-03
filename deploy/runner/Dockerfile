# Runner container image
# Kali Rolling + crucible-runner binary + actions.sh library
# Built separately from the main Crucible images (uses Kali, not Alpine)

# Kali base image, overridable to pin a digest:
#   docker build --build-arg KALI_BASE=kalilinux/kali-rolling@sha256:<digest> ...
#
# This ARG MUST stay above the first FROM. An ARG declared after a FROM belongs
# to that build stage, so `FROM ${KALI_BASE}` for stage 2 would expand to an
# empty string and fail with "base name (${KALI_BASE}) should not be blank".
ARG KALI_BASE=kalilinux/kali-rolling

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
#
# KALI_BASE is declared globally at the top of this file -- see the note there.
FROM ${KALI_BASE} AS runtime

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# The tool inventory is NOT written out here. It is derived from
# internal/runnertools/tools.txt, which is the single source of truth shared
# with the Go script validator.
#
# It used to be spelled out twice in this file — once as an apt list and once
# as a `command -v` loop — plus a third, implicit copy in the heads of whoever
# wrote the workflows. Those three could drift independently, and the drift was
# silent in the worst direction: a tool present in the apt list but absent from
# the verification loop would ship missing, and a tool absent from both could
# still be called by a saved workflow and fail as a bare exit 127 during a
# graded assessment.
#
# Packages are a HARD requirement. kali-rolling package names do drift, and an
# earlier draft of this file swallowed install failures with a "WARNING: SKIPPED"
# echo so a rename could not break the build. That is the wrong trade: a runner
# image that builds green but silently lacks netexec/hydra does not fail loudly —
# it produces wrong assessment results for students. A broken build is visible and
# takes minutes to fix; a toolless runner is invisible and corrupts grades.
# If CI fails here after a kali-rolling update, fix the package name in
# tools.txt. Do not reintroduce a best-effort loop.
COPY internal/runnertools/tools.txt /opt/crucible/tools.txt

RUN set -eu; \
    pkgs=$(awk '!/^[[:space:]]*#/ && NF >= 2 {print $2}' /opt/crucible/tools.txt | sort -u); \
    [ -n "$pkgs" ] || { echo "FATAL: tools.txt yielded no packages; manifest parse is broken" >&2; exit 1; }; \
    echo "installing from manifest:" $pkgs; \
    apt-get update \
    && apt-get install -y --no-install-recommends $pkgs \
    && rm -rf /var/lib/apt/lists/*

# Assert the tools actually resolve. Installing a package is not the same as
# getting the command: Kali repackages routinely rename wrapper scripts while
# keeping the package name stable, which apt reports as success. This is the
# check a workflow author actually depends on.
#
# Entries beginning with '/' are data paths (seclists wordlists, CA bundle,
# zoneinfo) and are checked with `test -e`; everything else must resolve on PATH.
RUN set -eu; \
    missing=""; \
    checked=0; \
    while read -r check pkg _rest; do \
        case "$check" in \
            ''|\#*) continue ;; \
        esac; \
        [ -n "${pkg:-}" ] || continue; \
        checked=$((checked + 1)); \
        case "$check" in \
            /*) [ -e "$check" ] || missing="$missing $check(from $pkg)" ;; \
            *)  command -v "$check" >/dev/null 2>&1 || missing="$missing $check(from $pkg)" ;; \
        esac; \
    done < /opt/crucible/tools.txt; \
    [ "$checked" -gt 0 ] || { echo "FATAL: tools.txt yielded no checks; manifest parse is broken" >&2; exit 1; }; \
    if [ -n "$missing" ]; then \
        echo "FATAL: required runner tooling missing:$missing" >&2; \
        echo "Fix the package name in internal/runnertools/tools.txt." >&2; \
        exit 1; \
    fi; \
    echo "runner tooling verified ($checked checks)"

# Strip file capabilities from every binary in the image.
#
# Kali's nmap package ships /usr/lib/nmap/nmap with
#   cap_net_bind_service,cap_net_admin,cap_net_raw=eip
# so that unprivileged users can run SYN scans. Inside this container that
# permitted set is NOT a subset of the process bounding set — CAP_NET_ADMIN
# (bit 12) is absent from a default containerd bounding set — and execve()
# refuses any file whose permitted caps exceed the bounding set. The result is
# that EVERY nmap invocation fails with "Operation not permitted", including
# unprivileged `-sT` connect scans that need no capability whatsoever.
#
# Removing the file caps is the right fix rather than granting NET_ADMIN to the
# Job: the runner already executes as root with CAP_NET_RAW effective, so raw
# sockets and SYN scans still work, and widening a Kali container's privileges
# on a student VLAN to solve a problem that requires no privilege would be a
# straightforwardly bad trade.
#
# The assertion is static (`getcap -r /` finds nothing) rather than a runtime
# `nmap` smoke test, because buildkit runs with a wider capability set than the
# deployed container — a runtime test here would pass while production stayed
# broken, which is exactly how this defect survived in the first place.
RUN set -eu; \
    apt-get update && apt-get install -y --no-install-recommends libcap2-bin; \
    scan="/usr /bin /sbin /lib /opt"; \
    getcap -r $scan 2>/dev/null | while read -r line; do \
        f=$(printf '%s' "$line" | cut -d' ' -f1); \
        echo "stripping file capabilities from $f"; \
        setcap -r "$f" || true; \
    done; \
    remaining=$(getcap -r $scan 2>/dev/null || true); \
    if [ -n "$remaining" ]; then \
        echo "FATAL: file capabilities still present after strip; execve() will refuse these binaries:" >&2; \
        echo "$remaining" >&2; \
        exit 1; \
    fi; \
    rm -rf /var/lib/apt/lists/*; \
    echo "no file capabilities remain"

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
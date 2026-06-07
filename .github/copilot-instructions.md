# GitHub Copilot Instructions — selfservice-api (Crucible)

This repository is **Crucible**, a self-service VM lab and automated-assessment platform for cybersecurity students. The backend is Go + chi + Postgres; the frontend (separate repo `selfservice-ui`) is SvelteKit + Skeleton.

## When helping an instructor author content (workflows, actions, playlists)

**Read [`../AGENTS.md`](../AGENTS.md) before suggesting any schema, JSON payload, or bash script.** It is the canonical authoring reference and documents exactly what the API will accept. Specifically, you must consult it for:

- The `Workflow`, `Action`, `Playlist` JSON schemas (§3).
- Allowed values for `execution_mode`, `status`, `creation_mode`, `scoring_mode`, `action_type`, `action_category` (these are enforced by Postgres `CHECK` constraints — invented values will 400).
- The `kali_runner` vs `vmware_tools` decision (§4).
- The `run_action`, `ctx_set`, `CTX_*` runtime contract (§5).
- The current `supported_platforms` tag vocabulary (§6).
- The reusable action library snapshot (§7).
- Bash conventions (`set -euo pipefail`, `source /opt/crucible/lib/actions.sh`, quoting, timeouts) and anti-patterns (§§8, 10).
- Validation checks to run before declaring "done" (§12).

If a user request would require bypassing approval gates, reaching outside the pod's VLAN, mutating the student VM destructively, or running privileged ops in the unprivileged runner pod, push back — see §13 of `AGENTS.md`.

## When working on the codebase itself

- Go: standard module layout, package per concern under `internal/`. Match existing naming and error-handling style.
- Tests: `go test ./...` from the repo root. The synthetic monitor in `internal/synthetic/` is the reference for the test-coverage bar we want for new code.
- DB migrations: numbered `internal/database/migrations/NNNNNN_name.up.sql` + matching `.down.sql`. Never edit a shipped migration in place.
- API routes: register in `internal/api/routes/routes.go`. Admin-only routes go under the `r.Use(middleware.RequireRole(models.RoleAdmin))` group.

For everything else, follow repository conventions visible in the existing code.

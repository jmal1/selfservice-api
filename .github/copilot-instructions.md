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

## When changing user-facing Crucible behavior — update the wiki

The instructor wiki (`/wiki` in the UI) is sourced from this repo via
`make wiki-bundle`. The seed files in `Makefile`'s `WIKI_SEEDS` plus
their transitively-linked closure are embedded into the binary and
served by `/api/v1/wiki/*` to instructors and admins.

**If your change touches any of the following, update the wiki in the
same PR:**

- `Workflow`, `Action`, `Playlist`, `Quota` JSON schemas (DB
  migrations under `internal/database/migrations/`, struct tags in
  `internal/models/`). Update `AGENTS.md` §3 and the relevant
  `docs/instructor/*.md` page (workflows, actions, playlists).
- Allowed enum values for `execution_mode`, `status`,
  `creation_mode`, `scoring_mode`, `action_type`, `action_category`,
  or `supported_platforms`. Update `AGENTS.md` §§3, 6.
- Runner contract — `run_action`, `ctx_set`, `CTX_*` env vars,
  `/opt/crucible/lib/actions.sh` helpers, validation hooks. Update
  `AGENTS.md` §5 and `docs/instructor/runner-environment.md` /
  `docs/instructor/actions.md`.
- Quota limits, rate limits, or assessment-engine timeouts that an
  instructor would hit during authoring. Update `AGENTS.md` §12 and
  `docs/instructor/troubleshooting.md`.
- New library actions in `internal/seeds/` — update `AGENTS.md` §7
  and `docs/instructor/actions.md` (library catalog section).

After editing markdown, run `make wiki-bundle` to regenerate
`internal/docs/_bundle/`; commit the regenerated bundle alongside the
markdown changes. CI runs `make verify-wiki` to ensure the checked-in
bundle matches what the seeds produce, so an out-of-sync bundle will
fail the build.

**Wiki architecture / schema changes:** If you are adding or removing
fields on the wiki manifest (`wikitypes.Manifest` /
`wikitypes.ManifestEntry`), read `docs/architecture/wiki.md` first.
The schema is single-sourced in `internal/wikitypes` and a golden
test in `internal/docs/embed_test.go` will fail on any unannounced
change — fix is to update the UI's `WikiManifestEntry` interface in
`selfservice-ui/src/lib/api/client.ts` and run
`go test ./internal/docs -update` to regenerate the fixture.

For everything else, follow repository conventions visible in the existing code.

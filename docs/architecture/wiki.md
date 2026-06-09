# Wiki Architecture

This document explains how the instructor wiki at `/wiki` is built,
served, and rendered. It exists so future maintainers (and AI agents
helping us) don't have to reverse-engineer the design from code, and
so we can answer the "why not use a real wiki engine?" question once
instead of every six months.

## TL;DR

The instructor wiki is **markdown-in-binary, served by Go, rendered by
Svelte**. Source markdown lives in this repo under `docs/instructor/`
and `docs/ai*/`. A build-time tool (`cmd/wiki-bundler`) walks a seed
list + transitive link closure, copies the files into
`internal/docs/_bundle/`, and writes a JSON manifest. That whole
directory is embedded into the API binary via `//go:embed all:_bundle`.
At runtime the API exposes three endpoints (`/api/v1/wiki/index`,
`/page/{path...}`, `/bundle.zip`) gated to instructor + admin. The UI
loads the index, fetches pages on click, and renders markdown via
`marked` + `marked-alert` + `highlight.js` + DOMPurify.

No external service. No separate database. No CMS. ~2,200 LoC total
(Go + TS + CSS).

## Data flow

```
selfservice-api repo
  docs/instructor/*.md      (authored markdown — source of truth)
  AGENTS.md                 (AI agent reference, also a wiki page)
  internal/api/*.go         (source files surfaced for AI context)
            │
            │  make wiki-bundle  (cmd/wiki-bundler/main.go)
            ▼
  internal/docs/_bundle/
    manifest.json           (uses wikitypes.Manifest schema)
    AGENTS.md               (copied verbatim)
    docs/instructor/*.md
    internal/api/.../*.go
            │
            │  //go:embed all:_bundle  (internal/docs/embed.go)
            ▼
  api-gateway binary
            │
            │  GET /api/v1/wiki/index → JSON manifest
            │  GET /api/v1/wiki/page/{path} → raw bytes
            │  GET /api/v1/wiki/bundle.zip → full bundle
            ▼
selfservice-ui repo
  /wiki page (Svelte) ──► fetches index, renders each .md via marked
                          fetches each page on click, renders with hljs
```

## Why a custom approach instead of MkDocs / BookStack / Outline?

Considered and rejected:

| Option | Why not |
| --- | --- |
| **MkDocs Material** | Beautiful static-site generator, but loses (a) integration with our existing Authentik / RBAC middleware, (b) the AI-bundle `.zip` download path students use with Cursor / Copilot, (c) "docs and code change in the same PR" workflow. Would also need a separate subdomain + Caddy forward-auth wiring. |
| **BookStack / Outline / Wiki.js** | WYSIWYG editing is nice, but docs would live in a DB, decoupled from the code they describe. Schema changes and doc updates would land in separate places, breaking the `make verify-wiki` CI check that today guarantees they stay in sync. New container + new Postgres + new backups. |
| **GitHub Wiki** | Requires the underlying repo to be public, or Enterprise. selfservice-api is private and the wiki audience (instructors) doesn't have repo access — that's the original reason we built this. |
| **Docusaurus / Read the Docs** | Same problems as MkDocs (separate deploy, auth proxy needed, AI-bundle export missing) plus heavier framework footprint. |

The thing that's genuinely unique to our use case is the **AI-bundle
`.zip` download**: instructors hand it to Cursor / Claude / Copilot as
the entire context for "build me a workflow that …". No off-the-shelf
wiki ships this; we'd have to build a custom exporter for any of them.

The thing that's genuinely valuable to our workflow is **docs colocated
with code**: when an API schema changes, the PR touches both behavior
and documentation in one diff, and `make verify-wiki` enforces that the
bundle stays in sync with the seeds. With a third-party wiki, schema
drift between code and docs becomes a chronic background bug.

Given those two constraints, owning the rendering pipeline costs about
2,200 LoC for the lifetime of the platform — small enough to be
justifiable.

## Schema source of truth

The bundle manifest schema lives in **one place**:
`internal/wikitypes/types.go`. Both `cmd/wiki-bundler` and
`internal/docs` import the types from there.

The UI's `WikiManifestEntry` TypeScript interface
(`selfservice-ui/src/lib/api/client.ts`) is the cross-repo mirror. It
must be updated by hand when the Go schema changes — there is no
codegen pipeline. To prevent silent drift:

1. `internal/docs/embed_test.go::TestManifestSchema` is a golden test
   that diffs the marshaled JSON against
   `internal/docs/testdata/manifest.golden.json`. Any field add/remove
   fails CI until the golden is regenerated via
   `go test ./internal/docs -update`.
2. `TestManifestSchema_EveryWireFieldDocumented` cross-checks that the
   Go JSON keys match a hand-maintained allowlist. Adding a field to
   `wikitypes` without also adding it to the allowlist fails the test
   with a pointer to update the TS interface.

Once both tests pass, the Go-side schema is locked. The TS side relies
on the test failure messages to prompt the corresponding update.

## Why this isn't easier

Some things that look simple but aren't:

- **Why duplicate types as type aliases in `internal/docs`?**
  Backward compatibility. The handler code in
  `internal/api/handlers/wiki.go` was written against
  `docs.ManifestEntry`. Aliasing keeps the call sites unchanged while
  the underlying type comes from `wikitypes`.
- **Why does the bundler walk a transitive closure rather than just
  bundle `docs/`?** Because instructor docs link to live source files
  (e.g. `internal/runner/executor.go`) to show the actual runner
  contract, and those files should travel with the bundle so the
  AI-bundle download is self-contained. The closure walk is the
  mechanism that pulls them in automatically when a doc adds a new
  link.
- **Why is `internal/docs/_bundle/` checked in?** So `go build`
  works on a fresh clone without running the bundler first. `make
  verify-wiki` in CI ensures the checked-in copy matches what the
  seeds would produce.
- **Why is the bundle directory prefixed with `_`?** So Go's build
  tooling skips it when walking packages — the bundle contains real
  `.go` files (copied verbatim for AI context), and we must NOT
  compile them.

## When to revisit this decision

Revisit if any of these are true:

- The wiki LoC grows past ~5,000 lines (currently ~2,200).
- Instructors start asking for WYSIWYG / in-app editing as a recurring
  request (option C in the audit becomes worth the trade-off).
- We need multi-author workflows with comments and revision history
  that aren't well-served by git.
- A third-party wiki engine ships a first-class "AI agent context
  bundle" export, eliminating the unique-feature blocker.

## Related files

- `internal/wikitypes/types.go` — schema source of truth
- `cmd/wiki-bundler/main.go` — build-time bundler
- `internal/docs/embed.go` — runtime bundle loader
- `internal/docs/embed_test.go` — schema golden test
- `internal/api/handlers/wiki.go` — HTTP handlers
- `Makefile` — `wiki-bundle` and `verify-wiki` targets
- `selfservice-ui/src/routes/wiki/+page.svelte` — UI shell
- `selfservice-ui/src/lib/wiki/markdown.ts` — renderer
- `selfservice-ui/src/lib/api/client.ts` — `WikiManifestEntry` mirror

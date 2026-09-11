# CLAUDE.md

This repository is **Crucible** — a self-service VM lab + automated assessment platform for cybersecurity students.

When asked to help author **workflows**, **actions**, or **playlists**, read [`AGENTS.md`](AGENTS.md) at the repo root. It is the canonical reference for the workflow/action/playlist schema, the runner contract, library actions, bash conventions, anti-patterns, and validation rules. Do not invent enum values, action types, or schema fields — `AGENTS.md` documents exactly what the API will accept.

For everything else (engine internals, refactoring, infrastructure), proceed normally — this codebase follows standard Go + SvelteKit conventions and there is no special slash-command or tool restriction.

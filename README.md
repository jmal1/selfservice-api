# selfservice-api

Go monorepo for **Crucible**: API gateway, provision worker, assessment engine, and internal synthetic API monitor.

Images published to GHCR (SHA-tagged):

- `ghcr.io/jmal1/selfservice-api-gateway`
- `ghcr.io/jmal1/selfservice-provision-worker`
- `ghcr.io/jmal1/selfservice-crucible-engine`
- `ghcr.io/jmal1/selfservice-synthetic-api-monitor`

## Public vs private config

This repository is **public**. It ships:

- Generic Helm chart defaults under `deploy/helm/selfservice/`
- Placeholder overlays: `values.example.yaml` / `values.example-full.yaml` (`*.example.test` hosts)

It does **not** ship production host inventories, management IPs, or live secrets.

| Concern | Where it lives |
| --- | --- |
| Production Helm overlays (`values.prod.yaml`) | Private [`jmal1/crucible-deploy`](https://github.com/jmal1/crucible-deploy) |
| Runtime secrets (OIDC, DB, vCenter, …) | Vault → External Secrets (cluster) |
| Lab topology / operator runbooks | Private ops vault (not this repo) |

CI runs `scripts/check-public-fingerprints.ps1` so lab fingerprints cannot creep back into the tree.

## Layout

| Path | Role |
| --- | --- |
| `cmd/` | Binaries (`api-gateway`, `provision-worker`, `crucible-engine`, `synthetic-api-monitor`, …) |
| `internal/` | Application packages |
| `deploy/helm/selfservice/` | Helm chart + example values |
| `deploy/scripts/deploy.sh` | Guarded apply helper (production apply prefers `crucible-deploy`) |
| `docs/instructor/` | Instructor wiki sources (bundled to `internal/docs/_bundle/`) |
| `AGENTS.md` | Authoring / schema contract for instructors and AI tools |

## Local development

```bash
# Optional: Postgres + NATS
docker compose -f docker-compose.dev.yaml up -d

export DB_HOST=localhost DB_PASSWORD=dev DB_NAME=selfservice DB_USER=selfservice
export NATS_URL=nats://localhost:4222

go build ./...
go test ./... -short
```

Windows local CI mirror:

```powershell
pwsh ./scripts/local-verify.ps1
```

After editing instructor docs or bundled sources:

```bash
make wiki-bundle   # then commit internal/docs/_bundle/
make verify-wiki
```

## Helm (forks / CI)

```bash
helm lint deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.example.yaml
helm template selfservice deploy/helm/selfservice -f deploy/helm/selfservice/values.yaml -f deploy/helm/selfservice/values.example.yaml
```

Production applies use private `crucible-deploy` overlays and the jump host release agent — not committed lab values in this tree.

## Related

- UI: [`jmal1/selfservice-ui`](https://github.com/jmal1/selfservice-ui)
- Kali runner image / module: [`jmal1/selfservice-crucible-runner`](https://github.com/jmal1/selfservice-crucible-runner)
- External UI synthetics: [`jmal1/selfservice-synthetic-ui`](https://github.com/jmal1/selfservice-synthetic-ui)
- Deploy / prod topology (private): [`jmal1/crucible-deploy`](https://github.com/jmal1/crucible-deploy)

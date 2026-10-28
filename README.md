# Grayling

[简体中文](README.zh-CN.md) · [API reference](docs/api.md) · [OpenAPI](openapi.yaml) · [Contributing](CONTRIBUTING.md) · [Growth Copilot evidence and proposal library](docs/growth-copilot.md)

Grayling is a self-hosted platform for feature flags, gradual rollouts, and A/B experiments. It also versions and gates AI resources such as prompts, skills, and agent specifications. The control plane, web console, and HTTP API can run as a standalone service or inside another Go service. This repository is named **Grayling**; the application and Go package remain **Grayling**.

The optional, unwired `internal/growth` library reads aggregate evidence and validates
versioned proposal JSON for human review. Missing/mismatched/HOLD evidence and
missing implementation attestations block readiness. For example, a complete
proposal paired with local HOLD evidence returns `needs_data`, even if its text
asks for approval. Readiness never approves or submits an experiment; sample-plan
inputs and attestations do not establish statistical adequacy or real instrumentation.
See the [contract, limits and JSON example](docs/growth-copilot.md#proposal-contract).

![Grayling experiment and staged rollout console](docs/acceptance/web-2026-10-08/23-rollout-plan.png)

## What it includes

- **Feature delivery:** targeting rules, deterministic percentage allocation, immutable published revisions, rollback, approval workflows, audit logs, and tenant-scoped access.
- **Experimentation:** pre-registered A/B tests, exposure and conversion collection, sample-ratio mismatch checks, guardrails, reports, and staged rollout plans with manual or automatic progression.
- **AI resource control:** versioned prompts, skills, and agent specifications; pinned versions, session-level configuration freeze, digest verification, and emergency revocation.
- **Clients:** local evaluation with the Go SDK, remote evaluation with Python and TypeScript SDKs, and an OpenFeature Go provider. An optional proxy supports remote evaluation and gateway integration.
- **Deployment choices:** embedded gonacos or external Nacos 3.2.1+ for configuration distribution; JSON file, pure-Go SQLite, or PostgreSQL for Grayling platform data. The web console is an embedded React SPA whose built assets are committed to the repository, so `make run` works without Node.js.
- **OpenAI client compatibility:** optionally relay Chat Completions, including SSE streams, to a configured OpenAI-compatible upstream by changing only the client's `baseURL`.

## Quick start

Requires **Go 1.27 or newer** and `make`. From a clone of this repository:

```bash
git clone https://github.com/saker-ai/grayling.git
cd grayling
make run
```

Open <http://127.0.0.1:27002/>. `make run` starts in developer mode: the console signs itself in as the global owner, so there is no password to type and no password file is created. That convenience covers every visitor to the port, so it is for local work only — keep the listener off untrusted networks, or start without it (`make run DEV=0`, which generates `data/admin-password` and asks you to sign in with `make show-admin-password`). The password file and runtime data are ignored by Git. The server also prints a one-time global owner token when it creates the first owner. Store that token securely if you plan to use it.

`make run` binds `:27002` on every interface — the console and API are reachable from any network over plaintext HTTP, so put TLS termination in front before exposing them beyond a trusted network (override with `make run RUN_PORT=...`). Embedded gonacos still starts on ephemeral loopback ports. Its default platform store is a JSON file under `data/`. Before starting, `make run` automatically stops any process listening on `RUN_PORT`: it sends SIGTERM, waits up to 5 seconds, then uses SIGKILL if needed. This requires Python 3 and either `lsof` or `ss`. To use SQLite for a persistent single-node installation:

```bash
make run RUN_ARGS='-sqlite data/grayling.db'
```

For multi-replica PostgreSQL deployments, use `-pg` and a shared auth secret; see [platform storage](docs/storage.md) and [multi-replica writes](docs/多副本管理写入.md). Set `-nacos-url` to use an external Nacos service; see [external Nacos](docs/external-nacos.md) for credentials, permissions, and data layout. The storage flag selects Grayling's platform database; Nacos stores the published configuration separately.

## Use Grayling in your application

Flags and segments use a stable ASCII `key` for SDK calls and references. Use optional `displayName` (up to 128 Unicode characters) and `description` (up to 2048) for Chinese or other human-readable text. The console supports creating, editing, displaying and searching this metadata; changes follow the existing draft review and publish lifecycle.

The root package is importable as `github.com/saker-ai/grayling`. `grayling.Start` owns the stores and background jobs, while your service owns the HTTP listener and shutdown sequence. Mount `App.Handler()` for the API and console, or `App.APIHandler()` for the API alone. See the [complete embedding example](docs/embedding.md).

| Client | Evaluation path | Documentation |
| --- | --- | --- |
| Go SDK | Local evaluation from verified snapshots | [`pkg/sdk`](pkg/sdk), [API reference](docs/api.md) |
| OpenFeature Go provider | Local evaluation through the Go SDK | [`pkg/ofprovider`](pkg/ofprovider), [provider compatibility](docs/OpenFeature-Provider-规范验收.md) |
| Python SDK | Remote evaluation with bounded cache and polling | [Python SDK](sdk/python/README.md) |
| TypeScript SDK | Remote evaluation with bounded cache and polling | [TypeScript SDK](sdk/typescript/README.md) |
| Standalone proxy | Remote evaluation, SSE, and event collection relay | [Proxy and API reference](docs/api.md) |

The Grayling API issues separate credentials for administration, SDK reads, and event collection. Do not expose administrative credentials to browser clients.

### OpenAI-compatible Chat Completions

Start with `make run RUN_ARGS='-openai-upstream-url https://api.openai.com/v1 -openai-workspace acme -openai-environment prod'` after creating that workspace, environment, a published JSON flag named `openai-chat`, a pinned prompt, and a model allowlist entry. Point an existing OpenAI client at `http://127.0.0.1:27002/v1`; keep its existing upstream API key and request, including `stream: true`. Grayling evaluates the flag, selects its allowlisted `modelRef`, prepends the pinned `promptRef` as a system message, checks revocation, and forwards the original Bearer key and JSON or SSE response. See [API details](docs/api.md).

## Architecture

```mermaid
flowchart LR
    Admin[Web console / Admin API] --> Core[Grayling]
    Core --> DB[(JSON / SQLite / PostgreSQL)]
    Core --> Nacos[(Embedded gonacos / external Nacos)]
    GoSDK[Go SDK / OpenFeature] -->|snapshot sync and events| Core
    Remote[Python / TypeScript / proxy] -->|evaluation and events| Core
    AI[AI application] -->|pinned resource and revocation checks| Core
```

Published flag and segment documents are immutable; a manifest identifies the active revisions. SDKs verify snapshots before replacing local state. The platform database keeps experiments, tokens, audit entries, events, and AI resource indexes. [Technical design](docs/技术方案.md) explains the publication and evaluation contracts.

## Deployment notes

- The GORM SQLite and PostgreSQL schemas do not read the former pgx schema. Plan an explicit migration for an existing installation; see [storage](docs/storage.md).
- External Nacos mode stores AI versions in Nacos Config, not in the native Nacos AI Registry. It does not migrate embedded gonacos data automatically; see [external Nacos](docs/external-nacos.md).
- Use HTTPS for a console or management API reachable beyond loopback. Keep embedded gonacos listeners private and protect production auth secrets. See the [threat model](docs/threat-model.md).
- Native Nacos SDK access to embedded gonacos has its own compatibility and network constraints; see the [client matrix](docs/Nacos原生客户端.md). Validate capacity and joint backup/restore with your deployment topology before production use.

## Development and tests

```bash
make build
GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test -short ./ ./cmd/... ./internal/... ./pkg/... ./web
make sdk-test
make license-check
```

`GOWORK=off` prevents a parent Go workspace from overriding the pinned gonacos dependency. The fast test command skips environment-dependent acceptance suites. Full integration tests can use Docker or explicit PostgreSQL/Redis test endpoints; see [test guidance](CONTRIBUTING.md) and the [implementation record](docs/实施状态.md). CI covers build, vet, race tests, PostgreSQL integration, SDK tests, and license evidence.

### Web console

The console is a React 19 + TypeScript SPA built with Vite, organized as a pnpm workspace of three packages: `web/` (Vite app, shell, hooks, and the Go embed), `web-pages/` (page, editor, and workbench components), and `web-client/` (fetch client and API types). Working on the frontend requires **Node.js 26+** and **pnpm 10**. `make build` runs the frontend build before compiling the binaries, and the resulting `web/static/` tree is embedded and committed to git — `make check-static` (also run in CI) fails if the committed tree drifts from a fresh build. For console iteration run `make dev-backend` (the normal server) and `make dev-frontend` (Vite dev server with hot reload, proxying the API to the backend) side by side. Frontend tests are vitest suites (`pnpm -r test`); `go test ./web/` drives the same suites plus Go-side preset re-validation.

## Documentation

| Topic | Guide |
| --- | --- |
| API endpoints | [API reference](docs/api.md) · [OpenAPI specification](openapi.yaml) |
| Embedding in Go | [Embedding guide](docs/embedding.md) |
| Storage | [JSON, SQLite, and PostgreSQL](docs/storage.md) |
| External Nacos | [Setup and limitations](docs/external-nacos.md) |
| Product and console roadmap | [Complete implementation plan](docs/产品与Web体验完整实施方案.md) (Chinese) |
| Growth experiments | [Scope](docs/增长实验规划.md) · [Domain implementation plan](docs/增长实验实施方案.md) (Chinese) |
| Admin console redesign | [UI/UX plan](docs/Web控制台重设计实施方案.md) · [Page specifications](docs/Web控制台页面规格与交付清单.md) (Chinese) |
| Security | [Threat model](docs/threat-model.md) · [Vulnerability reporting](SECURITY.md) |
| Design and verification | [Technical design](docs/技术方案.md) · [Implementation record](docs/实施状态.md) |

## Contributing and license

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and checks, and [SECURITY.md](SECURITY.md) for private vulnerability reports.

Grayling is licensed under [Apache-2.0](LICENSE). Third-party attribution and the pinned dependency license inventory are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and [`third_party/licenses.json`](third_party/licenses.json). Run `make release-check` before redistributing binaries.

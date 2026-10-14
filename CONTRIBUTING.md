# Contributing to Grayling

Thanks for helping improve Grayling. Bug reports, documentation fixes, tests, and focused code changes are welcome. For security issues, use the private channel in [SECURITY.md](SECURITY.md).

## Set up

1. Install Go 1.27 or newer, `make`, Python 3.9+ for the Python SDK, and Node.js 22.6+ for the TypeScript SDK.
2. Clone the repository and run `make build`.
3. Run `GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test -short ./ ./cmd/... ./internal/... ./pkg/... ./web` before changing code. `GOWORK=off` keeps the pinned gonacos version independent of a parent workspace.
4. For the local console, run `make run`; use `make show-admin-password` in another terminal to read the generated password.

The fast tests do not require Docker. PostgreSQL, Redis, Traefik, and cluster acceptance tests may need Docker or explicit test endpoints. Use `GRAYLING_TEST_PG_DSN` for an existing PostgreSQL server; the test harness creates isolated databases. Do not point tests at production data. The repository's [Go quality workflow](.github/workflows/go-quality.yml) shows the CI commands.

## Make a change

- Open an issue for a large behavior change so its scope and API can be discussed. Small fixes can go straight to a pull request.
- Keep changes focused. Add tests for changed behavior and update the relevant documentation or OpenAPI contract.
- Format Go code with `gofmt`; run `GRAYLING_TEST_PG_SKIP=1 GOWORK=off go test -short ./ ./cmd/... ./internal/... ./pkg/... ./web`, `GOWORK=off go vet ./...`, and `git diff --check`.
- If SDK behavior changes, run `make sdk-test`. If dependencies or distributed artifacts change, run `make license-check` and `make release-check`, then review any changes to [third-party notices](THIRD_PARTY_NOTICES.md).
- Never commit credentials, runtime data, generated admin passwords, or local database files. The default `data/` directory is Git-ignored.

## Pull requests

Describe the user-visible change, why it is needed, how it was tested, and any migration or deployment impact. Link a related issue when there is one. Maintainers may ask for a smaller scope or additional verification before merging.

By contributing, you agree that your contribution is licensed under the repository's [Apache-2.0 license](LICENSE).

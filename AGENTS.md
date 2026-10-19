# AGENTS.md

## Project

Ratatoskr provides OpenShift release and OLM catalog version logic for Go
applications and exposes it in forms suitable for Renovate.

Keep changes focused. Prefer existing Go libraries and in-process APIs over
shelling out to external tools.

## Repository map

- `cmd/ratatoskr/` — CLI
- `pkg/catalog/` — reusable public OLM catalog package
- `internal/config/` — strict config parsing and source expansion
- `internal/service/` — HTTP API, refresh lifecycle, caching, Renovate responses
- `internal/openshift/` — OpenShift graph, release stream, advisories
- `charts/ratatoskr/` — Helm chart
- `docs/` — user and implementation contracts

## Core invariants

### OLM

- Treat the catalog graph as authoritative. A newer version is not automatically
  an update.
- Updates and channel changes must be justified by `replaces`, `skips`, or
  `skipRange`.
- Do not infer an installed bundle from the current channel; reject ambiguity
  instead of guessing.
- Keep `pkg/catalog` independent of HTTP/Renovate types and cluster access.
- Use Operator Framework and containers/image libraries; do not invoke `opm`,
  `oc-mirror`, Podman, or Docker.

### OpenShift

- Cincinnati is authoritative for update eligibility.
- Release-controller data may enrich release history but must not create
  selectable updates.
- Red Hat advisories provide changelog content.
- Changelog lookup failure must not remove valid Cincinnati candidates.

See `docs/OPENSHIFT_RELEASE_ADVISORIES.md` before changing this behavior.

### Configuration and service

- Keep configuration strict; unknown fields are errors.
- Preserve generated catalog sources versus explicit source overrides.
- Validate relationships using effective values after defaults and fallbacks.
  `maxSnapshotAge` must exceed the effective `refreshInterval + refreshTimeout`;
  preserve overflow-safe validation.
- Apply the same validation at startup and reload; invalid reloads must retain
  the last valid configuration.
- Test defaults, invalid combinations, exact boundaries, fallbacks, and
  overflow.
- Keep refresh work bounded.
- Do not weaken registry authentication, TLS, signature-policy, or refresh-token
  behavior.

## Development

Use the Go version in `go.mod`. Run `gofmt`. Follow `godoclint` for exported
declarations.

Prefer deterministic behavior, `context.Context` for cancellable I/O, and `%w`
for wrapped errors. Avoid new dependencies unless they clearly improve on the
standard library or existing dependencies.

Test changed behavior with assertions for its contract and relevant error cases;
cover concurrency and cancellation when affected. For bug fixes, reproduce the
original failure when practical. Prefer local fixtures, `httptest`, `fstest`, or
equivalent test doubles over live external services. Explain material testing
limitations in the PR.

Treat exported `pkg/catalog` APIs, CLI flags, HTTP/JSON behavior, config
keys/defaults, Helm values, and Renovate datasource semantics as compatibility
surfaces. Update tests and docs when changing them.

When changing CLI, configuration, API, Helm, or Renovate behavior, update
affected examples in `README.md`, `docs/`, and `config.example.yaml` in the same
change. Keep examples copyable and supported.

## Verification

Run the narrowest relevant checks while iterating, then the applicable CI checks
before finishing.

Go:

```sh
go mod download
go mod verify
coverage_dir="$(mktemp -d)"
go test -mod=readonly -count=1 -race -tags=containers_image_openpgp \
  -covermode=atomic -coverpkg=./... -coverprofile="$coverage_dir/coverage.out" ./...
go tool cover -func="$coverage_dir/coverage.out" > "$coverage_dir/coverage.txt"
go tool cover -html="$coverage_dir/coverage.out" -o "$coverage_dir/coverage.html"
cat "$coverage_dir/coverage.txt"
printf 'Coverage output: %s\n' "$coverage_dir"
CGO_ENABLED=0 go build -tags=containers_image_openpgp -trimpath -o /tmp/ratatoskr ./cmd/ratatoskr
golangci-lint run --build-tags=containers_image_openpgp
```

Do not omit `containers_image_openpgp` when validating the production build.

`just coverage` (also `just test-race`) runs the same coverage checks. CI
publishes native Go reports in its summary and a 14-day artifact. Coverage
includes the CLI and measures statement execution, not assertion quality or
branch correctness. Review changed behavior using the PR diff and reports; there
is no numeric coverage gate or custom coverage tooling.

Helm:

```sh
helm lint --strict charts/ratatoskr --values .github/fixtures/helm/valid-values.yaml
helm lint --strict charts/ratatoskr --values .github/fixtures/helm/explicit-source-values.yaml
```

For chart changes, run the strict lint fixtures and mirror the affected
render/API-validation cases from the Helm CI job. `ct lint` enforces chart
version bumps.

Bump `charts/ratatoskr/Chart.yaml` when the packaged chart changes, including
templates, values/defaults, schema, chart metadata, or chart-shipped files. Do
not bump it for application-only, repository-only, or documentation-only changes
that do not alter the chart package.

For `Containerfile` or container-build changes, verify an OCI image build before
finishing.

Do not claim a check passed unless it was actually run.

## Releases and commits

- Releases are managed by semantic-release from `master`; release notes are
  published to GitHub Releases. The Node/pnpm files are release tooling only.
- When changing release behavior, inspect `release.config.cjs`, `package.json`,
  and `.github/workflows/release.yaml` together.
- Use Conventional Commits and PR titles:
  `type(scope): description` or `type: description`.
- Supported types include `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`,
  `refactor`, `revert`, `style`, and `test`. Follow the default Conventional
  Commits release rules: `feat` triggers minor, `fix` and `perf` trigger patch,
  and breaking changes trigger major releases. Other types do not trigger releases
  by default. Select commit types from actual changes, not from AI-generated
  suggestions or desired release outcomes. Keep release triggering and
  release-note visibility separate.
- Keep commits and PRs tightly scoped. Before finishing, inspect the diff,
  remove temporary artifacts, and report only verification actually performed.

## Task scope

- Implement only what is necessary to satisfy the task's acceptance criteria.
- Prefer the smallest complete, maintainable change.
- Do not expand scope to unrelated improvements.
- Report unrelated findings separately without fixing them.
- Stop when the acceptance criteria are verified.
- For larger tasks, propose independently verifiable subtasks before
  implementation.

## Security and threat model

Assume registry/catalog data, upstream HTTP responses, configuration, and client
input are untrusted. Treat credentials, registry tokens, TLS material, and
generated auth files as secrets.

- HTTP clients may select only configured catalogs and source IDs; do not turn
  the service into an arbitrary image or HTTP proxy.
- Preserve TLS verification, signature-policy, authentication, and refresh-token
  protections.
- Do not leak credentials or sensitive headers through logs, errors, metrics, or
  responses.
- Bound external input, response sizes, concurrency, retries, and refresh work.
- Failed refreshes or invalid reloads must not replace known-good state.
- Preserve least-privilege container and Kubernetes defaults.

For changes to authentication, registry access, signature verification, external
destinations, HTTP exposure, config ingestion, or filesystem handling, assess
the threat-model impact and add negative tests where practical.

Never commit credentials, tokens, keys, generated auth files, or other secrets.
Do not turn development-only insecure settings into production defaults.

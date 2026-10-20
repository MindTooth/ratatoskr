# Issue #104: OLM-compatible skipRange execution plan

Research and implementation date: 2026-10-21.
Scope: [issue #104][issue], pinned operator-registry v1.74.0 and blang/semver/v4 v4.0.0.
Repository baseline: `68abe5ce223c8a58b9f5ea54d3d17a1733f74791`.

## Implementation status

Implemented the plan below. The new regression tests reproduced both baseline
failures before the production change. Focused race tests and the full
`just coverage` suite passed with Go 1.26.3; total statement coverage was 81.2%.
Module download/verification, the production OpenPGP-tagged static build,
golangci-lint v2.15.0, and whitespace checks passed. Independent diff review
reported no issues. No chart or container-build inputs changed.

## Outcome and scope

Use the pinned OLM range parser for both catalog ingestion and graph evaluation.
Reject parser errors and unsafe OR branches before a catalog can become a
snapshot. Apply exactly the same range semantics to version updates and channel
transitions, while preserving existing strict version parsing, ordering,
installed-bundle resolution, and refresh publication rules.

This is one focused application fix. No new dependency version, public field,
configuration option, cache, full FBC validator, or Helm change is needed.

## Repository behavior traced

| Owner | Current behavior | Planned change |
| --- | --- | --- |
| `pkg/catalog/catalog.go`: `Reader.Read`, `readExtracted`, `ReadFS`, `readFS` | Both entry points share metadata ingestion; failures return a nil snapshot. | Preserve this path and its cancellation/concurrency handling. |
| `pkg/catalog/catalog.go`: `addMeta`, `olm.channel` case | Decodes entries and stores the channel without validating ranges; later same-name records can overwrite it. | Validate every nonempty range before storing the channel. |
| `pkg/catalog/resolve.go`: `rangeMatches` | Uses Masterminds constraints with prereleases enabled. | Use the shared blang parsing helper and strict `blang.Parse` for the bundle version. |
| `successors`, `channelUpgradeHeads` | Both call `rangeMatches`; `ChannelHeads` also reaches it through `successors`. | Keep traversal code unchanged; test these consumers. |
| `internal/service/service.go`: `refresh` | Builds privately; publishes only after a complete successful read and context check. Failures retain the old snapshot. | Add malformed-range coverage to the existing lifecycle test; no expected production service change. |
| CLI and v1/v2 HTTP/Renovate handlers | Delegate to the public catalog query methods. | Inherit the fix without changing flags, URLs, response shapes, or adapters. |

## Ordered implementation

### 1. Establish the contract with regression tests

- Add `ReadFS` fixtures with complete package/channel/bundle metadata and
  `skipRange: "^1.0.0"`. Assert an error and a **nil** snapshot; this must fail
  against the baseline. Include a malformed entry alongside valid entries so
  partial acceptance cannot pass.
- Add matcher tests with fixed expected results from the matrix below. The caret
  case against `1.1.0` must fail against the baseline too.
- Extend the existing service lifecycle test with a real `Reader.ReadFS` failure
  caused by an invalid range. Do not substitute an injected generic error.

Gate: reproduce both original gaps before changing production code. Existing
prerelease coverage must remain green after the fix.

### 2. Put parsing and validation in the responsible layer

- In `pkg/catalog/resolve.go`, add one private
  `parseSkipRange(string) (blang.Range, error)` helper using the algorithm in
  “Upstream parser defect and narrow containment” below. Its input is a nonempty
  expression; empty/absent fields remain caller-level no-edge cases. Keep the
  original expression and predicate; do not normalize whitespace or translate
  npm syntax. Briefly explain the OR guard with the concrete upstream defect.
- In the `olm.channel` branch of `addMeta`, validate **every** entry whose raw
  `SkipRange != ""` before mutating the package's channel map. Wrap failures with
  package, channel, entry, and `skipRange` context, using `%w` for parser errors.
  A nonempty whitespace-only value is an error. Validate even if a `replaces` or
  `skips` edge would otherwise make that range unnecessary.
- In `rangeMatches`, keep empty expression/version as false, parse the raw version
  with `blang.Parse`, call the shared helper, and return false for any error.
  Evaluate the predicate only after successful parsing. This also safely handles
  caller-constructed public `Package` values that bypass `Reader`.
- Retain Masterminds for all existing sorting/channel-suffix uses. Promote the
  already pinned blang v4.0.0 requirement from indirect to direct in `go.mod`.
  Avoid unrelated dependency or `go.sum` churn.

Gate: ingestion and evaluation have one range policy. No changes to
`sortVersions`, `bundleLess`, `channelSuffix`, graph traversal, exported struct
layout, or service publication logic.

### 3. Prove each acceptance criterion

| Acceptance criterion | Required proof |
| --- | --- |
| Reject invalid catalog ranges | `reader_test.go`: JSON and YAML fixtures; empty/absent accepted; whitespace-only, caret, tilde, incomplete comparator, malformed OR rejected with nil snapshot and useful context. Exercise parse concurrency 1 and 4. |
| Do not hide malformed records | A malformed channel record followed or preceded by a valid same-name record must fail; include a multi-file fixture with multiple workers. Do not introduce a separate duplicate-record policy. |
| Exact OLM matching | `resolve_test.go`: fixed boolean assertions from the matrix, including invalid raw versions and unsafe OR inputs. Assert rejection of the OR inputs even where the first branch would match and mask the defect. |
| Version updates and channel transitions | Test range-only entry edges through `VersionUpdates` in direct/reachable modes, `ChannelReleases`, and its `ChannelUpdates` projection in next/reachable selections; assert exact versions, channels, and terminal bundle names. Include positive and excluded boundary/prerelease cases. |
| Graph consumers stay consistent | Include a second hop via `replaces`; assert direct versions stop after the first hop, reachable versions include the second hop, channel transitions identify its terminal bundle, and `ChannelHeads` agrees. Keep target versions outside their own skip ranges. |
| Failed refresh retains known-good data | Extend `TestCatalogLastKnownGoodLifecycle`: parser-error and unsafe-OR refresh failures retain the same snapshot, `GeneratedAt`, `LastSuccess`, and existing HTTP bodies; `LastError` records failure, the refresh slot is released, normal expiry still applies, and a later valid refresh recovers. Existing routes cover v1/v2 and readiness. |
| Compatibility is preserved | Keep exact current-bundle selection/ambiguity behavior, independent `replaces`/`skips` matching, deprecation behavior, and version/channel ordering tests passing. Do not reject all bundle versions at ingestion as an incidental change. |

Use local `fstest.MapFS` and existing service test helpers. For query fixtures,
versions `3.0.0` and `4.0.0` work as first and terminal targets for bounded ranges
below `2.0.0`; this avoids accidentally constructing self-edges in the tests.
Use fixed expected outputs rather than only comparing production code against
the same dependency it calls. No registry or cluster is required.

### 4. Document, verify, and stop

- Update the public `Reader`/`ReadFS` documentation, the reusable-package section
  in `README.md`, and the catalog graph/update contract in `docs/API.md`.
  Explain blang-compatible ranges, whole-refresh rejection, retained known-good
  state, and prerelease boundaries with the copyable bounded example
  `>=1.0.0 <2.0.0`. Do not promise npm caret/tilde compatibility or rejection of
  every conventionally malformed expression; see the pinned-parser quirks below.
- No affected configuration examples, chart package, or container-build files
  need modification. No chart version bump is warranted.
- Use Go **1.26.3**, as specified by `go.mod`, and run `gofmt` on changed Go files.
  Start with:

  ```sh
  export GOTOOLCHAIN=go1.26.3
  go version
  go test -mod=readonly -count=1 -race -tags=containers_image_openpgp ./pkg/catalog ./internal/service
  ```

- Then run the repository's applicable Go gates:

  ```sh
  go mod download
  go mod verify
  just coverage
  CGO_ENABLED=0 go build -tags=containers_image_openpgp -trimpath -o /tmp/ratatoskr ./cmd/ratatoskr
  golangci-lint run --build-tags=containers_image_openpgp
  git diff --check
  ```

  `just coverage` runs the required uncached full-module race/atomic coverage
  suite and generates native text/HTML reports. Use the CI-pinned lint version
  (currently v2.15.0). Helm/OCI validation is unnecessary unless implementation
  unexpectedly changes their inputs.
- Review the final diff against each acceptance row and inspect the native
  coverage report for the changed behavior; there is no numeric coverage target.
  Require a fresh independent review of parser/OR handling, both graph consumers,
  and snapshot retention. Resolve material findings and rerun affected checks;
  repeat until there are no unresolved acceptance or correctness findings.
  Keep one focused PR, suggested title: `fix(catalog): use OLM-compatible skipRange parsing`.

Stop when the matrix, integration behavior, snapshot retention, docs, and Go gates
are verified. An unavailable required tool/check is a reported limitation, never
a claimed pass.

## Primary-source conclusions

- OLM defines file-based catalog channel entries as the source of truth for upgrade
  edges. Its official update-graph documentation explicitly identifies blang/semver
  and applies the installed-version-in-skipRange rule both to direct upgrades and
  cross-channel updates. Versions covered by a range need not remain in the catalog.
  Prefer bounded examples; the docs recommend avoiding unbounded ranges. [OLM graph
  documentation][graph]
- operator-registry v1.74.0 validates each nonempty `Bundle.SkipRange` using
  `semver.ParseRange`. It does not use Masterminds constraints. [Bundle.Validate,
  lines 407–411][model]
- Reusing the entire `Bundle.Validate` or `ConvertToModel` would also enforce
  parent relationships, package properties, image availability, name normalization,
  graph structure, and other unrelated constraints. There is no separate narrow
  skipRange validator in these paths: reuse the already pinned blang parser directly.
  [model validation][model], [ConvertToModel][convert]
- Range comparison uses strict `semver.Parse` for bounds and ordinary SemVer
  precedence for prereleases; build metadata has no precedence.
  [range implementation][range], [version implementation][version]
- Keep the existing strict installed-version policy: use `blang.Parse(rawVersion)`
  when evaluating skipRange. `ParseTolerant` would additionally accept a leading
  `v`, abbreviated versions, leading zeros, and outer whitespace. The pinned
  operator-registry FBC converter uses strict `Parse`. OLM's separate runtime
  registry resolver uses `ParseTolerant`; matching that additional tolerance is
  outside this issue and would expand Ratatoskr's current input acceptance.
  [converter, lines 141–147][convert], [OLM runtime, lines 234–237 and 289–291][resolver],
  [strict and tolerant parser definitions][version]

## Actual pinned-parser behavior

These findings were checked with the exact fetched v4.0.0 implementation in an
isolated Go program; they are not inferred from generic SemVer range conventions.

| Input / condition | blang v4.0.0 behavior |
| --- | --- |
| `>=1.0.0 <2.0.0` | Includes 1.0.0, 1.5.0-beta.1, and 2.0.0-rc.1; excludes 1.0.0-rc.1 and 2.0.0. |
| `>=1.0.0-0 <2.0.0-0` | Includes 1.0.0-rc.1; excludes 2.0.0-0 and 2.0.0-rc.1. |
| `>1.0.0 <=2.0.0` | Excludes 1.0.0; includes 2.0.0. |
| `=1.2.3+build.1` | Matches 1.2.3 and 1.2.3+other. |
| Space-separated constraints; spaced operators | AND; ordinary ASCII spaces are tolerated. |
| `<1.0.0 || >=2.0.0` | OR, with AND binding more tightly. |
| `1.x`, `1.2.x` | Supported lowercase wildcards. |
| `^1.0.0`, `~1.0.0`, comma clauses, parentheses | Rejected. |
| `>=v1.0.0`, `>=1.0`, `*`, `x`, `1.X`, `1.*` | Rejected. |
| Empty string or whitespace-only nonempty input | ParseRange returns an error; absent/empty skipRange must remain a caller-level no-edge case. |
| Tab separating two constraints | Rejected; do not normalize arbitrary whitespace into accepted grammar. |

Sources: [blang range source][range], [blang documented grammar][readme], and the local
probe artifacts below.

## Upstream parser defect and narrow containment

`ParseRange(">=1.0.0 || || <2.0.0")` succeeds but returns a predicate that panics
when its left side does not match. The same happens for middle branches containing
a lone `x` or `|`. `splitAndTrim` discards one-character tokens, and
`splitORParts` does not reject internal empty branches. An `err == nil` check
alone therefore does not safely validate untrusted ranges. [implementation,
lines 112–173 and 205–230][range]

Recommended shared private parsing helper:

1. Parse the original expression with `blang.ParseRange`; preserve its error and
   reject a nil predicate defensively.
2. For expressions containing `||`, split the raw expression on that literal
   separator and parse every raw clause with the same parser; reject any error or
   nil clause predicate.
3. Return the original full predicate unchanged.

The original parse must happen first: splitting and combining clauses directly
would incorrectly start accepting `>=1.0.0||<2.0.0`, which the pinned parser rejects.
Testing only `TrimSpace(clause) == ""` misses the token-dropped branches.
The wrapper was probed across 34 expressions and eight versions per accepted
expression: it rejected all three panic-producing disjunctions, preserved the
sampled accepted-range results, and allowed no predicate panic. This is a focused
regression probe, not proof over arbitrary input.

This adds one conditional scan and reparses each OR clause once. Work remains
proportional to input length and clause count, with a constant-factor parse cost.
There is no evidence requiring caching or compiled predicates in the snapshot.

Other upstream quirks remain intentional non-goals: `1.0.0 - 2.0.0` is accepted
as contradictory equality clauses, `1.0.0 - 1.0.0` matches 1.0.0, and `!=1.x`
matches nothing because wildcard expansion produces contradictory AND bounds.
Do not describe these as rejected unless deliberately adding a new grammar policy.
The issue should define malformed input as parser errors plus unsafe disjunctions;
full conventional range-syntax policing is a separate change. [range implementation][range]

## Evidence and limitations

- Exact source downloads:
  `/private/tmp/ratatoskr-issue104-probe/upstream/v4/range.go`,
  `/private/tmp/ratatoskr-issue104-probe/upstream/v4/semver.go`.
- Runnable copies in the same root change only the package name to `main`;
  `probe.go` contains the matrix and proposed wrapper.
- Complete output: `/private/tmp/ratatoskr-issue104-probe/output.txt`.
  Initial uncaught panic output:
  `/private/tmp/ratatoskr-issue104-probe/initial-panic-output.txt`.
- Probe command, from that directory:
  `env GOWORK=off GOTOOLCHAIN=local GO111MODULE=off GOCACHE=/private/tmp/ratatoskr-issue104-probe/cache go run range.go semver.go probe.go`.
- Executed with installed Go 1.27.3 on darwin/arm64. This is parser research,
  not repository verification with required Go 1.26.3.
- Repository tests, service refresh regression, and production build were not
  executed during initial research; subsequent implementation verification is
  recorded above.

[issue]: https://github.com/MindTooth/ratatoskr/issues/104
[graph]: https://github.com/operator-framework/olm-docs/blob/0710715cb969c1d35a36557bf28966c09e14c525/content/en/docs/Concepts/olm-architecture/operator-catalog/creating-an-update-graph.md#skiprange
[model]: https://github.com/operator-framework/operator-registry/blob/v1.74.0/alpha/model/model.go#L380-L448
[convert]: https://github.com/operator-framework/operator-registry/blob/v1.74.0/alpha/declcfg/declcfg_to_model.go#L141-L147
[range]: https://github.com/blang/semver/blob/v4.0.0/v4/range.go
[version]: https://github.com/blang/semver/blob/v4.0.0/v4/semver.go
[readme]: https://github.com/blang/semver/blob/v4.0.0/README.md#ranges
[resolver]: https://github.com/operator-framework/operator-lifecycle-manager/blob/630d63a56fb43470075b5af1fe1f35176ee296b0/pkg/controller/registry/resolver/source_registry.go#L234-L291

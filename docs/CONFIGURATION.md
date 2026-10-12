# Configuration

The configuration has a short path for Red Hat's standard operator catalogs
and an exact-source path for mirrors and custom catalogs. Both paths resolve to
the same source model before the service starts or reloads.

## Standard catalogs

Select one or more OpenShift catalog versions with `channels`:

```yaml
channels:
  - "4.22"
  - v4.23
```

Here, `channel` means an OpenShift catalog image tag, not an operator package
channel such as `stable`. Both `4.22` and `v4.22` are accepted and normalize to
`v4.22`. Quote a version without `v`; otherwise YAML treats it as a number and
the service rejects it with a corrective error.

Each channel creates these sources by default:

| Catalog | Generated source ID | Image |
| --- | --- | --- |
| `redhat` | `redhat-v4.22` | `registry.redhat.io/redhat/redhat-operator-index:v4.22` |
| `certified` | `certified-v4.22` | `registry.redhat.io/redhat/certified-operator-index:v4.22` |
| `community` | `community-v4.22` | `registry.redhat.io/redhat/community-operator-index:v4.22` |

Select a subset when all three are not needed:

```yaml
channels: ["4.22"]
catalogs: [redhat, community]
```

An omitted or empty `catalogs` list selects all built-in catalogs. Catalog and
channel order determines source order. Duplicate channels, including `4.22`
combined with `v4.22`, are rejected after normalization.

## Platform

Every source defaults to `linux/amd64`, independent of the host that runs the
service:

```yaml
platform: linux/amd64
```

The field can normally be omitted. Set it globally only when all catalogs use
another OCI platform. An explicit source can override the global value:

```yaml
platform: linux/amd64
sources:
  - id: private-arm
    image: registry.example.com/private/index:latest
    platform: linux/arm64
```

Platforms use `os/architecture[/variant]` form.

## Explicit sources and overrides

The original source form remains supported:

```yaml
sources:
  - id: private-v4.22
    image: registry.example.com/private/index:v4.22
```

Explicit sources can be used alone or together with generated sources. When an
explicit source ID matches a generated ID, it replaces that complete source:

```yaml
channels: ["4.22"]
sources:
  - id: community-v4.22
    image: mirror.example.com/community-operator-index:v4.22
```

Replacement is whole-record rather than field-by-field. Include `image`, and
include `platform` only when it should differ from the global platform. A new
ID adds a custom source after the generated sources.

## Settings

| Field | Default | Meaning |
| --- | --- | --- |
| `channels` | none | Catalog versions in `major.minor` or `vmajor.minor` form. |
| `catalogs` | all built-ins | Built-in catalog subset. Requires `channels`. |
| `sources` | none | Exact source overrides and custom sources. |
| `platform` | `linux/amd64` | Platform inherited by generated and explicit sources. |
| `listenAddress` | `:8080` | HTTP listen address. A reload requires a restart to change it. |
| `debug` | `false` | Include query strings, user agents, and refresh progress in logs. |
| `refreshInterval` | `6h` | Scheduled catalog refresh interval. |
| `refreshTimeout` | `30m` | Timeout for one catalog refresh. |
| `maxSnapshotAge` | `24h` | Maximum acceptable age of a complete datasource snapshot. Must be positive and greater than `refreshInterval + refreshTimeout`. |
| `parseConcurrency` | `2` | Concurrent FBC metadata parsers. |
| `signaturePolicy` | environment default | Explicit containers/image policy path. |
| `refreshTokenFile` | none | Bearer token file that enables refresh-control endpoints. |
| `openshiftGraphURL` | official graph | Trusted OpenShift update-graph override. |
| `openshiftTimeout` | `30s` | OpenShift update-graph request timeout. |

At least one channel or explicit source is required. Configuration decoding is
strict: unknown fields, malformed values, duplicate IDs, and multiple YAML
documents are rejected. During live reload, an invalid replacement leaves the
last valid configuration active.

`maxSnapshotAge` must strictly exceed the scheduled refresh interval plus one
refresh timeout, leaving room for a refresh to complete before retained data
expires. With the defaults, it must exceed `6h30m`; equality is rejected.
Nonpositive `refreshInterval` values use the service's `6h` fallback for this
check. Startup and live reload apply the same validation; an invalid reload
preserves the active configuration.

Complete datasource results are retained in memory. Catalog refreshes replace
one whole source snapshot at a time; a failed pull or parse preserves the previous
snapshot and its original generation time. OpenShift requests still fetch upstream
on every lookup and retain only successful, complete results for the exact channel,
architecture, installed version, and lag. A discovery failure can serve that exact
previous result while it is within `maxSnapshotAge`.

Changing `signaturePolicy` or `parseConcurrency` queues a catalog refresh. An
active read using the previous configuration cannot publish a snapshot or update
failure status; it is followed by a read using the latest configuration, unless
the service is shutting down. Already published snapshots retain their original
generation time until a new read succeeds.

The maximum age policy applies at each read, including after a live policy reload.
A snapshot is acceptable through its maximum age; older or missing data returns `503`.
Failed refreshes never renew the timestamp. Successful empty datasets remain valid.
Catalog status reports generation time (`lastSuccess`), `snapshotAgeSeconds`,
`stale`, and policy-aware `available`; readiness requires at least one acceptable
catalog snapshot. Successful datasource and catalog inspection responses expose
`X-Ratatoskr-Generated-At` and `X-Ratatoskr-Snapshot-Age-Seconds` headers.

Retention does not survive process restarts: catalogs return `503` until their
initial refresh succeeds, and OpenShift lookups return `503` if their first lookup
fails. OpenShift retention is bounded to 128 distinct lookups, evicting the oldest
successful lookup when full; an evicted lookup must succeed upstream again.
No disk snapshot storage or deployment high availability is provided.

See [`config.example.yaml`](../config.example.yaml) for a copy-ready file and
the [getting-started guide](GETTING_STARTED.md) for authentication, signature
policy, container, and Kubernetes setup.

## Migration from source-only configuration

Existing source-only configuration remains valid. A standard configuration:

```yaml
sources:
  - id: redhat-v4.22
    image: registry.redhat.io/redhat/redhat-operator-index:v4.22
  - id: certified-v4.22
    image: registry.redhat.io/redhat/certified-operator-index:v4.22
  - id: community-v4.22
    image: registry.redhat.io/redhat/community-operator-index:v4.22
```

This configuration can be reduced to:

```yaml
channels: ["4.22"]
```

The generated IDs and images are identical. The intentional behavior change is
that an omitted platform now resolves to `linux/amd64` instead of the runtime
host platform. Set a global or per-source platform when another architecture is
required.

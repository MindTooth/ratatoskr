![Ratatoskr, a squirrel carrying a letter beneath Yggdrasil](assets/images/ratatoskr-banner.jpg)

# ratatoskr

`ratatoskr` provides OpenShift release and OLM catalog version logic
for Go applications. Its `ratatoskr` service exposes OpenShift
cluster releases and file-based operator catalog updates to Renovate. Cluster
releases come from the official OpenShift update graph. Operator catalogs are pulled
through the upstream Operator Framework image libraries; the service does not
execute `opm`, `oc-mirror`, or a container client.

Start with [the getting-started guide](docs/GETTING_STARTED.md) for local,
container, and pod deployment instructions, including registry authentication
and signature-policy setup. The [configuration guide](docs/CONFIGURATION.md)
defines channel expansion, defaults, and exact-source overrides. See the
[HTTP API guide](docs/API.md) for every endpoint, parameter, response, and
copy-ready `curl` example.

The service returns only versions connected to the current bundle by declared
`replaces`, `skips`, or `skipRange` edges. It deliberately does not treat every
newer catalog version as an upgrade.

## About the name

Ratatoskr is the squirrel of Norse mythology who travels along Yggdrasil, the
world tree, carrying messages between the eagle in its branches and Níðhöggr
beneath its roots. His journey appears in [*Grímnismál*, stanza 32](https://sacred-texts.com/neu/poe/poe06.htm)
and [*Gylfaginning*, chapter 16](https://en.wikisource.org/wiki/The_Prose_Edda_(1916_translation_by_Arthur_Gilchrist_Brodeur)/Gylfaginning).

For this project, the name celebrates the messenger: a small, tireless traveller
through a branching world, bringing news from one place to another. `ratatoskr`
follows the paths through OpenShift release graphs and operator catalogs,
carrying version information to the tools that need it. The tree is our
metaphor for connected releases; the squirrel is our reminder to keep the
messenger small and useful.

## Status

The service supports direct OpenShift cluster-release updates, native catalog
refresh, graph-filtered bundle-version responses, and graph-validated channel
transitions. A channel update needs the installed bundle identity or an
unambiguous installed version; the service deliberately refuses to guess.

## Configuration

```yaml
channels:
  - "4.22"
```

This creates the Red Hat, certified, and community catalogs for version `4.22`.
Use v2 URLs such as `/v2/catalogs/redhat/4.22`; `v4.22` is also accepted in
the path. Catalog images default to `linux/amd64` on every host.

Registry credentials are read from the standard containers/image locations. In
Fish, a mounted Docker config can be selected with:

```fish
set -gx REGISTRY_AUTH_FILE /var/run/registry-auth/auth.json
```

Select a catalog subset, change the global platform, or retain exact source
control when needed:

```yaml
platform: linux/amd64
channels: [v4.22]
catalogs: [redhat, community]
sources:
  - id: community-v4.22
    image: mirror.example.com/community-operator-index:v4.22
```

The explicit source replaces the generated source with the same ID. See the
[configuration guide](docs/CONFIGURATION.md) for precedence and migration.

## Run

```fish
go run ./cmd/ratatoskr serve --config ./config.yaml
```

Each HTTP request is logged with method, path, response status, response size,
duration, and remote address. Add `--debug` (or `debug: true` in the
configuration) to also log query strings and user agents, plus catalog refresh
progress:

```fish
go run ./cmd/ratatoskr serve --config ./config.yaml --debug
```

The server automatically reloads a changed configuration file every five
seconds, including Kubernetes ConfigMap mounts. Invalid updates leave the
last valid configuration active. Set `--config-reload-interval 0` to disable
this behavior; changing `listenAddress` still requires a restart.
Changing `signaturePolicy` schedules a new catalog read; an active read using the
previous policy cannot publish its result after reload.

For a one-off query, pass the same selection explicitly:

```fish
go run ./cmd/ratatoskr query \
  --image registry.redhat.io/redhat/community-operator-index:v4.20 \
  --package strimzi-kafka-operator \
  --channel stable \
  --current-version 0.47.0
```

After the initial refresh:

```fish
curl --fail-with-body \
  'http://localhost:8080/v2/catalogs/redhat/4.22/packages/openshift-gitops-operator/channel-updates?currentChannel=gitops-1.20&currentBundle=openshift-gitops-operator.v1.20.6&selection=next'
```

The response follows Renovate's minimal custom datasource shape:

```json
{"releases":[{"version":"gitops-1.21","digest":"openshift-gitops-operator.v1.21.2"}]}
```

If the installed release is not in the requested channel, the service returns
`{"releases":[]}` with HTTP 200. This means there is no valid update path;
it is not a catalog error.

## Cluster subscription discovery

Run `ratatoskr discover` inside a pod to print OLM subscription channel heads
as JSON. The pod service account needs cluster-wide `list` access to
`subscriptions.operators.coreos.com` and
`packagemanifests.packages.operators.coreos.com`.

Discovery matches the package and CatalogSource name and namespace. An omitted
subscription channel uses the package's default channel; a package with one
channel can use that channel as its implicit default. Subscriptions with an
empty catalog namespace or unresolved channel versions are omitted.

## Renovate

Every Renovate configuration using Ratatoskr must include a fail-safe host rule
for the hostname in its datasource URLs:

```json
{
  "hostRules": [
    {
      "matchHost": "ratatoskr.example",
      "abortOnError": true,
      "abortIgnoreStatusCodes": []
    }
  ]
}
```

Replace `ratatoskr.example` with your service hostname and merge this rule into
the configuration alongside `customDatasources`. Ratatoskr returns `503 Service
Unavailable` when release data cannot be produced. Renovate's
[`abortOnError` rule](https://docs.renovatebot.com/configuration-options/#hostrulesabortonerror)
aborts the run on HTTP or transport failures so a transient outage cannot cause
existing dependency PRs to be closed or rewritten from incomplete data. Keep
5xx statuses out of `abortIgnoreStatusCodes`; the empty list above ignores none.
This rule applies to both OpenShift releases and operator catalogs.

OpenShift cluster releases require only a channel and the installed version:

```json
{
  "customDatasources": {
    "openshift-releases": {
      "defaultRegistryUrlTemplate": "http://ratatoskr.example/v1/openshift-releases/{{packageName}}/updates?currentVersion={{currentValue}}&arch=multi&lag=1",
      "format": "json"
    }
  }
}
```

Use the channel, for example `stable-4.21`, as `packageName`. The response
contains the installed release and only its direct unconditional successors.
`lag=1` withholds the newest successor; the installed release is never removed.

Operator catalog releases use the catalog endpoints:

```json
{
  "customDatasources": {
    "openshift-operators-v4-22": {
      "defaultRegistryUrlTemplate": "http://ratatoskr.example/v2/catalogs/redhat/4.22/packages/{{packageName}}/updates?currentVersion={{currentValue}}&operatorChannel=gitops-1.20&mode=reachable",
      "format": "json"
    }
  }
}
```

Use explicit Renovate versioning, for example `semver` or `loose`.

## Channel upgrades

Channel names are not upgrade edges. The v2 `channel-updates` endpoint returns
a target only when its channel graph has a `replaces`, `skips`, or `skipRange`
entry covering the installed bundle or version. Its `digest` response field is
the companion bundle-state marker Renovate must persist as `currentDigest`.

```fish
ratatoskr channel-query \
  --image registry.redhat.io/redhat/redhat-operator-index:v4.20 \
  --package openshift-gitops-operator \
  --current-channel gitops-1.20 \
  --current-bundle openshift-gitops-operator.v1.20.6
```

```text
GET /v2/catalogs/redhat/4.22/packages/openshift-gitops-operator/channel-updates
    ?currentChannel=gitops-1.20&currentBundle=openshift-gitops-operator.v1.20.6&selection=next
```

## Manual catalog inspection

These endpoints expose compact catalog metadata for manual lookup; unlike the
update endpoints, they are not Renovate datasource responses.

```text
GET /v2/catalogs
GET /v2/catalogs/{catalog}/{version}
POST /v2/catalogs/refresh
POST /v2/catalogs/{catalog}/{version}/refresh
GET /v2/catalogs/{catalog}/{version}/packages?prefix=strimzi&limit=100
GET /v2/catalogs/{catalog}/{version}/packages/{package}?include=channels,bundles,graph
GET /v2/catalogs/{catalog}/{version}/packages/{package}/updates
GET /v2/catalogs/{catalog}/{version}/packages/{package}/channel-updates
GET /v2/sources
GET /v2/sources/{source}/packages/{package}
```

The package response is compact by default. `include=channels` adds channel
heads and deprecation state, `include=bundles` adds bundle metadata, and
`include=graph` adds the raw `replaces`, `skips`, and `skipRange` edges. Exact
custom source IDs use the equivalent `/v2/sources/{source}` routes.

## Go package

Other Go applications can import
`github.com/MindTooth/ratatoskr/pkg/catalog` without starting the
datasource server or accessing a cluster:

```go
reader := catalog.Reader{SignaturePolicy: "/etc/containers/policy.json"}
snapshot, err := reader.Read(ctx, catalog.Source{
    ID:    "redhat-operators-v4.20",
    Image: "registry.redhat.io/redhat/redhat-operator-index:v4.20",
})
if err != nil {
    return err
}
pkg := snapshot.Packages["openshift-gitops-operator"]
if pkg == nil {
    return fmt.Errorf("operator is not in the catalog")
}
versions, err := pkg.VersionUpdates(catalog.UpdateRequest{
    Channel:        "latest",
    CurrentVersion: "1.18.0",
    Mode:           "reachable",
})
```

For unpacked FBC data, use `reader.ReadFS(ctx, catalog.Source{ID: "local"},
os.DirFS("/path/to/configs"))`. Both entry points use the same parser.
`Read` confines config paths and file reads to the extracted image filesystem,
including relative symlinks within that root. `ReadFS` uses the supplied
filesystem's access rules; `os.DirFS` does not confine symlink targets.
`ReadFS` requires `fs.StatFS` with metadata lookup that does not open files;
adapters must preserve that interface. Filesystems exposing only `Open` are rejected.
Catalog metadata and `.indexignore` files must be regular files (or links to
regular files); FIFOs, devices, and other special files are rejected before parsing.
`Snapshot.Packages` exposes channels and bundle metadata; `ChannelHeads` returns
terminal bundles, and `ChannelReleases` resolves graph-valid channel transitions.
`VersionUpdates` includes the current version and graph successors, defaults to
the package's default channel, and retains the existing version ordering.

Both readers apply `olm.deprecations` records after all metadata files have
loaded, regardless of record or file order. Bundle and channel references set
`Bundle.Deprecated` and `Channel.Deprecated` within the named package. Invalid
references and duplicate records for a package fail the read without returning
a partial snapshot. Pending records retain only references, and parsing and
post-processing check cancellation between records and references. Deprecation
messages and package-level deprecation state are not retained; a valid package-level reference does not deprecate its children.
`VersionUpdates` excludes deprecated successor bundles but retains the current
version and traverses deprecated intermediates in reachable mode. Channel
transitions exclude deprecated destination channels and deprecated terminal
bundles, while retaining the current state. Inspection methods and raw graph
entries continue to include deprecated objects.

Build consumers with `-tags=containers_image_openpgp`, as with the datasource.
Image loading uses the existing registry authentication and signature policy
behavior. Consumers own refresh scheduling and snapshot caching. The package
does not import the service or its Renovate response types.

OLM bundle metadata currently includes names, versions, images, and deprecation
fields. Changelog titles and errata enrichment belong to the separate OpenShift
release datasource and are unchanged by this package extraction. Cluster state
collection and Prometheus metrics are outside the package's scope.

## Security

Use a mounted registry-authentication file, an explicit containers/image
signature policy, and a refresh-token file. Refresh endpoints are disabled
until `refreshTokenFile` is configured; callers must then send its contents as
an `Authorization: Bearer` token. The service does not disable TLS verification
or accept unsigned images by default. Do not expose it as an arbitrary image
proxy: the HTTP API can query only configured catalogs and source IDs.

## Limitations

Catalog reachability is not a guarantee that an InstallPlan will succeed. Cluster
dependencies, admission controls, mirrors, and existing state can still block an
upgrade.

## Container

The `Containerfile` builds a static binary and runs it without a shell or
fixed root UID. Mount a writable `/tmp`, a cache volume through `OLM_CACHE_DIR`,
the configuration file, and registry authentication or policy files as needed.

Production Kubernetes/OpenShift deployments can use the [Helm chart](charts/ratatoskr/README.md),
which defaults to two replicas, a PodDisruptionBudget and preferred node
anti-affinity. Each replica retains complete snapshots independently; HA improves
availability while HTTP failure semantics and Renovate's `abortOnError` rule
protect datasource correctness.

## AI use

AI tools may be used to assist development. AI-assisted changes are reviewed
before they are accepted, and responsibility for the resulting code remains
with the project maintainers and contributors.

## License

This project is licensed under the [MIT License](LICENSE).

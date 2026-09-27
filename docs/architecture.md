# Local execution architecture

```text
REST / MCP DTOs (models)
        |
internal/api facade: explicit DTO <-> domain conversion
        |
internal/service: application policy and creation compensation
        |
internal/sandbox: typed Runtime / Process / Filesystem / Cache ports
        |
internal/runtimeio adapter: private native records, identity mapping and leases
        |
Docker engine client or Apple container CLI 1.4.1
```

`internal/sandbox`, the application service and runtime adapters do not import
`models`. Domain records are independent types, not aliases to HTTP structs.
`SandboxID`, `CommandID`, `ImageID`, `Port`, `PublishedPort`, `ResourceLimits`,
process requests/results and durations are used by the actual ports. `models`
remains the stable REST/MCP JSON contract. The image CLI has its own serialization
boundary. Adapter-local `runtimeio` records are not application or HTTP DTOs.

## Identity and creation transactions

The service assigns a public `sbx-...` ID. Only the adapter's repository view
resolves it to the native reference. Existing public IDs are retained; an empty
legacy native-reference column falls back to its unchanged public ID. Process
history stores public sandbox backreferences. Cache preparation returns an opaque
`PreparedImage`; the application sees source root/manifest identity, not a Docker
config ID or Apple cache reference.

Successful native creation returns a bounded `Provisioned` transaction, not a
native ID. Its adapter-owned handle binds compensation to that exact new resource:

1. `Adopt` verifies ownership and native identity and persists image provenance.
2. Any post-create adoption/ownership failure triggers `Rollback` under a separate
   bounded cleanup context, never the canceled request context.
3. If compensation fails, `Recover` retains/corrects ownership and provenance and
   records the original failure in private execution metadata. Recovery failures
   are returned together with the original error, never silently hidden.

Compensation never targets a native reference obtained from an inconsistent row
or an API parameter. Existing sandboxes and historical IDs are not rewritten.
Persistent database failures can also prevent recovery writes; the combined error
must be investigated rather than interpreted as successful cleanup.

## Coherent routing and inventory

`Runtime.Routing` reads running state and published endpoints together. Docker
uses one inspect response; Apple uses one inventory entry. The service coalesces
only concurrent in-flight reads for the same owned sandbox, with a bounded read
context and no completed-result TTL. A subsequent request reads current state.
Stopped/unowned resources and non-TCP main ports do not route.

List reconciliation uses batch ownership maps, not one database query per native
item. Runtime identifiers are translated inside the adapter; the service and API
operate on public identities.

## Owned image store

Pull/import stage, verify and decompress outside the catalog lock. Publication
briefly takes the process mutex and OS file lock, atomically publishes verified
blobs, rereads the current catalog and merges references/platform availability.
Unrelated tag changes are not overwritten. Export snapshots its descriptor under
the lock and performs archive I/O after releasing it. Resolved artifact callbacks
retain the original immutable descriptor; deleting/repointing a tag cannot change
or invalidate the artifact they export while its blobs remain present.

`catalog.json` is authoritative. The derived OCI index is replaced only when its
contents differ, including recovery from an interrupted publication. Read-only
operations do not rewrite an unchanged index. Interrupted commits may leave
unreferenced verified blobs; automatic garbage collection is deliberately absent.

Verification reuse is process-local and bounded to 64 graphs with at most 256
files each. Entries are populated only after digest, size, graph and expanded
layer/diffID checks. File identity, size, modification time and available change
time invalidate reuse; materialization rechecks those fingerprints. Unchanged
prepared images do not repeatedly decompress every layer. This is not a promise
to defeat a privileged host actor capable of manipulating files and metadata.

## Network trust boundaries

The control plane rejects foreign/null Origin and cross-site/same-site browser
Fetch Metadata before handlers run. Origin-less native clients remain supported.
Sandbox proxy traffic is not subject to the control-plane origin policy.

Registry requests use an explicit per-pull transport policy, including injected
transports. Credentials are resolved only for the requested repository. Foreign
descriptor/layer URLs are rejected. Bearer realms must be same-origin or the
known Docker Hub token authority. Only the intended registry may use private or
loopback addresses; permitted public token/CDN hosts are DNS-checked and dialed
using the checked addresses. Cross-host blob CDN requests never carry registry
Authorization/Cookie headers. Environment HTTP proxies are not used by this
default transport because they would bypass the destination checks.

An injected transport remains behind URL/realm/header checks, but its networking
implementation is trusted code owned by the caller/test. DNS address pinning is
provided by the default production transport, not by arbitrary injected code.

Docker Hub and GHCR token/CDN paths are supported. Custom registries with other
cross-origin token services or CDNs are denied by default rather than implicitly
trusted. Explicit local registry addresses remain supported. Tests inject with
`images.WithTransport`, which cannot remove the surrounding policy.

OCI images are filesystem/configuration artifacts, not running-process snapshots.
There is no checkpoint or snapshot portability API.

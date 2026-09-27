# OpenSBX-owned OCI images

Images belong to the common local OpenSBX store, not the selected runtime's image
database. REST/MCP image routes/tools retain their names and JSON fields. `id`
means the **canonical source root descriptor digest**, either an index or a
manifest. A selected platform manifest digest and Docker's native config digest
are different identities. Tags reference roots; they are not execution handles.
No native-image inventory is automatically exposed or adopted. Legacy config-ID
lookups are not aliases for newly managed roots.

Explicit pull retains multi-platform index metadata but initially downloads only
the requested OS/architecture/variant graph. Native execution resolves the local
prepared platform before cache mutation, with no automatic emulation. Unknown
catalog references keep the image-not-found behavior. `arm64/v8` and `amd64/v1`
are treated as their baseline architectures, not as emulation requests.

## Runtime-independent CLI

Flags precede positional arguments:

```sh
opensbx image help
opensbx image pull --platform linux/arm64 node:22
opensbx image pull --platform linux/amd64 node:22
opensbx image list
opensbx image inspect --platform linux/arm64 node:22
opensbx image export --platform linux/arm64 --output node-arm64.tar node:22
opensbx image import --platform linux/arm64 --reference node:22 node-arm64.tar
opensbx image export --all-platforms --output complete.tar node:22
opensbx image remove node:22
```

Use `--data-dir` or `OPENSBX_DATA_DIR` to select the same store as the server.
The default CLI platform is `linux/<host architecture>`; API pull uses the selected
runtime's native platform. Registry credentials use go-containerregistry's
default keychain. Registry downloads are explicit and optional: OCI archives can
prepare an offline store. Building/pushing images is not implemented.

Variant export has the selected **manifest** as its root, not the original index.
Full-index export verifies every referenced blob and fails on sparse graphs,
including missing attestation artifacts. It never labels a sparse archive as
portable/complete. Import accepts a complete OCI layout tar or gzip tar, not a
Docker legacy `save` archive. All root graphs must validate; use `--reference`
for an untagged single-root archive. Import explicitly prepares the chosen
platform; other variants may be prepared by another import/pull. Only prepared
platforms are executable. `size` reports verified prepared graph bytes, not
native-runtime disk usage or the size of missing index variants.

## Integrity and lifecycle

- SHA-256 blob sizes/digests and descriptor graphs are checked. Archives use
  allowlisted regular files only, never arbitrary extraction, links or traversal.
  External descriptor URLs are rejected rather than delegated to a native loader;
  embedded descriptor data, when present, must match its declared digest and size.
- Operations are bounded to 20 GiB of archive/graph data, 16 MiB metadata, 100,000
  entries and 16 nested index levels. Outer gzip and selected rootfs gzip/zstd
  expansion are bounded; expanded layer hashes must match config diffIDs. Zstd
  decoding uses one worker and a 128 MiB window/memory bound. Disk errors are
  returned; temporary import/export files are cleaned up.
- An OS file lock serializes short catalog snapshots/publications, not network
  downloads or large archive I/O. Pull/import validate private staging first;
  publication rereads and merges the latest catalog. `catalog.json` is authoritative;
  unchanged read-only operations do not replace the derived `index.json`.
- Resolved artifacts pin immutable descriptors independently of catalog tags.
  Unreferencing/repointing a tag cannot redirect an in-flight materialization.
  A bounded process-local verification cache checks file fingerprints before
  reusing verified content, avoiding repeated layer decompression on each create.
- Unreference never deletes native images, active sandboxes or blobs. No automatic
  garbage collection is implemented, so execution pins and legacy resources are
  preserved. Budget limits apply per operation, not as a total store quota.
- Creation verifies content and materializes from the local store. Docker receives
  a selected-image Docker-compatible archive and executes by verified config ID.
  Apple 1.4.1 loads OCI and verifies the selected manifest/config via local export;
  its synthesized native index must not be confused with source identity.
- Apple uses a content-derived private cache reference and the version-pinned
  zero-download guard described in [Apple runtime notes](apple-container.md).
  Runtime caches are disposable; clearing them does not require another registry
  download for a prepared variant. Do not mutate owned resources externally while
  OpenSBX operates on them.

OCI images describe filesystem/configuration artifacts, not running-process
snapshots. Checkpointing, snapshot migration and cross-runtime process portability
are not implemented.

## Dependency rationale and primary contracts

`go-containerregistry v0.21.6` provides registry, OCI image, layout and Docker
archive translation APIs compatible with the Go 1.25.6 minimum. OpenSBX writes
selected graph blobs directly rather than using `AppendIndex`, which would copy
all variants. `gofrs/flock v0.12.1` provides cross-platform OS file locking.

The actual dependency `go.mod` declares Go **1.25.0**; its release notes' Go
1.26.3 update concerns upstream CI/releases, not the module minimum. Version
0.21.6 includes the Bearer-realm and foreign-layer SSRF fixes (#2243/#2293), plus
redirect/body bounds. See [release](https://github.com/google/go-containerregistry/releases/tag/v0.21.6)
and [module](https://github.com/google/go-containerregistry/blob/v0.21.6/go.mod).
OpenSBX additionally restricts registry-controlled URLs and credential forwarding;
see the [architecture and transport policy](architecture.md). Normal catalog
operations never need a registry connection; custom registries with unapproved
cross-origin token/CDN services fail closed.

- [OCI 1.1.1 layout](https://github.com/opencontainers/image-spec/blob/v1.1.1/image-layout.md)
  permits sparse referenced blobs; OpenSBX tracks prepared graphs separately.
- [go-containerregistry](https://github.com/google/go-containerregistry) documents
  image sources/sinks including `layout.Image`, `tarball.Write` and `remote.Index`.
- [Apple ImageLoad 1.4.1](https://github.com/apple/container/blob/1.4.1/Sources/ContainerCommands/Image/ImageLoad.swift):
  “Load images from an OCI compatible tar archive”.

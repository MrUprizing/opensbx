# Architecture

```text
REST / MCP models
        |
internal/api: transport and domain conversion
        |
internal/service: application policy
        |
internal/sandbox: runtime, process, filesystem and cache contracts
        |
internal/runtimeio: native IDs, ownership and runtime records
        |
Docker engine client or Apple container CLI
```

## Boundaries

- `models` defines the public REST/MCP JSON contract. Domain and runtime packages
  use their own types; HTTP DTOs stay at the API edge.
- `internal/service` owns application policy and sandbox creation compensation.
- `internal/sandbox` defines the interfaces implemented by the runtime adapters.
- `internal/runtimeio` translates public IDs into native references and keeps
  adapter-specific persistence details out of the service.
- `internal/images` manages a shared OCI catalog independent of the selected
  runtime's native image cache.

When adding behavior, start at the layer that owns it: HTTP or MCP conversion in
`internal/api`, shared policy in `internal/service`, runtime-specific behavior in
the relevant adapter, and OCI image operations in `internal/images`.

## Invariants

- The public REST/MCP contract is independent of Docker and Apple identifiers.
- Runtime operations require recorded ownership; listing native resources does
  not adopt them.
- Sandbox creation retains a bounded compensation/recovery path if adoption fails.
- App-host routing happens before path routing; a sandbox URL cannot reach
  management API or MCP routes.
- The image catalog is separate from runtime caches. Pull/import validate OCI
  content before publication; retagging cannot redirect an already-resolved image.
- Registry requests validate destinations and do not forward credentials across
  hosts. Custom cross-origin token/CDN services fail closed by default.
- OCI images represent filesystem/configuration, not running process snapshots.
  Checkpointing and snapshot portability are not implemented.

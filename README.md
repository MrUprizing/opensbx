# Opensbx

Local sandboxes for code you did not write.

Opensbx is an API-first sandbox runtime for untrusted or AI-generated code. It creates isolated environments on demand, lets you execute commands and edit files, exposes ports through subdomains, and tears everything down cleanly when done.

- Docs: [Installation](docs/install.md) · [Local usage and migration](docs/deployment.md) · [Images](docs/images.md) · [Releases](docs/releases.md) · [Testing](docs/testing.md)
- API docs: `http://localhost:8080/swagger/index.html`
- Internal contracts and trust boundaries: [Architecture](docs/architecture.md)

## Why Opensbx

- **Local control**: One loopback listener, one selected local runtime, and your own OCI image store.
- **Lightweight by design**: Single runtime dependency; no Kubernetes control plane, no bare-metal-only setup.
- **API-first**: Build sandbox workflows directly into your product with a simple REST API.
- **Built for AI workflows**: Safely execute generated code, tools, and scripts in ephemeral environments.
- **MCP-ready**: Expose sandbox operations to MCP clients with built-in MCP endpoints.
- **Owned images**: Explicitly pull or import into a common OCI catalog, independent of native runtime caches.

## Built for

- AI coding agents and tool execution
- Code execution platforms
- User-provided script runners
- Disposable CI/test environments
- Internal dev sandboxes and prototypes

## What you can do

- Create, inspect, list, start, stop, restart, pause, resume, and delete sandboxes
- Execute commands inside sandboxes and stream logs
- Read, write, delete files and list directories
- Pull, import, export, list, inspect, and unreference managed OCI images
- Expose app ports through subdomain routing
- Set resource limits and automatic expiration
- Protect endpoints with optional Bearer API key auth

Docker remains the default backend. Native Apple Silicon macOS can also use
[Apple container](docs/apple-container.md), with the same REST/MCP contract and
the runtime-specific limitations documented there.
Apple offline creation also requires its configured init image and kernel to be
available locally; see [runtime prerequisites](docs/apple-container.md).

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
opensbx
```

Health check:

```bash
curl http://127.0.0.1:8080/v1/health
```

Explicitly prepare an image, then create a sandbox (native runtime images are not automatically adopted):

```bash
curl -X POST http://127.0.0.1:8080/v1/images/pull \
  -H "Content-Type: application/json" -d '{"image":"node:22"}'
curl -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"image":"node:22","ports":["3000"],"timeout":900}'
```

## How it works

1. Explicitly pull/import an image into OpenSBX (`opensbx image help`). Registry access is optional for imported images.
2. Create a sandbox with optional ports, resources, and timeout.
3. Execute commands and edit files through the API.
4. Access exposed services through generated subdomain URLs.
5. Stop or delete the sandbox when finished.

## Security posture

- Sandboxes run isolated from your host application context.
- Exposed services are routed through the built-in reverse proxy.
- API access can be protected with Bearer authentication.
- Runtime limits (CPU, memory, timeout) reduce abuse and runaway workloads.
- Management and sandbox app hosts are separated before path routing. Sandbox URLs cannot reach REST/MCP routes.
- Only Docker and Apple container adapters are implemented; gVisor is not an OpenSBX backend.
- Containers are not a security guarantee against every hostile workload. Keep the host runtime patched.

## MCP support

Opensbx includes MCP endpoints so MCP clients can create sandboxes, execute commands, manage files, and orchestrate workflows through tool calls.

- Endpoint: `/v1/mcp`
- Docs: see deployment and API docs for setup details

## Configuration

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `ADDR` | `-addr` | `127.0.0.1:8080` | Single loopback listener for API, MCP and sandbox URLs |
| `OPENSBX_DATA_DIR` | `-data-dir` | `~/.local/share/opensbx` | Common local OCI catalog and runtime-specific execution state |
| — | `-legacy-db` | none | Explicit compatibility mode for a backed-up legacy execution database |
| `LOG_FILE` | `-log-file` | `opensbx.log` | Log file path for API and MCP metadata |
| `API_KEY` | — | *(empty, auth disabled)* | Bearer token for API authentication |
| — | `-runtime` | Docker, or a menu on interactive macOS | `docker` or `container`; selected once at startup |

Bind with `127.0.0.1:<port>` or `[::1]:<port>`; port `0` selects a free port.
Other bind addresses, including wildcard `:8080`, are rejected.

`PROXY_ADDR`, `BASE_DOMAIN`, `-proxy-addr` and `-base-domain` are rejected rather
than silently ignored. Existing working-directory databases require the original
runtime, a backup and an explicit `-legacy-db` path; see [migration](docs/deployment.md).
A generated app URL is `http://<sandbox>.localhost:8080`, using the actual
listener port, not a separate proxy port. No URL is returned without a usable TCP
main port. These URLs route all paths, including `/v1` and `/swagger`, to the app.

Image CLI flags follow the image subcommand and precede positional arguments:

```sh
opensbx image pull --data-dir /absolute/path/to/data --platform linux/arm64 node:25-alpine
opensbx image export --data-dir /absolute/path/to/data --platform linux/arm64 --output node-arm64.tar node:25-alpine
```

These commands do not select or connect a runtime. Use the same data directory as
the server; the image CLI defaults to `linux/<host architecture>`.

## Sandbox defaults

| Setting | Default | Max |
|---------|---------|-----|
| Memory | 1 GB | 8 GB |
| CPUs | 1.0 | 4.0 |
| Timeout | 15 min | — |

## Testing

Run unit tests:

```bash
go test ./...
```

Run integration tests (Docker required):

```bash
go test -tags=integration ./... -run '^TestIntegration'
```

Docker live tests and prepared-image Docker/Apple 1.4.1 portability verification
passed on the recorded validation host. See [testing](docs/testing.md) for the
tested revisions, exact opt-in commands, coverage results and verification limits.

## Sponsors

Thanks to these amazing people for supporting this project:

<table>
  <tr>
    <td align="center">
      <a href="https://github.com/camilocbarrera">
        <img src="https://avatars.githubusercontent.com/u/85809276?v=4" width="80" alt="camilocbarrera" /><br />
        <sub><b>Cris</b></sub>
      </a>
    </td>
    <td align="center">
      <a href="https://github.com/cuevaio">
        <img src="https://avatars.githubusercontent.com/u/83598208?v=4" width="80" alt="cuevaio" /><br />
        <sub><b>anthony</b></sub>
      </a>
    </td>
  </tr>
</table>

Become a sponsor on [GitHub Sponsors](https://github.com/sponsors/MrUprizing).

## License

[Apache License 2.0](LICENSE)

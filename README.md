# Opensbx

The self-hosted way to run code you did not write.

Opensbx is an API-first sandbox runtime for untrusted or AI-generated code. It creates isolated environments on demand, lets you execute commands and edit files, exposes ports through subdomains, and tears everything down cleanly when done.

- Docs: [Installation](docs/install.md) · [Deployment](docs/deployment.md) · [Releases](docs/releases.md) · [Testing](docs/testing.md)
- API docs: `http://localhost:8080/swagger/index.html`

## Why Opensbx

- **Self-hosted control**: Run on your own infra with your own network, policies, and costs.
- **Lightweight by design**: Single runtime dependency; no Kubernetes control plane, no bare-metal-only setup.
- **API-first**: Build sandbox workflows directly into your product with a simple REST API.
- **Built for AI workflows**: Safely execute generated code, tools, and scripts in ephemeral environments.
- **MCP-ready**: Expose sandbox operations to MCP clients with built-in MCP endpoints.
- **No heavy platform lock-in**: You own the runtime, images, and deployment model.

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
- Pull, list, inspect, and remove Docker images
- Expose app ports through subdomain routing
- Set resource limits and automatic expiration
- Protect endpoints with optional Bearer API key auth

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
opensbx
```

Health check:

```bash
curl http://127.0.0.1:8080/v1/health
```

Create a sandbox:

```bash
curl -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"image":"node:22","ports":["3000"],"timeout":900}'
```

## How it works

1. Pull or use an available image (`node:22`, `python:3.12`, etc.).
2. Create a sandbox with optional ports, resources, and timeout.
3. Execute commands and edit files through the API.
4. Access exposed services through generated subdomain URLs.
5. Stop or delete the sandbox when finished.

## Security posture

- Sandboxes run isolated from your host application context.
- Exposed services are routed through the built-in reverse proxy.
- API access can be protected with Bearer authentication.
- Runtime limits (CPU, memory, timeout) reduce abuse and runaway workloads.
- Optional hardened runtime setup with gVisor gives stronger isolation without adding orchestration complexity.
- gVisor setup is documented in [docs/install.md](docs/install.md).

## MCP support

Opensbx includes MCP endpoints so MCP clients can create sandboxes, execute commands, manage files, and orchestrate workflows through tool calls.

- Endpoint: `/v1/mcp`
- Docs: see deployment and API docs for setup details

## Configuration

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `ADDR` | `-addr` | `:8080` | HTTP API listen address |
| `PROXY_ADDR` | `-proxy-addr` | `:80,:3000` | Proxy listen addresses (comma-separated) |
| `BASE_DOMAIN` | `-base-domain` | `localhost` | Base domain for subdomain routing |
| `LOG_FILE` | `-log-file` | `opensbx.log` | Log file path for API and MCP metadata |
| `API_KEY` | — | *(empty, auth disabled)* | Bearer token for API authentication |

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

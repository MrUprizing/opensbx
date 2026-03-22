# Open Sandbox

A lightweight, self-hosted API for creating and managing isolated Docker containers on demand. Think of it as your own mini cloud — spin up sandboxes, run code, manage files, and tear them down when you're done.

## What is this?

Open Sandbox lets you programmatically create Docker containers through a simple REST API. Each container (called a "sandbox") is an isolated environment where you can execute commands, read/write files, and expose ports — all without touching Docker directly.

It's useful for:

- **Code execution platforms** — Run untrusted code safely in isolated containers
- **AI code generation** — Spin up isolated environments to execute AI-generated code safely
- **Development environments** — Spin up temporary environments on the fly
- **CI/CD tooling** — Create disposable build/test environments
- **Prototyping** — Quickly test things inside containers without Docker CLI

## How it works

1. Pull a Docker image (e.g. `node:20`, `python:3.12`)
2. Create a sandbox from that image
3. Execute commands, manage files, expose ports
4. The sandbox auto-stops after a configurable timeout (default: 15 min)
5. Delete it when you're done

Every sandbox gets resource limits (CPU + memory), automatic expiration, and its own port mappings.

## Features

- **Container lifecycle** — Create, stop, restart, pause, resume, and delete sandboxes
- **Command execution** — Run arbitrary commands inside sandboxes
- **File management** — Read, write, delete files and list directories
- **Image management** — Pull and list Docker images
- **Auto-expiration** — Sandboxes stop automatically after a configurable timeout
- **Resource limits** — Set CPU and memory constraints per sandbox
- **API key authentication** — Optional Bearer token auth
- **Swagger docs** — Interactive API documentation out of the box
- **Graceful shutdown** — Tracked containers are stopped cleanly on exit

## Getting Started

See the [Installation Guide](docs/install.md) for detailed setup instructions including Docker and gVisor configuration.

### Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensandbox/main/scripts/install.sh | bash
open-sandbox -addr :8080 -proxy-addr :3000 -base-domain localhost
```

The API runs on `http://localhost:8080` by default. Swagger docs at `/swagger/index.html`.

Release automation and binaries are documented in [docs/releases.md](docs/releases.md).
Deployment with Cloudflare Tunnel is documented in [docs/deployment.md](docs/deployment.md).

## Testing

See the full guide at [docs/testing.md](docs/testing.md).

Run unit tests:

```bash
go test ./...
```

Run integration tests (Docker required):

```bash
go test -tags=integration ./... -run '^TestIntegration'
```

## Configuration

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `ADDR` | `-addr` | `:8080` | HTTP listen address |
| `API_KEY` | — | *(empty, auth disabled)* | Bearer token for API authentication |

## Sandbox Defaults

| Setting | Default | Max |
|---------|---------|-----|
| Memory | 1 GB | 8 GB |
| CPUs | 1.0 | 4.0 |
| Timeout | 15 min | — |

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

# OpenSBX

Run code you do not trust in disposable local sandboxes.

OpenSBX provides a REST API and MCP tools to create sandboxes, run commands,
manage files, and expose app ports on your local machine.

**Start here:** [Use OpenSBX](docs/usage.md) · [Choose a runtime](docs/runtimes.md) · [Contribute](CONTRIBUTING.md) · [Architecture](docs/architecture.md)

## Quick start

Requirements: Go **1.25.6+** only when building from source, and a running local
[Docker runtime or supported Apple container runtime](docs/runtimes.md).

```sh
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
opensbx -runtime docker -addr 127.0.0.1:8080
```

In another terminal, prepare an image, create a sandbox, run a command and delete
it when finished. This example requires `jq` to read the sandbox ID:

```sh
opensbx image pull node:22

SANDBOX_ID=$(curl -fsS -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"image":"node:22","timeout":900}' | jq -r .id)

curl -fsS -X POST "http://127.0.0.1:8080/v1/sandboxes/$SANDBOX_ID/cmd" \
  -H 'Content-Type: application/json' \
  -d '{"command":"node","args":["--version"]}'

curl -fsS -X DELETE "http://127.0.0.1:8080/v1/sandboxes/$SANDBOX_ID"
```

Image pulls are explicit; images already cached by the runtime are not imported
automatically. For configuration, image import/export, MCP and migration, see
[Usage](docs/usage.md). The API reference is available at
`http://localhost:8080/swagger/index.html` while the server is running.

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

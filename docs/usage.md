# Use OpenSBX

## Install and start

Install the latest release on Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

On Windows, download the matching binary from [GitHub Releases](https://github.com/MrUprizing/opensbx/releases).

Start with the default configuration. On Linux, Docker is selected by default;
on supported Apple Silicon Macs, OpenSBX lets you select Docker or Apple
Container:

```sh
opensbx start
```

The server starts in the background and binds to `127.0.0.1:18089` by default.
Use `opensbx start -runtime container` to select Apple Container, or `-addr` to
change the address. Stop the server with `opensbx stop`. See [runtime setup](runtimes.md).
The server binds to loopback only. API, MCP, Swagger and sandbox app URLs share this listener.

Use `opensbx -h` or `opensbx -help` for the command list and options. Add `-h`
to a command, such as `opensbx start -h`, for command-specific help.

## Create and use a sandbox

Prepare an image explicitly. Images already cached by Docker or Apple are not
automatically imported into OpenSBX:

```sh
opensbx image pull node:22
```

Create a sandbox with the image, an exposed app port and a 15-minute timeout:

```sh
curl -fsS -X POST http://127.0.0.1:18089/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"image":"node:22","ports":["3000"],"timeout":900}'
```

Save the returned `id`. Run a command:

```sh
curl -fsS -X POST http://127.0.0.1:18089/v1/sandboxes/SANDBOX_ID/cmd \
  -H 'Content-Type: application/json' \
  -d '{"command":"node","args":["--version"]}'
```

When a sandbox exposes a TCP port, its response includes an app URL such as
`http://<sandbox>.localhost:18089`. Delete the sandbox when finished:

```sh
curl -i -X DELETE http://127.0.0.1:18089/v1/sandboxes/SANDBOX_ID
```

Sandboxes default to 1 CPU, 1 GiB memory and a 15-minute timeout. Limits are 4
CPUs and 8 GiB memory.

## Images

The `opensbx image` commands manage OpenSBX's shared local OCI catalog and do not
need a running runtime:

```sh
opensbx image pull node:22
opensbx image list
opensbx image inspect node:22
opensbx image export --output node.tar node:22
opensbx image import --reference node:22 node.tar
opensbx image remove node:22
```

For an offline machine, import an OCI layout archive. Docker legacy `save` archives
are not OCI archives. Set `OPENSBX_DATA_DIR` or use `--data-dir` to share a
non-default image catalog with the server. See `opensbx image help` for options.

## Configuration

| Setting | Default | Purpose |
| --- | --- | --- |
| `ADDR` / `-addr` | `127.0.0.1:18089` | Loopback address for API and sandbox URLs |
| `OPENSBX_DATA_DIR` / `-data-dir` | `~/.local/share/opensbx` | Image catalog and runtime state |
| `LOG_FILE` / `-log-file` | `opensbx.log` | Server log file |
| `API_KEY` | unset | Optional Bearer token for API requests |
| `-runtime` | Docker | `docker` or `container` (Apple) |

Use `127.0.0.1:<port>` or `[::1]:<port>`; wildcard addresses are rejected. If
`API_KEY` is set, send `Authorization: Bearer <key>`. Keep the data directory
private to the local user, on a local filesystem, and back it up with the server
stopped.

## API and MCP

- Interactive REST API: `http://localhost:18089/swagger/index.html`
- MCP endpoint: `http://localhost:18089/v1/mcp`
- Health check: `http://localhost:18089/v1/health`

The MCP server also exposes `opensbx://docs/quickstart` and
`opensbx://docs/how-it-works` resources to connected clients.

## Existing installations

If you have an older working-directory database, stop OpenSBX and make a
consistent backup (including SQLite `-wal`/`-shm` files if present). The Docker
database must be named `sandbox.db`; the Apple database must be named
`sandbox-container.db`. Start the same runtime with its matching database path:

```sh
opensbx start -runtime docker -legacy-db /absolute/path/sandbox.db
opensbx start -runtime container -legacy-db /absolute/path/sandbox-container.db
```

Do not rename a database to switch runtimes. Legacy mode updates the database in place;
keep the backup until you have verified the migration. Do not run multiple
OpenSBX processes against the same database.

Existing databases are not copied or adopted automatically. New installations
store runtime databases and the image catalog under `OPENSBX_DATA_DIR`.

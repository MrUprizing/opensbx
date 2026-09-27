# Local usage and state migration

OpenSBX runs locally. REST, MCP, health, Swagger and friendly sandbox URLs share
one HTTP server. Bind to `127.0.0.1:<port>` or `[::1]:<port>`; port `0` selects a
free port. Other bind addresses, including wildcard `:8080`, are rejected.
Docker endpoints must be a local Unix socket, Windows named pipe or
loopback TCP endpoint. OpenSBX does not provision runtimes or global services.

```sh
opensbx -runtime docker -addr 127.0.0.1:8080
# Apple Silicon, with container CLI/server 1.4.1 already running:
opensbx -runtime container -addr 127.0.0.1:8080
```

Management: `http://localhost:8080/v1/mcp`, `/v1/health`, and
`/swagger/index.html`. Use `API_KEY` for optional Bearer auth on `/v1`.
MCP localhost protection remains enabled. Friendly app URLs use
`http://<sandbox>.localhost:8080`. See [routing](reverse-proxy.md).

## Existing installations

1. Stop the old OpenSBX process cleanly. Do not delete any sandbox or image.
2. Make a consistent SQLite backup of the old execution DB. With all writers
   stopped, copy the DB and any `-wal`/`-shm` sidecars as a set; alternatively use
   SQLite's online backup facility. Keep the backup outside the active path.
3. Remove legacy `PROXY_ADDR`, `BASE_DOMAIN`, `-proxy-addr`, and `-base-domain`
   options. Replace wildcard `-addr :8080` with `-addr 127.0.0.1:8080`.
4. Explicitly select the original backend and original database:

   ```sh
   opensbx -runtime docker -legacy-db /absolute/old/path/sandbox.db
   opensbx -runtime container -legacy-db /absolute/old/path/sandbox-container.db
   ```

These are alternatives, not two commands to run for the same database. Legacy
mode opens the original DB in place and adds metadata columns without changing
public IDs or command backreferences. The basename must match the selected
runtime. Never rename an execution DB to switch runtimes. One process holds an
exclusive execution DB lock. Before reverting to an older release, stop all
writers and restore the backup as a set; do not overwrite a running database.

If either legacy filename is detected in the working directory without explicit
legacy mode, startup fails rather than silently creating an empty installation.
There is no automatic copy, resource deletion, image adoption, or tag-to-digest
guessing. Existing sandboxes keep working through their recorded backend IDs.
Legacy image-root/manifest columns remain empty: a historical tag or Docker
config ID is not proof of the OCI artifact originally used.

New installations use `~/.local/share/opensbx` (override with `-data-dir` or
`OPENSBX_DATA_DIR`):

- `images/`: shared OCI blobs, layout index and authoritative locked catalog.
- `runtimes/docker/sandbox.db`: Docker execution metadata.
- `runtimes/container/sandbox.db`: Apple execution metadata.

The image CLI uses the same data directory without selecting or connecting a
runtime. Native images are adopted only by explicitly producing an OCI archive
and importing it; do not point the manager at native cache directories. A Docker
`save` archive is not necessarily an OCI archive. For a registry image, an explicit
OpenSBX pull is the simplest preparation path.

OpenSBX's data directory should be private to the local user and on a local
filesystem with working OS file locks and atomic renames. Do not share it over a
network filesystem. Back up the catalog, layout, blobs and execution DBs together
with writers stopped. Image unreference retains blobs, including execution pins;
there is deliberately no automatic garbage collection.

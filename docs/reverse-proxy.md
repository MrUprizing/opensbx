# One local HTTP listener

The management API and sandbox applications share `127.0.0.1:8080` by default.
There is no second proxy listener or configurable public domain.

| Original request Host | Destination |
| --- | --- |
| `localhost:<port>` or `127.0.0.1:<port>` | REST/MCP, health, Swagger |
| `[::1]:<port>` when bound to IPv6 loopback | Management |
| `<sandbox>.localhost:<port>` | Owned sandbox's published main TCP port |
| Other, nested, malformed, or mismatched-port Host | Rejected |

Host dispatch happens before path routing. `/v1/sandboxes`, `/v1/mcp`,
`/swagger/index.html` and every other path on a sandbox host go to the sandbox,
never to management. Unknown sandbox names fail without management fallback.
`X-Forwarded-Host` is not trusted. WebSocket upgrades, SSE and relative application
URLs use the existing streaming reverse proxy. Apps receive the original Host.

On control hosts, browser requests with an Origin must match the canonical
request host, HTTP scheme and actual port exactly. Foreign, sandbox and `null`
origins are rejected with 403 before body parsing, including simple HTML forms.
Cross-site/same-site Fetch Metadata is also rejected. Origin-less CLI/MCP clients
continue to work; Bearer authentication, when configured, is still required.
This is server-side CSRF protection, not a CORS-only restriction. Sandbox app
traffic and its WebSocket/SSE connections keep their own origin semantics.

```sh
opensbx -addr 127.0.0.1:8080 -runtime docker
curl http://localhost:8080/v1/health
curl http://<sandbox>.localhost:8080/
```

The returned URL uses the actual bound port, including when `-addr 127.0.0.1:0`
selects a free port. Native sandbox publications remain on `127.0.0.1`; the first
requested port is the main route, and a UDP-only/non-published main port does not
produce an HTTP URL. Local hostname resolution must support `*.localhost` (as
modern browsers do); for diagnostics use curl's `--resolve` for the chosen host.

Old proxy/domain options now fail explicitly. See [migration](deployment.md).

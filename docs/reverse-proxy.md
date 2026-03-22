# Reverse Proxy

Access sandbox services via subdomain (`my-app.localhost`) instead of raw Docker ports.

Container ports are bound to `127.0.0.1` only — they are **not** accessible directly. All traffic must go through the reverse proxy.

## Configuration

| Env Variable  | Flag           | Default       | Description                                          |
|---------------|----------------|---------------|------------------------------------------------------|
| `PROXY_ADDR`  | `-proxy-addr`  | `:80,:3000`   | Comma-separated proxy listen addresses               |
| `BASE_DOMAIN` | `-base-domain` | `localhost`   | Base domain for subdomain routing                    |

The **first** address in `PROXY_ADDR` is used to generate sandbox URLs. If it's `:80` or `:443`, the URL omits the port (e.g. `http://my-app.localhost`). Otherwise the port is included (e.g. `http://my-app.localhost:3000`).

## Creating a Sandbox with Proxy

Include `ports` in the create request. The first port becomes the default for proxy routing:

```bash
curl -X POST localhost:8080/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{
    "image": "node:22",
    "ports": ["3000", "8080"]
  }'
```

Response includes the auto-generated name and proxy URL:

```json
{
  "id": "a1b2c3d4...",
  "name": "eager-turing",
  "ports": ["3000/tcp", "8080/tcp"],
  "url": "http://eager-turing.localhost"
}
```

Sandboxes can also be created without any ports — the proxy URL is omitted in that case.

## Local Development

`*.localhost` resolves to `127.0.0.1` in modern browsers (RFC 6761). No DNS setup needed.

```bash
# Default: proxy listens on :80 and :3000
go run ./cmd/api

# API   → localhost:8080
# Proxy → *.localhost (port 80) and *.localhost:3000
```

Open `http://eager-turing.localhost` in your browser (use the name from the create response).

> **Note:** Port 80 requires root/sudo. For development without elevated privileges:
>
> ```bash
> PROXY_ADDR=:3000 go run ./cmd/api
> ```

### If `*.localhost` doesn't resolve

Use dnsmasq for automatic wildcard resolution:

```bash
brew install dnsmasq
echo "address=/localhost/127.0.0.1" >> $(brew --prefix)/etc/dnsmasq.conf
sudo brew services start dnsmasq
sudo mkdir -p /etc/resolver
echo "nameserver 127.0.0.1" | sudo tee /etc/resolver/localhost
```

## Production

### 1. DNS

Create a wildcard A record pointing to your server:

```
*.sandbox.example.com  →  A  →  YOUR_SERVER_IP
```

### 2. Run

```bash
PROXY_ADDR=:80 \
BASE_DOMAIN=sandbox.example.com \
API_KEY=your-secret \
go run ./cmd/api
```

Sandboxes are now accessible at `http://<name>.sandbox.example.com`.

### 3. HTTPS (optional)

Place a TLS-terminating reverse proxy in front of the proxy server with a wildcard certificate for `*.sandbox.example.com`. The opensbx proxy handles plain HTTP behind it.

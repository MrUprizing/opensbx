# Deployment (Cloudflare Tunnel)

Goal:

- API at `your-domain.com`
- Sandboxes at `*.your-domain.com`

Cloudflare handles TLS. `cloudflared` forwards traffic to local Open Sandbox ports.

## Fast setup (macOS)

### 1) Install `cloudflared`

```bash
brew install cloudflared
cloudflared --version
```

### 2) Authenticate

```bash
cloudflared tunnel login
```

This creates `~/.cloudflared/cert.pem`.

### 3) Create tunnel (this command gives you the tunnel ID)

```bash
cloudflared tunnel create opensbx-local
```

Look at the command output for:

```text
Created tunnel opensbx-local with id <TUNNEL_ID>
Tunnel credentials written to ~/.cloudflared/<TUNNEL_ID>.json
```

Use that `<TUNNEL_ID>` in the config below.

### 4) Add DNS routes

```bash
cloudflared tunnel route dns opensbx-local your-domain.com
cloudflared tunnel route dns opensbx-local '*.your-domain.com'
```

### 5) Create `~/.cloudflared/config.yml`

```yaml
tunnel: <TUNNEL_ID>
credentials-file: /Users/<YOUR_USER>/.cloudflared/<TUNNEL_ID>.json

ingress:
  - hostname: your-domain.com
    service: http://127.0.0.1:8080
  - hostname: "*.your-domain.com"
    service: http://127.0.0.1:3000
  - service: http_status:404
```

Note: ingress order matters (API first, wildcard second).

### 6) Validate + run tunnel

```bash
cloudflared tunnel ingress validate
cloudflared tunnel run opensbx-local
```

## Run Open Sandbox

### Install binary from script

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensandbox/main/scripts/install.sh | bash
```

### Run for deployment

```bash
open-sandbox -addr :8080 -proxy-addr :3000 -base-domain your-domain.com
```

## Verify

```bash
curl https://your-domain.com/v1/health
```

```bash
curl -X POST https://your-domain.com/v1/sandboxes \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-secret-key" \
  -d '{"image":"nginx:alpine","ports":["80"]}'
```

Sandbox URL should be:

```text
https://<sandbox-name>.your-domain.com
```

## MCP

With `-base-domain your-domain.com`, MCP works without `MCPGODEBUG`.

Quick check:

```bash
curl -i https://your-domain.com/v1/mcp \
  -X POST \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"1.0"}}}'
```

## Ubuntu notes (VPS)

Install `cloudflared`:

```bash
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg | sudo gpg --dearmor -o /usr/share/keyrings/cloudflare-main.gpg
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared jammy main' | sudo tee /etc/apt/sources.list.d/cloudflared.list
sudo apt update
sudo apt install -y cloudflared
```

Run as service:

```bash
sudo cloudflared service install
sudo systemctl enable cloudflared
sudo systemctl restart cloudflared
sudo journalctl -u cloudflared --no-pager -n 50
```

Keep only SSH exposed publicly:

```bash
sudo ufw allow 22/tcp
sudo ufw enable
```

Do not expose `8080` or `3000`.

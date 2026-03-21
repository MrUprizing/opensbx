# Production Deployment Guide (Cloudflare Tunnel, No Caddy)

Deploy Open Sandbox on a VPS with HTTPS and wildcard subdomains **without Caddy**.

Cloudflare handles TLS at the edge, and `cloudflared` forwards traffic through a private tunnel to your local services.

**Stack:** Any VPS (Ubuntu 24.04) + Cloudflare DNS + Cloudflare Tunnel + gVisor

## Architecture

```
Internet
    │
    ▼
Cloudflare Edge (TLS termination)
    │
    ▼
Cloudflare Tunnel (cloudflared on VPS)
    │
    ├── yourdomain.com      → http://localhost:8080  (API server)
    │
    └── *.yourdomain.com    → http://localhost:3000  (Sandbox proxy)
                                    │
                                    ▼
                              Docker containers (:32xxx)
```

This keeps local behavior almost identical to development: Open Sandbox still listens on `:8080` and `:3000`.

---

## Prerequisites

- A VPS (Ubuntu 24.04 recommended)
- A domain managed by Cloudflare
- Go, Docker, and gVisor installed (see [install.md](install.md))
- A Cloudflare account with access to your zone

---

## Step 1: Run Open Sandbox normally

On the VPS, run Open Sandbox the same way as local/proxy mode:

```bash
BASE_DOMAIN=yourdomain.com \
ADDR=:8080 \
PROXY_ADDR=:3000 \
API_KEY=your-secret-key \
go run ./cmd/api
```

Quick local check from the VPS:

```bash
curl http://127.0.0.1:8080/v1/health
# {"status":"healthy"}
```

---

## Step 2: Install cloudflared

Install Cloudflare Tunnel client on Ubuntu:

```bash
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg | sudo gpg --dearmor -o /usr/share/keyrings/cloudflare-main.gpg
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared jammy main' \
  | sudo tee /etc/apt/sources.list.d/cloudflared.list
sudo apt update
sudo apt install -y cloudflared
```

Verify:

```bash
cloudflared --version
```

---

## Step 3: Authenticate and create a tunnel

Login (opens browser auth flow):

```bash
cloudflared tunnel login
```

Create a named tunnel:

```bash
cloudflared tunnel create opensandbox
```

This creates credentials at:

```text
~/.cloudflared/<TUNNEL_ID>.json
```

---

## Step 4: Configure ingress routes

Create config file:

```bash
sudo mkdir -p /etc/cloudflared
sudo nano /etc/cloudflared/config.yml
```

Use this config (replace placeholders):

```yaml
tunnel: <TUNNEL_ID>
credentials-file: /root/.cloudflared/<TUNNEL_ID>.json

ingress:
  - hostname: yourdomain.com
    service: http://localhost:8080

  - hostname: "*.yourdomain.com"
    service: http://localhost:3000

  - service: http_status:404
```

Notes:
- `yourdomain.com` points to the API server.
- `*.yourdomain.com` points to the Open Sandbox reverse proxy.
- The final `http_status:404` is required as a catch-all rule.

---

## Step 5: Create DNS routes to the tunnel

Map hostnames to tunnel:

```bash
cloudflared tunnel route dns opensandbox yourdomain.com
cloudflared tunnel route dns opensandbox '*.yourdomain.com'
```

Cloudflare creates the DNS records automatically.

---

## Step 6: Run cloudflared as a system service

Install and start the service:

```bash
sudo cloudflared service install
sudo systemctl enable cloudflared
sudo systemctl restart cloudflared
```

Verify service health:

```bash
sudo systemctl status cloudflared
sudo journalctl -u cloudflared --no-pager -n 50
```

Should show the tunnel connected.

---

## Step 7: Verify end-to-end

### API check

```bash
curl https://yourdomain.com/v1/health
```

### Create a sandbox

```bash
curl -X POST https://yourdomain.com/v1/sandboxes \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-secret-key" \
  -d '{"image":"node:20-alpine","ports":["3000"]}'
```

### Access sandbox by subdomain

Using response name (example `eager-turing`):

```bash
curl https://eager-turing.yourdomain.com
```

---

## Firewall

With Cloudflare Tunnel, you usually only need SSH exposed:

```bash
sudo ufw allow 22/tcp
sudo ufw enable
```

Do **not** expose `8080` or `3000` publicly.

---

## Troubleshooting

| Problem | Cause | Fix |
|---------|-------|-----|
| `ERR_NAME_NOT_RESOLVED` | DNS route not created | Re-run `cloudflared tunnel route dns ...` |
| `HTTP 502` from Cloudflare | Open Sandbox not running locally | Verify `curl http://127.0.0.1:8080/v1/health` |
| Sandbox subdomain returns 502 | Sandbox down or wrong container port | Check sandbox status and port map |
| Tunnel not connected | Invalid credentials file / wrong tunnel ID | Check `/etc/cloudflared/config.yml` and service logs |
| API works but wildcard does not | Missing wildcard DNS route | Add `cloudflared tunnel route dns opensandbox '*.yourdomain.com'` |

---

## Notes

- No Caddy, no manual TLS certificate management.
- HTTPS is terminated at Cloudflare edge.
- WebSockets/HMR are supported through Cloudflare Tunnel for most app workflows.
- Open Sandbox still generates internal-style URLs in some responses; the public URL format is `https://name.yourdomain.com`.
- The proxy on `:3000` still has no auth by default. Keep API auth enabled and consider proxy auth hardening for production.

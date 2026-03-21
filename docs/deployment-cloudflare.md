# Production Deployment Guide (Cloudflare DNS)

Complete guide for deploying Open Sandbox on a VPS with HTTPS (wildcard TLS via Caddy + Let's Encrypt), using Cloudflare for DNS.

**Stack:** Any VPS (Ubuntu 24.04) + Cloudflare DNS + Caddy reverse proxy + gVisor

## Architecture

```
Internet
    │
    ▼
  Cloudflare DNS (DNS only, no proxy)
    │
    ▼
  Caddy (:443, TLS termination)
    │
    ├── yourdomain.com      → localhost:8080  (API server)
    │
    └── *.yourdomain.com    → localhost:3000  (Sandbox proxy)
                                    │
                                    ▼
                              Docker containers (:32xxx)
```

Caddy handles HTTPS with automatic wildcard certificates from Let's Encrypt. Open Sandbox runs behind it on plain HTTP (localhost only, not exposed to the internet).

---

## Prerequisites

- A VPS (any provider: Hetzner, DigitalOcean, Linode, AWS, etc.) running Ubuntu 24.04
- A domain name with DNS managed by Cloudflare
- Go, Docker, and gVisor installed (see [install.md](install.md))

---

## Step 1: DNS Setup in Cloudflare

You need two A records pointing to your VPS IP. The wildcard record (`*`) makes `anything.yourdomain.com` resolve to your server.

### 1.1 Add DNS records

Go to the Cloudflare dashboard → select your domain → **DNS** → **Records** → **Add record**.

| Type | Name | Content          | Proxy status     |
|------|------|------------------|------------------|
| A    | `@`  | `YOUR_SERVER_IP` | **DNS only** (grey cloud) |
| A    | `*`  | `YOUR_SERVER_IP` | **DNS only** (grey cloud) |

> **Important:** Proxy status must be **DNS only** (grey cloud icon), not **Proxied** (orange cloud). Caddy handles TLS termination — enabling Cloudflare's proxy causes double proxying, breaks WebSocket connections, and prevents wildcard certificate issuance via DNS-01 challenge.

### 1.2 Verify DNS propagation

Wait a few minutes after adding the records, then verify:

```bash
dig yourdomain.com +short
# Should return: YOUR_SERVER_IP

dig test.yourdomain.com +short
# Should also return: YOUR_SERVER_IP
```

Both must return your server IP before proceeding.

---

## Step 2: Cloudflare API Token

Caddy needs an API token to create temporary DNS TXT records for the Let's Encrypt DNS-01 challenge (required for wildcard certs).

1. Go to [Cloudflare dashboard](https://dash.cloudflare.com) → **My Profile** (top right) → **API Tokens** → **Create Token**
2. Use the **Edit zone DNS** template, or create a custom token with these permissions:
   - **Zone → DNS → Edit**
   - **Zone → Zone → Read**
3. Under **Zone Resources**, select **Include → Specific zone → yourdomain.com**
4. Create the token and copy it

Verify the token works:

```bash
curl -H "Authorization: Bearer YOUR_TOKEN" \
  https://api.cloudflare.com/client/v4/zones
```

Should return a JSON response with your zone listed.

---

## Step 3: Install Caddy with Cloudflare DNS Plugin

The default Caddy binary doesn't include DNS provider plugins. You need to compile a custom build with the Cloudflare plugin using `xcaddy`.

```bash
# Install xcaddy
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

# Build Caddy with Cloudflare DNS plugin (~1-2 minutes)
~/go/bin/xcaddy build --with github.com/caddy-dns/cloudflare

# Verify
./caddy version

# Move to system PATH
sudo mv caddy /usr/local/bin/caddy
```

---

## Step 4: Configure Caddy

### 4.1 Create the Caddyfile

```bash
sudo mkdir -p /etc/caddy

sudo tee /etc/caddy/Caddyfile > /dev/null << 'EOF'
yourdomain.com {
    reverse_proxy localhost:8080
}

*.yourdomain.com {
    reverse_proxy localhost:3000

    tls {
        dns cloudflare {env.CLOUDFLARE_API_TOKEN}
        propagation_delay 30s
    }
}
EOF
```

Replace `yourdomain.com` with your actual domain.

The `propagation_delay 30s` tells Caddy to wait 30 seconds before checking DNS propagation, which avoids failures caused by slow DNS updates.

### 4.2 Create systemd service

Create the system user for Caddy:

```bash
sudo groupadd --system caddy 2>/dev/null
sudo useradd --system --gid caddy --create-home \
  --home-dir /var/lib/caddy --shell /usr/sbin/nologin caddy 2>/dev/null
```

Create the service file:

```bash
sudo tee /etc/systemd/system/caddy.service > /dev/null << 'EOF'
[Unit]
Description=Caddy web server
After=network.target network-online.target
Requires=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
Environment=CLOUDFLARE_API_TOKEN=YOUR_TOKEN_HERE
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
```

Replace `YOUR_TOKEN_HERE` with your Cloudflare API token:

```bash
sudo sed -i 's/YOUR_TOKEN_HERE/paste_your_real_token/' /etc/systemd/system/caddy.service
```

### 4.3 Start Caddy

```bash
sudo systemctl daemon-reload
sudo systemctl enable caddy
sudo systemctl start caddy
```

Verify it's running:

```bash
sudo systemctl status caddy
```

Should show `active (running)`. Check logs for certificate issuance:

```bash
sudo journalctl -u caddy --no-pager -n 30
```

Look for `"certificate obtained successfully"`. The first wildcard cert takes ~30-60 seconds due to the DNS-01 challenge.

---

## Step 5: Firewall

Only expose ports 80 (HTTP redirect), 443 (HTTPS), and 22 (SSH). The API (`:8080`) and proxy (`:3000`) are only accessible via Caddy on localhost.

```bash
sudo ufw allow 22/tcp
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
```

Verify:

```bash
sudo ufw status
```

Expected output:

```
To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
80/tcp                     ALLOW       Anywhere
443/tcp                    ALLOW       Anywhere
```

---

## Step 6: Run Open Sandbox

```bash
BASE_DOMAIN=yourdomain.com \
ADDR=:8080 \
PROXY_ADDR=:3000 \
API_KEY=your-secret-key \
go run ./cmd/api
```

---

## Step 7: Verify

### API health check

```bash
curl https://yourdomain.com/v1/health
# {"status":"healthy"}
```

### Create a sandbox

```bash
curl -X POST https://yourdomain.com/v1/sandboxes \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-secret-key" \
  -d '{"image": "node:20-alpine", "port": "3000"}'
```

### Access sandbox via HTTPS

Using the `name` from the create response (e.g. `eager-turing`):

```bash
curl https://eager-turing.yourdomain.com
```

---

## Troubleshooting

| Problem | Cause | Solution |
|---------|-------|----------|
| `curl: (7) Failed to connect` on :443 | Firewall blocking | `sudo ufw allow 443/tcp` |
| Caddy: `tls: failed to solve challenge` | Wrong API token or insufficient permissions | Verify token has Zone DNS Edit + Zone Read permissions |
| Caddy: `tls: no matching certificate` | Wildcard cert not yet issued | Wait ~60s, check `journalctl -u caddy` |
| `no subdomain in request` | `BASE_DOMAIN` doesn't match domain | Verify the env var matches your domain exactly |
| API returns 502 | Open Sandbox not running on `:8080` | Start the application |
| Sandbox returns 502 | Container not running or wrong port | Check sandbox status and port mapping |
| DNS not resolving | Records not added or proxy enabled | Check Cloudflare DNS records are set to **DNS only** (grey cloud) |
| ERR_TOO_MANY_REDIRECTS | Cloudflare proxy is enabled (orange cloud) | Disable proxy — set to **DNS only** for both `@` and `*` records |

### Useful commands

```bash
# Caddy logs (live)
sudo journalctl -u caddy -f

# Restart Caddy after config changes
sudo systemctl restart caddy

# Check if Caddy owns the certificate
curl -vI https://yourdomain.com 2>&1 | grep "issuer"

# Check wildcard cert
curl -vI https://test.yourdomain.com 2>&1 | grep "issuer"
```

---

## Notes

- **Caddy auto-renews certificates.** No cron jobs or manual renewal needed.
- **HTTP is automatically redirected to HTTPS** by Caddy.
- **The proxy server (`:3000`) has no authentication.** Anyone with a valid sandbox name can access it.
- **Timers are in-memory.** If the process restarts, auto-stop timers are lost.
- **The `url` field in API responses** currently generates `http://` URLs with the internal proxy port. When running behind Caddy, the actual URL is `https://name.yourdomain.com`.

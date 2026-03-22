# Installation

## Prerequisites

- Docker
- Go (recommended: latest stable)
- Optional for hardened runtime: gVisor (`runsc`)

## Quick install (recommended)

Install latest release binary:

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

Run locally:

```bash
opensbx -addr :8080 -proxy-addr :3000 -base-domain localhost
```

Health check:

```bash
curl http://127.0.0.1:8080/v1/health
```

## Docker setup

### macOS

Install Docker Desktop, then verify:

```bash
docker --version
docker info
```

### Ubuntu

```bash
sudo apt update
sudo apt install -y docker.io
sudo systemctl enable docker
sudo systemctl start docker
docker --version
```

## gVisor setup (optional, Ubuntu)

Use this if you want stronger isolation for untrusted workloads.

```bash
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list > /dev/null
sudo apt update
sudo apt install -y runsc
```

Configure Docker runtime:

```bash
RUNSC_PATH=$(command -v runsc)
sudo mkdir -p /etc/docker
cat <<EOF | sudo tee /etc/docker/daemon.json
{
  "default-runtime": "runsc",
  "runtimes": {
    "runsc": {
      "path": "${RUNSC_PATH}"
    }
  }
}
EOF
sudo systemctl restart docker
```

Validate:

```bash
docker info | grep "Default Runtime"
docker run --rm hello-world
```

## Next docs

- Deployment with Cloudflare Tunnel: [deployment.md](deployment.md)
- Releases and tags: [releases.md](releases.md)
- Testing: [testing.md](testing.md)

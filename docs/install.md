# Installation

## Prerequisites

- Go **1.25.6 or newer** to build from source; not needed for release binaries.
- A running local Docker daemon, or [Apple container 1.4.1](apple-container.md)
  on supported Apple Silicon macOS. OpenSBX does not install/start a runtime.
- Image catalog/import/export commands require neither runtime.

Install the release binary:

```sh
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

Or build this checkout (release binaries may predate the redesign):

```sh
go build -o opensbx ./cmd/api
./opensbx image help
./opensbx -runtime docker -addr 127.0.0.1:8080
```

Read-only prerequisite checks:

```sh
docker version
docker info
# On the Apple backend:
container system status --format json
```

Obtain the host runtime separately from its official distribution. OpenSBX only
accepts local Docker endpoints, not remote daemon URLs. There is no gVisor backend
or automatic runtime configuration. Only Docker and Apple container are in scope.

Next: [local usage/migration](deployment.md), [images](images.md),
[releases](releases.md), [testing](testing.md).

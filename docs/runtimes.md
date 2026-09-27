# Runtimes

OpenSBX uses one local runtime per server process. The REST API and MCP interface
are the same for both supported backends.

| Runtime | Supported host | Requirements and notes |
| --- | --- | --- |
| Docker | Linux, macOS, Windows | Local Docker daemon already running; default backend |
| Apple container | Apple Silicon, macOS 26+ | Apple `container` CLI and server 1.4.1 already installed and running; `linux/arm64` images only |

Select the backend when starting the server:

```sh
opensbx -runtime docker
opensbx -runtime container
```

## Apple container setup

The Apple CLI/server are installed separately; OpenSBX does not install them or
start the service. Ensure `container` is on the launching process's `PATH` and
that its service is running for the same macOS user.

The configured kernel and trusted vminit image must be available locally before
creating sandboxes. With the default configuration, prepare vminit explicitly:

```sh
container image pull --platform linux/arm64 --progress plain \
  ghcr.io/apple/containerization/vminit:0.45.0
```

Kernel setup is done using the Apple runtime's `container system kernel` workflow.
OpenSBX does not download either runtime prerequisite during sandbox creation.

## Runtime differences

- Apple requires managed `linux/arm64` images and supports whole CPUs from 1 to 4;
  fractional CPU limits and pause/resume are not available there.
- Docker supports the resource limits accepted by OpenSBX's API, including
  fractional CPUs.
- Apple guest images need `/bin/sh`, Linux `/proc`, `sleep`, and basic file
  utilities for command and filesystem operations.
- App ports are published on loopback. Sandbox URLs route app traffic; they do not
  expose the management API through the sandbox host.
- OpenSBX does not automatically adopt images from either runtime's native cache.
  Prepare images with `opensbx image pull` or import an OCI archive.

Containers provide isolation but are not a complete security boundary against all
hostile workloads. Keep the host OS and selected runtime up to date.

# Runtimes

OpenSBX uses one local runtime per server process. The REST API and MCP interface
are the same for both supported backends.

| Runtime | Supported host | Requirements and notes |
| --- | --- | --- |
| Docker | Linux, macOS, Windows | Local Docker daemon already running; default backend |
| Apple container | Apple Silicon, macOS 26+ | Apple `container` CLI and server 1.4.1 already installed and running; `linux/arm64` images only |

Select the backend when starting the server (it runs in the background):

```sh
opensbx start -runtime docker
opensbx start -runtime container
opensbx stop
```

## Apple container setup

Install Apple's signed `container` CLI from [GitHub Releases](https://github.com/apple/container/releases)
or follow the [official repository](https://github.com/apple/container). OpenSBX
does not install the CLI or start its service. Ensure `container` is on the
launching process's `PATH`, then start the service for the same macOS user:

```sh
container system start
```

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

## Registry failure diagnostics

A failed image pull emits one `registry_pull_failed` log event with only
`category`, `stage`, and `status` fields. No debug environment variable is needed.
Categories are `upstream_status`, `authentication`, `dns`, `timeout`, `read`,
`policy`, or `unknown`. Stages are `registry-ping`, `auth`, `manifest`, `blob`,
`transport`, or `unknown`. Status is the actual HTTP response code when available,
including `200` when a successful response body fails to read; `0` means no
response status is available. Local non-registry pull failures have unknown
metadata. These fields do not identify a host, image reference, URL, or credential.

Registry errors retain the public message
`registry request failed or was rejected by local URL policy`; context cancellation
and deadline errors retain their context semantics. Internal Go callers can use
`errors.As` with an interface declaring
`RegistryDiagnostic() (category string, stage string, status int)`. Sanitized
errors do not unwrap upstream causes. Classification uses typed failures and
controlled request stages, never upstream error-message substrings. A diagnostic
is evidence about the failed operation, not proof of an external outage.

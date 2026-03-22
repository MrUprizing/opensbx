# Releases Guide

This project publishes versioned binaries using a single GitHub Action and GoReleaser.

## How it works

- Workflow: `.github/workflows/release.yml`
- Trigger: push a tag that starts with `v` (example: `v1.2.0`)
- Tooling: `goreleaser/goreleaser-action` + `.goreleaser.yml`

When the workflow runs, it builds and publishes binaries to GitHub Releases for:

- `linux/amd64`
- `linux/arm64`
- `macos/amd64`
- `macos/arm64`
- `windows/amd64`
- `windows/arm64`

Artifacts include archives and `checksums.txt`.

## Create a release

From your local repo:

```bash
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions will create the Release and upload binaries automatically.

## Install without cloning

Linux/macOS install script:

```bash
curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

Optional env vars:

- `OPENSBX_VERSION` (example: `v1.0.0`)
- `OPENSBX_INSTALL_DIR` (default: `/usr/local/bin`)
- `OPENSBX_REPO` (default: `MrUprizing/opensbx`)

Example pinned install:

```bash
OPENSBX_VERSION=v1.0.0 curl -fsSL https://raw.githubusercontent.com/MrUprizing/opensbx/main/scripts/install.sh | bash
```

Windows users can download the `.zip` binary directly from the GitHub Release page.

## Local dry run (optional)

Before tagging, you can test packaging locally:

```bash
goreleaser release --snapshot --clean
```

This builds artifacts in `dist/` without publishing.

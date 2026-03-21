# Testing Guide

Quick reference to run tests locally.

## Prerequisites

- Go installed
- Docker running (only for integration tests)

## Unit Tests

Run all default tests:

```bash
go test ./...
```

## Integration Tests

Integration tests are tagged and use Docker.

```bash
go test -tags=integration ./... -run '^TestIntegration'
```

Notes:

- Test image is hardcoded to `node:25-alpine`.
- If the image is not available locally, tests try to pull it automatically.
- If Docker is not available, integration tests are skipped.

## CI Behavior

- Unit tests run on pushes to `main`.
- Unit tests run on every pull request.
- Integration tests run only on pushes to `main`.

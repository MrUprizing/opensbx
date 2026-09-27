---
name: go-code-organization
description: Review Go code organization, split large .go files by cohesive responsibility, and decide file versus package boundaries. Use for Go structural reviews and behavior-preserving refactors, not unrelated bug fixes or feature work.
---

# Go Code Organization

Improve discoverability and local reasoning without changing behavior or adding
unnecessary abstractions. File length is a signal to investigate, not a rule to
enforce.

## Scope

- For review or planning requests, inspect and propose changes without editing.
- For an explicit implementation request, carry out the scoped reorganization
  and relevant verification. Loading this skill alone does not authorize edits.
- Follow repository instructions and preserve existing work. Separate unrelated
  defects and semantic improvements from a structural refactor.

## Principles

1. **Group related behavior.** A maintainer should be able to predict which file
   contains an operation and understand its lifecycle without excessive jumps.
2. **Prefer a same-package split first.** Files in one Go package share private
   declarations. Moving methods between them does not require new interfaces,
   exported helpers, getters, or packages.
3. **Extract a package for an independent concept, not a shorter file.** Require
   a coherent purpose, a small usable boundary, clear dependency direction, and
   no import cycles. If extraction exposes internal state merely to reconnect
   the pieces, keep them in the same package.
4. **Keep invariants together.** State transitions, lock ownership, cancellation,
   readiness, completion, and resource cleanup often form one responsibility.
5. **Avoid both extremes.** Do not enforce arbitrary line/function counts, one
   type per file, or many tiny files that obscure a single workflow.
6. **Name by responsibility.** Prefer `lifecycle.go`, `commands.go`, or
   `image_handlers.go` over `client_part2.go`, `misc.go`, or catch-all helpers.
   Use the surrounding package's naming conventions.
7. **Comment decisions and contracts.** Explain why a guard, lock, or ordering
   exists; do not narrate obvious statements. Keep documentation accurate and
   adjacent to the declarations it describes.

## Review workflow

### 1. Establish the boundary

Read applicable instructions, `go.mod`, relevant architecture documentation,
working-tree changes, the candidate file, neighboring files, and its tests.
Inspect callers where needed. Do not assume a previous file map is still current.

Identify:

- Public contracts, private implementation details, and package dependencies.
- Method families and their shared receiver state.
- Mutexes, channels, goroutines, ownership rules, and cleanup paths.
- Existing files that already own related behavior.
- Build constraints, platform filename suffixes, package initializers, generated
  code, and annotations used by tooling.

### 2. Choose the smallest useful change

| Evidence | Preferred action |
| --- | --- |
| One cohesive workflow that is easy to navigate | Keep the file, even if long. |
| Multiple recognizable operation families | Split files within the same package. |
| A function hides several distinct steps | Consider focused helper extraction separately from declaration moves. |
| A concept has a narrow boundary and independent dependencies | Consider a new package. |
| Extraction requires exposing mutexes, mutable state, or many private helpers | Keep the concept in its current package. |

Propose a map with **source group → target file → rationale → shared invariants**.
Explain why each boundary helps navigation. Prefer extending an existing file
over creating a competing home for the same responsibility.

### 3. Apply OpenSBX context

Use these as inspection starting points, not mandatory filenames or a stale
checklist of files that must be split:

| Area | Cohesive groups to look for |
| --- | --- |
| `internal/docker` | Client construction; sandbox lifecycle and expiration; commands/readiness/signals/logs; file operations; port conversion. |
| `internal/applecontainer` | Client setup; inventory/ownership conversion; creation/rollback; lifecycle/timers. Reuse existing command, runner, cache, and routing files. |
| `internal/api` | Sandbox handlers; command handlers and streaming; filesystem handlers; image handlers. |
| MCP registration | Sandbox, process, filesystem, and image tools; a central registration coordinator; resources and prompts. |

Keep Docker execution readiness, signaling, wait, and logs close to the state
they coordinate. Keep Apple locked lifecycle helpers, timers, and shutdown in
the same logical group. Splitting files does not resolve duplicated lifecycle
logic between runtimes; assess that as a separate change.

Preserve the domain boundaries documented in `docs/architecture.md`: transport
DTOs stay at the API edge, native identifiers stay in adapters, and the OCI
catalog remains independent of runtime caches. Organization work must not move
Docker types into the domain or turn a public route/schema change into a rename.

MCP registration is not always a mechanical move: locally declared argument
types and registration closures may need extraction. Assess this separately and
check tool names, schemas, resource/prompt content, and registration order.

## Implementation workflow

1. Establish the relevant baseline, using existing verified evidence when it
   still applies. Record pre-existing failures rather than attributing them to
   the reorganization.
2. Work on one cohesive package or family at a time. Prefer declaration moves
   first; avoid combining them with renames, deduplication, or behavior changes.
3. Keep receivers, visibility, signatures, error behavior, defaults, ordering,
   and synchronization semantics intact. Preserve build tags and OS/architecture
   suffixes. Review initialization dependencies before moving package variables
   or `init` functions; file order can affect initialization order.
4. Move GoDoc and Swagger annotations with their declarations. Use `doc.go` only
   when a package overview benefits from its own file, not for every package.
   Keep long design rationale in the existing architecture docs rather than
   duplicating it in every source file.
5. Update per-file imports and format changed Go files. Move tests only when it
   improves their organization; tests in the same package do not need relocation
   merely because production declarations moved.
6. Review the diff for unexpected semantic changes, new exports/dependencies,
   accidental deletion of existing work, and avoidable navigation fragmentation.

## Verification

For an implemented source refactor, run the applicable package tests and required
repository checks. OpenSBX's commands and integration prerequisites are in
`CONTRIBUTING.md`; typical checks are `go test ./...`, `go vet ./...`, and
`git diff --check`. Use `go test -race` for affected concurrency paths and compile
relevant build-tag/platform variants when their declarations move.

Preserve the established OpenSBX goal of at least 90% aggregate statement coverage
for the default suite when measuring coverage. Do not exclude packages or add
tests that merely mirror moved declarations to manufacture that result. Reuse
meaningful existing regression coverage; add tests for newly exposed gaps.

Run live runtime tests when the changed behavior or repository requirements make
them relevant. A pure file split does not by itself justify image downloads,
daemon configuration changes, or creating runtime resources. Report skips and
environment blockers distinctly from passes. After checks pass, repeat them only
for new changes, failures, or uncovered acceptance criteria.

## Report

- **Assessment:** keep as-is, split files, or extract a package, with evidence.
- **Organization:** proposed or completed source-to-target map and rationale.
- **Behavior:** any semantic changes or compatibility concerns found; a
  structural-only refactor should have none.
- **Verification:** commands actually run and results, or explicitly not run
  for a read-only review.
- **Follow-ups:** separate optional simplifications from required corrections.

## Sources and interpretation

- [Effective Go](https://go.dev/doc/effective_go): idioms, naming, formatting, and
  commentary. It notes that it was written in 2009 and is not actively updated.
  Its statement about no line-length limit concerns individual lines, not file
  size; do not cite it as a file-organization rule.
- [Organizing a Go module](https://go.dev/doc/modules/layout): “A Go package can
  be split into multiple files, all residing within the same directory.” Also
  covers `internal` packages and `cmd` organization for server projects.
- [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments): naming,
  documentation, interfaces, and maintainable concurrency conventions.
- [Google Go Best Practices: Package size](https://google.github.io/styleguide/go/best-practices.html#package-size):
  “There is no ‘one type, one file’ convention as in some other languages.”
  Recommends focused, discoverable files and warns against both huge files and
  excessive tiny files. This is Google's style guidance, not a Go language rule.
- [Google Go Style Guide](https://google.github.io/styleguide/go/guide.html):
  clarity, simplicity, maintainability, and comments explaining why. Prefer the
  simplest mechanism that preserves the required behavior.

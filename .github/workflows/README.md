# GitHub Actions – CI

This repository uses a single workflow, `.github/workflows/ci.yml`, triggered on pushes/PRs targeting `main`.

## Jobs

1. **Tests**
   - Matrix over `ubuntu-latest` and `macos-latest`.
   - Runs `go test -v -cover ./...`, prints coverage summary, then `go test -race ./...`.
   - Publishes `coverage.out` from the Linux job for inspection.

2. **Lint**
   - Executes `golangci-lint run ./...`.

3. **Build**
   - Matrix over Linux/macOS.
   - Builds via `nix build -L .#tasmota-homekit`, verifies the resulting binary, and uploads the Linux artifact.

4. **Flake Check**
   - Runs `nix flake check` to validate the flake outputs/module definitions.

5. **NixOS Module Smoke Test**
   - Evaluates a small NixOS configuration that imports `flake.nix` and enables the module (basic sanity check).

Every job:

- Checks out the repo.
- Installs Nix via `NixOS/nix-installer-action` and warms the Nix store cache via `Mic92/hestia/action`.

A workflow-level default shell (`nix develop --command bash`) runs every step
inside the flake devShell, so tools are available without a per-step prefix. A
scheduled `gc.yml` workflow garbage-collects the hestia cache daily.

## Local Parity

Before pushing, run:

```bash
nix develop --command go test -v ./...
nix develop --command go test -race ./...
nix develop --command golangci-lint run ./...
nix build -L .#tasmota-homekit
nix flake check
```

## Badge

```markdown
[![CI](https://github.com/kradalby/tasmota-nefit/actions/workflows/ci.yml/badge.svg)](https://github.com/kradalby/tasmota-nefit/actions/workflows/ci.yml)
```

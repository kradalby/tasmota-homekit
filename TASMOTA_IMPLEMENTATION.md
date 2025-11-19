# Tasmota HomeKit – Implementation Notes

This document mirrors `../nefit-homekit/NEFIT_IMPLEMENTATION.md` so both HomeKit services stay in lockstep.

## Shared workflow (tasmota-homekit ⇄ nefit-homekit)

- Always work inside `nix develop`; the shell brings Go 1.25, golangci-lint, prek, kra helpers, etc.
- Use the flake apps instead of bespoke scripts: `nix run .#test`, `.#test-race`, `.#lint`, `.#coverage`, `nix build .#tasmota-homekit`.
- Run `nix flake check` before pushing; it builds the package, evaluates the module, and runs the VM smoke test wired in CI.
- GitHub Actions runs the same commands on macOS + Linux matrices, so keep them green locally.
- Vendor hash is pinned to `sha256-Ifb2XZsLrRWuW7zYvI9jTa9rk3bajRXVlwhVY/B2cbU=`. Any Go dependency change requires updating the hash and re-running `nix flake check`. Switch to `modSha256` once upstream nixpkgs gains proxy-less support.

## Architecture at a glance

- `config`: loads `TASMOTA_HOMEKIT_*` environment variables and validates plugs + networking.
- `plugs`: HuJSON parsing, state tracking, and events for each plug; orchestrates HTTP fast-path updates.
- `mqtt.go`: embeds Mochi MQTT, brokers telemetry, and feeds state changes onto the event channels.
- `hap.go`: builds brutella/hap accessories representing each plug, listens for commands, and publishes updates back onto the bus.
- `web.go`: kra/web server with elem-go dashboard, HTMX toggle handlers, SSE event stream, QR code output, metrics, and Tailscale listener integration.
- `app.go`: ties the pieces together, handles graceful shutdown, ensures MQTT + HTTP + HAP lifecycles stay synchronized.
- `nix/`: exposes the package, overlay, and NixOS module with firewall + credential wiring.

The runtime keeps parity with `nefit-homekit`: a single eventbus fans state out to HAP, MQTT, and the web dashboard so state stays consistent whether commands originate from HomeKit, the dashboard, or MQTT telemetry.

## Configuration surfaces

### Environment variables (`TASMOTA_HOMEKIT_*`)

- Required: `TASMOTA_HOMEKIT_PLUGS_CONFIG` (HuJSON path) and HAP secrets (`TASMOTA_HOMEKIT_HAP_PIN`, optional `TASMOTA_HOMEKIT_HAP_STORAGE_PATH`).
- Listeners: prefer `TASMOTA_HOMEKIT_HAP_ADDR`, `TASMOTA_HOMEKIT_WEB_ADDR`, and `TASMOTA_HOMEKIT_MQTT_ADDR` (Go-style `addr:port`). When omitted, the runtime combines `TASMOTA_HOMEKIT_*_BIND_ADDRESS` (defaults `0.0.0.0`) with `TASMOTA_HOMEKIT_*_PORT` (defaults `8080/8081/1883`).
- Logging: `TASMOTA_HOMEKIT_LOG_LEVEL`, `TASMOTA_HOMEKIT_LOG_FORMAT`.
- Tailscale: `TASMOTA_HOMEKIT_TS_HOSTNAME`, `TASMOTA_HOMEKIT_TS_AUTHKEY` (usually injected through the module credential loader).

Store the environment values in `/etc/tasmota-homekit/env` (or agenix) and reference it via `services.tasmota-homekit.environmentFile`.

### Plug definitions (HuJSON)

- `plugs.hujson` is loaded at startup and copied onto persistent storage via the module.
- Each entry includes `id`, `name`, `address`, optional `model`, and feature flags (power monitoring, etc.).
- The module expects a read-only file; host configs (e.g., `dotfiles/machines/home.ldn`) create `/etc/tasmota-homekit/plugs.hujson` via `environment.etc` to keep git history of plug assignments.

### NixOS module

`nixosModules.default` exposes:

- `services.tasmota-homekit.{enable,package,environmentFile,environment}`.
- `services.tasmota-homekit.ports.{hap,web,mqtt}`.
- `services.tasmota-homekit.hap.{pin,storagePath}`.
- `services.tasmota-homekit.plugsConfig`.
- `services.tasmota-homekit.log.{level,format}`.
- `services.tasmota-homekit.tailscale.{hostname,authKeyFile}`.
- `services.tasmota-homekit.openFirewall`, `.user`, `.group`.

The module provisions the service user, persistent directories, strict systemd hardening, firewall rules, and injects the Tailscale credential via `LoadCredential` when provided.

## Tailscale + web expectations

- kra/web binds to `:TASMOTA_HOMEKIT_WEB_PORT` locally and optionally brings up a tailnet HTTPS endpoint.
- `/`, `/toggle/<plug>`, `/events`, `/health`, `/metrics`, `/qrcode` must remain in sync with `nefit-homekit` for troubleshooting and shared UI widgets.
- SSE payloads stream plug state updates; keep payload format stable.

## Testing + CI expectations

- `nix run .#test` → `go test -v -cover ./...`; keep coverage high (current suite exercises config, plugs, MQTT, web, hap packages).
- `nix run .#test-race` must pass on Linux/macOS.
- `nix run .#lint` wraps golangci-lint and enforces slog-only logging plus the repo rule of keeping packages at the root.
- `nix flake check` builds the package for default systems and evaluates the module.
- `ci.yml` also evaluates a NixOS import that enables the service with a dummy plug to catch option regressions.

## Current status / hand-off notes

- Runtime parity with `nefit-homekit` is complete (shared kra server behavior, eventbus patterns, flake apps, module capabilities).
- Host configs now import both HomeKit modules; update `dotfiles/machines/home.ldn` whenever the module inputs/options change.
- Vendor hash is stable; re-run `nix flake check` whenever `go.mod`/`go.sum` change.
- Track the nixpkgs work for proxy-less `buildGoModule` so we can switch to `modSha256` as soon as it lands.

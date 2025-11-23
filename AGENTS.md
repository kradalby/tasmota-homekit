# AGENTS NOTES – `tasmota-homekit` (Tasmota HomeKit)

## Working Rules

- Enter the repository via `nix develop`; do not rely on ad-hoc Makefile targets.
- Use stdlib `slog` everywhere; legacy `log/slog` conversions are underway—finish them.
- Keep packages at repo root (e.g., `config`, `mqtt`, `web`, `hap`, `plugs`); avoid `internal/`, `pkg/`, or `pkgs/`.
- HTML rendering stays in [elem-go](https://github.com/chasefleming/elem-go).
- Avoid wrappers unless they clearly improve lifecycle/testability.
- Align structure/config/options with `nefit-homekit`.

## Daily Commands (inside `nix develop`)

```
go test ./...
golangci-lint run ./...
prek run --all-files
nix flake check
```

## Repository Notes

- `flake.nix` is the source of truth for shells, packages, and the NixOS module.
- Binary entrypoint lives in `cmd/tasmota-homekit` (calls `tasmotahomekit.Main()` from `app.go`).
- Reusable helpers should live in `../kra` or be exported here so `nefit-homekit` can import them.
- Keep README concise and factual; remove emojis/brag copy when editing.
- Keep planning notes updated privately without referencing them within documentation.
- Listener env vars mirror `nefit-homekit`: prefer `TASMOTA_HOMEKIT_{HAP,WEB,MQTT}_ADDR` Go-style bindings so tests/docs stay aligned with the module’s `bindAddresses.*` options.
- Web routes must match `nefit-homekit`: `/`, `/toggle/<plug>`, `/events`, `/health`, `/metrics`, `/qrcode`, `/debug/eventbus`. `/events` now streams JSON `StateUpdateEvent` payloads; keep the SSE tests and elem-go UI in sync.
- CI (`.github/workflows/ci.yml`) runs tests (Linux/macOS, coverage + race), golangci-lint, `nix build`, `nix flake check`, and a NixOS module eval. Match these locally before pushing.
- Flake apps: `nix run .#test`, `nix run .#test-race`, `nix run .#lint`, `nix run .#coverage`; use these instead of ad-hoc scripts.

## Expectations

- Tests and lint **must** pass for every change/commit.
- Maintain parity with `nefit-homekit` (config schema, service layout, CI steps, docs).
- Document any repo-specific gotchas here for future agents.

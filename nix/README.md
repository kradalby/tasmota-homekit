# NixOS Module & Tests

This directory contains the NixOS module and tests for `tasmota-homekit`.

## Files

- `module.nix` – defines `services.tasmota-homekit`
- `test.nix` – basic NixOS VM test that boots the service with a sample plugs config

## Running

```bash
# Run every check
nix flake check

# Only the module test
nix build .#checks.x86_64-linux.module-test
```

## Test Coverage

`test.nix` ensures:

- Service starts with a HuJSON plugs file
- Web/HAP/MQTT ports are listening
- HTTP endpoint responds
- Systemd unit runs as the expected user

Extend these tests whenever you add significant functionality or module options.

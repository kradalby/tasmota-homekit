# Tasmota HomeKit Bridge

Control your Tasmota smart plugs through Apple HomeKit and a simple web interface.

## Features

- **HomeKit Integration**: Full HomeKit support for Tasmota plugs with QR code pairing
- **Hybrid Control**: Fast direct HTTP commands + reactive MQTT updates
- **Web Interface**: Simple control panel with HomeKit QR code, accessible over Tailscale or local network
- **Tailscale Integration**: Built-in Tailscale support via kra/web for secure remote access
- **Event-Driven**: Real-time state synchronization across all interfaces
- **Embedded MQTT**: No external broker needed
- **Single Binary**: Easy deployment with NixOS module included

## Quick Start

### Prerequisites

- Nix with flakes enabled
- Tasmota devices on your network
- (Optional) Tailscale for remote access

### Development

```bash
# Enter development shell
nix develop

# Quick commands (flake apps — no Makefile required)
# Run everything inside `nix develop`; the flake apps replace the old Makefile targets.
nix run .#test          # go test ./...
nix run .#test-race     # go test -race
nix run .#lint          # golangci-lint
nix run .#coverage      # HTML coverage report
nix flake check         # NixOS module + packaging checks
nix build .#tasmota-homekit

# Run in development mode
go run ./cmd/tasmota-homekit

# Run via Nix
nix run .#tasmota-homekit
```

## Configuration

### Project Layout

- `cmd/tasmota-homekit`: entrypoint that wires everything together
- `config`: environment configuration loader/validator
- `plugs`: plug configuration, state management, MQTT integration
- `hap.go`, `web.go`, `mqtt.go`: runtime components that consume the shared packages

### Plug Configuration

Copy `plugs.hujson.example` to `plugs.hujson` and configure your devices:

```jsonc
{
  "plugs": [
    {
      "id": "living-room-lamp",
      "name": "Living Room Lamp",
      "address": "192.168.1.100",
    },
  ],
}
```

### Environment Variables

Copy `.env.example` to `.env` and configure:

```bash
TASMOTA_HOMEKIT_HAP_PIN=12345678
TASMOTA_HOMEKIT_HAP_ADDR=0.0.0.0:8080
TASMOTA_HOMEKIT_BRIDGE_NAME=tasmota-homekit-dev
TASMOTA_HOMEKIT_TS_HOSTNAME=tasmota-homekit-dev
TASMOTA_HOMEKIT_PLUGS_CONFIG=./plugs.hujson
```

For NixOS, convert that file into `/etc/tasmota-homekit/env` (or an agenix secret) and point `services.tasmota-homekit.environmentFile` at it so the module loads the same values that the CLI uses during development.

See `.env.example` for the full list of options (`TASMOTA_HOMEKIT_{HAP,WEB,MQTT}_ADDR` all accept Go-style `addr:port` strings; if omitted, they fall back to the matching `*_BIND_ADDRESS` + `*_PORT` variables with defaults of `0.0.0.0` and `8080/8081/1883`).

### Web Interface & Endpoints

The embedded kra web server exposes a consistent set of endpoints (locally and over Tailscale):

- `/` – elem-go dashboard with plug controls, event log, and HomeKit QR code.
- `/toggle/<plug-id>` – HTMX form to toggle a specific plug.
- `/events` – JSON SSE stream mirroring `nefit-homekit` (`StateUpdateEvent` payloads with plug name, connection state, etc.).
- `/health` – JSON health summary (plug count, SSE clients).
- `/metrics` – Prometheus metrics (register your collector here).
- `/qrcode` – Plain-text QR/PIN output for headless setups.
- `/debug/eventbus` – Diagnostics page mirroring `nefit-homekit` (live state + SSE client count).

Set `TASMOTA_HOMEKIT_BRIDGE_NAME` (and optionally `TASMOTA_HOMEKIT_TS_HOSTNAME`) if you want a custom HomeKit/Tailscale identity. By default, both names stay in sync and use `tasmota-homekit`. Provide `TASMOTA_HOMEKIT_TS_AUTHKEY` to enable Tailscale; kra handles the auth-key lifecycle, so no temp files are needed. `TASMOTA_HOMEKIT_TS_STATE_DIR` controls where the embedded tsnet instance stores its state (defaults to `./data/tailscale` and maps to `dataDir/tailscale` when using the NixOS module).

## NixOS Deployment

The NixOS module includes comprehensive security hardening and follows systemd best practices:

**Features:**

- Automatic startup with `multi-user.target`
- Waits for network to be online before starting
- Automatic restart on failure (max 5 attempts per minute)
- Systemd security hardening (filesystem isolation, syscall filtering, etc.)
- Dedicated dynamic user with minimal privileges
- Persistent state and cache directories
- Secure credential loading for secrets
- Built-in Tailscale integration for secure remote access
- HomeKit QR code displayed in terminal and web interface

Add to your NixOS configuration:

```nix
{
  inputs.tasmota-homekit.url = "github:kradalby/tasmota-homekit";

  # In your configuration:
  imports = [ inputs.tasmota-homekit.nixosModules.default ];

  services.tasmota-homekit = {
    enable = true;

    # Automatically open firewall ports (default: false)
    openFirewall = true;

    # Port configuration (defaults shown)
    ports = {
      hap = 8080;   # HomeKit Accessory Protocol
      web = 8081;   # Web interface
      mqtt = 1883;  # MQTT broker
    };

    # Data + HomeKit configuration
    dataDir = "/var/lib/tasmota-homekit";  # Base directory; module uses dataDir/hap and dataDir/tailscale
    hap = {
      pin = "12345678";  # Default: "00102003"
    };

    # Path to plugs configuration file (required)
    plugsConfig = /etc/tasmota-homekit/plugs.hujson;

    # Optional: Tailscale configuration for remote access
    # Setting authKeyFile enables Tailscale integration
    tailscale = {
      hostname = "tasmota-homekit";  # Tailscale hostname (default: "tasmota-homekit")
      authKeyFile = "/run/secrets/tailscale-authkey";  # Path to auth key file (enables Tailscale when set)
    };

    # Optional: Additional environment variables
    # environment = {
    #   CUSTOM_VAR = "value";
    # };

    # Optional: Load secrets from file
    # environmentFile = "/run/secrets/tasmota-homekit.env";
  };
}
```

**Service Management:**

```bash
# Check service status
systemctl status tasmota-homekit

# View logs
journalctl -u tasmota-homekit -f

# Restart service
systemctl restart tasmota-homekit
```

**Storage Locations:**

- Data dir (configurable via `services.tasmota-homekit.dataDir`): `/var/lib/tasmota-homekit/`
- HAP pairing state: `$dataDir/hap`
- Tailscale state: `$dataDir/tailscale`
- Cache: `/var/cache/tasmota-homekit/`
- Runtime: `/run/tasmota-homekit/`

**Firewall Configuration:**

The module can automatically open the required firewall ports when `openFirewall = true`:

- HAP port (default: 8080) - HomeKit Accessory Protocol
- Web port (default: 8081) - Web interface
- MQTT port (default: 1883) - Embedded MQTT broker

You can customize the ports using the `ports` option. Port values are automatically passed to the service via environment variables.

**Tailscale Integration:**

When `tailscale.authKeyFile` is set, the web interface is accessible via:

- **HTTPS**: `https://<hostname>` (Tailscale with automatic TLS certificates)
- **HTTP**: `http://localhost:<port>` (local access)

The service uses [kra/web](https://github.com/kradalby/kra) to provide seamless Tailscale integration. The auth key is securely loaded from the file specified in `tailscale.authKeyFile`. Omit `authKeyFile` to disable Tailscale (local-only mode).

### Available Options

```
services.tasmota-homekit.enable             # Enable the service
services.tasmota-homekit.package            # Package derivation (defaults to pkgs.tasmota-homekit)
services.tasmota-homekit.environmentFile    # Path to file containing TASMOTA_HOMEKIT_* values
services.tasmota-homekit.environment        # Attrset of extra TASMOTA_HOMEKIT_* overrides
services.tasmota-homekit.ports.hap          # HAP port (default 8080)
services.tasmota-homekit.ports.web          # Web UI port (default 8081)
services.tasmota-homekit.ports.mqtt         # Embedded MQTT broker port (default 1883)
services.tasmota-homekit.bindAddresses.hap  # IP for HAP listener (default 0.0.0.0)
services.tasmota-homekit.bindAddresses.web  # IP for web listener (default 0.0.0.0)
services.tasmota-homekit.bindAddresses.mqtt # IP for MQTT listener (default 0.0.0.0)
services.tasmota-homekit.dataDir            # Base directory for persistent data (contains hap + tailscale)
services.tasmota-homekit.hap.pin            # HomeKit PIN (8 digits)
services.tasmota-homekit.plugsConfig        # HuJSON description of plugs
services.tasmota-homekit.bridgeName         # Override HomeKit bridge name (defaults to TS hostname)
services.tasmota-homekit.log.level          # slog level (debug/info/warn/error)
services.tasmota-homekit.log.format         # slog format (json/console)
services.tasmota-homekit.tailscale.hostname # Tailnet hostname
services.tasmota-homekit.tailscale.authKeyFile # Credential used for Tailscale auth
services.tasmota-homekit.openFirewall       # Open HAP/web/MQTT and mDNS ports automatically
services.tasmota-homekit.user               # Service user (default tasmota-homekit)
services.tasmota-homekit.group              # Service group (default tasmota-homekit)
```

Use the module options instead of ad-hoc environment variables so deployments stay consistent with the README and CI.

## Continuous Integration

`.github/workflows/ci.yml` enforces the same workflow used locally:

- `go test -v ./...` with coverage on Linux and macOS
- `go test -race ./...`
- `golangci-lint run ./...`
- `nix build .#tasmota-homekit`
- `nix flake check` followed by the module smoke test

Run the flake apps (`nix run .#test`, `nix run .#test-race`, `nix run .#lint`, `nix flake check`) before pushing so GitHub Actions stays green.

## How It Works

1. **Startup**: Server connects to all configured Tasmota plugs and configures them to use the embedded MQTT broker
2. **Monitoring**: Background process validates plugs connect to MQTT within 60 seconds
   - If a plug never connects, attempts automatic reconfiguration
   - Ongoing monitoring detects plugs that go offline and validates connectivity
   - Automatically reconfigures MQTT if plug is reachable via HTTP but not MQTT
3. **Control**: Commands from HomeKit/Web UI are sent directly via HTTP for low latency
4. **Updates**: Plug state changes (button presses, power events) are published via MQTT
5. **Sync**: All interfaces stay synchronized through the event bus

## Using with HomeKit

After starting the server, you'll see a QR code in the terminal output:

```
========================================
HomeKit bridge ready - pair with PIN: 00102003

[QR CODE DISPLAYED HERE]

========================================
```

**To add to HomeKit:**

**Option 1: Scan QR Code (Easiest)**

1. Open the Home app on your iPhone/iPad
2. Tap the "+" button → "Add Accessory"
3. Point your camera at the QR code (shown in terminal or web interface)
4. Follow the on-screen instructions

**Option 2: Manual PIN Entry**

1. Open the Home app on your iPhone/iPad
2. Tap the "+" button → "Add Accessory"
3. Tap "More options..." at the bottom
4. Select "Tasmota Bridge" from the list
5. Enter the PIN when prompted (default: `00102003`)
6. Follow the on-screen instructions

Your Tasmota plugs will appear as individual outlets in HomeKit. You can:

- Turn them on/off via Siri, Control Center, or Home app
- Add them to scenes and automations
- Control them remotely (if you have a HomeKit hub)

**Important**: Change the default PIN by setting `TASMOTA_HOMEKIT_HAP_PIN` in your environment.

## Web Interface

A simple web dashboard is available at `http://localhost:8081` (configurable via ports) or via Tailscale when enabled.

Features:

- **HomeKit QR Code**: Scan directly from the web interface to pair with HomeKit
- **PIN Display**: HomeKit pairing PIN shown prominently
- View all configured plugs and their current state
- Toggle plugs on/off with a single click
- **Connection monitoring**: Visual indicators show plug connectivity status
  - Green: Connected (seen in last 30s)
  - Orange: Stale (seen 30-60s ago)
  - Red: Disconnected (not seen in 60+ seconds)
- **Lifecycle table**: `/debug/eventbus` renders MQTT/HAP/Web status rows so you can confirm which components are connected without tailing logs.
- See recent events and state changes
- **Real-time automatic updates** via Server-Sent Events (SSE)
- HTMX-powered interface for smooth, reactive UX
- Works without JavaScript (graceful degradation)
- **Secure remote access** via Tailscale with automatic TLS

## Architecture

```
  HomeKit          Browser
     │                │
     ▼                ▼
┌──────────┐    ┌───────────┐
│HAPManager│    │ WebServer │ ◀── Snapshot() for pages and new SSE streams
└────┬─────┘    └─────┬─────┘
     │ SetPower       │ SetPower
     ▼                ▼
┌──────────────────────────┐  HTTP: Power, Status 0  ┌───────────────┐
│       PlugManager        │ ──────────────────────▶ │ Tasmota plugs │
│ immutable state snapshot │                         └───────┬───────┘
│ one writer: merge, store │                                 │ MQTT
│ and publish, in order    │                                 ▼
│                          │ ◀── StateChangedEvent ── MQTT broker + MQTTHook
└────────────┬─────────────┘
             │ StateUpdateEvent
             ├──▶ HAPManager → HomeKit characteristics
             └──▶ WebServer  → SSE → browsers
```

- **PlugManager owns plug state.** It keeps one immutable snapshot of every
  plug behind an atomic pointer. Each change (MQTT merge or `Status 0` reply)
  builds a new snapshot, stores it and publishes a `StateUpdateEvent` under one
  writer mutex, so subscribers see changes in the order they were stored.
  `Snapshot()` and `Plug()` read without locking.
- **Newer observation wins.** Each reading carries when it was observed: an
  MQTT report its receive time, a `Status 0` reply its request's send time.
  Power state and energy are ordered separately; a reading older than the
  stored one of its kind is dropped, so a report queued on the eventbus
  cannot undo a newer reply. `LastSeen` and `LastUpdated` never move back.
- **Commands are synchronous.** HomeKit and web writes call
  `PlugManager.SetPower`, which sends `Power` and then reads `Status 0`. A
  HomeKit write the plug rejects fails, so HomeKit keeps its old value.
- **Views follow the snapshot.** HAPManager subscribes to state updates and
  then seeds its accessories from the snapshot; WebServer renders pages from
  the snapshot and starts each SSE stream with it before streaming updates.
- **EventBus** (`tailscale.com/util/eventbus`) carries MQTT reports
  (`StateChangedEvent`), state updates, commands, errors and component status.

Files:

```
app.go              wiring and startup
plugs/manager.go    PlugManager: state, Tasmota HTTP client, MQTT merge
plugs/types.go      plug config, State, events
hap.go              HAPManager: HomeKit accessories
web.go              WebServer: dashboard, SSE, debug pages
mqtt.go             MQTTHook: Tasmota telemetry
events/             eventbus wrapper and shared event types
metrics/            Prometheus collector
```

## Development Status

🚧 **Early Development** - Core functionality in progress

See [TASMOTA_IMPLEMENTATION.md](./TASMOTA_IMPLEMENTATION.md) for the full implementation plan.

## License

MIT

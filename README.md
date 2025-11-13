# Tasmota HomeKit Bridge

Control your Tasmota smart plugs through Apple HomeKit and a simple web interface.

## Features

- **HomeKit Integration**: Full HomeKit support for Tasmota plugs
- **Hybrid Control**: Fast direct HTTP commands + reactive MQTT updates
- **Web Interface**: Simple control panel accessible over Tailscale or local network
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

# Run tests
make test

# Run linter
make lint

# Build
make build

# Run in development mode
make dev
```

## Configuration

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
TASMOTA_HOMEKIT_HAP_PORT=8080
TASMOTA_HOMEKIT_PLUGS_CONFIG=./plugs.hujson
```

See `.env.example` for all available options.

## NixOS Deployment

Add to your NixOS configuration:

```nix
{
  inputs.tasmota-homekit.url = "github:kradalby/tasmota-homekit";

  # In your configuration:
  imports = [ inputs.tasmota-homekit.nixosModules.default ];

  services.tasmota-homekit = {
    enable = true;

    environment = {
      TASMOTA_HOMEKIT_HAP_PIN = "12345678";
      TASMOTA_HOMEKIT_HAP_PORT = "8080";
      TASMOTA_HOMEKIT_PLUGS_CONFIG = "/etc/tasmota-homekit/plugs.hujson";
    };

    # Optional: Load secrets from file
    # environmentFile = "/run/secrets/tasmota-homekit.env";
  };
}
```

## How It Works

1. **Startup**: Server connects to all configured Tasmota plugs and configures them to use the embedded MQTT broker
2. **Control**: Commands from HomeKit/Web UI are sent directly via HTTP for low latency
3. **Updates**: Plug state changes (button presses, power events) are published via MQTT
4. **Sync**: All interfaces stay synchronized through the event bus

## Using with HomeKit

After starting the server, you'll see a message like:

```
HomeKit bridge ready - pair with PIN: 00102003
```

**To add to HomeKit:**

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

A simple web dashboard is available at `http://localhost:8081` (configurable via `TASMOTA_HOMEKIT_WEB_PORT`).

Features:

- View all configured plugs and their current state
- Toggle plugs on/off with a single click
- See recent events and state changes
- Real-time status updates

The interface uses simple, clean HTML with no JavaScript required.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Tasmota HomeKit Bridge                          │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────┐      ┌──────────┐      ┌──────────┐                 │
│  │   HAP    │      │   Web    │      │   MQTT   │                 │
│  │  Server  │      │  Server  │      │  Broker  │                 │
│  │ :8080    │      │  :8081   │      │  :1883   │                 │
│  └────┬─────┘      └────┬─────┘      └────┬─────┘                 │
│       │                 │                  │                        │
│       ▼                 ▼                  ▼                        │
│  ┌────────────┐    ┌──────────┐      ┌──────────┐                 │
│  │    HAP     │    │   Web    │      │   MQTT   │                 │
│  │  Manager   │    │  Manager │      │   Hook   │                 │
│  └─────┬──────┘    └─────┬────┘      └─────┬────┘                 │
│        │                 │                  │                       │
│        │    ┌────────────┴───────────┬──────┘                      │
│        │    │                        │                             │
│        ▼    ▼                        ▼                             │
│  ┌───────────────────────────────────────────┐                     │
│  │          Event Channels (Go)              │                     │
│  │  • commands     (PlugCommandEvent)        │                     │
│  │  • stateChanges (PlugStateChangedEvent)   │                     │
│  │  • errors       (PlugErrorEvent)          │                     │
│  └──────────────────┬────────────────────────┘                     │
│                     │                                              │
│                     ▼                                              │
│            ┌─────────────────┐                                     │
│            │  PlugManager    │                                     │
│            │ (Thread-Safe)   │                                     │
│            │  • State Map    │                                     │
│            │  • RW Mutex     │                                     │
│            └────────┬────────┘                                     │
│                     │                                              │
└─────────────────────┼──────────────────────────────────────────────┘
                      │
                      │ HTTP Commands (Fast)
                      ▼
         ┌────────────────────────────┐
         │     Tasmota Devices        │
         │  ┌──────┐ ┌──────┐ ┌────┐ │
         │  │Plug 1│ │Plug 2│ │... │ │
         │  └──┬───┘ └──┬───┘ └─┬──┘ │
         └─────┼────────┼───────┼─────┘
               │        │       │
               └────────┴───────┘
                      │
                      │ MQTT Telemetry (Reactive)
                      ▼
              (Back to MQTT Broker)


Data Flow:
──────────
1. HomeKit/Web → commands → PlugManager → HTTP → Tasmota
2. Tasmota → MQTT → MQTTHook → stateChanges → HAPManager
3. Thread-safe state via sync.RWMutex in PlugManager

Files:
──────
main.go   - Orchestration & initialization
types.go  - Data structures & events
plug.go   - PlugManager (state + Tasmota client)
hap.go    - HAPManager (HomeKit accessories)
web.go    - WebServer (dashboard)
mqtt.go   - MQTTHook (telemetry processing)
```

## Development Status

🚧 **Early Development** - Core functionality in progress

See [TASMOTA_IMPLEMENTATION.md](./TASMOTA_IMPLEMENTATION.md) for the full implementation plan.

## License

MIT

# Tasmota HomeKit Server Implementation Plan

## Core Principle

**Simplicity above all.** Start with everything in main.go. Extract packages only when complexity truly demands it. Don't abstract prematurely.

## Project Overview

Build a HomeKit bridge server that controls Tasmota smart plugs using:

- **HomeKit Interface**: Expose plugs to Apple HomeKit
- **Web Interface**: Simple control panel over Tailscale/local network
- **Hybrid Control**: Direct HTTP commands for speed + MQTT for reactive updates
- **Event-Driven Architecture**: All components communicate via event bus

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Tasmota HomeKit Server                    │
├─────────────────────────────────────────────────────────────┤
│                                                               │
│  ┌──────────┐      ┌───────────┐      ┌──────────────┐     │
│  │   HAP    │◄────►│  Event    │◄────►│   MQTT       │     │
│  │  Server  │      │   Bus     │      │   Server     │     │
│  └──────────┘      └─────┬─────┘      └──────────────┘     │
│                           │                                   │
│                           │                                   │
│  ┌──────────┐      ┌─────▼─────┐      ┌──────────────┐     │
│  │   Web    │◄────►│  Plug     │◄────►│   Tasmota    │     │
│  │    UI    │      │  Manager  │      │   Plugs      │     │
│  └──────────┘      └───────────┘      └──────────────┘     │
│                                                               │
└─────────────────────────────────────────────────────────────┘
```

## Implementation Approach

**Start Simple, Refactor When Needed**

Begin with everything in `main.go` and only extract packages when complexity truly demands it.

### Initial Structure

```
main.go           - Everything starts here
  ├─ Config loading (env vars + HuJSON)
  ├─ Event bus setup (tailscale.com/util/eventbus)
  ├─ MQTT broker (mochi-mqtt embedded)
  ├─ Plug management (tasmota-go client)
  ├─ HAP server (brutella/hap)
  └─ Web UI (kraweb + elem-go)
```

**MQTT Broker Choice:** `github.com/mochi-mqtt/server/v2`

- Simple, pure Go, actively maintained
- Easy to embed in main.go

### When to Extract Packages

Only create packages when:

1. Code becomes difficult to navigate (>500-1000 lines)
2. Clear separation of concerns emerges naturally
3. Testing requires isolation
4. Reusability across multiple files is needed

Potential future packages (only if needed):

- `config/` - If config logic becomes complex
- `mqtt/` - If MQTT handling needs isolation
- `web/` - If UI grows significantly

**Don't prematurely abstract.** The event bus provides the decoupling we need.

## Configuration Schema

### Environment Variables (prefix: `TASMOTA_HOMEKIT_`)

```bash
# HAP Configuration
TASMOTA_HOMEKIT_HAP_PIN=12345678           # HomeKit PIN
TASMOTA_HOMEKIT_HAP_PORT=8080              # HAP server port
TASMOTA_HOMEKIT_HAP_STORAGE_PATH=/data/hap # HAP storage directory

# Tailscale Configuration
TASMOTA_HOMEKIT_TS_HOSTNAME=tasmota-server # Tailscale hostname
TASMOTA_HOMEKIT_TS_AUTHKEY=tskey-xxx       # Tailscale auth key (optional)

# Web Server
TASMOTA_HOMEKIT_WEB_PORT=8081              # Web UI port

# MQTT Configuration
TASMOTA_HOMEKIT_MQTT_PORT=1883             # Embedded MQTT broker port

# Plug Configuration
TASMOTA_HOMEKIT_PLUGS_CONFIG=/config/plugs.hujson
```

### Plug Configuration (HuJSON)

```jsonc
{
  "plugs": [
    {
      "id": "living-room-lamp", // Unique identifier
      "name": "Living Room Lamp", // Display name
      "address": "192.168.1.100", // IP or hostname
      "model": "Sonoff S31", // Optional: plug model
      "features": {
        // Optional: feature flags
        "power_monitoring": true,
        "energy_tracking": true,
      },
    },
    {
      "id": "bedroom-fan",
      "name": "Bedroom Fan",
      "address": "192.168.1.101",
    },
  ],
}
```

## Data Flow

### Startup Sequence

1. **Load Configuration**
   - Parse environment variables
   - Load and validate plug configuration

2. **Initialize Event Bus**
   - Set up event types and channels

3. **Start MQTT Broker**
   - Bind to configured port
   - Set up authentication (if needed)

4. **Configure Plugs**
   - Connect to each plug via `tasmota-go`
   - Configure MQTT settings on each plug
   - Subscribe to plug state topics
   - Initial state fetch

5. **Start HAP Server**
   - Create accessories for each plug
   - Wire up event handlers
   - Publish to HomeKit

6. **Start Web Server**
   - Initialize kraweb with Tailscale
   - Serve UI and API endpoints
   - Expose metrics

### Control Flow (User → Plug)

```
User Action (HomeKit/Web) → Event Bus → Plug Manager
                                            ↓
                               Direct HTTP Command (fast)
                                            ↓
                                     Tasmota Plug
                                            ↓
                               MQTT State Update → Event Bus
                                            ↓
                               HAP + Web UI Update
```

### Update Flow (Plug → UI)

```
Tasmota Plug → MQTT Publish → MQTT Server → Event Bus
                                                ↓
                                    ┌───────────┴────────────┐
                                    ↓                        ↓
                              HAP Update                 Web UI Update
```

### Hybrid Command Strategy

For each command:

1. Send direct HTTP command via `tasmota-go` (fastest path)
2. If direct command fails, fall back to MQTT publish
3. Wait for MQTT confirmation (timeout: 2s)
4. Update state via event bus

## Implementation Phases

**Build incrementally in main.go, extract packages only when needed**

### Phase 0: Project Setup ✅ Complete

- [x] Initialize Go module
- [x] Create .gitignore
- [x] Create Nix flake with dev shell (Go 1.25, golangci-lint)
- [x] Set up GitHub Actions (test, lint, build)
- [x] Create example configuration files
- [x] Create README

### Phase 1: Core Functionality (main.go) ✅ Complete

- [x] Config loading
  - [x] Environment variable parsing (go-env)
  - [x] HuJSON plug config parser
  - [x] Basic validation
- [x] Event system with channels
  - [x] Define event types (PlugStateChanged, CommandRequested, etc.)
  - [x] Event processor goroutine
- [x] Embedded MQTT broker
  - [x] Start mochi-mqtt server
  - [x] MQTT message hook for Tasmota messages
  - [x] Topic parsing and routing
- [x] Plug integration
  - [x] Load plugs from config
  - [x] Connect via tasmota-go
  - [x] Configure each plug to use embedded MQTT
  - [x] MQTT subscription handling
  - [x] Implement direct command sending
  - [x] Status fetching
- [x] Basic tests written and passing

### Phase 2: HomeKit Bridge ✅ Complete

- [x] HAP server setup
  - [x] Create bridge accessory
  - [x] Create outlet accessory for each plug
  - [x] Wire up to event channels
  - [x] Handle commands from HomeKit
- [x] State synchronization (channels → HAP)
- [x] Documentation for HomeKit pairing

### Phase 3: Web Interface ✅ Complete

- [x] Web server setup (HTTP)
  - [x] Basic routing
  - [x] Clean shutdown handling
- [x] Simple UI with elem-go
  - [x] Plug list/dashboard with status cards
  - [x] On/off toggle controls
  - [x] Event log/debug view
  - [x] Clean, responsive styling
- Note: Tailscale/Prometheus can be added later via kraweb if needed

### Phase 4: Polish & Deployment

- [ ] Error handling and recovery
- [ ] Logging improvements
- [ ] Performance testing
- [ ] Extract packages if main.go is too large (>1000 lines)
- [ ] Documentation
  - [ ] README with setup instructions
  - [ ] Example configs
- [ ] NixOS module
  - [ ] Service definition
  - [ ] Configuration options
  - [ ] systemd integration
- [ ] Test NixOS module deployment

## Testing Strategy

**Test what matters, don't over-test**

- Write tests for complex logic (config parsing, state management)
- Use table-driven tests for multiple scenarios
- Mock external dependencies when needed (HTTP servers, MQTT)
- Integration tests for critical flows (plug control, state sync)
- Manual testing with real plugs during development

### CI/CD

- Run `go test ./...` in Nix shell
- Run golangci-lint
- Build binary
- Build and test NixOS module

## Development Workflow

**Simplicity First**

1. Build in main.go until it's unwieldy
2. Write tests for non-trivial logic
3. Run golangci-lint often
4. Refactor when complexity demands it, not before
5. Update this document as we learn and evolve

## Technical Decisions

### Why Direct Commands + MQTT?

- **Direct commands**: Lowest latency for user actions (HomeKit responsiveness)
- **MQTT**: Reactive updates for plug state changes (button presses, power monitoring)
- **Best of both worlds**: Fast control + real-time monitoring

### Why Event Bus?

- Decouples components
- Makes testing easier (mock event sources)
- Enables debugging (log all events)
- Allows future extensions (new interfaces, automation rules)

### Why Embedded MQTT vs External?

- Simpler deployment (single binary)
- Automatic configuration (no external broker setup)
- Controlled environment (authentication, permissions)
- Still allows external MQTT clients if needed

## Open Questions

1. **MQTT Authentication**: Do we need authentication for the embedded broker?
   - Decision: Start without, add if needed

2. **Plug Discovery**: Should we support automatic plug discovery (mDNS)?
   - Decision: Start with static configuration, consider auto-discovery later

3. **State Persistence**: Should we persist plug states across restarts?
   - Decision: Fetch state on startup, persist only HAP pairing data

4. **Multiple Plug Types**: Should we support different Tasmota device types (lights, sensors)?
   - Decision: Start with plugs/switches, design for extensibility

5. **HTMX for Web UI**: Use HTMX for reactive UI or plain elem-go?
   - Decision: Start with SSE for real-time updates, evaluate HTMX if needed

## Success Criteria

- [ ] Plugs appear in HomeKit and are controllable
- [ ] Command latency < 500ms for local network
- [ ] MQTT updates propagate to HomeKit within 1 second
- [ ] Web UI provides real-time status updates
- [ ] All tests pass in CI
- [ ] golangci-lint passes with zero issues
- [ ] NixOS module deploys successfully
- [ ] Documentation is complete and accurate

## Next Steps

1. Set up Nix flake with dev shell (Go 1.25, golangci-lint)
2. Create GitHub Actions workflows
3. Start building in main.go:
   - Config loading
   - Event bus
   - MQTT broker
   - Plug integration

---

**Last Updated**: 2025-11-12
**Current Phase**: Phase 3 - Web Interface
**Principle**: Start simple, refactor when needed

## What's Working

✅ **Full HomeKit Integration** - Control Tasmota plugs via Apple Home app, Siri, and automations
✅ **Web Dashboard** - Simple, clean UI to view and control plugs at http://localhost:8081
✅ **Direct Command Control** - Fast HTTP commands to plugs (tasmota-go)
✅ **MQTT State Sync** - Reactive updates from plug button presses and events
✅ **Embedded MQTT Broker** - No external dependencies, auto-configures plugs
✅ **Event-driven Architecture** - All components communicate via channels
✅ **Tests & CI** - Tests pass (3/3), golangci-lint passes (0 issues), GitHub Actions ready
✅ **Single Binary** - Everything in main.go, simple deployment

## Quick Start

```bash
# Configure plugs
cp plugs.hujson.example plugs.hujson
# Edit plugs.hujson with your device IPs

# Run server
./tasmota-homekit

# Access interfaces
# - HomeKit: Pair with PIN 00102003
# - Web UI: http://localhost:8081
# - MQTT: localhost:1883
```

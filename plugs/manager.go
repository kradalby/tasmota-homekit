package plugs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kradalby/tasmota-go"
	"github.com/kradalby/tasmota-nefit/events"
	"tailscale.com/util/eventbus"
)

// Manager manages all Tasmota plug clients and their state.
type Manager struct {
	plugs            map[string]*Info
	states           map[string]*State
	mu               sync.RWMutex
	commands         chan CommandEvent
	statePublisher   *eventbus.Publisher[StateChangedEvent]
	errorPublisher   *eventbus.Publisher[ErrorEvent]
	stateSubscriber  *eventbus.Subscriber[StateChangedEvent]
	eventBus         *events.Bus
	stateEventClient *eventbus.Client
}

// Info holds the client and configuration for a plug.
type Info struct {
	Config Plug
	Client client
}

type client interface {
	ExecuteCommand(context.Context, string) ([]byte, error)
	ExecuteBacklog(context.Context, ...string) ([]byte, error)
}

type tasmotaClient struct {
	*tasmota.Client
}

func (c *tasmotaClient) ExecuteCommand(ctx context.Context, cmd string) ([]byte, error) {
	return c.Client.ExecuteCommand(ctx, cmd)
}

func (c *tasmotaClient) ExecuteBacklog(ctx context.Context, cmds ...string) ([]byte, error) {
	return c.Client.ExecuteBacklog(ctx, cmds...)
}

// NewManager creates a new plug manager.
func NewManager(
	plugConfigs []Plug,
	commands chan CommandEvent,
	bus *events.Bus,
) (*Manager, error) {
	client, err := bus.Client(events.ClientPlugManager)
	if err != nil {
		return nil, fmt.Errorf("failed to get plugmanager eventbus client: %w", err)
	}

	pm := &Manager{
		plugs:            make(map[string]*Info),
		states:           make(map[string]*State),
		commands:         commands,
		statePublisher:   eventbus.Publish[StateChangedEvent](client),
		errorPublisher:   eventbus.Publish[ErrorEvent](client),
		stateSubscriber:  eventbus.Subscribe[StateChangedEvent](client),
		eventBus:         bus,
		stateEventClient: client,
	}

	for _, plugConfig := range plugConfigs {
		client, err := tasmota.NewClient(plugConfig.Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create client for %s: %w", plugConfig.ID, err)
		}

		pm.plugs[plugConfig.ID] = &Info{
			Config: plugConfig,
			Client: &tasmotaClient{Client: client},
		}

		pm.states[plugConfig.ID] = &State{
			ID:            plugConfig.ID,
			Name:          plugConfig.Name,
			On:            false,
			LastUpdated:   time.Now(),
			MQTTConnected: false,
			LastSeen:      time.Time{},
		}

		pm.publishStateUpdate("initial", plugConfig.ID, *pm.states[plugConfig.ID])

		slog.Info("Initialized plug client",
			"id", plugConfig.ID,
			"address", plugConfig.Address,
		)
	}

	return pm, nil
}

// ConfigureMQTT configures a plug to use the specified MQTT broker.
func (pm *Manager) ConfigureMQTT(ctx context.Context, plugID, brokerHost string, brokerPort int) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	slog.Info("Configuring MQTT for plug",
		"plug_id", plugID,
		"broker", brokerHost,
		"port", brokerPort,
	)

	commands := []string{
		fmt.Sprintf("MqttHost %s", brokerHost),
		fmt.Sprintf("MqttPort %d", brokerPort),
		fmt.Sprintf("Topic tasmota/%s", plugID),
	}

	if _, err := info.Client.ExecuteBacklog(ctx, commands...); err != nil {
		return fmt.Errorf("failed to configure MQTT: %w", err)
	}

	slog.Info("MQTT configured for plug", "plug_id", plugID)
	return nil
}

// SetPower sets the power state of a plug.
func (pm *Manager) SetPower(ctx context.Context, plugID string, on bool) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	pm.mu.RLock()
	state := pm.states[plugID]
	if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 60*time.Second {
		slog.Warn("Attempting to control plug that hasn't been seen recently",
			"id", plugID,
			"last_seen", state.LastSeen,
			"time_since", time.Since(state.LastSeen).Round(time.Second),
		)
	}
	pm.mu.RUnlock()

	command := "Power OFF"
	if on {
		command = "Power ON"
	}

	if _, err := info.Client.ExecuteCommand(ctx, command); err != nil {
		pm.errorPublisher.Publish(ErrorEvent{
			PlugID: plugID,
			Error:  fmt.Errorf("failed to set power: %w", err),
		})
		return err
	}

	pm.mu.Lock()
	state = pm.states[plugID]
	state.On = on
	state.LastUpdated = time.Now()
	stateCopy := *state
	pm.mu.Unlock()

	pm.statePublisher.Publish(StateChangedEvent{
		PlugID: plugID,
		State:  stateCopy,
	})
	pm.publishStateUpdate("command", plugID, stateCopy)

	return nil
}

// GetStatus fetches the current status of a plug.
func (pm *Manager) GetStatus(ctx context.Context, plugID string) (*State, error) {
	info, exists := pm.plugs[plugID]
	if !exists {
		return nil, fmt.Errorf("plug %s not found", plugID)
	}

	response, err := info.Client.ExecuteCommand(ctx, "Status 0")
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	var statusResp struct {
		Status struct {
			Power string `json:"Power"`
		} `json:"Status"`
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	if err := json.Unmarshal(response, &statusResp); err != nil {
		var altResp struct {
			Power string `json:"POWER"`
		}
		if err2 := json.Unmarshal(response, &altResp); err2 == nil {
			state := pm.states[plugID]
			state.On = altResp.Power == "ON"
			state.LastUpdated = time.Now()
			copy := *state
			return &copy, nil
		}
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	state := pm.states[plugID]
	state.On = statusResp.Status.Power == "ON"
	state.LastUpdated = time.Now()
	copy := *state
	pm.publishStateUpdate("status", plugID, copy)
	return &copy, nil
}

// ProcessCommands handles command events.
func (pm *Manager) ProcessCommands(ctx context.Context) {
	for {
		select {
		case cmd := <-pm.commands:
			if err := pm.SetPower(ctx, cmd.PlugID, cmd.On); err != nil {
				slog.Error("Failed to process command",
					"plug_id", cmd.PlugID,
					"error", err,
				)
			}
		case <-ctx.Done():
			return
		}
	}
}

// ProcessStateEvents merges state change events from the eventbus.
func (pm *Manager) ProcessStateEvents(ctx context.Context) {
	for {
		select {
		case event := <-pm.stateSubscriber.Events():
			pm.mu.Lock()
			state, exists := pm.states[event.PlugID]
			if !exists {
				pm.mu.Unlock()
				slog.Warn("Received state event for unknown plug", "plug_id", event.PlugID)
				continue
			}

			if !event.State.LastSeen.IsZero() {
				state.LastSeen = event.State.LastSeen
				state.MQTTConnected = event.State.MQTTConnected
			}

			if !event.State.LastUpdated.IsZero() {
				state.LastUpdated = event.State.LastUpdated
				state.On = event.State.On
				state.Power = event.State.Power
				state.Voltage = event.State.Voltage
				state.Current = event.State.Current
				state.Energy = event.State.Energy
			}

			stateCopy := *state
			pm.mu.Unlock()

			slog.Debug("Merged state from eventbus",
				"plug_id", event.PlugID,
				"on", stateCopy.On,
				"power", stateCopy.Power,
				"voltage", stateCopy.Voltage,
				"current", stateCopy.Current,
				"mqtt_connected", stateCopy.MQTTConnected,
				"last_seen", stateCopy.LastSeen,
			)
			pm.publishStateUpdate("eventbus", event.PlugID, stateCopy)

		case <-ctx.Done():
			return
		}
	}
}

// MonitorConnections monitors plug connections and reconfigures MQTT when needed.
func (pm *Manager) MonitorConnections(ctx context.Context, brokerHost string, brokerPort int) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	initialConfigTime := time.Now()
	initialCheckDone := false

	for {
		select {
		case <-ticker.C:
			if !initialCheckDone && time.Since(initialConfigTime) > 60*time.Second {
				initialCheckDone = true
				pm.mu.RLock()
				for plugID, state := range pm.states {
					if state.LastSeen.IsZero() {
						pm.mu.RUnlock()
						slog.Warn("Plug has never connected to MQTT, attempting reconfiguration",
							"plug_id", plugID,
							"time_since_startup", time.Since(initialConfigTime).Round(time.Second),
						)
						if err := pm.ConfigureMQTT(ctx, plugID, brokerHost, brokerPort); err != nil {
							slog.Error("Failed to reconfigure MQTT for offline plug",
								"plug_id", plugID,
								"error", err,
							)
							pm.errorPublisher.Publish(ErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug never connected, reconfiguration failed: %w", err),
							})
						} else {
							if _, err := pm.GetStatus(ctx, plugID); err != nil {
								slog.Error("Plug not reachable via HTTP",
									"plug_id", plugID,
									"error", err,
								)
							}
						}
						pm.mu.RLock()
					}
				}
				pm.mu.RUnlock()
			}

			if initialCheckDone {
				pm.mu.RLock()
				for plugID, state := range pm.states {
					if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 120*time.Second {
						timeSince := time.Since(state.LastSeen).Round(time.Second)
						pm.mu.RUnlock()

						slog.Warn("Plug hasn't been seen in a while, checking connectivity",
							"plug_id", plugID,
							"time_since_last_seen", timeSince,
						)

						if _, err := pm.GetStatus(ctx, plugID); err != nil {
							slog.Error("Plug not reachable via HTTP",
								"plug_id", plugID,
								"error", err,
								"time_since_last_seen", timeSince,
							)
							pm.errorPublisher.Publish(ErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug unreachable for %s: %w", timeSince, err),
							})
						} else {
							slog.Info("Plug reachable via HTTP but not MQTT, reconfiguring",
								"plug_id", plugID,
							)
							if err := pm.ConfigureMQTT(ctx, plugID, brokerHost, brokerPort); err != nil {
								slog.Error("Failed to reconfigure MQTT",
									"plug_id", plugID,
									"error", err,
								)
							}
						}
						pm.mu.RLock()
					}
				}
				pm.mu.RUnlock()
			}

		case <-ctx.Done():
			return
		}
	}
}

// Snapshot returns a copy of all plug configs and states.
func (pm *Manager) Snapshot() map[string]struct {
	Plug  Plug
	State State
} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	result := make(map[string]struct {
		Plug  Plug
		State State
	}, len(pm.plugs))

	for id, info := range pm.plugs {
		state := pm.states[id]
		result[id] = struct {
			Plug  Plug
			State State
		}{
			Plug:  info.Config,
			State: *state,
		}
	}

	return result
}

// Plug returns the plug info and state for the given ID.
func (pm *Manager) Plug(plugID string) (Plug, State, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	info, ok := pm.plugs[plugID]
	if !ok {
		return Plug{}, State{}, false
	}

	state, ok := pm.states[plugID]
	if !ok {
		return Plug{}, State{}, false
	}

	return info.Config, *state, true
}

func (pm *Manager) publishStateUpdate(source, plugID string, state State) {
	if pm.eventBus == nil || pm.stateEventClient == nil {
		return
	}

	info, ok := pm.plugs[plugID]
	name := plugID
	if ok {
		name = info.Config.Name
	}

	connectionState, connectionNote := connectionStatus(state.LastSeen)

	pm.eventBus.PublishStateUpdate(pm.stateEventClient, events.StateUpdateEvent{
		Timestamp:       time.Now(),
		Source:          source,
		PlugID:          plugID,
		Name:            name,
		On:              state.On,
		Power:           state.Power,
		Voltage:         state.Voltage,
		Current:         state.Current,
		Energy:          state.Energy,
		MQTTConnected:   state.MQTTConnected,
		LastSeen:        state.LastSeen,
		LastUpdated:     state.LastUpdated,
		ConnectionState: connectionState,
		ConnectionNote:  connectionNote,
	})
}

func connectionStatus(lastSeen time.Time) (string, string) {
	if lastSeen.IsZero() {
		return "disconnected", "Never seen"
	}

	since := time.Since(lastSeen)
	switch {
	case since < 30*time.Second:
		return "connected", fmt.Sprintf("Last seen: %s ago", since.Round(time.Second))
	case since < 60*time.Second:
		return "stale", fmt.Sprintf("Last seen: %s ago", since.Round(time.Second))
	default:
		return "disconnected", fmt.Sprintf("Last seen: %s ago", since.Round(time.Second))
	}
}

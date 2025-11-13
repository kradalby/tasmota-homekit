package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kradalby/tasmota-go"
	"tailscale.com/util/eventbus"
)

// PlugManager manages all Tasmota plug clients and their state
type PlugManager struct {
	plugs           map[string]*PlugInfo
	states          map[string]*PlugState
	statesMu        sync.RWMutex // Protects states map
	commands        chan PlugCommandEvent
	statePublisher  *eventbus.Publisher[PlugStateChangedEvent]
	errorPublisher  *eventbus.Publisher[PlugErrorEvent]
	stateSubscriber *eventbus.Subscriber[PlugStateChangedEvent]
}

// PlugInfo holds the client and configuration for a plug
type PlugInfo struct {
	Config Plug
	Client *tasmota.Client
}

// NewPlugManager creates a new plug manager
func NewPlugManager(
	plugConfigs []Plug,
	commands chan PlugCommandEvent,
	bus *eventbus.Bus,
) (*PlugManager, error) {
	client := bus.Client("plugmanager")

	pm := &PlugManager{
		plugs:           make(map[string]*PlugInfo),
		states:          make(map[string]*PlugState),
		commands:        commands,
		statePublisher:  eventbus.Publish[PlugStateChangedEvent](client),
		errorPublisher:  eventbus.Publish[PlugErrorEvent](client),
		stateSubscriber: eventbus.Subscribe[PlugStateChangedEvent](client),
	}

	// Initialize clients for each plug
	for _, plugConfig := range plugConfigs {
		client, err := tasmota.NewClient(plugConfig.Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create client for %s: %w", plugConfig.ID, err)
		}

		pm.plugs[plugConfig.ID] = &PlugInfo{
			Config: plugConfig,
			Client: client,
		}

		// Initialize state
		pm.states[plugConfig.ID] = &PlugState{
			ID:            plugConfig.ID,
			Name:          plugConfig.Name,
			On:            false,
			LastUpdated:   time.Now(),
			MQTTConnected: false,
			LastSeen:      time.Time{}, // Zero time until first message
		}

		slog.Info("Initialized plug client",
			"id", plugConfig.ID,
			"address", plugConfig.Address,
		)
	}

	return pm, nil
}

// ConfigureMQTT configures a plug to use the specified MQTT broker
func (pm *PlugManager) ConfigureMQTT(ctx context.Context, plugID, brokerHost string, brokerPort int) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	slog.Info("Configuring MQTT for plug",
		"plug_id", plugID,
		"broker", brokerHost,
		"port", brokerPort,
	)

	// Use Backlog to configure MQTT host and port in one command
	// Also set the topic to use the plug ID for easy identification
	commands := []string{
		fmt.Sprintf("MqttHost %s", brokerHost),
		fmt.Sprintf("MqttPort %d", brokerPort),
		fmt.Sprintf("Topic tasmota/%s", plugID),
	}

	_, err := info.Client.ExecuteBacklog(ctx, commands...)
	if err != nil {
		return fmt.Errorf("failed to configure MQTT: %w", err)
	}

	slog.Info("MQTT configured for plug", "plug_id", plugID)
	return nil
}

// SetPower sets the power state of a plug
func (pm *PlugManager) SetPower(ctx context.Context, plugID string, on bool) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	// Check connection status and warn if stale
	pm.statesMu.RLock()
	state := pm.states[plugID]
	if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 60*time.Second {
		slog.Warn("Attempting to control plug that hasn't been seen recently",
			"id", plugID,
			"last_seen", state.LastSeen,
			"time_since", time.Since(state.LastSeen).Round(time.Second),
		)
	}
	pm.statesMu.RUnlock()

	slog.Info("Setting plug power", "id", plugID, "on", on)

	// Send direct command to plug using Tasmota Power command
	command := "Power OFF"
	if on {
		command = "Power ON"
	}

	_, err := info.Client.ExecuteCommand(ctx, command)
	if err != nil {
		pm.errorPublisher.Publish(PlugErrorEvent{
			PlugID: plugID,
			Error:  fmt.Errorf("failed to set power: %w", err),
		})
		return err
	}

	// Update state with mutex protection
	pm.statesMu.Lock()
	state = pm.states[plugID]
	state.On = on
	state.LastUpdated = time.Now()
	stateCopy := *state
	pm.statesMu.Unlock()

	// Publish state change to eventbus
	pm.statePublisher.Publish(PlugStateChangedEvent{
		PlugID: plugID,
		State:  stateCopy,
	})

	return nil
}

// GetStatus fetches the current status of a plug
func (pm *PlugManager) GetStatus(ctx context.Context, plugID string) (*PlugState, error) {
	info, exists := pm.plugs[plugID]
	if !exists {
		return nil, fmt.Errorf("plug %s not found", plugID)
	}

	// Fetch status from device using Status 0 command (basic status)
	response, err := info.Client.ExecuteCommand(ctx, "Status 0")
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	// Parse the status response to extract power state
	// The response typically contains {"Status":{"Power":"ON"}} or similar
	var statusResp struct {
		Status struct {
			Power string `json:"Power"`
		} `json:"Status"`
	}

	pm.statesMu.Lock()
	defer pm.statesMu.Unlock()

	if err := json.Unmarshal(response, &statusResp); err != nil {
		// Try alternative format where Power is at root level
		var altResp struct {
			Power string `json:"POWER"`
		}
		if err2 := json.Unmarshal(response, &altResp); err2 == nil {
			state := pm.states[plugID]
			state.On = altResp.Power == "ON"
			state.LastUpdated = time.Now()
			stateCopy := *state
			return &stateCopy, nil
		}
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	// Update state from device
	state := pm.states[plugID]
	state.On = statusResp.Status.Power == "ON"
	state.LastUpdated = time.Now()
	stateCopy := *state

	return &stateCopy, nil
}

// ProcessCommands processes command events from the channel
func (pm *PlugManager) ProcessCommands(ctx context.Context) {
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

// ProcessStateEvents processes state change events from the eventbus (e.g., from MQTT)
func (pm *PlugManager) ProcessStateEvents(ctx context.Context) {
	for {
		select {
		case event := <-pm.stateSubscriber.Events():
			// Merge the incoming state with our existing state
			pm.statesMu.Lock()
			state, exists := pm.states[event.PlugID]
			if !exists {
				pm.statesMu.Unlock()
				slog.Warn("Received state event for unknown plug", "plug_id", event.PlugID)
				continue
			}

			// Merge fields from the event
			// Only update fields that are meaningful in the event
			if !event.State.LastSeen.IsZero() {
				state.LastSeen = event.State.LastSeen
				state.MQTTConnected = event.State.MQTTConnected
			}

			if !event.State.LastUpdated.IsZero() {
				state.LastUpdated = event.State.LastUpdated
				// Only update On state if LastUpdated was set (indicating power state was in message)
				state.On = event.State.On
			}

			stateCopy := *state
			pm.statesMu.Unlock()

			slog.Debug("Merged state from eventbus",
				"plug_id", event.PlugID,
				"on", stateCopy.On,
				"mqtt_connected", stateCopy.MQTTConnected,
				"last_seen", stateCopy.LastSeen,
			)

		case <-ctx.Done():
			return
		}
	}
}

// MonitorConnections monitors plug connections and reconfigures MQTT if plugs don't come online
func (pm *PlugManager) MonitorConnections(ctx context.Context, brokerHost string, brokerPort int) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Track initial configuration time
	initialConfigTime := time.Now()
	initialCheckDone := false

	for {
		select {
		case <-ticker.C:
			// Initial check: After 60 seconds, verify all plugs have connected at least once
			if !initialCheckDone && time.Since(initialConfigTime) > 60*time.Second {
				initialCheckDone = true
				pm.statesMu.RLock()
				for plugID, state := range pm.states {
					if state.LastSeen.IsZero() {
						slog.Warn("Plug has never connected to MQTT, attempting reconfiguration",
							"plug_id", plugID,
							"time_since_startup", time.Since(initialConfigTime).Round(time.Second),
						)
						pm.statesMu.RUnlock()

						// Try to reconfigure MQTT
						if err := pm.ConfigureMQTT(ctx, plugID, brokerHost, brokerPort); err != nil {
							slog.Error("Failed to reconfigure MQTT for offline plug",
								"plug_id", plugID,
								"error", err,
							)
							pm.errorPublisher.Publish(PlugErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug never connected, reconfiguration failed: %w", err),
							})
						} else {
							// Also try to fetch status to verify connectivity
							if _, err := pm.GetStatus(ctx, plugID); err != nil {
								slog.Error("Plug not reachable via HTTP",
									"plug_id", plugID,
									"error", err,
								)
							}
						}
						pm.statesMu.RLock()
					}
				}
				pm.statesMu.RUnlock()
			}

			// Ongoing monitoring: Check for plugs that were connected but haven't been seen recently
			if initialCheckDone {
				pm.statesMu.RLock()
				for plugID, state := range pm.states {
					if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 120*time.Second {
						timeSince := time.Since(state.LastSeen).Round(time.Second)
						pm.statesMu.RUnlock()

						slog.Warn("Plug hasn't been seen in a while, checking connectivity",
							"plug_id", plugID,
							"time_since_last_seen", timeSince,
						)

						// Try to fetch status to verify plug is still reachable
						if _, err := pm.GetStatus(ctx, plugID); err != nil {
							slog.Error("Plug not reachable via HTTP",
								"plug_id", plugID,
								"error", err,
								"time_since_last_seen", timeSince,
							)
							pm.errorPublisher.Publish(PlugErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug unreachable for %s: %w", timeSince, err),
							})
						} else {
							// Plug is reachable via HTTP, try reconfiguring MQTT
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
						pm.statesMu.RLock()
					}
				}
				pm.statesMu.RUnlock()
			}

		case <-ctx.Done():
			return
		}
	}
}

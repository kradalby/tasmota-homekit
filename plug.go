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
	plugs          map[string]*PlugInfo
	states         map[string]*PlugState
	statesMu       sync.RWMutex // Protects states map
	commands       chan PlugCommandEvent
	statePublisher *eventbus.Publisher[PlugStateChangedEvent]
	errorPublisher *eventbus.Publisher[PlugErrorEvent]
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
		plugs:          make(map[string]*PlugInfo),
		states:         make(map[string]*PlugState),
		commands:       commands,
		statePublisher: eventbus.Publish[PlugStateChangedEvent](client),
		errorPublisher: eventbus.Publish[PlugErrorEvent](client),
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
			ID:          plugConfig.ID,
			Name:        plugConfig.Name,
			On:          false,
			LastUpdated: time.Now(),
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
	state := pm.states[plugID]
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

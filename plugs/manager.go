package plugs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kradalby/tasmota-go"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
)

// Manager manages all Tasmota plug clients and their state.
type Manager struct {
	plugs map[string]*Info

	// mu serialises writers so every state is stored and published before
	// the next change starts: subscribers then see changes in the order they
	// were made. Readers load states without it.
	mu     sync.Mutex
	states atomic.Pointer[map[string]State]

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
	Client Client
}

// Client is the interface for communicating with a Tasmota device.
type Client interface {
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
		commands:         commands,
		statePublisher:   eventbus.Publish[StateChangedEvent](client),
		errorPublisher:   eventbus.Publish[ErrorEvent](client),
		stateSubscriber:  eventbus.Subscribe[StateChangedEvent](client),
		eventBus:         bus,
		stateEventClient: client,
	}

	states := make(map[string]State, len(plugConfigs))
	for _, plugConfig := range plugConfigs {
		client, err := tasmota.NewClient(plugConfig.Address)
		if err != nil {
			return nil, fmt.Errorf("failed to create client for %s: %w", plugConfig.ID, err)
		}

		pm.plugs[plugConfig.ID] = &Info{
			Config: plugConfig,
			Client: &tasmotaClient{Client: client},
		}

		states[plugConfig.ID] = State{
			ID:          plugConfig.ID,
			Name:        plugConfig.Name,
			LastUpdated: time.Now(),
		}

		slog.Info(
			"Initialized plug client",
			"id", plugConfig.ID,
			"address", plugConfig.Address,
		)
	}

	pm.states.Store(&states)
	for _, plugConfig := range plugConfigs {
		pm.publishStateUpdate("initial", plugConfig.ID, states[plugConfig.ID])
	}

	return pm, nil
}

// ConfigureMQTT configures a plug to use the specified MQTT broker.
func (pm *Manager) ConfigureMQTT(ctx context.Context, plugID, brokerHost string, brokerPort int) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	slog.Info(
		"Configuring MQTT for plug",
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

	state := (*pm.states.Load())[plugID]
	if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 60*time.Second {
		slog.Warn(
			"Attempting to control plug that hasn't been seen recently",
			"id", plugID,
			"last_seen", state.LastSeen,
			"time_since", time.Since(state.LastSeen).Round(time.Second),
		)
	}

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

	// Immediately query status to get actual device state
	// This replaces the optimistic update and ensures we only publish confirmed state
	if _, err := pm.GetStatus(ctx, plugID); err != nil {
		slog.Debug("Failed to get status after power command", "plug_id", plugID, "error", err)
		// Even if status query fails, the command likely succeeded
		// MQTT will report the state change shortly
	}

	return nil
}

// GetStatus fetches the current status of a plug.
func (pm *Manager) GetStatus(ctx context.Context, plugID string) (*State, error) {
	info, exists := pm.plugs[plugID]
	if !exists {
		return nil, fmt.Errorf("plug %s not found", plugID)
	}

	sent := time.Now()
	response, err := info.Client.ExecuteCommand(ctx, "Status 0")
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	slog.Debug("Raw Tasmota Status response", "plug_id", plugID, "response", string(response))

	reading, err := parseStatus(response)
	if err != nil {
		return nil, err
	}

	state, _ := pm.update("status", plugID, func(s State) State {
		return applyStatus(s, reading, sent, time.Now())
	})

	return &state, nil
}

// statusReading is what a Status 0 reply says about a plug.
type statusReading struct {
	On     bool
	Energy Energy
}

func parseStatus(response []byte) (statusReading, error) {
	// Status.Power is left out: its encoding differs between firmware
	// versions, and StatusSTS.POWER carries the same bit.
	var statusResp struct {
		StatusSTS struct {
			Power string `json:"POWER"`
		} `json:"StatusSTS"`
		StatusSNS struct {
			Energy Energy `json:"ENERGY"`
		} `json:"StatusSNS"`
	}

	if err := json.Unmarshal(response, &statusResp); err != nil {
		return statusReading{}, fmt.Errorf("failed to parse status: %w", err)
	}
	if statusResp.StatusSTS.Power == "" {
		return statusReading{}, fmt.Errorf("status has no StatusSTS.POWER")
	}

	return statusReading{
		On:     statusResp.StatusSTS.Power == "ON",
		Energy: statusResp.StatusSNS.Energy,
	}, nil
}

// applyStatus folds a reply to a status request sent at sent into s. The
// reply shows the plug no earlier than sent, so sent is when it was observed.
func applyStatus(s State, r statusReading, sent, now time.Time) State {
	if s.observe(sent, &r.On, &r.Energy) {
		s.LastUpdated = latest(s.LastUpdated, now)
	}
	return s
}

// mergeEvent folds an MQTT report into s.
func mergeEvent(s State, event StateChangedEvent) State {
	s.observe(event.Received, event.On, event.Energy)
	s.MQTTConnected = true
	s.LastSeen = latest(s.LastSeen, event.Received)
	s.LastUpdated = latest(s.LastUpdated, event.Received)
	return s
}

// observe stores on and e, where given, unless s holds a reading of the same
// kind observed after at: MQTT reports queue on the eventbus and status
// requests overlap, so readings arrive out of order. It reports whether
// anything was stored.
func (s *State) observe(at time.Time, on *bool, e *Energy) bool {
	stored := false
	if on != nil && !s.onAt.After(at) {
		s.On, s.onAt = *on, at
		stored = true
	}
	if e != nil && !s.energyAt.After(at) {
		s.Power, s.Voltage, s.Current, s.Energy = e.Power, e.Voltage, e.Current, e.Total
		s.energyAt = at
		stored = true
	}
	return stored
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// RefreshAll triggers a status update for all plugs concurrently.
func (pm *Manager) RefreshAll(ctx context.Context) {
	var wg sync.WaitGroup
	for id := range pm.plugs {
		wg.Add(1)
		go func(plugID string) {
			defer wg.Done()
			// Use a short timeout for individual refreshes to avoid blocking the whole page load too long
			refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			if _, err := pm.GetStatus(refreshCtx, plugID); err != nil {
				slog.Debug("Failed to refresh plug status", "plug_id", plugID, "error", err)
			}
		}(id)
	}
	wg.Wait()
}

// ProcessCommands handles command events.
func (pm *Manager) ProcessCommands(ctx context.Context) {
	for {
		select {
		case cmd := <-pm.commands:
			if err := pm.SetPower(ctx, cmd.PlugID, cmd.On); err != nil {
				slog.Error(
					"Failed to process command",
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
			state, ok := pm.update("eventbus", event.PlugID, func(s State) State {
				return mergeEvent(s, event)
			})
			if !ok {
				slog.Warn("Received state event for unknown plug", "plug_id", event.PlugID)
				continue
			}

			slog.Debug(
				"Merged state from eventbus",
				"plug_id", event.PlugID,
				"on", state.On,
				"power", state.Power,
				"voltage", state.Voltage,
				"current", state.Current,
				"mqtt_connected", state.MQTTConnected,
				"last_seen", state.LastSeen,
			)

		case <-ctx.Done():
			return
		}
	}
}

// update replaces plugID's state with f's result. The new snapshot is
// published before mu is released so no later change can overtake it; that
// is safe because eventbus queues per subscriber without bound, so Publish
// does not wait on a slow consumer.
func (pm *Manager) update(source, plugID string, f func(State) State) (State, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	states := *pm.states.Load()
	prev, ok := states[plugID]
	if !ok {
		return State{}, false
	}

	next := f(prev)
	if next == prev {
		return prev, true
	}

	states = maps.Clone(states)
	states[plugID] = next
	pm.states.Store(&states)
	pm.publishStateUpdate(source, plugID, next)

	return next, true
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
				for plugID, item := range pm.Snapshot() {
					if item.State.LastSeen.IsZero() {
						slog.Warn(
							"Plug has never connected to MQTT, attempting reconfiguration",
							"plug_id", plugID,
							"time_since_startup", time.Since(initialConfigTime).Round(time.Second),
						)
						if err := pm.ConfigureMQTT(ctx, plugID, brokerHost, brokerPort); err != nil {
							slog.Error(
								"Failed to reconfigure MQTT for offline plug",
								"plug_id", plugID,
								"error", err,
							)
							pm.errorPublisher.Publish(ErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug never connected, reconfiguration failed: %w", err),
							})
						} else {
							if _, err := pm.GetStatus(ctx, plugID); err != nil {
								slog.Error(
									"Plug not reachable via HTTP",
									"plug_id", plugID,
									"error", err,
								)
							}
						}
					}
				}
			}

			if initialCheckDone {
				for plugID, item := range pm.Snapshot() {
					state := item.State
					if !state.LastSeen.IsZero() && time.Since(state.LastSeen) > 120*time.Second {
						timeSince := time.Since(state.LastSeen).Round(time.Second)

						slog.Warn(
							"Plug hasn't been seen in a while, checking connectivity",
							"plug_id", plugID,
							"time_since_last_seen", timeSince,
						)

						if _, err := pm.GetStatus(ctx, plugID); err != nil {
							slog.Error(
								"Plug not reachable via HTTP",
								"plug_id", plugID,
								"error", err,
								"time_since_last_seen", timeSince,
							)
							pm.errorPublisher.Publish(ErrorEvent{
								PlugID: plugID,
								Error:  fmt.Errorf("plug unreachable for %s: %w", timeSince, err),
							})
						} else {
							slog.Info(
								"Plug reachable via HTTP but not MQTT, reconfiguring",
								"plug_id", plugID,
							)
							if err := pm.ConfigureMQTT(ctx, plugID, brokerHost, brokerPort); err != nil {
								slog.Error(
									"Failed to reconfigure MQTT",
									"plug_id", plugID,
									"error", err,
								)
							}
						}
					}
				}
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
	states := *pm.states.Load()
	result := make(map[string]struct {
		Plug  Plug
		State State
	}, len(pm.plugs))

	for id, info := range pm.plugs {
		result[id] = struct {
			Plug  Plug
			State State
		}{
			Plug:  info.Config,
			State: states[id],
		}
	}

	return result
}

// Plug returns the plug info and state for the given ID.
func (pm *Manager) Plug(plugID string) (Plug, State, bool) {
	info, ok := pm.plugs[plugID]
	if !ok {
		return Plug{}, State{}, false
	}

	state, ok := (*pm.states.Load())[plugID]
	if !ok {
		return Plug{}, State{}, false
	}

	return info.Config, state, true
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

// SetClientForTesting replaces the client for a plug (for testing only).
func (pm *Manager) SetClientForTesting(plugID string, c Client) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if info, ok := pm.plugs[plugID]; ok {
		info.Client = c
	}
}

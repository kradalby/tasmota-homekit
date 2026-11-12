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

	reconfigured atomic.Bool // MQTT settings were sent this run
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
	bus *events.Bus,
) (*Manager, error) {
	client, err := bus.Client(events.ClientPlugManager)
	if err != nil {
		return nil, fmt.Errorf("failed to get plugmanager eventbus client: %w", err)
	}

	pm := &Manager{
		plugs:            make(map[string]*Info),
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
		pm.publishStateUpdate("initial", states[plugConfig.ID])
	}

	return pm, nil
}

// checkMQTT reads plugID's status, which also refreshes its state, and sends
// only the MQTT settings that differ from the broker's: Tasmota restarts on
// MqttHost and MqttPort even when the value is unchanged. A plug is
// reconfigured at most once per run, so one that stores a value other than
// the one sent is not restarted on every check.
func (pm *Manager) checkMQTT(ctx context.Context, plugID, host string, port int) error {
	r, _, err := pm.status(ctx, plugID)
	if err != nil {
		return err
	}
	if r.MQTT == nil {
		return fmt.Errorf("status has no StatusMQT")
	}

	var cmds []string
	if r.MQTT.Host != host {
		cmds = append(cmds, "MqttHost "+host)
	}
	if r.MQTT.Port != port {
		cmds = append(cmds, fmt.Sprintf("MqttPort %d", port))
	}
	if topic := "tasmota/" + plugID; r.MQTT.Topic != topic {
		cmds = append(cmds, "Topic "+topic)
	}
	if len(cmds) == 0 {
		slog.Info("Plug MQTT settings point at the broker", "plug_id", plugID)
		return nil
	}

	info := pm.plugs[plugID]
	if !info.reconfigured.CompareAndSwap(false, true) {
		return fmt.Errorf("MQTT settings still differ after reconfiguring, not restarting again: %v", cmds)
	}
	slog.Info("Pointing plug at the MQTT broker, it will restart", "plug_id", plugID, "commands", cmds)
	if _, err := info.Client.ExecuteBacklog(ctx, cmds...); err != nil {
		return fmt.Errorf("failed to configure MQTT: %w", err)
	}
	return nil
}

// SetPower sets the power state of a plug.
func (pm *Manager) SetPower(ctx context.Context, plugID string, on bool) error {
	info, exists := pm.plugs[plugID]
	if !exists {
		return fmt.Errorf("plug %s not found", plugID)
	}

	command := "Power OFF"
	if on {
		command = "Power ON"
	}

	// Tasmota answers Power with the relay state it then has, which confirms
	// the switch without a Status 0 round trip.
	sent := time.Now()
	reply, err := info.Client.ExecuteCommand(ctx, command)
	var got bool
	if err == nil {
		got, err = parsePower(reply)
	}
	if err != nil {
		pm.errorPublisher.Publish(ErrorEvent{
			PlugID: plugID,
			Error:  fmt.Errorf("failed to set power: %w", err),
		})
		return err
	}

	pm.update("command", plugID, func(s State) State {
		return applyStatus(s, statusReading{On: got}, sent, time.Now())
	})

	if got != on {
		return fmt.Errorf("plug %s answered %q with %s", plugID, command, reply)
	}

	return nil
}

// parsePower reads the relay state a reply to Power reports.
func parsePower(reply []byte) (bool, error) {
	var r struct {
		Power string `json:"POWER"`
	}
	if err := json.Unmarshal(reply, &r); err != nil {
		return false, fmt.Errorf("failed to parse power reply: %w", err)
	}

	switch r.Power {
	case "ON":
		return true, nil
	case "OFF":
		return false, nil
	}
	return false, fmt.Errorf("power reply has no POWER: %s", reply)
}

// GetStatus fetches the current status of a plug.
func (pm *Manager) GetStatus(ctx context.Context, plugID string) (*State, error) {
	_, state, err := pm.status(ctx, plugID)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// status asks plugID for Status 0 and stores the state it reports.
func (pm *Manager) status(ctx context.Context, plugID string) (statusReading, State, error) {
	info, exists := pm.plugs[plugID]
	if !exists {
		return statusReading{}, State{}, fmt.Errorf("plug %s not found", plugID)
	}

	sent := time.Now()
	response, err := info.Client.ExecuteCommand(ctx, "Status 0")
	if err != nil {
		return statusReading{}, State{}, fmt.Errorf("failed to get status: %w", err)
	}

	slog.Debug("Raw Tasmota Status response", "plug_id", plugID, "response", string(response))

	reading, err := parseStatus(response)
	if err != nil {
		return statusReading{}, State{}, err
	}

	state, _ := pm.update("status", plugID, func(s State) State {
		return applyStatus(s, reading, sent, time.Now())
	})

	return reading, state, nil
}

// statusReading is what a plug's reply says about it. Energy is nil when the
// reply carries none, as a reply to Power never does; MQTT is nil when it
// has no StatusMQT.
type statusReading struct {
	On     bool
	Energy *Energy
	MQTT   *mqttSettings
}

// mqttSettings are where a plug's MQTT client publishes.
type mqttSettings struct {
	Host  string
	Port  int
	Topic string
}

func parseStatus(response []byte) (statusReading, error) {
	// Status.Power is left out: its encoding differs between firmware
	// versions, and StatusSTS.POWER carries the same bit.
	var statusResp struct {
		Status struct {
			Topic string `json:"Topic"`
		} `json:"Status"`
		StatusMQT *struct {
			MqttHost string `json:"MqttHost"`
			MqttPort int    `json:"MqttPort"`
		} `json:"StatusMQT"`
		StatusSTS struct {
			Power string `json:"POWER"`
		} `json:"StatusSTS"`
		StatusSNS struct {
			Energy *Energy `json:"ENERGY"`
		} `json:"StatusSNS"`
	}

	if err := json.Unmarshal(response, &statusResp); err != nil {
		return statusReading{}, fmt.Errorf("failed to parse status: %w", err)
	}
	if statusResp.StatusSTS.Power == "" {
		return statusReading{}, fmt.Errorf("status has no StatusSTS.POWER")
	}

	r := statusReading{
		On:     statusResp.StatusSTS.Power == "ON",
		Energy: statusResp.StatusSNS.Energy,
	}
	if m := statusResp.StatusMQT; m != nil {
		r.MQTT = &mqttSettings{Host: m.MqttHost, Port: m.MqttPort, Topic: statusResp.Status.Topic}
	}
	return r, nil
}

// applyStatus folds a reply to a request sent at sent into s. The reply
// shows the plug no earlier than sent, so sent is when it was observed.
func applyStatus(s State, r statusReading, sent, now time.Time) State {
	if s.observe(sent, &r.On, r.Energy) {
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
	pm.publishStateUpdate(source, next)

	return next, true
}

// silentAfter is how long a plug may go unheard on MQTT before it is checked
// and shown as disconnected. Tasmota reports at least every TelePeriod (300 s
// by default); twice that rides out one lost report.
const silentAfter = 10 * time.Minute

// MonitorConnections checks every plug at once, which also reads its initial
// state, and then each silentAfter checks the plugs that have gone silent.
func (pm *Manager) MonitorConnections(ctx context.Context, brokerHost string, brokerPort int) {
	ticker := time.NewTicker(silentAfter)
	defer ticker.Stop()

	now := time.Now()
	for {
		pm.checkConnections(ctx, now, brokerHost, brokerPort)
		select {
		case now = <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// checkConnections runs checkMQTT on every plug not heard on MQTT within
// silentAfter of now. A plug never heard counts as silent, so it is checked
// on every tick until it reports rather than given up on.
func (pm *Manager) checkConnections(ctx context.Context, now time.Time, host string, port int) {
	var wg sync.WaitGroup
	for id := range pm.plugs {
		if _, s, _ := pm.Plug(id); now.Sub(s.LastSeen) <= silentAfter {
			continue
		}
		wg.Go(func() {
			if err := pm.checkMQTT(ctx, id, host, port); err != nil {
				slog.Warn("Failed to check plug MQTT settings", "plug_id", id, "error", err)
			}
		})
	}
	wg.Wait()
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

func (pm *Manager) publishStateUpdate(source string, state State) {
	if pm.eventBus == nil || pm.stateEventClient == nil {
		return
	}

	pm.eventBus.PublishStateUpdate(pm.stateEventClient, state.Event(source))
}

// Event renders s as the update web and HAP subscribers receive.
func (s State) Event(source string) events.StateUpdateEvent {
	connectionState, connectionNote := Connection(s.LastSeen)

	return events.StateUpdateEvent{
		Timestamp:       time.Now(),
		Source:          source,
		PlugID:          s.ID,
		Name:            s.Name,
		On:              s.On,
		Power:           s.Power,
		Voltage:         s.Voltage,
		Current:         s.Current,
		Energy:          s.Energy,
		MQTTConnected:   s.MQTTConnected,
		LastSeen:        s.LastSeen,
		LastUpdated:     s.LastUpdated,
		ConnectionState: connectionState,
		ConnectionNote:  connectionNote,
	}
}

// Connection describes a plug last heard on MQTT at lastSeen. It counts as
// connected for as long as MonitorConnections leaves it alone.
func Connection(lastSeen time.Time) (state, note string) {
	if lastSeen.IsZero() {
		return "disconnected", "Never seen"
	}

	since := time.Since(lastSeen)
	note = fmt.Sprintf("Last seen: %s ago", since.Round(time.Second))
	if since > silentAfter {
		return "disconnected", note
	}
	return "connected", note
}

// SetClientForTesting replaces the client for a plug (for testing only).
func (pm *Manager) SetClientForTesting(plugID string, c Client) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if info, ok := pm.plugs[plugID]; ok {
		info.Client = c
	}
}

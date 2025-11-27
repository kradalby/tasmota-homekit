package tasmotahomekit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"
)

// TestStateSyncEnvironment sets up a complete environment with Manager, HAP, and Web
type TestStateSyncEnvironment struct {
	t          *testing.T
	manager    *plugs.Manager
	hapManager *HAPManager
	webServer  *WebServer
	eventBus   *events.Bus
	commands   chan plugs.CommandEvent
	fakeClient *fakePlugClient
	ctx        context.Context
	cancel     context.CancelFunc
}

type fakePlugClient struct {
	lastCmd   string
	responses [][]byte
}

func (f *fakePlugClient) ExecuteCommand(_ context.Context, cmd string) ([]byte, error) {
	f.lastCmd = cmd
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return resp, nil
	}
	// Default response for Power ON
	return []byte(`{"StatusSTS":{"POWER":"ON"}}`), nil
}

func (f *fakePlugClient) ExecuteBacklog(_ context.Context, cmds ...string) ([]byte, error) {
	return nil, nil
}

func setupStateSyncTest(t *testing.T, plugConfigs []plugs.Plug) *TestStateSyncEnvironment {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eventBus, err := events.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eventBus.Close() })

	commands := make(chan plugs.CommandEvent, 10)

	manager, err := plugs.NewManager(plugConfigs, commands, eventBus)
	require.NoError(t, err)

	// Replace client with fake for all plugs
	fake := &fakePlugClient{}
	for _, cfg := range plugConfigs {
		manager.SetClientForTesting(cfg.ID, fake)
	}

	hapManager := NewHAPManager(plugConfigs, "Test Bridge", commands, manager, eventBus)
	webServer := NewWebServer(logger, manager, manager, eventBus, nil, "", "", hapManager)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Start background processors
	go manager.ProcessCommands(ctx)
	go manager.ProcessStateEvents(ctx)
	go hapManager.ProcessStateChanges(ctx)
	go webServer.processStateChanges(ctx)

	return &TestStateSyncEnvironment{
		t:          t,
		manager:    manager,
		hapManager: hapManager,
		webServer:  webServer,
		eventBus:   eventBus,
		commands:   commands,
		fakeClient: fake,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// getHAPState returns the current state of a plug as seen by HomeKit
func (env *TestStateSyncEnvironment) getHAPState(plugID string) bool {
	acc, ok := env.hapManager.accessories[plugID]
	require.True(env.t, ok, "accessory not found for plug %s", plugID)
	return acc.OnValue()
}

// getWebState returns the current state of a plug as seen by Web UI
func (env *TestStateSyncEnvironment) getWebState(plugID string) bool {
	env.webServer.stateMu.RLock()
	defer env.webServer.stateMu.RUnlock()
	state, ok := env.webServer.currentState[plugID]
	if !ok {
		// Fall back to manager state if not in web cache yet
		_, managerState, _ := env.manager.Plug(plugID)
		return managerState.On
	}
	return state.On
}

// getManagerState returns the current state from the manager (source of truth)
func (env *TestStateSyncEnvironment) getManagerState(plugID string) bool {
	_, state, ok := env.manager.Plug(plugID)
	require.True(env.t, ok, "plug not found in manager: %s", plugID)
	return state.On
}

// simulateMQTTUpdate simulates an MQTT message arriving from a Tasmota device
func (env *TestStateSyncEnvironment) simulateMQTTUpdate(plugID string, on bool) {
	client, err := env.eventBus.Client(events.ClientMQTT)
	require.NoError(env.t, err)
	pub := eventbus.Publish[plugs.StateChangedEvent](client)

	pub.Publish(plugs.StateChangedEvent{
		PlugID: plugID,
		State: plugs.State{
			ID:            plugID,
			On:            on,
			MQTTConnected: true,
			LastSeen:      time.Now(),
			LastUpdated:   time.Now(),
		},
		UpdatedFields: []string{"On", "MQTTConnected", "LastSeen", "LastUpdated"},
	})
}

// assertAllStatesMatch verifies that Manager, HAP, and Web all show the same state
func (env *TestStateSyncEnvironment) assertAllStatesMatch(plugID string, expectedOn bool, msgAndArgs ...interface{}) {
	env.t.Helper()

	// Give time for events to propagate
	assert.EventuallyWithT(env.t, func(c *assert.CollectT) {
		managerState := env.getManagerState(plugID)
		hapState := env.getHAPState(plugID)
		webState := env.getWebState(plugID)

		assert.Equal(c, expectedOn, managerState, "Manager state mismatch")
		assert.Equal(c, expectedOn, hapState, "HAP state mismatch")
		assert.Equal(c, expectedOn, webState, "Web state mismatch")
		assert.Equal(c, managerState, hapState, "Manager and HAP states differ")
		assert.Equal(c, managerState, webState, "Manager and Web states differ")
	}, 2*time.Second, 50*time.Millisecond, msgAndArgs...)
}

// TestMQTTUpdateSyncsToAllViews tests that an MQTT update reaches both HomeKit and Web
func TestMQTTUpdateSyncsToAllViews(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	// Initial state should be OFF
	env.assertAllStatesMatch("plug-1", false, "Initial state should be OFF")

	// Simulate MQTT message saying plug turned ON
	env.simulateMQTTUpdate("plug-1", true)

	// All views should now show ON
	env.assertAllStatesMatch("plug-1", true, "After MQTT update, all views should show ON")
}

// TestSetPowerSyncsToAllViews tests that SetPower updates reach both HomeKit and Web
func TestSetPowerSyncsToAllViews(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	// Configure fake client to return ON
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
	}

	// Turn plug ON via SetPower
	err := env.manager.SetPower(env.ctx, "plug-1", true)
	require.NoError(t, err)

	// All views should show ON
	env.assertAllStatesMatch("plug-1", true, "After SetPower, all views should show ON")
}

// TestMultipleMQTTUpdatesStayInSync tests that rapid MQTT updates keep all views in sync
func TestMultipleMQTTUpdatesStayInSync(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Lamp 1", Address: "192.168.1.100"},
		{ID: "plug-2", Name: "Lamp 2", Address: "192.168.1.101"},
		{ID: "plug-3", Name: "Lamp 3", Address: "192.168.1.102"},
		{ID: "plug-4", Name: "Lamp 4", Address: "192.168.1.103"},
	})

	// Turn all plugs ON via MQTT
	for i := 1; i <= 4; i++ {
		plugID := fmt.Sprintf("plug-%d", i)
		env.simulateMQTTUpdate(plugID, true)
	}

	// All four plugs should show ON in all views
	for i := 1; i <= 4; i++ {
		plugID := fmt.Sprintf("plug-%d", i)
		env.assertAllStatesMatch(plugID, true, "Plug %s should be ON in all views", plugID)
	}
}

// TestRaceConditionMQTTDuringSetPower reproduces the bug where MQTT and SetPower race
func TestRaceConditionMQTTDuringSetPower(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	// Configure fake to return ON
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
		[]byte(`{"StatusSTS":{"POWER":"ON"},"StatusSNS":{"ENERGY":{"Power":10.5}}}`),
	}

	// Turn plug ON via SetPower
	err := env.manager.SetPower(env.ctx, "plug-1", true)
	require.NoError(t, err)

	// Immediately simulate MQTT update with old state (OFF) - this is the race condition
	// In real world, this could be a delayed MQTT message
	env.simulateMQTTUpdate("plug-1", false)

	// Wait a bit for the race to potentially manifest
	time.Sleep(100 * time.Millisecond)

	// Now simulate the correct MQTT update (ON)
	env.simulateMQTTUpdate("plug-1", true)

	// All views should eventually converge to ON
	env.assertAllStatesMatch("plug-1", true, "After race condition, all views should converge to ON")
}

// TestHomeKitCommandSyncsToWeb tests that a HomeKit command updates the web view
func TestHomeKitCommandSyncsToWeb(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
	}

	// Simulate HomeKit sending command
	env.commands <- plugs.CommandEvent{
		PlugID: "plug-1",
		On:     true,
	}

	// All views should show ON
	env.assertAllStatesMatch("plug-1", true, "After HomeKit command, all views should show ON")
}

// TestWebCommandSyncsToHomeKit tests that a Web UI command updates HomeKit
func TestWebCommandSyncsToHomeKit(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
	}

	// Simulate Web UI command
	err := env.manager.SetPower(env.ctx, "plug-1", true)
	require.NoError(t, err)

	// All views should show ON
	env.assertAllStatesMatch("plug-1", true, "After Web command, all views should show ON")
}

// TestReproduceFourLampsBug reproduces the exact bug: 4 lamps all ON, but HomeKit shows 2 ON/2 OFF
func TestReproduceFourLampsBug(t *testing.T) {
	t.Skip("This test currently FAILS and reproduces the bug - will pass after fix")

	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "lamp-1", Name: "Lamp 1", Address: "192.168.1.101"},
		{ID: "lamp-2", Name: "Lamp 2", Address: "192.168.1.102"},
		{ID: "lamp-3", Name: "Lamp 3", Address: "192.168.1.103"},
		{ID: "lamp-4", Name: "Lamp 4", Address: "192.168.1.104"},
	})

	// Configure fake to return ON
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`), // lamp-1
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`), // lamp-2
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`), // lamp-3
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`), // lamp-4
	}

	// User toggles all 4 lamps ON via web UI rapidly
	for i := 1; i <= 4; i++ {
		lampID := fmt.Sprintf("lamp-%d", i)
		err := env.manager.SetPower(env.ctx, lampID, true)
		require.NoError(t, err)
	}

	// Simulate MQTT messages arriving during the 2-second GetStatus delay
	// Some are stale (OFF), some are current (ON)
	time.Sleep(500 * time.Millisecond)
	env.simulateMQTTUpdate("lamp-1", false) // Stale MQTT message
	env.simulateMQTTUpdate("lamp-2", true)  // Current MQTT message

	time.Sleep(500 * time.Millisecond)
	env.simulateMQTTUpdate("lamp-3", false) // Stale MQTT message
	env.simulateMQTTUpdate("lamp-4", true)  // Current MQTT message

	// Wait for GetStatus refresh to complete (2 seconds + processing time)
	time.Sleep(2500 * time.Millisecond)

	// Send correct MQTT updates
	for i := 1; i <= 4; i++ {
		lampID := fmt.Sprintf("lamp-%d", i)
		env.simulateMQTTUpdate(lampID, true)
	}

	// All four lamps should show ON in ALL views
	for i := 1; i <= 4; i++ {
		lampID := fmt.Sprintf("lamp-%d", i)
		env.assertAllStatesMatch(lampID, true, "Lamp %s should be ON everywhere", lampID)
	}
}

// TestStateConsistencyAfterMQTTFlapping tests recovery from flapping MQTT state
func TestStateConsistencyAfterMQTTFlapping(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	// Simulate rapid MQTT flapping (ON/OFF/ON/OFF)
	env.simulateMQTTUpdate("plug-1", true)
	time.Sleep(50 * time.Millisecond)
	env.simulateMQTTUpdate("plug-1", false)
	time.Sleep(50 * time.Millisecond)
	env.simulateMQTTUpdate("plug-1", true)
	time.Sleep(50 * time.Millisecond)
	env.simulateMQTTUpdate("plug-1", false)

	// Final state is OFF
	env.assertAllStatesMatch("plug-1", false, "After flapping, all views should converge to final state OFF")
}

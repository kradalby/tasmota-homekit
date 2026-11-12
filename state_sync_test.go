package tasmotahomekit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
)

// TestStateSyncEnvironment sets up a complete environment with Manager, HAP, and Web
type TestStateSyncEnvironment struct {
	t          *testing.T
	manager    *plugs.Manager
	hapManager *HAPManager
	webServer  *WebServer
	eventBus   *events.Bus
	fakeClient *fakePlugClient
	ctx        context.Context
	cancel     context.CancelFunc

	webMu   sync.Mutex
	webView map[string]events.StateUpdateEvent
}

type fakePlugClient struct {
	mu        sync.Mutex
	responses [][]byte

	// beforeReply, if set, runs before each command is answered.
	beforeReply func(cmd string)
}

func (f *fakePlugClient) ExecuteCommand(_ context.Context, cmd string) ([]byte, error) {
	f.mu.Lock()
	var resp []byte
	if len(f.responses) > 0 {
		resp, f.responses = f.responses[0], f.responses[1:]
	}
	before := f.beforeReply
	f.mu.Unlock()

	if before != nil {
		before(cmd)
	}
	if resp != nil {
		return resp, nil
	}
	// Answer as a single-relay plug that is on: Power with the state it
	// switched to, Status 0 with its status.
	if state, ok := strings.CutPrefix(cmd, "Power "); ok {
		return []byte(`{"POWER":"` + state + `"}`), nil
	}
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

	manager, err := plugs.NewManager(plugConfigs, eventBus)
	require.NoError(t, err)

	// Replace client with fake for all plugs
	fake := &fakePlugClient{}
	for _, cfg := range plugConfigs {
		manager.SetClientForTesting(cfg.ID, fake)
	}

	hapManager := NewHAPManager(plugConfigs, "Test Bridge", manager, eventBus)
	webServer := NewWebServer(logger, manager, manager, eventBus, nil, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Start background processors
	go manager.ProcessStateEvents(ctx)
	go hapManager.ProcessStateChanges(ctx)
	go webServer.processStateChanges(ctx)

	env := &TestStateSyncEnvironment{
		t:          t,
		manager:    manager,
		hapManager: hapManager,
		webServer:  webServer,
		eventBus:   eventBus,
		fakeClient: fake,
		ctx:        ctx,
		cancel:     cancel,
	}

	// Watch the web view the way a browser does: current snapshot first,
	// then the SSE stream.
	sse := make(chan events.StateUpdateEvent, 100)
	webServer.sseClientsMu.Lock()
	webServer.sseClients[sse] = struct{}{}
	webServer.sseClientsMu.Unlock()

	env.webView = make(map[string]events.StateUpdateEvent)
	for _, evt := range webServer.snapshotState() {
		env.webView[evt.PlugID] = evt
	}
	go func() {
		for {
			select {
			case evt := <-sse:
				env.webMu.Lock()
				env.webView[evt.PlugID] = evt
				env.webMu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	return env
}

// getHAPState returns the current state of a plug as seen by HomeKit
func (env *TestStateSyncEnvironment) getHAPState(plugID string) bool {
	acc, ok := env.hapManager.accessories[plugID]
	require.True(env.t, ok, "accessory not found for plug %s", plugID)
	return acc.OnValue()
}

// getWebState returns the current state of a plug as seen by Web UI
func (env *TestStateSyncEnvironment) getWebState(plugID string) bool {
	env.webMu.Lock()
	defer env.webMu.Unlock()
	return env.webView[plugID].On
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
		PlugID:   plugID,
		Received: time.Now(),
		On:       &on,
	})
}

// assertAllStatesMatch verifies that Manager, HAP, and Web all show the same state
func (env *TestStateSyncEnvironment) assertAllStatesMatch(plugID string, expectedOn bool, msgAndArgs ...any) {
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

// TestHomeKitCommandSyncsToWeb tests that a HomeKit command updates the web view
func TestHomeKitCommandSyncsToWeb(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	require.Zero(t, homeKitWrite(env.hapManager, "plug-1", true))

	// All views should show ON
	env.assertAllStatesMatch("plug-1", true, "After HomeKit command, all views should show ON")
}

// TestWebCommandSyncsToHomeKit tests that a Web UI command updates HomeKit
func TestWebCommandSyncsToHomeKit(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	req := httptest.NewRequest(http.MethodPost, "/toggle/plug-1", strings.NewReader("action=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	env.webServer.HandleToggle(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)

	env.assertAllStatesMatch("plug-1", true, "After Web command, all views should show ON")
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

// gateLog parks the first goroutine that logs msg until release is closed.
type gateLog struct {
	msg     string
	hit     chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateLog) Enabled(context.Context, slog.Level) bool { return true }
func (g *gateLog) WithAttrs([]slog.Attr) slog.Handler       { return g }
func (g *gateLog) WithGroup(string) slog.Handler            { return g }

func (g *gateLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == g.msg {
		g.once.Do(func() {
			close(g.hit)
			<-g.release
		})
	}
	return nil
}

// An MQTT merge and an HTTP status reply race. Whichever is stored last must
// also be published last, or HAP and web keep the value the manager dropped.
func TestStatePublishedInMutationOrder(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
		{ID: "plug-2", Name: "Barrier", Address: "192.168.1.101"},
	})
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"OFF"}}`),
	}

	// Park the MQTT merge at its debug log, which used to sit between
	// storing the merged state and publishing it.
	gate := &gateLog{
		msg:     "Merged state from eventbus",
		hit:     make(chan struct{}),
		release: make(chan struct{}),
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(gate))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env.simulateMQTTUpdate("plug-1", true)
	select {
	case <-gate.hit:
	case <-time.After(2 * time.Second):
		t.Fatal("MQTT merge never logged")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = env.manager.GetStatus(env.ctx, "plug-1")
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}
	close(gate.release)
	<-done

	// Events are merged and delivered in order, so once HAP sees plug-2
	// anything the parked merge still had to publish has landed.
	env.simulateMQTTUpdate("plug-2", true)
	require.Eventually(t, func() bool { return env.getHAPState("plug-2") }, 2*time.Second, 10*time.Millisecond)

	env.assertAllStatesMatch("plug-1", false, "HTTP OFF was stored last")
}

// A status reply already in flight when a newer MQTT update lands must not
// overwrite it.
func TestStaleStatusReplyDropped(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})

	env.simulateMQTTUpdate("plug-1", true)
	env.assertAllStatesMatch("plug-1", true)

	started := make(chan struct{})
	release := make(chan struct{})
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
	}
	env.fakeClient.beforeReply = func(string) {
		close(started)
		<-release
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = env.manager.GetStatus(env.ctx, "plug-1")
	}()
	<-started

	env.simulateMQTTUpdate("plug-1", false)
	env.assertAllStatesMatch("plug-1", false)

	close(release)
	<-done

	env.assertAllStatesMatch("plug-1", false, "stale status reply won")
}

// An MQTT report held up in the hook after it arrived must not be ordered
// after a status reply requested meanwhile.
func TestMQTTReportStampedOnArrival(t *testing.T) {
	env := setupStateSyncTest(t, []plugs.Plug{
		{ID: "plug-1", Name: "Test Lamp", Address: "192.168.1.100"},
	})
	env.fakeClient.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"OFF"}}`),
	}

	client, err := env.eventBus.Client(events.ClientMQTT)
	require.NoError(t, err)
	hook := &MQTTHook{statePublisher: eventbus.Publish[plugs.StateChangedEvent](client)}

	gate := &gateLog{
		msg:     "MQTT message received",
		hit:     make(chan struct{}),
		release: make(chan struct{}),
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(gate))
	t.Cleanup(func() { slog.SetDefault(prev) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = hook.OnPublish(nil, packets.Packet{
			TopicName: "stat/tasmota/plug-1/RESULT",
			Payload:   []byte(`{"POWER":"ON"}`),
		})
	}()
	select {
	case <-gate.hit:
	case <-time.After(2 * time.Second):
		t.Fatal("hook never logged the report")
	}

	_, err = env.manager.GetStatus(env.ctx, "plug-1")
	require.NoError(t, err)

	close(gate.release)
	<-done

	require.Eventually(t, func() bool {
		_, s, _ := env.manager.Plug("plug-1")
		return s.MQTTConnected
	}, 2*time.Second, 10*time.Millisecond, "MQTT report never merged")

	env.assertAllStatesMatch("plug-1", false, "held MQTT report overwrote newer status reply")
}

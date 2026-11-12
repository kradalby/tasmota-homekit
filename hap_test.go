package tasmotahomekit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
)

// noPlugs stands in for a manager that knows no plugs.
type noPlugs struct{}

func (noPlugs) SetPower(context.Context, string, bool) error { return nil }

func (noPlugs) Plug(string) (plugs.Plug, plugs.State, bool) {
	return plugs.Plug{}, plugs.State{}, false
}

func newTestEventsBus(t *testing.T) *events.Bus {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev, err := events.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ev.Close() })
	return ev
}

func TestHAPManagerUpdateState(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}

	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)
	if len(hm.accessories) != 1 {
		t.Fatalf("expected 1 accessory, got %d", len(hm.accessories))
	}

	hm.UpdateState(events.StateUpdateEvent{
		PlugID: "plug-1",
		On:     true,
	})

	if !hm.accessories["plug-1"].OnValue() {
		t.Fatalf("expected outlet to be ON")
	}
}

func TestHAPManagerProcessesEvents(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)
	ctx := t.Context()

	hm.Start(ctx)

	client, err := eventBus.Client(events.ClientPlugManager)
	require.NoError(t, err)
	eventBus.PublishStateUpdate(client, events.StateUpdateEvent{
		PlugID: "plug-1",
		On:     true,
	})

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, hm.accessories["plug-1"].OnValue())
	}, time.Second, 10*time.Millisecond)
}

func TestHAPManagerExposesAccessories(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)

	acc := hm.GetAccessories()
	if len(acc) != 2 {
		t.Fatalf("expected bridge + 1 outlet, got %d", len(acc))
	}

	if acc[0].Type != accessory.TypeBridge {
		t.Fatalf("expected bridge accessory first, got %d", acc[0].Type)
	}
}

func TestHAPManagerAccessoryOrderStable(t *testing.T) {
	plugCfg := []plugs.Plug{
		{ID: "plug-1", Name: "First Plug", Address: "1.2.3.4"},
		{ID: "plug-2", Name: "Second Plug", Address: "1.2.3.5"},
		{ID: "plug-3", Name: "Third Plug", Address: "1.2.3.6"},
	}

	// HomeKit keys pairings, rooms and automations on these IDs; a new
	// derivation orphans every one of them.
	wantIDs := map[string]uint64{
		"plug-1": 17220761463179327959,
		"plug-2": 17220762562690956170,
		"plug-3": 17220763662202584381,
	}

	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, newTestEventsBus(t))
	acc := hm.GetAccessories()
	require.Len(t, acc, len(plugCfg)+1)

	for i, plug := range plugCfg {
		got := acc[i+1] // bridge is first
		require.Equal(t, plug.Name, got.Info.Name.Value())
		require.Equal(t, wantIDs[plug.ID], got.Id, "accessory ID for %s", plug.ID)
	}
}

func TestHAPManagerSkipsHomeKitDisabledPlugs(t *testing.T) {
	plugCfg := []plugs.Plug{
		{ID: "plug-1", Name: "Shown", Address: "1.2.3.4"},
		{ID: "plug-2", Name: "Hidden", Address: "1.2.3.5", HomeKit: new(false)},
	}

	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, newTestEventsBus(t))

	acc := hm.GetAccessories()
	require.Len(t, acc, 2)
	require.Equal(t, "Shown", acc[1].Info.Name.Value())
	require.NotContains(t, hm.accessories, "plug-2")
}

func TestHAPManagerPublishesCommandEvents(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)

	client, err := eventBus.Client(events.ClientHAP)
	require.NoError(t, err)
	sub := eventbus.Subscribe[events.CommandEvent](client)
	t.Cleanup(sub.Close)

	hm.publishCommand("plug-1", true)

	select {
	case evt := <-sub.Events():
		require.Equal(t, "plug-1", evt.PlugID)
		require.NotNil(t, evt.On)
		require.True(t, *evt.On)
		require.Equal(t, events.CommandTypeSetPower, evt.CommandType)
	case <-time.After(time.Second):
		t.Fatal("expected command event")
	}
}

func TestHAPManagerCreatesBulb(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "bulb-1",
		Name:    "Ceiling Light",
		Address: "1.2.3.5",
		Type:    "bulb",
	}}

	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)

	if len(hm.accessories) != 1 {
		t.Fatalf("expected 1 accessory, got %d", len(hm.accessories))
	}

	acc := hm.GetAccessories()
	// Bridge + 1 accessory
	if len(acc) != 2 {
		t.Fatalf("expected 2 accessories, got %d", len(acc))
	}

	// Check if it's a lightbulb wrapper
	_, ok := hm.accessories["bulb-1"].(*LightbulbWrapper)
	if !ok {
		t.Fatalf("expected LightbulbWrapper for bulb type")
	}
}

func TestHAPManagerStats(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", noPlugs{}, eventBus)

	hm.UpdateState(events.StateUpdateEvent{
		PlugID: "plug-1",
		On:     true,
	})

	if hm.outgoingUpdates.Load() != 1 {
		t.Errorf("expected 1 outgoing update, got %d", hm.outgoingUpdates.Load())
	}

	if hm.lastActivity.Load() == 0 {
		t.Error("expected lastActivity to be set")
	}
}

type failingPlugClient struct{}

func (failingPlugClient) ExecuteCommand(context.Context, string) ([]byte, error) {
	return nil, errors.New("unreachable")
}

func (failingPlugClient) ExecuteBacklog(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("unreachable")
}

func newHAPWithPlug(t *testing.T, client plugs.Client) (*HAPManager, *plugs.Manager) {
	t.Helper()

	plugCfg := []plugs.Plug{{ID: "plug-1", Name: "Desk Lamp", Address: "1.2.3.4"}}
	eventBus := newTestEventsBus(t)

	pm, err := plugs.NewManager(plugCfg, eventBus)
	require.NoError(t, err)
	pm.SetClientForTesting("plug-1", client)

	hm := NewHAPManager(plugCfg, "Test Bridge", pm, eventBus)
	t.Cleanup(hm.Close)
	return hm, pm
}

func homeKitWrite(hm *HAPManager, plugID string, on bool) int {
	c := hm.accessories[plugID].(*OutletWrapper).Outlet.Outlet.On
	_, code := c.SetValueRequest(on, httptest.NewRequest(http.MethodPut, "/characteristics", nil))
	return code
}

// errorLog records the plug_id of each error logged while it is the default.
type errorLog struct {
	mu    sync.Mutex
	plugs []string
}

func captureErrors(t *testing.T) *errorLog {
	l := &errorLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

func (l *errorLog) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelError }
func (l *errorLog) WithAttrs([]slog.Attr) slog.Handler               { return l }
func (l *errorLog) WithGroup(string) slog.Handler                    { return l }

func (l *errorLog) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "plug_id" {
			l.mu.Lock()
			l.plugs = append(l.plugs, a.Value.String())
			l.mu.Unlock()
		}
		return true
	})
	return nil
}

func (l *errorLog) plugIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.plugs)
}

// heldPlug hands each command to the test, which answers it when it chooses.
type heldPlug chan heldCall

type heldCall struct {
	cmd   string
	reply chan []byte // nil fails the command
}

func (p heldPlug) ExecuteCommand(ctx context.Context, cmd string) ([]byte, error) {
	reply := make(chan []byte)
	select {
	case p <- heldCall{cmd, reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case b := <-reply:
		if b == nil {
			return nil, errors.New("unreachable")
		}
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p heldPlug) ExecuteBacklog(ctx context.Context, _ ...string) ([]byte, error) {
	return p.ExecuteCommand(ctx, "Backlog")
}

func TestHAPWriteAppliedWhenPlugAccepts(t *testing.T) {
	hm, pm := newHAPWithPlug(t, &fakePlugClient{})

	require.Zero(t, homeKitWrite(hm, "plug-1", true))
	require.True(t, hm.accessories["plug-1"].OnValue())
	require.Equal(t, uint64(1), hm.incomingCommands.Load())

	hm.wg.Wait()
	require.True(t, hm.accessories["plug-1"].OnValue())
	_, state, _ := pm.Plug("plug-1")
	require.True(t, state.On)
}

// HomeKit's write is accepted before the plug answers. When the plug does
// not take it, the failure is logged and HomeKit is set back to the plug's
// last confirmed state; the manager publishes only changes, so nothing else
// would undo the written value.
func TestHAPFailedWriteRestoresPlugState(t *testing.T) {
	errs := captureErrors(t)
	hm, pm := newHAPWithPlug(t, &fakePlugClient{})
	require.Zero(t, homeKitWrite(hm, "plug-1", true))
	hm.wg.Wait()

	pm.SetClientForTesting("plug-1", failingPlugClient{})
	require.Zero(t, homeKitWrite(hm, "plug-1", false))
	require.False(t, hm.accessories["plug-1"].OnValue())

	hm.wg.Wait()
	require.True(t, hm.accessories["plug-1"].OnValue(), "HomeKit kept a state the plug did not take")
	require.Equal(t, []string{"plug-1"}, errs.plugIDs())
}

// hungPlug never answers; only the command's deadline ends it.
type hungPlug struct{}

func (hungPlug) ExecuteCommand(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hungPlug) ExecuteBacklog(ctx context.Context, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A plug that never answers holds its command only until hapWriteTimeout,
// after which HomeKit is set back.
func TestHAPTimedOutWriteRestoresPlugState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hm, _ := newHAPWithPlug(t, hungPlug{})

		start := time.Now()
		require.Zero(t, homeKitWrite(hm, "plug-1", true))
		hm.wg.Wait()

		require.Equal(t, hapWriteTimeout, time.Since(start))
		require.False(t, hm.accessories["plug-1"].OnValue())
	})
}

// HomeKit switches a plug on and straight back off while the plug is slow
// to take the first command. The plug and HomeKit must both end up off,
// whether that slow command then succeeds or fails.
func TestHAPLastWriteWins(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstReply []byte
	}{
		{"slow command succeeds", []byte(`{"POWER":"ON"}`)},
		{"slow command fails", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plug := make(heldPlug)
			hm, pm := newHAPWithPlug(t, plug)

			require.Zero(t, homeKitWrite(hm, "plug-1", true))
			first := <-plug
			require.Equal(t, "Power ON", first.cmd)

			require.Zero(t, homeKitWrite(hm, "plug-1", false))
			first.reply <- tc.firstReply

			second := <-plug
			require.Equal(t, "Power OFF", second.cmd)
			second.reply <- []byte(`{"POWER":"OFF"}`)
			hm.wg.Wait()

			require.False(t, hm.accessories["plug-1"].OnValue())
			_, state, _ := pm.Plug("plug-1")
			require.False(t, state.On)
		})
	}
}

// A change stored before HomeKit subscribes is never published to it, so
// HomeKit must start from the manager's state.
func TestHAPStartsFromManagerState(t *testing.T) {
	plugCfg := []plugs.Plug{{ID: "plug-1", Name: "Desk Lamp", Address: "1.2.3.4"}}
	eventBus := newTestEventsBus(t)

	pm, err := plugs.NewManager(plugCfg, eventBus)
	require.NoError(t, err)
	pm.SetClientForTesting("plug-1", &fakePlugClient{})

	_, err = pm.GetStatus(t.Context(), "plug-1")
	require.NoError(t, err)

	hm := NewHAPManager(plugCfg, "Test Bridge", pm, eventBus)
	t.Cleanup(hm.Close)
	require.True(t, hm.accessories["plug-1"].OnValue())
}

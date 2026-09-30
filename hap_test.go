package tasmotahomekit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
)

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
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)
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
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)
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
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)

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

	newManager := func() *HAPManager {
		eventBus := newTestEventsBus(t)
		return NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)
	}

	hm1 := newManager()
	hm2 := newManager()

	acc1 := hm1.GetAccessories()
	acc2 := hm2.GetAccessories()

	require.Len(t, acc1, len(plugCfg)+1)
	require.Len(t, acc2, len(plugCfg)+1)

	for i, plug := range plugCfg {
		accessoryIndex := i + 1 // Skip bridge at index 0
		require.Equal(t, plug.Name, acc1[accessoryIndex].Info.Name.Value())
		require.Equal(t, plug.Name, acc2[accessoryIndex].Info.Name.Value())
		require.Equal(t, acc1[accessoryIndex].Id, acc2[accessoryIndex].Id, "accessory IDs must remain stable")
		require.Equal(t, hashString(plug.ID), acc1[accessoryIndex].Id, "hash mismatch for plug %s", plug.ID)
	}
}

func TestHAPManagerPublishesCommandEvents(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)

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
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)

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
	hm := NewHAPManager(plugCfg, "Test Bridge", nil, eventBus)

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

	return NewHAPManager(plugCfg, "Test Bridge", pm, eventBus), pm
}

func homeKitWrite(hm *HAPManager, plugID string, on bool) int {
	c := hm.accessories[plugID].(*OutletWrapper).Outlet.Outlet.On
	_, code := c.SetValueRequest(on, httptest.NewRequest(http.MethodPut, "/characteristics", nil))
	return code
}

// A write the plug rejects must fail in HomeKit and leave the characteristic
// alone; nothing would reset it later.
func TestHAPWriteRejectedWhenPlugFails(t *testing.T) {
	hm, _ := newHAPWithPlug(t, failingPlugClient{})

	require.NotZero(t, homeKitWrite(hm, "plug-1", true))
	require.False(t, hm.accessories["plug-1"].OnValue())
}

func TestHAPWriteAppliedWhenPlugAccepts(t *testing.T) {
	hm, pm := newHAPWithPlug(t, &fakePlugClient{})

	require.Zero(t, homeKitWrite(hm, "plug-1", true))
	require.True(t, hm.accessories["plug-1"].OnValue())
	require.Equal(t, uint64(1), hm.incomingCommands.Load())

	_, state, _ := pm.Plug("plug-1")
	require.True(t, state.On)
}

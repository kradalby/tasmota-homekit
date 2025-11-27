package tasmotahomekit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"
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

	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)
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
	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)

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
		commands := make(chan plugs.CommandEvent, 1)
		eventBus := newTestEventsBus(t)
		return NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)
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
	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)

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

	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)

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
	commands := make(chan plugs.CommandEvent, 1)
	eventBus := newTestEventsBus(t)
	hm := NewHAPManager(plugCfg, "Test Bridge", commands, nil, eventBus)

	// Simulate incoming command
	acc := hm.accessories["plug-1"]
	acc.OnValueRemoteUpdate(func(on bool) {
		// This closure is what HAP calls, which calls hm.publishCommand
		// We need to manually trigger what the closure does or call the closure itself if we could access it.
		// But we can't easily access the closure registered in NewHAPManager without exposing it.
		// However, NewHAPManager registers the closure on the Switchable.
		// So if we trigger the callback on the Switchable, it should ripple through.
	})

	// Wait, Switchable.OnValueRemoteUpdate registers a callback.
	// The closure in NewHAPManager IS the callback.
	// But we can't trigger it from here easily because we don't have access to the underlying characteristic's callback mechanism directly via Switchable interface.
	// Actually, the OutletWrapper wraps accessory.Outlet.
	// We can access the underlying characteristic if we cast it.

	// Trigger the callback manually to simulate HAP interaction
	// But `OnValueRemoteUpdate` just sets the callback, it doesn't trigger it.
	// The callback is triggered by the HAP library when a request comes in.
	// We can manually call the function we registered if we had a way to get it back, but we don't.

	// However, we can test UpdateState (outgoing)
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

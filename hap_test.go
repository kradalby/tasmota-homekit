package tasmotahomekit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/kradalby/tasmota-nefit/events"
	"github.com/kradalby/tasmota-nefit/plugs"
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
	hm := NewHAPManager(plugCfg, commands, nil, eventBus)
	if len(hm.outlets) != 1 {
		t.Fatalf("expected 1 outlet, got %d", len(hm.outlets))
	}

	hm.UpdateState("plug-1", plugs.State{On: true})

	if !hm.outlets["plug-1"].Outlet.On.Value() {
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
	hm := NewHAPManager(plugCfg, commands, nil, eventBus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)

	client, err := eventBus.Client(events.ClientPlugManager)
	require.NoError(t, err)
	pub := eventbus.Publish[plugs.StateChangedEvent](client)
	pub.Publish(plugs.StateChangedEvent{
		PlugID: "plug-1",
		State: plugs.State{
			On: true,
		},
	})

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.True(c, hm.outlets["plug-1"].Outlet.On.Value())
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
	hm := NewHAPManager(plugCfg, commands, nil, eventBus)

	acc := hm.GetAccessories()
	if len(acc) != 2 {
		t.Fatalf("expected bridge + 1 outlet, got %d", len(acc))
	}

	if acc[0].Type != accessory.TypeBridge {
		t.Fatalf("expected bridge accessory first, got %d", acc[0].Type)
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
	hm := NewHAPManager(plugCfg, commands, nil, eventBus)

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

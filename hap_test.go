package tasmotahomekit

import (
	"context"
	"testing"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/kradalby/tasmota-nefit/plugs"
	"tailscale.com/util/eventbus"
)

func TestHAPManagerUpdateState(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}

	commands := make(chan plugs.CommandEvent, 1)
	bus := eventbus.New()

	hm := NewHAPManager(plugCfg, commands, nil, bus)
	if len(hm.outlets) != 1 {
		t.Fatalf("expected 1 outlet, got %d", len(hm.outlets))
	}

	hm.UpdateState("plug-1", true)

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
	bus := eventbus.New()

	hm := NewHAPManager(plugCfg, commands, nil, bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)

	pub := eventbus.Publish[plugs.StateChangedEvent](bus.Client("publisher"))
	pub.Publish(plugs.StateChangedEvent{
		PlugID: "plug-1",
		State: plugs.State{
			On: true,
		},
	})

	time.Sleep(50 * time.Millisecond)

	if !hm.outlets["plug-1"].Outlet.On.Value() {
		t.Fatalf("expected outlet to be ON after event")
	}
}

func TestHAPManagerExposesAccessories(t *testing.T) {
	plugCfg := []plugs.Plug{{
		ID:      "plug-1",
		Name:    "Desk Lamp",
		Address: "1.2.3.4",
	}}
	commands := make(chan plugs.CommandEvent, 1)
	bus := eventbus.New()

	hm := NewHAPManager(plugCfg, commands, nil, bus)

	acc := hm.GetAccessories()
	if len(acc) != 2 {
		t.Fatalf("expected bridge + 1 outlet, got %d", len(acc))
	}

	if acc[0].Type != accessory.TypeBridge {
		t.Fatalf("expected bridge accessory first, got %d", acc[0].Type)
	}
}

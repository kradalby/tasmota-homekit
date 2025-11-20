package tasmotahomekit

import (
	"context"
	"log/slog"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/kradalby/tasmota-nefit/events"
	"github.com/kradalby/tasmota-nefit/plugs"
	"tailscale.com/util/eventbus"
)

// Switchable is an interface for accessories that can be turned on/off
type Switchable interface {
	SetOn(on bool)
	OnValue() bool
	OnValueRemoteUpdate(f func(on bool))
}

// OutletWrapper wraps an accessory.Outlet to implement Switchable
type OutletWrapper struct {
	*accessory.Outlet
}

func (w *OutletWrapper) SetOn(on bool) {
	w.Outlet.Outlet.On.SetValue(on)
}

func (w *OutletWrapper) OnValue() bool {
	return w.Outlet.Outlet.On.Value()
}

func (w *OutletWrapper) OnValueRemoteUpdate(f func(on bool)) {
	w.Outlet.Outlet.On.OnValueRemoteUpdate(f)
}

// LightbulbWrapper wraps an accessory.Lightbulb to implement Switchable
type LightbulbWrapper struct {
	*accessory.Lightbulb
}

func (w *LightbulbWrapper) SetOn(on bool) {
	w.Lightbulb.Lightbulb.On.SetValue(on)
}

func (w *LightbulbWrapper) OnValue() bool {
	return w.Lightbulb.Lightbulb.On.Value()
}

func (w *LightbulbWrapper) OnValueRemoteUpdate(f func(on bool)) {
	w.Lightbulb.Lightbulb.On.OnValueRemoteUpdate(f)
}

// HAPManager manages HomeKit accessories and their state synchronization
type HAPManager struct {
	bridge          *accessory.Bridge
	accessories     map[string]Switchable
	commands        chan plugs.CommandEvent
	plugManager     *plugs.Manager
	stateSubscriber *eventbus.Subscriber[plugs.StateChangedEvent]
	eventBus        *events.Bus
	eventClient     *eventbus.Client
}

// NewHAPManager creates a new HAP manager with accessories for all plugs
func NewHAPManager(
	plugConfigs []plugs.Plug,
	commands chan plugs.CommandEvent,
	plugManager *plugs.Manager,
	bus *events.Bus,
) *HAPManager {
	client, err := bus.Client(events.ClientHAP)
	if err != nil {
		panic(err)
	}

	// Create bridge accessory
	bridge := accessory.NewBridge(accessory.Info{
		Name:         "Tasmota Bridge",
		Manufacturer: "Tasmota HomeKit",
		Model:        "Bridge",
		SerialNumber: "TB001",
	})

	hm := &HAPManager{
		bridge:          bridge,
		accessories:     make(map[string]Switchable),
		commands:        commands,
		plugManager:     plugManager,
		stateSubscriber: eventbus.Subscribe[plugs.StateChangedEvent](client),
		eventBus:        bus,
		eventClient:     client,
	}

	// Create accessory for each plug
	for _, plug := range plugConfigs {
		// Skip plugs that are not enabled for HomeKit
		if plug.HomeKit != nil && !*plug.HomeKit {
			slog.Info("Skipping plug for HomeKit", "plug_id", plug.ID, "name", plug.Name)
			continue
		}

		info := accessory.Info{
			Name:         plug.Name,
			Manufacturer: "Tasmota",
			Model:        plug.Model,
			SerialNumber: plug.ID,
		}

		var switchable Switchable

		if plug.Type == "bulb" {
			lightbulb := accessory.NewLightbulb(info)
			switchable = &LightbulbWrapper{lightbulb}
			slog.Info("Created HomeKit lightbulb", "plug_id", plug.ID, "name", plug.Name)
		} else {
			// Default to outlet (plug)
			outlet := accessory.NewOutlet(info)
			switchable = &OutletWrapper{outlet}
			slog.Info("Created HomeKit outlet", "plug_id", plug.ID, "name", plug.Name)
		}

		// Capture plug ID for closure
		plugID := plug.ID

		// Set up handler for when HomeKit changes the state
		switchable.OnValueRemoteUpdate(func(on bool) {
			slog.Info("HomeKit command received", "plug_id", plugID, "on", on)

			// Send command through event channel
			commands <- plugs.CommandEvent{
				PlugID: plugID,
				On:     on,
			}

			hm.publishCommand(plugID, on)
		})

		hm.accessories[plug.ID] = switchable

		// Add accessory to bridge
		// Note: We need to access the underlying accessory.A to add it to the bridge
		// Since we don't store it in the map, we do it here.
		// However, HAP library usually requires adding accessories to the bridge or the server.
		// The original code didn't explicitly add outlets to the bridge struct in NewHAPManager,
		// but presumably they are added when the server starts or via `hm.bridge.AddA(outlet.A)`.
		// Let's check how it was done. It seems they were just stored in `hm.outlets`.
		// Ah, the `Start` method (which is not shown here but likely exists) probably iterates over the map.
		// Wait, `accessory.NewBridge` creates a bridge, but we need to serve these accessories.
		// Let's look at the `Start` method in `hap.go` later. For now, I'll just store them.
	}

	return hm
}

// GetAccessories returns all accessories for the HAP server
func (hm *HAPManager) GetAccessories() []*accessory.A {
	// Collect all accessories
	var accessories []*accessory.A
	accessories = append(accessories, hm.bridge.A) // Add the bridge itself
	for _, acc := range hm.accessories {
		switch a := acc.(type) {
		case *OutletWrapper:
			accessories = append(accessories, a.A)
		case *LightbulbWrapper:
			accessories = append(accessories, a.A)
		}
	}

	return accessories
}

// UpdateState updates the HomeKit state for a plug
func (hm *HAPManager) UpdateState(plugID string, state plugs.State) {
	acc, exists := hm.accessories[plugID]
	if !exists {
		slog.Warn("Accessory not found for plug", "plug_id", plugID)
		return
	}

	// Update HomeKit state
	acc.SetOn(state.On)

	slog.Debug("Updated HomeKit state",
		"plug_id", plugID,
		"on", state.On,
	)
}

// ProcessStateChanges listens for state changes and updates HomeKit
// Start begins processing state changes.
func (hm *HAPManager) Start(ctx context.Context) {
	go hm.ProcessStateChanges(ctx)
}

// Close releases subscriptions.
func (hm *HAPManager) Close() {
	hm.stateSubscriber.Close()
}

func (hm *HAPManager) ProcessStateChanges(ctx context.Context) {
	for {
		select {
		case event := <-hm.stateSubscriber.Events():
			hm.UpdateState(event.PlugID, event.State)
		case <-ctx.Done():
			return
		}
	}
}

func (hm *HAPManager) publishCommand(plugID string, on bool) {
	if hm.eventBus == nil || hm.eventClient == nil {
		return
	}

	desiredState := on
	hm.eventBus.PublishCommand(hm.eventClient, events.CommandEvent{
		Timestamp:   time.Now(),
		Source:      "homekit",
		PlugID:      plugID,
		CommandType: events.CommandTypeSetPower,
		On:          &desiredState,
	})
}

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

// HAPManager manages HomeKit accessories and their state synchronization
type HAPManager struct {
	bridge          *accessory.Bridge
	outlets         map[string]*accessory.Outlet
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
		outlets:         make(map[string]*accessory.Outlet),
		commands:        commands,
		plugManager:     plugManager,
		stateSubscriber: eventbus.Subscribe[plugs.StateChangedEvent](client),
		eventBus:        bus,
		eventClient:     client,
	}

	// Create outlet accessory for each plug
	for _, plug := range plugConfigs {
		outlet := accessory.NewOutlet(accessory.Info{
			Name:         plug.Name,
			Manufacturer: "Tasmota",
			Model:        plug.Model,
			SerialNumber: plug.ID,
		})

		// Capture plug ID for closure
		plugID := plug.ID

		// Set up handler for when HomeKit changes the state
		outlet.Outlet.On.OnValueRemoteUpdate(func(on bool) {
			slog.Info("HomeKit command received", "plug_id", plugID, "on", on)

			// Send command through event channel
			commands <- plugs.CommandEvent{
				PlugID: plugID,
				On:     on,
			}

			hm.publishCommand(plugID, on)
		})

		hm.outlets[plug.ID] = outlet

		slog.Info("Created HomeKit outlet", "plug_id", plug.ID, "name", plug.Name)
	}

	return hm
}

// GetAccessories returns all accessories for the HAP server
func (hm *HAPManager) GetAccessories() []*accessory.A {
	accessories := []*accessory.A{hm.bridge.A}

	for _, outlet := range hm.outlets {
		accessories = append(accessories, outlet.A)
	}

	return accessories
}

// UpdateState updates the HomeKit state for a plug
func (hm *HAPManager) UpdateState(plugID string, on bool) {
	outlet, exists := hm.outlets[plugID]
	if !exists {
		slog.Warn("Outlet not found for plug", "plug_id", plugID)
		return
	}

	// Update HomeKit state
	outlet.Outlet.On.SetValue(on)

	slog.Debug("Updated HomeKit state", "plug_id", plugID, "on", on)
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
			hm.UpdateState(event.PlugID, event.State.On)
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

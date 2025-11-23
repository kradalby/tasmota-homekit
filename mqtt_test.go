package tasmotahomekit

import (
	"testing"
	"time"

	"github.com/kradalby/tasmota-homekit/plugs"
	"github.com/mochi-mqtt/server/v2/packets"
	"tailscale.com/util/eventbus"
)

func TestMQTTHookPublishesPowerState(t *testing.T) {
	bus := eventbus.New()
	pubClient := bus.Client("publisher")
	subClient := bus.Client("subscriber")

	hook := &MQTTHook{
		statePublisher: eventbus.Publish[plugs.StateChangedEvent](pubClient),
	}

	sub := eventbus.Subscribe[plugs.StateChangedEvent](subClient)
	t.Cleanup(sub.Close)

	pk := packets.Packet{
		TopicName: "stat/tasmota/plug-1/RESULT",
		Payload:   []byte(`{"POWER":"ON"}`),
	}

	if _, err := hook.OnPublish(nil, pk); err != nil {
		t.Fatalf("OnPublish() error = %v", err)
	}

	select {
	case evt := <-sub.Events():
		if evt.PlugID != "plug-1" {
			t.Fatalf("unexpected plug id: %s", evt.PlugID)
		}
		if !evt.State.On {
			t.Fatalf("expected state.On true, got false")
		}
	case <-time.After(time.Second):
		t.Fatal("expected state event")
	}
}

func TestMQTTHookParsesTelemetryState(t *testing.T) {
	bus := eventbus.New()
	pubClient := bus.Client("publisher")
	subClient := bus.Client("subscriber")

	hook := &MQTTHook{
		statePublisher: eventbus.Publish[plugs.StateChangedEvent](pubClient),
	}

	sub := eventbus.Subscribe[plugs.StateChangedEvent](subClient)
	t.Cleanup(sub.Close)

	pk := packets.Packet{
		TopicName: "tele/tasmota/plug-2/STATE",
		Payload:   []byte(`{"StatusSTS":{"POWER":"OFF"}}`),
	}

	if _, err := hook.OnPublish(nil, pk); err != nil {
		t.Fatalf("OnPublish() error = %v", err)
	}

	select {
	case evt := <-sub.Events():
		if evt.PlugID != "plug-2" {
			t.Fatalf("unexpected plug id: %s", evt.PlugID)
		}
		if evt.State.On {
			t.Fatalf("expected OFF state")
		}
	case <-time.After(time.Second):
		t.Fatal("expected event from telemetry topic")
	}
}

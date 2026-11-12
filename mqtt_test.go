package tasmotahomekit

import (
	"context"
	"testing"
	"time"

	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
)

// What Tasmota publishes must land in the manager's state: with switching
// no longer reading Status 0, SENSOR reports are what keep energy fresh, and
// any JSON report marks the plug as heard. Plain-text payloads add nothing
// the JSON ones do not, and an Offline will must not count as heard.
func TestMQTTReportsReachState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		topic   string
		payload string
		heard   bool
		on      bool
		energy  plugs.Energy
	}{
		{
			"telemetry state", "tele/tasmota/plug-1/STATE",
			`{"Time":"2026-01-01T12:00:00","Uptime":"0T01:00:00","UptimeSec":3600,"Heap":26,"SleepMode":"Dynamic","Sleep":50,"LoadAvg":19,"MqttCount":1,"POWER":"ON","Wifi":{"AP":1,"SSId":"iot","RSSI":80,"Signal":-60}}`,
			true, true,
			plugs.Energy{},
		},
		{
			"telemetry sensor", "tele/tasmota/plug-1/SENSOR",
			`{"Time":"2026-01-01T12:00:00","ENERGY":{"TotalStartTime":"2025-01-01T00:00:00","Total":1.234,"Yesterday":0.1,"Today":0.2,"Period":0,"Power":42,"ApparentPower":50,"ReactivePower":27,"Factor":0.84,"Voltage":230,"Current":0.183}}`,
			true, false,
			plugs.Energy{Power: 42, Voltage: 230, Current: 0.183, Total: 1.234},
		},
		{"command result", "stat/tasmota/plug-1/RESULT", `{"POWER":"ON"}`, true, true, plugs.Energy{}},
		{
			"info on connect", "tele/tasmota/plug-1/INFO1",
			`{"Info1":{"Module":"Sonoff S31","Version":"14.2.0(release-tasmota)","FallbackTopic":"cmnd/DVES_ABCDEF_fb/","GroupTopic":"cmnd/tasmotas/"}}`,
			true, false,
			plugs.Energy{},
		},
		{"plain power", "stat/tasmota/plug-1/POWER", `ON`, false, false, plugs.Energy{}},
		{"will online", "tele/tasmota/plug-1/LWT", `Online`, false, false, plugs.Energy{}},
		{"will offline", "tele/tasmota/plug-1/LWT", `Offline`, false, false, plugs.Energy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := newTestEventsBus(t)
			pm, err := plugs.NewManager([]plugs.Plug{
				{ID: "plug-1", Name: "Lamp", Address: "1.2.3.4"},
				{ID: "plug-2", Name: "Barrier", Address: "1.2.3.5"},
			}, bus)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go pm.ProcessStateEvents(ctx)

			client, err := bus.Client(events.ClientMQTT)
			require.NoError(t, err)
			hook := &MQTTHook{statePublisher: eventbus.Publish[plugs.StateChangedEvent](client)}

			publish := func(topic, payload string) {
				_, err := hook.OnPublish(nil, packets.Packet{TopicName: topic, Payload: []byte(payload)})
				require.NoError(t, err)
			}
			publish(tc.topic, tc.payload)
			// Reports merge in order, so once plug-2 is on, plug-1's is done.
			publish("stat/tasmota/plug-2/RESULT", `{"POWER":"ON"}`)
			require.Eventually(t, func() bool {
				_, s, _ := pm.Plug("plug-2")
				return s.On
			}, time.Second, time.Millisecond)

			_, state, _ := pm.Plug("plug-1")
			assert.Equal(t, tc.heard, !state.LastSeen.IsZero(), "heard")
			assert.Equal(t, tc.on, state.On, "on")
			assert.Equal(t, tc.energy, plugs.Energy{Power: state.Power, Voltage: state.Voltage, Current: state.Current, Total: state.Energy})
		})
	}
}

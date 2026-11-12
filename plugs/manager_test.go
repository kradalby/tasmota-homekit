package plugs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
)

type fakeClient struct {
	mu        sync.Mutex
	cmds      []string
	backlog   []string
	responses [][]byte
	err       error // fails every request when set
}

func (f *fakeClient) ExecuteCommand(_ context.Context, cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return resp, nil
	}
	// Answer as a single-relay plug that is on: Power with the state it
	// switched to, Status 0 with its status.
	if state, ok := strings.CutPrefix(cmd, "Power "); ok {
		return []byte(`{"POWER":"` + state + `"}`), nil
	}
	return []byte(`{"StatusSTS":{"POWER":"ON"}}`), nil
}

func (f *fakeClient) ExecuteBacklog(_ context.Context, cmds ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.backlog = append(f.backlog, cmds...)
	return nil, nil
}

func (f *fakeClient) statusReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, cmd := range f.cmds {
		if cmd == "Status 0" {
			n++
		}
	}
	return n
}

// tasmotaStatus is a Status 0 reply from a plug that is on, cut to the
// sections the manager reads plus some of their neighbours.
func tasmotaStatus(host string, port int, topic string) []byte {
	return fmt.Appendf(nil, `{
		"Status":{"Module":1,"DeviceName":"Plug","FriendlyName":["Plug"],"Topic":%q,"ButtonTopic":"0","Power":"1"},
		"StatusLOG":{"SerialLog":2,"WebLog":2,"MqttLog":0,"SysLog":0,"TelePeriod":300},
		"StatusMQT":{"MqttHost":%q,"MqttPort":%d,"MqttClientMask":"DVES_%%06X","MqttClient":"DVES_ABCDEF","MqttUser":"DVES_USER","MqttCount":0,"KEEPALIVE":30},
		"StatusSTS":{"POWER":"ON"}
	}`, topic, host, port)
}

var _ interface {
	ExecuteCommand(context.Context, string) ([]byte, error)
	ExecuteBacklog(context.Context, ...string) ([]byte, error)
} = (*fakeClient)(nil)

func newTestManager(t *testing.T) (*Manager, *fakeClient) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eventBus, err := events.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eventBus.Close() })

	pm, err := NewManager([]Plug{{ID: "plug-1", Name: "Plug", Address: "1"}}, eventBus)
	require.NoError(t, err)

	fake := &fakeClient{}
	pm.plugs["plug-1"].Client = fake

	return pm, fake
}

// SetPower takes the plug's state from its reply to the Power command, in
// one round trip, and fails unless the reply confirms the state asked for:
// HomeKit must not show a state the plug did not take.
func TestSetPowerStoresReply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		wantErr bool
		wantOn  bool
	}{
		{"confirmed", `{"POWER":"ON"}`, false, true},
		{"plug stayed off", `{"POWER":"OFF"}`, true, false},
		{"other relay", `{"POWER1":"ON"}`, true, false},
		{"unknown command", `{"Command":"Unknown"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm, fake := newTestManager(t)
			fake.responses = [][]byte{[]byte(tc.reply)}

			err := pm.SetPower(context.Background(), "plug-1", true)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, []string{"Power ON"}, fake.cmds)
			_, state, _ := pm.Plug("plug-1")
			require.Equal(t, tc.wantOn, state.On)
		})
	}
}

// A Power reply carries no energy reading, so switching leaves the last one
// in place until the next SENSOR report or status refresh replaces it.
func TestSetPowerKeepsEnergy(t *testing.T) {
	pm, fake := newTestManager(t)
	fake.responses = [][]byte{[]byte(`{"StatusSTS":{"POWER":"ON"},"StatusSNS":{"ENERGY":{"Power":42}}}`)}

	ctx := context.Background()
	_, err := pm.GetStatus(ctx, "plug-1")
	require.NoError(t, err)
	require.NoError(t, pm.SetPower(ctx, "plug-1", false))

	_, state, _ := pm.Plug("plug-1")
	require.False(t, state.On)
	require.InDelta(t, 42, state.Power, 0.001)
}

// A Power reply shows the plug as of the request, so a report received
// while the request was out is newer and must survive it.
func TestPowerReplyOlderThanReportDropped(t *testing.T) {
	pm, _ := newTestManager(t)
	held := &heldClient{calls: make(chan chan []byte)}
	pm.plugs["plug-1"].Client = held

	done := make(chan error)
	go func() { done <- pm.SetPower(context.Background(), "plug-1", true) }()
	reply := <-held.calls

	off := false
	pm.update("eventbus", "plug-1", func(s State) State {
		return mergeEvent(s, StateChangedEvent{PlugID: "plug-1", Received: time.Now(), On: &off})
	})

	reply <- []byte(`{"POWER":"ON"}`)
	require.NoError(t, <-done)

	_, state, _ := pm.Plug("plug-1")
	require.False(t, state.On, "Power reply overwrote a newer report")
}

// A plug is sent only the MQTT settings that differ: Tasmota restarts on
// MqttHost and MqttPort even when the value is unchanged, so a plug already
// pointed at the broker must get nothing. The read also refreshes its state.
func TestCheckMQTT(t *testing.T) {
	const host, port = "10.0.0.2", 1883
	for _, tc := range []struct {
		name    string
		status  []byte
		want    []string
		wantErr bool
	}{
		{"settings match", tasmotaStatus(host, port, "tasmota/plug-1"), nil, false},
		{"other broker", tasmotaStatus("10.0.0.9", port, "tasmota/plug-1"), []string{"MqttHost 10.0.0.2"}, false},
		{"other port", tasmotaStatus(host, 1884, "tasmota/plug-1"), []string{"MqttPort 1883"}, false},
		{"fresh plug", tasmotaStatus("", port, "tasmota_ABCDEF"), []string{"MqttHost 10.0.0.2", "Topic tasmota/plug-1"}, false},
		{"no MQTT section", []byte(`{"StatusSTS":{"POWER":"ON"}}`), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm, fake := newTestManager(t)
			fake.responses = [][]byte{tc.status}

			err := pm.checkMQTT(context.Background(), "plug-1", host, port)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.want, fake.backlog)
			_, state, _ := pm.Plug("plug-1")
			require.True(t, state.On)
		})
	}
}

// Tasmota may store a setting other than the one sent; resending it would
// restart the plug on every check, so a plug is reconfigured once per run.
func TestCheckMQTTRestartsPlugOnce(t *testing.T) {
	pm, fake := newTestManager(t)
	stale := tasmotaStatus("10.0.0.9", 1883, "tasmota/plug-1")
	fake.responses = [][]byte{stale, stale}

	ctx := context.Background()
	require.NoError(t, pm.checkMQTT(ctx, "plug-1", "10.0.0.2", 1883))
	require.Error(t, pm.checkMQTT(ctx, "plug-1", "10.0.0.2", 1883))
	require.Equal(t, []string{"MqttHost 10.0.0.2"}, fake.backlog)
}

// A tick checks the plugs not heard on MQTT within silentAfter. Plugs report
// at least every TelePeriod, so a healthy one is never checked; one never
// heard counts as silent, so every plug is checked at start and one that
// never reports keeps being checked.
func TestCheckConnections(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		lastSeen time.Time
		want     int // Status 0 reads over two ticks
	}{
		{"never heard", time.Time{}, 2},
		{"reported a TelePeriod ago", now.Add(-5 * time.Minute), 0},
		{"reported silentAfter ago", now.Add(-silentAfter), 0},
		{"silent longer", now.Add(-silentAfter - time.Second), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pm, fake := newTestManager(t)
			if !tc.lastSeen.IsZero() {
				pm.update("eventbus", "plug-1", func(s State) State {
					return mergeEvent(s, StateChangedEvent{PlugID: "plug-1", Received: tc.lastSeen})
				})
			}
			ok := tasmotaStatus("10.0.0.2", 1883, "tasmota/plug-1")
			fake.responses = [][]byte{ok, ok}

			for range 2 {
				pm.checkConnections(context.Background(), now, "10.0.0.2", 1883)
			}
			require.Equal(t, tc.want, fake.statusReads())
			require.Empty(t, fake.backlog)
		})
	}
}

// Current Tasmota sends Status.Power as a bitmask string ("1"); the reply must
// still be read, and what it changes must reach subscribers.
func TestGetStatusIgnoresStatusPowerEncoding(t *testing.T) {
	pm, fake := newTestManager(t)

	client, err := pm.eventBus.Client(events.ClientWeb)
	require.NoError(t, err)
	sub := eventbus.Subscribe[events.StateUpdateEvent](client)
	t.Cleanup(sub.Close)

	fake.responses = [][]byte{[]byte(`{
		"Status":{"Module":0,"Power":"1"},
		"StatusSNS":{"ENERGY":{"Total":1.5,"Power":42,"Voltage":230,"Current":0.18}},
		"StatusSTS":{"POWER":"ON"}
	}`)}

	got, err := pm.GetStatus(context.Background(), "plug-1")
	require.NoError(t, err)
	require.True(t, got.On)
	require.InDelta(t, 42, got.Power, 0.001)

	_, state, ok := pm.Plug("plug-1")
	require.True(t, ok)
	require.True(t, state.On)
	require.InDelta(t, 230, state.Voltage, 0.001)
	require.InDelta(t, 0.18, state.Current, 0.001)
	require.InDelta(t, 1.5, state.Energy, 0.001)

	// The initial event from NewManager may still be in flight; skip it.
	timeout := time.After(time.Second)
	for {
		select {
		case evt := <-sub.Events():
			if evt.On {
				require.InDelta(t, 42, evt.Power, 0.001)
				return
			}
		case <-timeout:
			t.Fatal("status change was not published")
		}
	}
}

func TestGetStatusWithoutPowerLeavesStateAlone(t *testing.T) {
	pm, fake := newTestManager(t)

	fake.responses = [][]byte{
		[]byte(`{"StatusSTS":{"POWER":"ON"}}`),
		[]byte(`{"StatusSTS":{"POWER1":"OFF"}}`),
	}

	_, err := pm.GetStatus(context.Background(), "plug-1")
	require.NoError(t, err)

	_, err = pm.GetStatus(context.Background(), "plug-1")
	require.Error(t, err)

	_, state, _ := pm.Plug("plug-1")
	require.True(t, state.On)
}

// heldClient hands each command's reply channel to the test, which answers
// it when it chooses.
type heldClient struct {
	calls chan chan []byte
}

func (c *heldClient) ExecuteCommand(context.Context, string) ([]byte, error) {
	reply := make(chan []byte)
	c.calls <- reply
	return <-reply, nil
}

func (c *heldClient) ExecuteBacklog(context.Context, ...string) ([]byte, error) {
	return nil, nil
}

// Two status requests overlap and the older one answers last: its reply
// must not replace the newer one.
func TestOlderStatusReplyDropped(t *testing.T) {
	pm, _ := newTestManager(t)
	held := &heldClient{calls: make(chan chan []byte)}
	pm.plugs["plug-1"].Client = held

	ctx := context.Background()

	older := make(chan struct{})
	go func() {
		defer close(older)
		_, _ = pm.GetStatus(ctx, "plug-1")
	}()
	olderReply := <-held.calls

	newer := make(chan struct{})
	go func() {
		defer close(newer)
		_, _ = pm.GetStatus(ctx, "plug-1")
	}()
	(<-held.calls) <- []byte(`{"StatusSTS":{"POWER":"ON"}}`)
	<-newer

	olderReply <- []byte(`{"StatusSTS":{"POWER":"OFF"}}`)
	<-older

	_, state, _ := pm.Plug("plug-1")
	require.True(t, state.On, "older status reply won")
}

// update publishes while holding mu, so a subscriber that stops reading
// must not hold up later changes.
func TestStalledSubscriberDoesNotBlockUpdates(t *testing.T) {
	pm, fake := newTestManager(t)

	client, err := pm.eventBus.Client(events.ClientWeb)
	require.NoError(t, err)
	stalled := eventbus.Subscribe[events.StateUpdateEvent](client)
	t.Cleanup(stalled.Close)

	const n = 200
	for i := range n {
		power := "OFF"
		if i%2 == 0 {
			power = "ON"
		}
		fake.responses = append(fake.responses, []byte(`{"StatusSTS":{"POWER":"`+power+`"}}`))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			_, _ = pm.GetStatus(context.Background(), "plug-1")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updates blocked behind a stalled subscriber")
	}
}

// An MQTT report waits on the eventbus while a status request sent after it
// is answered and stored. Merging the report late must not undo the newer
// reply or move its timestamps backward.
func TestQueuedMQTTReportOlderThanStatusDropped(t *testing.T) {
	pm, fake := newTestManager(t)

	client, err := pm.eventBus.Client(events.ClientMQTT)
	require.NoError(t, err)
	pub := eventbus.Publish[StateChangedEvent](client)

	// ProcessStateEvents is not running, so the report stays queued.
	received := time.Now().Add(-time.Second)
	on := true
	pub.Publish(StateChangedEvent{
		PlugID:   "plug-1",
		Received: received,
		On:       &on,
		Energy:   &Energy{Power: 40, Voltage: 230, Current: 0.17, Total: 1},
	})

	fake.responses = [][]byte{[]byte(`{
		"StatusSTS":{"POWER":"OFF"},
		"StatusSNS":{"ENERGY":{"Power":0,"Voltage":231,"Current":0,"Total":1.1}}
	}`)}
	_, err = pm.GetStatus(context.Background(), "plug-1")
	require.NoError(t, err)
	_, replied, _ := pm.Plug("plug-1")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pm.ProcessStateEvents(ctx)

	require.Eventually(t, func() bool {
		_, s, _ := pm.Plug("plug-1")
		return s.MQTTConnected
	}, time.Second, time.Millisecond, "MQTT report never merged")

	_, state, _ := pm.Plug("plug-1")
	require.False(t, state.On, "queued MQTT report undid a newer status reply")
	require.Zero(t, state.Power)
	require.InDelta(t, 231, state.Voltage, 0.001)
	require.Equal(t, received, state.LastSeen)
	require.Equal(t, replied.LastUpdated, state.LastUpdated, "LastUpdated moved backward")
}

// Power state and energy are ordered apart: an MQTT power report newer than
// a status request does not make the reply's energy sample stale.
func TestStatusEnergyKeptPastNewerPowerReport(t *testing.T) {
	sent := time.Now()
	off := false

	s := mergeEvent(State{}, StateChangedEvent{Received: sent.Add(time.Second), On: &off})
	s = applyStatus(s, statusReading{On: true, Energy: &Energy{Power: 40}}, sent, sent.Add(2*time.Second))

	require.False(t, s.On)
	require.InDelta(t, 40, s.Power, 0.001)
}

package plugs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"tailscale.com/util/eventbus"

	"github.com/kradalby/tasmota-homekit/events"
)

type fakeClient struct {
	mu        sync.Mutex
	lastCmd   string
	backlog   []string
	responses [][]byte
}

func (f *fakeClient) ExecuteCommand(_ context.Context, cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCmd = cmd
	if len(f.responses) > 0 {
		resp := f.responses[0]
		f.responses = f.responses[1:]
		return resp, nil
	}
	// Default response for Status 0 command - matches real Tasmota format
	return []byte(`{"StatusSTS":{"POWER":"ON"}}`), nil
}

func (f *fakeClient) ExecuteBacklog(_ context.Context, cmds ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backlog = append(f.backlog, cmds...)
	return nil, nil
}

var _ interface {
	ExecuteCommand(context.Context, string) ([]byte, error)
	ExecuteBacklog(context.Context, ...string) ([]byte, error)
} = (*fakeClient)(nil)

func newTestManager(t *testing.T) (*Manager, *fakeClient, chan CommandEvent) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	eventBus, err := events.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eventBus.Close() })

	commands := make(chan CommandEvent, 1)

	pm, err := NewManager([]Plug{{ID: "plug-1", Name: "Plug", Address: "1"}}, commands, eventBus)
	require.NoError(t, err)

	fake := &fakeClient{}
	pm.plugs["plug-1"].Client = fake

	return pm, fake, commands
}

func TestSetPowerUpdatesState(t *testing.T) {
	pm, fake, _ := newTestManager(t)

	ctx := context.Background()
	require.NoError(t, pm.SetPower(ctx, "plug-1", true))

	// SetPower now calls GetStatus immediately after sending the power command
	// So the last command should be "Status 0"
	require.Equal(t, "Status 0", fake.lastCmd)

	_, state, ok := pm.Plug("plug-1")
	require.True(t, ok)
	require.True(t, state.On)
}

func TestConfigureMQTTBacklog(t *testing.T) {
	pm, fake, _ := newTestManager(t)

	err := pm.ConfigureMQTT(context.Background(), "plug-1", "host", 1234)
	require.NoError(t, err)

	require.Contains(t, fake.backlog, "MqttHost host")
	require.Contains(t, fake.backlog, "MqttPort 1234")
	require.Contains(t, fake.backlog, "Topic tasmota/plug-1")
}

// Current Tasmota sends Status.Power as a bitmask string ("1"); the reply must
// still be read, and what it changes must reach subscribers.
func TestGetStatusIgnoresStatusPowerEncoding(t *testing.T) {
	pm, fake, _ := newTestManager(t)

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
	pm, fake, _ := newTestManager(t)

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

// update publishes while holding mu, so a subscriber that stops reading
// must not hold up later changes.
func TestStalledSubscriberDoesNotBlockUpdates(t *testing.T) {
	pm, fake, _ := newTestManager(t)

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

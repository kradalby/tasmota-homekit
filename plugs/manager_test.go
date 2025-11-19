package plugs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/kradalby/tasmota-nefit/events"
	"github.com/stretchr/testify/require"
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
	return []byte(`{"Status":{"Power":"ON"}}`), nil
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

	require.Equal(t, "Power ON", fake.lastCmd)

	state, ok := pm.states["plug-1"]
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

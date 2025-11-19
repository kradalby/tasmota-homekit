package metrics

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kradalby/tasmota-nefit/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBus(t *testing.T) *events.Bus {
	t.Helper()
	bus, err := events.New(testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func TestCollectorObservesEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := newTestBus(t)
	reg := prometheus.NewRegistry()

	collector, err := NewCollector(ctx, testLogger(), bus, reg)
	require.NoError(t, err)
	defer collector.Close()

	componentClient, err := bus.Client(events.ClientWeb)
	require.NoError(t, err)
	bos := events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: "web",
		Status:    events.ConnectionStatusConnected,
	}
	bus.PublishConnectionStatus(componentClient, bos)

	require.Eventually(t, func() bool {
		value := gaugeValue(collector.statusGauge.WithLabelValues("web", string(events.ConnectionStatusConnected)))
		return value == 1.0
	}, time.Second, 20*time.Millisecond, "expected component status gauge to update")

	cmd := events.CommandEvent{
		Timestamp:   time.Now(),
		Source:      "web",
		PlugID:      "plug-1",
		CommandType: events.CommandTypeSetPower,
	}
	bus.PublishCommand(componentClient, cmd)

	require.Eventually(t, func() bool {
		value := counterValue(collector.commandCounter.WithLabelValues("web", "plug-1", string(events.CommandTypeSetPower)))
		return value == 1.0
	}, time.Second, 20*time.Millisecond, "expected command counter to increment")
}

func gaugeValue(g prometheus.Gauge) float64 {
	var m io_prometheus_client.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	if m.Gauge == nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func counterValue(c prometheus.Counter) float64 {
	var m io_prometheus_client.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

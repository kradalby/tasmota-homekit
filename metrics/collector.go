package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/kradalby/tasmota-nefit/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"tailscale.com/util/eventbus"
)

// Collector subscribes to eventbus updates and exposes Prometheus metrics.
type Collector struct {
	logger         *slog.Logger
	statusSub      *eventbus.Subscriber[events.ConnectionStatusEvent]
	commandSub     *eventbus.Subscriber[events.CommandEvent]
	statusGauge    *prometheus.GaugeVec
	commandCounter *prometheus.CounterVec
	ctx            context.Context
	cancel         context.CancelFunc
	shutdownOnce   sync.Once
	workers        sync.WaitGroup
}

// NewCollector wires eventbus subscribers into Prometheus metrics.
func NewCollector(ctx context.Context, logger *slog.Logger, bus *events.Bus, reg prometheus.Registerer) (*Collector, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if bus == nil {
		return nil, fmt.Errorf("event bus is required")
	}
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	client, err := bus.Client(events.ClientMetrics)
	if err != nil {
		return nil, fmt.Errorf("failed to get metrics client: %w", err)
	}

	collectorCtx, cancel := context.WithCancel(ctx)
	statusSub := eventbus.Subscribe[events.ConnectionStatusEvent](client)
	commandSub := eventbus.Subscribe[events.CommandEvent](client)

	statusGauge := promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
		Name: "tasmota_homekit_component_status",
		Help: "Lifecycle state per component (1 when matching status, 0 otherwise)",
	}, []string{"component", "status"})

	commandCounter := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "tasmota_homekit_command_total",
		Help: "Total control commands by source and plug",
	}, []string{"source", "plug_id", "command_type"})

	c := &Collector{
		logger:         logger,
		statusSub:      statusSub,
		commandSub:     commandSub,
		statusGauge:    statusGauge,
		commandCounter: commandCounter,
		ctx:            collectorCtx,
		cancel:         cancel,
	}

	c.workers.Add(2)
	go c.consumeStatuses()
	go c.consumeCommands()

	logger.Info("metrics collector started")

	return c, nil
}

// Close stops the collector and releases subscribers.
func (c *Collector) Close() {
	c.shutdownOnce.Do(func() {
		c.cancel()
		if c.statusSub != nil {
			c.statusSub.Close()
		}
		if c.commandSub != nil {
			c.commandSub.Close()
		}
		c.workers.Wait()
		c.logger.Info("metrics collector stopped")
	})
}

func (c *Collector) consumeStatuses() {
	defer c.workers.Done()
	for {
		select {
		case evt := <-c.statusSub.Events():
			c.observeStatus(evt)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Collector) consumeCommands() {
	defer c.workers.Done()
	for {
		select {
		case evt := <-c.commandSub.Events():
			c.observeCommand(evt)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Collector) observeStatus(evt events.ConnectionStatusEvent) {
	for _, status := range []events.ConnectionStatus{
		events.ConnectionStatusDisconnected,
		events.ConnectionStatusConnecting,
		events.ConnectionStatusConnected,
		events.ConnectionStatusReconnecting,
		events.ConnectionStatusFailed,
	} {
		value := 0.0
		if status == evt.Status {
			value = 1.0
		}
		c.statusGauge.WithLabelValues(evt.Component, string(status)).Set(value)
	}
}

func (c *Collector) observeCommand(evt events.CommandEvent) {
	commandType := string(evt.CommandType)
	if commandType == "" {
		commandType = "unknown"
	}
	source := evt.Source
	if source == "" {
		source = "unknown"
	}
	plugID := evt.PlugID
	if plugID == "" {
		plugID = "unknown"
	}
	c.commandCounter.WithLabelValues(source, plugID, commandType).Inc()
}

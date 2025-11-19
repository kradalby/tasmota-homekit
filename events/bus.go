package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"tailscale.com/util/eventbus"
)

// ClientName represents named clients used on the shared event bus.
type ClientName string

const (
	ClientPlugManager ClientName = "plugmanager"
	ClientHAP         ClientName = "hap"
	ClientWeb         ClientName = "web"
	ClientMQTT        ClientName = "mqtt"
	ClientMetrics     ClientName = "metrics"
)

// Bus wraps tailscale's eventbus and provides helpers for publishing state updates.
type Bus struct {
	bus     *eventbus.Bus
	clients map[ClientName]*eventbus.Client
	logger  *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc

	lastStates map[string]StateUpdateEvent
	stateMu    sync.Mutex
	mu         sync.RWMutex
}

// New constructs a new bus with the known clients registered.
func New(logger *slog.Logger) (*Bus, error) {
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}

	ctx, cancel := context.WithCancel(context.Background())

	b := &Bus{
		bus:        eventbus.New(),
		clients:    make(map[ClientName]*eventbus.Client),
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
		lastStates: make(map[string]StateUpdateEvent),
	}

	for _, name := range []ClientName{
		ClientPlugManager,
		ClientHAP,
		ClientWeb,
		ClientMQTT,
		ClientMetrics,
	} {
		b.clients[name] = b.bus.Client(string(name))
	}

	logger.Info("eventbus initialized",
		slog.Int("client_count", len(b.clients)),
	)

	return b, nil
}

// Client returns the named eventbus client.
func (b *Bus) Client(name ClientName) (*eventbus.Client, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	client, ok := b.clients[name]
	if !ok {
		return nil, fmt.Errorf("client %q not found", name)
	}

	return client, nil
}

// PublishStateUpdate emits a deduplicated state update event for SSE consumers.
func (b *Bus) PublishStateUpdate(client *eventbus.Client, event StateUpdateEvent) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	last, ok := b.lastStates[event.PlugID]
	if ok && event.Equals(last) {
		b.logger.Debug("skipping duplicate state update",
			slog.String("plug_id", event.PlugID),
			slog.String("source", event.Source),
		)
		return
	}

	b.logger.Debug("publishing state update",
		slog.String("plug_id", event.PlugID),
		slog.String("source", event.Source),
		slog.Bool("on", event.On),
	)

	publisher := eventbus.Publish[StateUpdateEvent](client)
	defer publisher.Close()
	publisher.Publish(event)

	b.lastStates[event.PlugID] = event
}

// PublishCommand emits a command event for metrics/debug consumers.
func (b *Bus) PublishCommand(client *eventbus.Client, event CommandEvent) {
	b.logger.Debug("publishing command event",
		slog.String("plug_id", event.PlugID),
		slog.String("source", event.Source),
		slog.String("command_type", string(event.CommandType)),
	)

	publisher := eventbus.Publish[CommandEvent](client)
	defer publisher.Close()
	publisher.Publish(event)
}

// PublishConnectionStatus emits lifecycle updates for components (web, hap, mqtt, etc.).
func (b *Bus) PublishConnectionStatus(client *eventbus.Client, event ConnectionStatusEvent) {
	b.logger.Debug("publishing connection status",
		slog.String("component", event.Component),
		slog.String("status", string(event.Status)),
	)

	publisher := eventbus.Publish[ConnectionStatusEvent](client)
	defer publisher.Close()
	publisher.Publish(event)
}

// Close shuts down the event bus and releases clients.
func (b *Bus) Close() error {
	b.cancel()

	b.mu.Lock()
	defer b.mu.Unlock()

	for name, client := range b.clients {
		client.Close()
		delete(b.clients, name)
	}

	b.logger.Info("eventbus shut down")
	return nil
}

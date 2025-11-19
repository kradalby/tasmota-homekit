package tasmotahomekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chasefleming/elem-go"
	"github.com/chasefleming/elem-go/attrs"
	"github.com/kradalby/kra/web"
	"github.com/kradalby/tasmota-nefit/events"
	"github.com/kradalby/tasmota-nefit/plugs"
	"tailscale.com/util/eventbus"
)

type plugStateProvider interface {
	Snapshot() map[string]struct {
		Plug  plugs.Plug
		State plugs.State
	}
	Plug(string) (plugs.Plug, plugs.State, bool)
}

// WebServer manages the web UI
type WebServer struct {
	logger           *slog.Logger
	kraweb           *web.KraWeb
	plugProvider     plugStateProvider
	commands         chan plugs.CommandEvent
	eventLog         []string
	eventBus         *events.Bus
	client           *eventbus.Client
	stateSubscriber  *eventbus.Subscriber[events.StateUpdateEvent]
	statusSubscriber *eventbus.Subscriber[events.ConnectionStatusEvent]
	currentState     map[string]events.StateUpdateEvent
	connectionState  map[string]events.ConnectionStatusEvent
	stateMu          sync.RWMutex
	statusMu         sync.RWMutex
	sseClients       map[chan events.StateUpdateEvent]struct{}
	sseClientsMu     sync.RWMutex
	hapPin           string
	qrCode           string
	ctx              context.Context
}

// NewWebServer creates a new web server
func NewWebServer(logger *slog.Logger, plugProvider plugStateProvider, commands chan plugs.CommandEvent, bus *events.Bus, kraweb *web.KraWeb, hapPin, qrCode string) *WebServer {
	client, err := bus.Client(events.ClientWeb)
	if err != nil {
		panic(fmt.Sprintf("failed to create web client: %v", err))
	}

	return &WebServer{
		logger:           logger,
		kraweb:           kraweb,
		plugProvider:     plugProvider,
		commands:         commands,
		eventLog:         make([]string, 0, 100),
		eventBus:         bus,
		client:           client,
		stateSubscriber:  eventbus.Subscribe[events.StateUpdateEvent](client),
		statusSubscriber: eventbus.Subscribe[events.ConnectionStatusEvent](client),
		currentState:     make(map[string]events.StateUpdateEvent),
		connectionState:  make(map[string]events.ConnectionStatusEvent),
		sseClients:       make(map[chan events.StateUpdateEvent]struct{}),
		hapPin:           hapPin,
		qrCode:           qrCode,
		ctx:              context.Background(),
	}
}

// LogEvent adds an event to the log
func (ws *WebServer) LogEvent(event string) {
	ws.eventLog = append(ws.eventLog, fmt.Sprintf("%s: %s", time.Now().Format("15:04:05"), event))
	if len(ws.eventLog) > 100 {
		ws.eventLog = ws.eventLog[1:]
	}
}

func (ws *WebServer) Start(ctx context.Context) {
	ws.ctx = ctx
	go ws.processStateChanges(ctx)
	go ws.processConnectionStatuses(ctx)
	ws.publishConnectionStatus(events.ConnectionStatusConnecting, "")

	go func() {
		if ws.kraweb == nil {
			return
		}
		ws.logger.Info("Starting web interface")
		ws.publishConnectionStatus(events.ConnectionStatusConnected, "")
		if err := ws.kraweb.ListenAndServe(ctx); err != nil {
			ws.logger.Error("Web server error", slog.Any("error", err))
			if errors.Is(err, context.Canceled) {
				ws.publishConnectionStatus(events.ConnectionStatusDisconnected, "")
			} else {
				ws.publishConnectionStatus(events.ConnectionStatusFailed, err.Error())
			}
			return
		}
		ws.publishConnectionStatus(events.ConnectionStatusDisconnected, "")
	}()
}

func (ws *WebServer) Close() {
	ws.stateSubscriber.Close()
	ws.statusSubscriber.Close()

	ws.sseClientsMu.Lock()
	for client := range ws.sseClients {
		close(client)
	}
	ws.sseClients = make(map[chan events.StateUpdateEvent]struct{})
	ws.sseClientsMu.Unlock()
}

func (ws *WebServer) publishConnectionStatus(status events.ConnectionStatus, errMsg string) {
	if ws.eventBus == nil || ws.client == nil {
		return
	}

	ws.eventBus.PublishConnectionStatus(ws.client, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: "web",
		Status:    status,
		Error:     errMsg,
	})
}

func (ws *WebServer) publishCommand(plugID string, on bool) {
	if ws.eventBus == nil || ws.client == nil {
		return
	}

	desiredState := on
	ws.eventBus.PublishCommand(ws.client, events.CommandEvent{
		Timestamp:   time.Now(),
		Source:      "web",
		PlugID:      plugID,
		CommandType: events.CommandTypeSetPower,
		On:          &desiredState,
	})
}

func (ws *WebServer) processStateChanges(ctx context.Context) {
	for {
		select {
		case event := <-ws.stateSubscriber.Events():
			ws.stateMu.Lock()
			ws.currentState[event.PlugID] = event
			ws.stateMu.Unlock()

			ws.logger.Debug("Web UI: State change received", "plug_id", event.PlugID, "on", event.On)
			ws.broadcastSSE(event)
		case <-ctx.Done():
			return
		}
	}
}

func (ws *WebServer) processConnectionStatuses(ctx context.Context) {
	for {
		select {
		case event := <-ws.statusSubscriber.Events():
			ws.statusMu.Lock()
			ws.connectionState[event.Component] = event
			ws.statusMu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// broadcastSSE sends state updates to connected clients.
func (ws *WebServer) broadcastSSE(event events.StateUpdateEvent) {
	ws.sseClientsMu.RLock()
	defer ws.sseClientsMu.RUnlock()

	for client := range ws.sseClients {
		select {
		case client <- event:
		default:
		}
	}
}

func (ws *WebServer) snapshotState() []events.StateUpdateEvent {
	ws.stateMu.RLock()
	defer ws.stateMu.RUnlock()

	snapshot := make([]events.StateUpdateEvent, 0, len(ws.currentState))
	for _, evt := range ws.currentState {
		snapshot = append(snapshot, evt)
	}

	sort.Slice(snapshot, func(i, j int) bool {
		return snapshot[i].PlugID < snapshot[j].PlugID
	})

	return snapshot
}

func (ws *WebServer) snapshotStatuses() []events.ConnectionStatusEvent {
	ws.statusMu.RLock()
	defer ws.statusMu.RUnlock()

	statuses := make([]events.ConnectionStatusEvent, 0, len(ws.connectionState))
	for _, evt := range ws.connectionState {
		statuses = append(statuses, evt)
	}

	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Component < statuses[j].Component
	})

	return statuses
}

// renderPage renders a basic HTML page
func (ws *WebServer) renderPage(title string, content elem.Node) string {
	sseScript := `
(function() {
  function formatTime(value) {
    if (!value) {
      return "unknown";
    }
    const date = new Date(value);
    if (isNaN(date)) {
      return value;
    }
    return date.toLocaleTimeString();
  }

  function updatePlugCard(data) {
    const card = document.querySelector('[data-plug-id="' + data.plug_id + '"]');
    if (!card) {
      return;
    }

    card.classList.toggle('on', data.on);
    card.classList.toggle('off', !data.on);

    const status = card.querySelector('[data-role="status-text"]');
    if (status) {
      status.textContent = 'Status: ' + (data.on ? 'ON' : 'OFF') + ' | Last updated: ' + formatTime(data.last_updated);
    }

    const indicator = card.querySelector('[data-role="connection-indicator"]');
    if (indicator) {
      indicator.classList.remove('connected', 'stale', 'disconnected');
      indicator.classList.add(data.connection_state || 'disconnected');
    }

    const connectionText = card.querySelector('[data-role="connection-text"]');
    if (connectionText) {
      connectionText.textContent = data.connection_note || '';
    }

    const actionInput = card.querySelector('[data-role="action-input"]');
    const button = card.querySelector('[data-role="toggle-button"]');
    if (actionInput && button) {
      if (data.on) {
        actionInput.value = 'off';
        button.textContent = 'Turn Off';
        button.classList.remove('off');
        button.classList.add('on');
      } else {
        actionInput.value = 'on';
        button.textContent = 'Turn On';
        button.classList.remove('on');
        button.classList.add('off');
      }
    }
  }

  document.addEventListener('DOMContentLoaded', function() {
    const source = new EventSource('/events');
    source.onmessage = function(event) {
      try {
        const data = JSON.parse(event.data);
        updatePlugCard(data);
      } catch (err) {
        console.error('invalid SSE payload', err);
      }
    };
  });
})();`

	page := elem.Html(nil,
		elem.Head(nil,
			elem.Title(nil, elem.Text(title)),
			elem.Script(attrs.Props{
				attrs.Src: "https://unpkg.com/htmx.org@2.0.4",
			}),
			elem.Style(nil, elem.Text(`
				body { font-family: system-ui; max-width: 800px; margin: 40px auto; padding: 0 20px; }
				h1 { color: #333; }
				.plug { border: 1px solid #ddd; padding: 20px; margin: 10px 0; border-radius: 8px; display: flex; justify-content: space-between; align-items: center; }
				.plug.on { background: #e8f5e9; }
				.plug.off { background: #ffebee; }
				.plug-info { flex: 1; }
				.plug-name { font-size: 1.2em; font-weight: 500; }
				.plug-status { font-size: 0.9em; color: #666; margin-top: 4px; }
				.connection-status { display: inline-flex; align-items: center; gap: 6px; margin-top: 4px; font-size: 0.85em; }
				.connection-indicator { width: 10px; height: 10px; border-radius: 50%; display: inline-block; }
				.connection-indicator.connected { background: #4caf50; }
				.connection-indicator.stale { background: #ff9800; }
				.connection-indicator.disconnected { background: #f44336; }
				button { padding: 10px 20px; font-size: 1em; cursor: pointer; border: none; border-radius: 4px; }
				button.on { background: #4caf50; color: white; }
				button.off { background: #f44336; color: white; }
				.events { margin-top: 40px; padding: 20px; background: #f5f5f5; border-radius: 8px; max-height: 300px; overflow-y: auto; }
				.event { font-family: monospace; font-size: 0.9em; padding: 4px 0; }
			`)),
			elem.Script(nil, elem.Text(sseScript)),
		),
		elem.Body(nil, content),
	)
	return page.Render()
}

// renderPlugCard renders a single plug card element
func (ws *WebServer) renderPlugCard(plugID string, info plugs.Plug, state plugs.State) elem.Node {
	statusClass := "off"
	statusText := "OFF"
	buttonClass := "off"
	buttonText := "Turn On"
	buttonAction := "on"

	if state.On {
		statusClass = "on"
		statusText = "ON"
		buttonClass = "on"
		buttonText = "Turn Off"
		buttonAction = "off"
	}

	// Determine connection status
	var connectionIndicator, connectionText string
	if state.LastSeen.IsZero() {
		connectionIndicator = "disconnected"
		connectionText = "Never seen"
	} else {
		timeSinceSeen := time.Since(state.LastSeen)
		if timeSinceSeen < 30*time.Second {
			connectionIndicator = "connected"
			connectionText = fmt.Sprintf("Last seen: %s ago", timeSinceSeen.Round(time.Second))
		} else if timeSinceSeen < 60*time.Second {
			connectionIndicator = "stale"
			connectionText = fmt.Sprintf("Last seen: %s ago", timeSinceSeen.Round(time.Second))
		} else {
			connectionIndicator = "disconnected"
			connectionText = fmt.Sprintf("Last seen: %s ago", timeSinceSeen.Round(time.Second))
		}
	}

	return elem.Div(
		attrs.Props{
			attrs.ID:       "plug-" + plugID,
			attrs.Class:    "plug " + statusClass,
			"data-plug-id": plugID,
		},
		elem.Div(attrs.Props{attrs.Class: "plug-info"},
			elem.Div(attrs.Props{attrs.Class: "plug-name"}, elem.Text(info.Name)),
			elem.Div(attrs.Props{attrs.Class: "plug-status", "data-role": "status-text"},
				elem.Text(fmt.Sprintf("Status: %s | Last updated: %s",
					statusText,
					state.LastUpdated.Format("15:04:05"),
				)),
			),
			elem.Div(attrs.Props{attrs.Class: "connection-status"},
				elem.Span(attrs.Props{"data-role": "connection-indicator", attrs.Class: "connection-indicator " + connectionIndicator}),
				elem.Span(attrs.Props{"data-role": "connection-text"}, elem.Text(connectionText)),
			),
		),
		elem.Form(
			attrs.Props{
				"hx-post":   "/toggle/" + plugID,
				"hx-target": "#plug-" + plugID,
				"hx-swap":   "outerHTML",
			},
			elem.Input(attrs.Props{attrs.Type: "hidden", attrs.Name: "action", attrs.Value: buttonAction, "data-role": "action-input"}),
			elem.Button(
				attrs.Props{attrs.Type: "submit", attrs.Class: buttonClass, "data-role": "toggle-button"},
				elem.Text(buttonText),
			),
		),
	)
}

// HandleIndex renders the main dashboard
func (ws *WebServer) HandleIndex(w http.ResponseWriter, r *http.Request) {
	var plugElements []elem.Node

	snapshot := ws.plugProvider.Snapshot()
	var plugIDs []string
	for id := range snapshot {
		plugIDs = append(plugIDs, id)
	}
	sort.Strings(plugIDs)

	for _, id := range plugIDs {
		item := snapshot[id]
		plugElements = append(plugElements, ws.renderPlugCard(id, item.Plug, item.State))
	}

	// Add event log
	var eventElements []elem.Node
	for i := len(ws.eventLog) - 1; i >= 0 && i >= len(ws.eventLog)-20; i-- {
		eventElements = append(eventElements, elem.Div(attrs.Props{attrs.Class: "event"}, elem.Text(ws.eventLog[i])))
	}

	// Build HomeKit pairing section
	var homekitSection elem.Node
	if ws.hapPin != "" {
		var qrElements []elem.Node
		qrElements = append(qrElements,
			elem.H2(nil, elem.Text("HomeKit Pairing")),
			elem.P(nil, elem.Text(fmt.Sprintf("Pair with PIN: %s", ws.hapPin))),
		)

		if ws.qrCode != "" {
			qrElements = append(qrElements,
				elem.P(nil, elem.Text("Scan this QR code with your iPhone/iPad Home app:")),
				elem.Pre(attrs.Props{attrs.Style: "font-family: monospace; line-height: 1; font-size: 8px;"},
					elem.Text(ws.qrCode),
				),
			)
		}

		qrElements = append(qrElements,
			elem.P(nil,
				elem.Text("Open the Home app → Add Accessory → More Options → Select 'Tasmota Bridge'"),
			),
		)

		homekitSection = elem.Div(attrs.Props{attrs.Class: "homekit-section", attrs.Style: "border: 2px solid #007aff; padding: 20px; margin: 20px 0; border-radius: 8px; background: #f0f8ff;"},
			qrElements...,
		)
	}

	content := elem.Div(nil,
		elem.H1(nil, elem.Text("Tasmota HomeKit Bridge")),
		elem.P(nil, elem.Text(fmt.Sprintf("Managing %d plugs", len(snapshot)))),
		homekitSection,
		elem.Div(nil, plugElements...),
		elem.Div(attrs.Props{attrs.Class: "events"},
			elem.H2(nil, elem.Text("Recent Events")),
			elem.Div(nil, eventElements...),
		),
	)

	w.Header().Set("Content-Type", "text/html")
	if _, err := fmt.Fprint(w, ws.renderPage("Tasmota HomeKit", content)); err != nil {
		ws.logger.Error("Failed to write response", slog.Any("error", err))
	}
}

// HandleToggle handles plug toggle requests
func (ws *WebServer) HandleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract plug ID from path
	path := strings.TrimPrefix(r.URL.Path, "/toggle/")
	plugID := path

	plug, state, exists := ws.plugProvider.Plug(plugID)
	if !exists {
		http.Error(w, "Plug not found", http.StatusNotFound)
		return
	}

	action := r.FormValue("action")
	on := action == "on"

	ws.commands <- plugs.CommandEvent{
		PlugID: plugID,
		On:     on,
	}

	ws.publishCommand(plugID, on)

	ws.LogEvent(fmt.Sprintf("Web UI: Toggle %s → %v", plugID, on))

	// If HTMX request, return partial HTML
	if r.Header.Get("HX-Request") == "true" {
		// Wait a moment for the state to update
		time.Sleep(100 * time.Millisecond)

		if updatedPlug, updatedState, ok := ws.plugProvider.Plug(plugID); ok {
			plug = updatedPlug
			state = updatedState
		}

		w.Header().Set("Content-Type", "text/html")
		if _, err := fmt.Fprint(w, ws.renderPlugCard(plugID, plug, state).Render()); err != nil {
			ws.logger.Error("Failed to write response", slog.Any("error", err))
		}
		return
	}

	// Redirect back to index for non-HTMX requests
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleEventBusDebug renders a simple diagnostic view of the current state map.
func (ws *WebServer) HandleEventBusDebug(w http.ResponseWriter, r *http.Request) {
	snapshot := ws.snapshotState()

	ws.sseClientsMu.RLock()
	clientCount := len(ws.sseClients)
	ws.sseClientsMu.RUnlock()

	rows := []elem.Node{
		elem.Tr(nil,
			elem.Th(nil, elem.Text("Plug ID")),
			elem.Th(nil, elem.Text("Name")),
			elem.Th(nil, elem.Text("On")),
			elem.Th(nil, elem.Text("Last Updated")),
			elem.Th(nil, elem.Text("Last Seen")),
			elem.Th(nil, elem.Text("Connection")),
		),
	}

	for _, evt := range snapshot {
		rows = append(rows,
			elem.Tr(nil,
				elem.Td(nil, elem.Text(evt.PlugID)),
				elem.Td(nil, elem.Text(evt.Name)),
				elem.Td(nil, elem.Text(fmt.Sprintf("%t", evt.On))),
				elem.Td(nil, elem.Text(evt.LastUpdated.Format(time.RFC3339))),
				elem.Td(nil, elem.Text(evt.LastSeen.Format(time.RFC3339))),
				elem.Td(nil, elem.Text(evt.ConnectionNote)),
			),
		)
	}

	statusRows := []elem.Node{
		elem.Tr(nil,
			elem.Th(nil, elem.Text("Component")),
			elem.Th(nil, elem.Text("Status")),
			elem.Th(nil, elem.Text("Updated")),
			elem.Th(nil, elem.Text("Error")),
		),
	}

	for _, status := range ws.snapshotStatuses() {
		statusRows = append(statusRows,
			elem.Tr(nil,
				elem.Td(nil, elem.Text(status.Component)),
				elem.Td(nil, elem.Text(string(status.Status))),
				elem.Td(nil, elem.Text(status.Timestamp.Format(time.RFC3339))),
				elem.Td(nil, elem.Text(status.Error)),
			),
		)
	}

	content := elem.Div(nil,
		elem.H1(nil, elem.Text("EventBus Debug")),
		elem.P(nil, elem.Text(fmt.Sprintf("Connected SSE clients: %d", clientCount))),
		elem.Table(attrs.Props{"border": "1", "cellpadding": "4", "cellspacing": "0"}, rows...),
		elem.H2(nil, elem.Text("Component Status")),
		elem.Table(attrs.Props{"border": "1", "cellpadding": "4", "cellspacing": "0"}, statusRows...),
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := fmt.Fprint(w, ws.renderPage("EventBus Debug", content)); err != nil {
		ws.logger.Error("Failed to write eventbus debug response", slog.Any("error", err))
	}
}

// HandleSSE streams JSON state updates to clients.
func (ws *WebServer) HandleSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan events.StateUpdateEvent, 10)

	ws.sseClientsMu.Lock()
	ws.sseClients[clientChan] = struct{}{}
	ws.sseClientsMu.Unlock()

	defer func() {
		ws.sseClientsMu.Lock()
		delete(ws.sseClients, clientChan)
		ws.sseClientsMu.Unlock()
		close(clientChan)
	}()

	// Send current snapshot immediately.
	for _, evt := range ws.snapshotState() {
		select {
		case clientChan <- evt:
		default:
		}
	}

	for {
		select {
		case evt := <-clientChan:
			payload, err := json.Marshal(evt)
			if err != nil {
				ws.logger.Error("Failed to marshal SSE payload", slog.Any("error", err))
				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()

		case <-r.Context().Done():
			return
		case <-ws.ctx.Done():
			return
		}
	}
}

// HandleHealth exposes a JSON health summary that matches nefit-homekit.
func (ws *WebServer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snapshot := ws.plugProvider.Snapshot()

	ws.sseClientsMu.RLock()
	sseClients := len(ws.sseClients)
	ws.sseClientsMu.RUnlock()

	resp := struct {
		Status     string    `json:"status"`
		Plugs      int       `json:"plugs"`
		SSEClients int       `json:"sse_clients"`
		Timestamp  time.Time `json:"timestamp"`
	}{
		Status:     "ok",
		Plugs:      len(snapshot),
		SSEClients: sseClients,
		Timestamp:  time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		ws.logger.Error("Failed to write health response", slog.Any("error", err))
	}
}

// HandleQRCode renders the current HomeKit QR code for terminal access.
func (ws *WebServer) HandleQRCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if ws.qrCode == "" {
		if _, err := fmt.Fprintf(w, "HomeKit PIN: %s\nQR code is not available on this host.\n", ws.hapPin); err != nil {
			ws.logger.Error("failed to render QR fallback", slog.Any("error", err))
		}
		return
	}

	if _, err := fmt.Fprintf(w, "HomeKit PIN: %s\n\n%s\n", ws.hapPin, ws.qrCode); err != nil {
		ws.logger.Error("failed to render QR code", slog.Any("error", err))
	}
}

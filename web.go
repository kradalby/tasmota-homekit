package tasmotahomekit

import (
	"context"
	_ "embed"
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

//go:embed assets/style.css
var cssContent string

//go:embed assets/script.js
var jsContent string

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
	page := elem.Html(nil,
		elem.Head(nil,
			elem.Meta(attrs.Props{attrs.Charset: "utf-8"}),
			elem.Meta(attrs.Props{attrs.Name: "viewport", attrs.Content: "width=device-width, initial-scale=1"}),
			elem.Title(nil, elem.Text(title)),
			elem.Script(attrs.Props{
				attrs.Src: "https://unpkg.com/htmx.org@2.0.4",
			}),
			elem.Style(nil, elem.Text(cssContent)),
			elem.Script(nil, elem.Text(jsContent)),
		),
		elem.Body(nil, content),
	)
	return page.Render()
}

// renderPlugCard renders a single plug card element
func (ws *WebServer) renderPlugCard(plugID string, info plugs.Plug, state plugs.State) elem.Node {
	statusClass := "off"
	statusText := "OFF"
	buttonClass := "on" // Green for Turn On
	buttonText := "Turn On"
	buttonAction := "on"

	if state.On {
		statusClass = "on"
		statusText = "ON"
		buttonClass = "off" // Red for Turn Off
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

	// Build electrical stats section if power monitoring is enabled
	var statsSection elem.Node
	if info.Features.PowerMonitoring {
		statsSection = elem.Div(attrs.Props{attrs.Class: "electrical-stats"},
			elem.Div(attrs.Props{attrs.Class: "stat-item"},
				elem.Span(attrs.Props{attrs.Class: "stat-label"}, elem.Text("Power:")),
				elem.Span(attrs.Props{attrs.Class: "stat-value", "data-role": "power-value"},
					elem.Text(fmt.Sprintf("%.1f W", state.Power)),
				),
			),
			elem.Div(attrs.Props{attrs.Class: "stat-item"},
				elem.Span(attrs.Props{attrs.Class: "stat-label"}, elem.Text("Voltage:")),
				elem.Span(attrs.Props{attrs.Class: "stat-value", "data-role": "voltage-value"},
					elem.Text(fmt.Sprintf("%.1f V", state.Voltage)),
				),
			),
			elem.Div(attrs.Props{attrs.Class: "stat-item"},
				elem.Span(attrs.Props{attrs.Class: "stat-label"}, elem.Text("Current:")),
				elem.Span(attrs.Props{attrs.Class: "stat-value", "data-role": "current-value"},
					elem.Text(fmt.Sprintf("%.2f A", state.Current)),
				),
			),
			elem.Div(attrs.Props{attrs.Class: "stat-item"},
				elem.Span(attrs.Props{attrs.Class: "stat-label"}, elem.Text("Energy:")),
				elem.Span(attrs.Props{attrs.Class: "stat-value", "data-role": "energy-value"},
					elem.Text(fmt.Sprintf("%.3f kWh", state.Energy)),
				),
			),
		)
	}

	// Icon selection
	icon := "🔌" // Default plug icon
	if info.Type == "bulb" {
		icon = "💡"
	}

	return elem.Div(
		attrs.Props{
			attrs.ID:       "plug-" + plugID,
			attrs.Class:    "plug " + statusClass,
			"data-plug-id": plugID,
		},
		elem.Div(attrs.Props{attrs.Class: "plug-header"},
			elem.Div(attrs.Props{attrs.Class: "plug-icon"}, elem.Text(icon)),
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
		),
		statsSection,
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
		// Skip plugs that are not enabled for Web
		if item.Plug.Web != nil && !*item.Plug.Web {
			continue
		}
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
		var qrContent []elem.Node
		qrContent = append(qrContent,
			elem.Div(attrs.Props{attrs.Class: "homekit-pin"},
				elem.Span(attrs.Props{attrs.Class: "homekit-pin-label"}, elem.Text("Setup PIN")),
				elem.Span(attrs.Props{attrs.Class: "homekit-pin-value"}, elem.Text(ws.hapPin)),
			),
		)

		if ws.qrCode != "" {
			qrContent = append(qrContent,
				elem.Div(attrs.Props{attrs.Class: "qr-code-block"},
					elem.Pre(attrs.Props{attrs.Class: "qr-code"}, elem.Text(ws.qrCode)),
				),
				elem.P(attrs.Props{attrs.Class: "homekit-instructions"},
					elem.Text("Scan the QR code from the Home app or camera on your iPhone/iPad."),
				),
			)
		} else {
			qrContent = append(qrContent,
				elem.P(attrs.Props{attrs.Class: "homekit-instructions"},
					elem.Text("QR code is not available on this host. Use the PIN above in the Home app."),
				),
			)
		}

		qrContent = append(qrContent,
			elem.P(attrs.Props{attrs.Class: "homekit-instructions"},
				elem.Text("Home app → Add Accessory → More Options → Select \"Tasmota Bridge\"."),
			),
			elem.A(attrs.Props{attrs.Href: "/qrcode", attrs.Class: "homekit-link"}, elem.Text("Open standalone QR view")),
		)

		homekitSection = elem.Details(attrs.Props{attrs.Class: "homekit-banner"},
			elem.Summary(nil,
				elem.Span(attrs.Props{attrs.Class: "homekit-summary-title"}, elem.Text("HomeKit Pairing")),
				elem.Span(attrs.Props{attrs.Class: "homekit-summary-caption"}, elem.Text("Tap to reveal setup PIN & QR code")),
			),
			elem.Div(attrs.Props{attrs.Class: "homekit-banner-content"}, qrContent...),
		)
	}

	content := elem.Div(nil,
		elem.H1(nil, elem.Text("Tasmota HomeKit Bridge")),
		elem.P(nil, elem.Text(fmt.Sprintf("Managing %d plugs", len(snapshot)))),
		homekitSection,
		elem.Div(attrs.Props{attrs.Class: "plugs-grid"}, plugElements...),
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

	// Check if plug is enabled for Web
	if plug.Web != nil && !*plug.Web {
		http.Error(w, "Plug not available on web", http.StatusNotFound)
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

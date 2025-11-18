package tasmotahomekit

import (
	"context"
	"encoding/json"
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
	logger          *slog.Logger
	kraweb          *web.KraWeb
	plugProvider    plugStateProvider
	commands        chan plugs.CommandEvent
	events          []string
	sseClients      map[chan string]struct{}
	sseClientsMu    sync.RWMutex
	stateSubscriber *eventbus.Subscriber[plugs.StateChangedEvent]
	hapPin          string
	qrCode          string
}

// NewWebServer creates a new web server
func NewWebServer(logger *slog.Logger, plugProvider plugStateProvider, commands chan plugs.CommandEvent, bus *eventbus.Bus, kraweb *web.KraWeb, hapPin, qrCode string) *WebServer {
	client := bus.Client("webserver")

	return &WebServer{
		logger:          logger,
		kraweb:          kraweb,
		plugProvider:    plugProvider,
		commands:        commands,
		events:          make([]string, 0, 100),
		sseClients:      make(map[chan string]struct{}),
		stateSubscriber: eventbus.Subscribe[plugs.StateChangedEvent](client),
		hapPin:          hapPin,
		qrCode:          qrCode,
	}
}

// LogEvent adds an event to the log
func (ws *WebServer) LogEvent(event string) {
	ws.events = append(ws.events, fmt.Sprintf("%s: %s", time.Now().Format("15:04:05"), event))
	if len(ws.events) > 100 {
		ws.events = ws.events[1:]
	}
}

// broadcastSSE sends a message to all connected SSE clients
func (ws *WebServer) broadcastSSE(message string) {
	ws.sseClientsMu.RLock()
	defer ws.sseClientsMu.RUnlock()

	for client := range ws.sseClients {
		select {
		case client <- message:
		default:
			// Client channel is full, skip
		}
	}
}

func (ws *WebServer) Start(ctx context.Context) {
	go ws.processStateChanges(ctx)

	go func() {
		ws.logger.Info("Starting web interface")
		if err := ws.kraweb.ListenAndServe(ctx); err != nil {
			ws.logger.Error("Web server error", slog.Any("error", err))
		}
	}()
}

func (ws *WebServer) Close() {
	ws.stateSubscriber.Close()

	ws.sseClientsMu.Lock()
	for client := range ws.sseClients {
		close(client)
	}
	ws.sseClients = make(map[chan string]struct{})
	ws.sseClientsMu.Unlock()
}

func (ws *WebServer) processStateChanges(ctx context.Context) {
	for {
		select {
		case event := <-ws.stateSubscriber.Events():
			ws.logger.Debug("Web UI: State change received", "plug_id", event.PlugID, "on", event.State.On)
			ws.broadcastSSE(event.PlugID)
		case <-ctx.Done():
			return
		}
	}
}

// renderPage renders a basic HTML page
func (ws *WebServer) renderPage(title string, content elem.Node) string {
	page := elem.Html(nil,
		elem.Head(nil,
			elem.Title(nil, elem.Text(title)),
			elem.Script(attrs.Props{
				attrs.Src: "https://unpkg.com/htmx.org@2.0.4",
			}),
			elem.Script(attrs.Props{
				attrs.Src: "https://unpkg.com/htmx-ext-sse@2.2.2/sse.js",
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
			attrs.ID:    "plug-" + plugID,
			attrs.Class: "plug " + statusClass,
			"sse-swap":  plugID,
			"hx-swap":   "outerHTML",
		},
		elem.Div(attrs.Props{attrs.Class: "plug-info"},
			elem.Div(attrs.Props{attrs.Class: "plug-name"}, elem.Text(info.Name)),
			elem.Div(attrs.Props{attrs.Class: "plug-status"},
				elem.Text(fmt.Sprintf("Status: %s | Last updated: %s",
					statusText,
					state.LastUpdated.Format("15:04:05"),
				)),
			),
			elem.Div(attrs.Props{attrs.Class: "connection-status"},
				elem.Span(attrs.Props{attrs.Class: "connection-indicator " + connectionIndicator}),
				elem.Text(connectionText),
			),
		),
		elem.Form(
			attrs.Props{
				"hx-post":   "/toggle/" + plugID,
				"hx-target": "#plug-" + plugID,
				"hx-swap":   "outerHTML",
			},
			elem.Input(attrs.Props{attrs.Type: "hidden", attrs.Name: "action", attrs.Value: buttonAction}),
			elem.Button(
				attrs.Props{attrs.Type: "submit", attrs.Class: buttonClass},
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
	for i := len(ws.events) - 1; i >= 0 && i >= len(ws.events)-20; i-- {
		eventElements = append(eventElements, elem.Div(attrs.Props{attrs.Class: "event"}, elem.Text(ws.events[i])))
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
		elem.Div(
			attrs.Props{
				"hx-ext":      "sse",
				"sse-connect": "/events",
			},
			plugElements...,
		),
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

// HandleSSE handles Server-Sent Events for real-time updates
func (ws *WebServer) HandleSSE(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Create a channel for this client
	clientChan := make(chan string, 10)

	// Register the client
	ws.sseClientsMu.Lock()
	ws.sseClients[clientChan] = struct{}{}
	ws.sseClientsMu.Unlock()

	// Ensure cleanup on disconnect
	defer func() {
		ws.sseClientsMu.Lock()
		delete(ws.sseClients, clientChan)
		ws.sseClientsMu.Unlock()
		close(clientChan)
	}()

	// Get flusher for SSE
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Send events to this client
	for {
		select {
		case plugID := <-clientChan:
			plug, state, ok := ws.plugProvider.Plug(plugID)
			if !ok {
				continue
			}

			html := ws.renderPlugCard(plugID, plug, state).Render()

			if _, err := fmt.Fprintf(w, "event: %s\n", plugID); err != nil {
				ws.logger.Error("Failed to write SSE event", slog.Any("error", err))
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", html); err != nil {
				ws.logger.Error("Failed to write SSE data", slog.Any("error", err))
				return
			}
			flusher.Flush()

		case <-r.Context().Done():
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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chasefleming/elem-go"
	"github.com/chasefleming/elem-go/attrs"
	"tailscale.com/util/eventbus"
)

// WebServer manages the web UI
type WebServer struct {
	plugManager     *PlugManager
	commands        chan PlugCommandEvent
	events          []string // Simple event log for debugging
	sseClients      map[chan string]struct{}
	sseClientsMu    sync.RWMutex
	stateSubscriber *eventbus.Subscriber[PlugStateChangedEvent]
}

// NewWebServer creates a new web server
func NewWebServer(plugManager *PlugManager, commands chan PlugCommandEvent, bus *eventbus.Bus) *WebServer {
	client := bus.Client("webserver")

	return &WebServer{
		plugManager:     plugManager,
		commands:        commands,
		events:          make([]string, 0, 100),
		sseClients:      make(map[chan string]struct{}),
		stateSubscriber: eventbus.Subscribe[PlugStateChangedEvent](client),
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

// ProcessStateChanges listens for state changes and broadcasts them via SSE
func (ws *WebServer) ProcessStateChanges(ctx context.Context) {
	for {
		select {
		case event := <-ws.stateSubscriber.Events():
			slog.Debug("Web UI: State change received", "plug_id", event.PlugID, "on", event.State.On)
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
				.plug-name { font-size: 1.2em; font-weight: 500; }
				.plug-status { font-size: 0.9em; color: #666; }
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
func (ws *WebServer) renderPlugCard(plugID string, info *PlugInfo, state *PlugState) elem.Node {
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

	return elem.Div(
		attrs.Props{
			attrs.ID:    "plug-" + plugID,
			attrs.Class: "plug " + statusClass,
			"sse-swap":  plugID,
			"hx-swap":   "outerHTML",
		},
		elem.Div(nil,
			elem.Div(attrs.Props{attrs.Class: "plug-name"}, elem.Text(info.Config.Name)),
			elem.Div(attrs.Props{attrs.Class: "plug-status"},
				elem.Text(fmt.Sprintf("Status: %s | Last updated: %s",
					statusText,
					state.LastUpdated.Format("15:04:05"),
				)),
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

	ws.plugManager.statesMu.RLock()
	for id, info := range ws.plugManager.plugs {
		state := ws.plugManager.states[id]
		plugElements = append(plugElements, ws.renderPlugCard(id, info, state))
	}
	ws.plugManager.statesMu.RUnlock()

	// Add event log
	var eventElements []elem.Node
	for i := len(ws.events) - 1; i >= 0 && i >= len(ws.events)-20; i-- {
		eventElements = append(eventElements, elem.Div(attrs.Props{attrs.Class: "event"}, elem.Text(ws.events[i])))
	}

	content := elem.Div(nil,
		elem.H1(nil, elem.Text("Tasmota HomeKit Bridge")),
		elem.P(nil, elem.Text(fmt.Sprintf("Managing %d plugs", len(ws.plugManager.plugs)))),
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
		slog.Error("Failed to write response", "error", err)
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

	info, exists := ws.plugManager.plugs[plugID]
	if !exists {
		http.Error(w, "Plug not found", http.StatusNotFound)
		return
	}

	action := r.FormValue("action")
	on := action == "on"

	ws.commands <- PlugCommandEvent{
		PlugID: plugID,
		On:     on,
	}

	ws.LogEvent(fmt.Sprintf("Web UI: Toggle %s → %v", plugID, on))

	// If HTMX request, return partial HTML
	if r.Header.Get("HX-Request") == "true" {
		// Wait a moment for the state to update
		time.Sleep(100 * time.Millisecond)

		ws.plugManager.statesMu.RLock()
		state := ws.plugManager.states[plugID]
		ws.plugManager.statesMu.RUnlock()

		w.Header().Set("Content-Type", "text/html")
		if _, err := fmt.Fprint(w, ws.renderPlugCard(plugID, info, state).Render()); err != nil {
			slog.Error("Failed to write response", "error", err)
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
			// Get current state
			ws.plugManager.statesMu.RLock()
			info, infoExists := ws.plugManager.plugs[plugID]
			state, stateExists := ws.plugManager.states[plugID]
			ws.plugManager.statesMu.RUnlock()

			if !infoExists || !stateExists {
				continue
			}

			// Render the plug card
			html := ws.renderPlugCard(plugID, info, state).Render()

			// Send SSE event with the plug ID as the event name
			if _, err := fmt.Fprintf(w, "event: %s\n", plugID); err != nil {
				slog.Error("Failed to write SSE event", "error", err)
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", html); err != nil {
				slog.Error("Failed to write SSE data", "error", err)
				return
			}
			flusher.Flush()

		case <-r.Context().Done():
			// Client disconnected
			return
		}
	}
}

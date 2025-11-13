package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/chasefleming/elem-go"
	"github.com/chasefleming/elem-go/attrs"
)

// WebServer manages the web UI
type WebServer struct {
	plugManager *PlugManager
	commands    chan PlugCommandEvent
	events      []string // Simple event log for debugging
}

// NewWebServer creates a new web server
func NewWebServer(plugManager *PlugManager, commands chan PlugCommandEvent) *WebServer {
	return &WebServer{
		plugManager: plugManager,
		commands:    commands,
		events:      make([]string, 0, 100),
	}
}

// LogEvent adds an event to the log
func (ws *WebServer) LogEvent(event string) {
	ws.events = append(ws.events, fmt.Sprintf("%s: %s", time.Now().Format("15:04:05"), event))
	if len(ws.events) > 100 {
		ws.events = ws.events[1:]
	}
}

// renderPage renders a basic HTML page
func (ws *WebServer) renderPage(title string, content elem.Node) string {
	page := elem.Html(nil,
		elem.Head(nil,
			elem.Title(nil, elem.Text(title)),
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

// HandleIndex renders the main dashboard
func (ws *WebServer) HandleIndex(w http.ResponseWriter, r *http.Request) {
	var plugElements []elem.Node

	ws.plugManager.statesMu.RLock()
	for id, info := range ws.plugManager.plugs {
		state := ws.plugManager.states[id]

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

		plugDiv := elem.Div(
			attrs.Props{attrs.Class: "plug " + statusClass},
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
				attrs.Props{attrs.Method: "POST", attrs.Action: "/toggle/" + id},
				elem.Input(attrs.Props{attrs.Type: "hidden", attrs.Name: "action", attrs.Value: buttonAction}),
				elem.Button(
					attrs.Props{attrs.Type: "submit", attrs.Class: buttonClass},
					elem.Text(buttonText),
				),
			),
		)
		plugElements = append(plugElements, plugDiv)
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
		elem.Div(nil, plugElements...),
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

	if _, exists := ws.plugManager.plugs[plugID]; !exists {
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

	// Redirect back to index
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

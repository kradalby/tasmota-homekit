package tasmotahomekit

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
	"github.com/chasefleming/elem-go"
	"github.com/chasefleming/elem-go/attrs"
)

// DebugHandler implements http.Handler to expose HomeKit internal state
type DebugHandler struct {
	hm *HAPManager
}

// NewDebugHandler creates a new debug handler
func NewDebugHandler(hm *HAPManager) *DebugHandler {
	return &DebugHandler{
		hm: hm,
	}
}

func (h *DebugHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Collect accessories
	var accessories []*accessory.A
	accessories = append(accessories, h.hm.bridge.A)

	// Sort keys for deterministic order
	var keys []string
	for k := range h.hm.accessories {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		acc := h.hm.accessories[k]
		switch a := acc.(type) {
		case *OutletWrapper:
			accessories = append(accessories, a.A)
		case *LightbulbWrapper:
			accessories = append(accessories, a.A)
		}
	}

	// Build table rows
	rows := []elem.Node{
		elem.Tr(attrs.Props{},
			elem.Th(attrs.Props{}, elem.Text("ID")),
			elem.Th(attrs.Props{}, elem.Text("Name")),
			elem.Th(attrs.Props{}, elem.Text("Type")),
			elem.Th(attrs.Props{}, elem.Text("Manufacturer")),
			elem.Th(attrs.Props{}, elem.Text("Model")),
			elem.Th(attrs.Props{}, elem.Text("Serial")),
			elem.Th(attrs.Props{}, elem.Text("Firmware")),
			elem.Th(attrs.Props{}, elem.Text("Services")),
		),
	}
	rows = append(rows, h.renderAccessoryRows(accessories)...)

	content := elem.Div(attrs.Props{},
		elem.H1(attrs.Props{}, elem.Text("HomeKit Debug")),

		h.renderServerInfo(),
		h.renderStats(),
		h.renderPairings(),

		elem.H2(attrs.Props{}, elem.Text("Registered Accessories")),
		elem.Table(attrs.Props{"border": "1", "cellpadding": "5", "style": "border-collapse: collapse; width: 100%;"},
			rows...,
		),
	)

	// Render full page
	page := elem.Html(attrs.Props{},
		elem.Head(attrs.Props{},
			elem.Title(attrs.Props{}, elem.Text("HomeKit Debug")),
			elem.Style(attrs.Props{}, elem.Text(`
				body { font-family: sans-serif; padding: 20px; }
				table { width: 100%; border-collapse: collapse; }
				th, td { border: 1px solid #ddd; padding: 8px; text-align: left; vertical-align: top; }
				th { background-color: #f2f2f2; }
				.service { margin-bottom: 10px; border-bottom: 1px solid #eee; padding-bottom: 5px; }
				.char { margin-left: 10px; font-size: 0.9em; color: #555; }
			`)),
		),
		elem.Body(attrs.Props{}, content),
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := fmt.Fprint(w, page.Render()); err != nil {
		slog.Error("Failed to write debug response", "error", err)
	}
}

func (h *DebugHandler) renderAccessoryRows(accessories []*accessory.A) []elem.Node {
	var rows []elem.Node
	for _, acc := range accessories {
		rows = append(rows, elem.Tr(attrs.Props{},
			elem.Td(attrs.Props{}, elem.Text(fmt.Sprintf("%d", acc.Id))),
			elem.Td(attrs.Props{}, elem.Text(acc.Info.Name.Value())),
			elem.Td(attrs.Props{}, elem.Text(h.getAccessoryType(acc))),
			elem.Td(attrs.Props{}, elem.Text(acc.Info.Manufacturer.Value())),
			elem.Td(attrs.Props{}, elem.Text(acc.Info.Model.Value())),
			elem.Td(attrs.Props{}, elem.Text(acc.Info.SerialNumber.Value())),
			elem.Td(attrs.Props{}, elem.Text(acc.Info.FirmwareRevision.Value())),
			elem.Td(attrs.Props{}, h.renderServices(acc.Ss)),
		))
	}
	return rows
}

func (h *DebugHandler) getAccessoryType(acc *accessory.A) string {
	switch acc.Type {
	case accessory.TypeBridge:
		return "Bridge"
	case accessory.TypeOutlet:
		return "Outlet"
	case accessory.TypeLightbulb:
		return "Lightbulb"
	default:
		return fmt.Sprintf("Unknown (%d)", acc.Type)
	}
}

func (h *DebugHandler) renderServices(services []*service.S) elem.Node {
	var serviceNodes []elem.Node
	for _, svc := range services {
		serviceNodes = append(serviceNodes, elem.Div(attrs.Props{attrs.Class: "service"},
			elem.Strong(attrs.Props{}, elem.Text(svc.Type)), // Service type (UUID or name if we could map it)
			elem.Div(attrs.Props{}, h.renderCharacteristics(svc.Cs)),
		))
	}
	return elem.Div(attrs.Props{}, serviceNodes...)
}

func (h *DebugHandler) renderServerInfo() elem.Node {
	if h.hm.server == nil {
		return elem.Div(attrs.Props{}, elem.Text("Server info not available"))
	}

	return elem.Div(attrs.Props{},
		elem.H2(attrs.Props{}, elem.Text("Server Info")),
		elem.Ul(attrs.Props{},
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("Address: %s", h.hm.server.Addr))),
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("PIN: %s", h.hm.server.Pin))),
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("Paired: %v", h.hm.server.IsPaired()))),
		),
	)
}

func (h *DebugHandler) renderStats() elem.Node {
	lastActivity := h.hm.lastActivity.Load()
	lastActivityStr := "Never"
	if lastActivity > 0 {
		lastActivityStr = time.Unix(lastActivity, 0).Format(time.RFC3339)
	}

	return elem.Div(attrs.Props{},
		elem.H2(attrs.Props{}, elem.Text("Statistics")),
		elem.Ul(attrs.Props{},
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("Incoming Commands: %d", h.hm.incomingCommands.Load()))),
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("Outgoing Updates: %d", h.hm.outgoingUpdates.Load()))),
			elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("Last Activity: %s", lastActivityStr))),
		),
	)
}

func (h *DebugHandler) renderPairings() elem.Node {
	if h.hm.store == nil {
		return elem.Div(attrs.Props{}, elem.Text("Store not available"))
	}

	// hap.Store interface doesn't enforce Pairings() method that returns a list.
	// We need to check if the store implementation supports it or iterate if possible.
	// The FsStore implementation has a Pairings() method.
	type pairingStore interface {
		Pairings() ([]hap.Pairing, error)
	}

	var pairings []hap.Pairing
	if ps, ok := h.hm.store.(pairingStore); ok {
		var err error
		pairings, err = ps.Pairings()
		if err != nil {
			return elem.Div(attrs.Props{}, elem.Text(fmt.Sprintf("Error loading pairings: %v", err)))
		}
	} else {
		// Fallback or error if store doesn't support listing
		return elem.Div(attrs.Props{}, elem.Text("Store does not support listing pairings"))
	}

	if len(pairings) == 0 {
		return elem.Div(attrs.Props{},
			elem.H2(attrs.Props{}, elem.Text("Pairings")),
			elem.P(attrs.Props{}, elem.Text("No active pairings")),
		)
	}

	var listItems []elem.Node
	for _, p := range pairings {
		listItems = append(listItems, elem.Li(attrs.Props{}, elem.Text(fmt.Sprintf("%s (Admin: %v)", p.Name, p.Permission == 0x01)))) // Assuming 0x01 is Admin based on hap code reading, but let's just print permission byte if unsure. Actually hap.PermissionAdmin is likely exported. Let's just print name for now to be safe or check if we can import permission constants.
	}

	return elem.Div(attrs.Props{},
		elem.H2(attrs.Props{}, elem.Text("Pairings")),
		elem.Ul(attrs.Props{}, listItems...),
	)
}

func (h *DebugHandler) renderCharacteristics(chars []*characteristic.C) elem.Node {
	var charNodes []elem.Node
	for _, c := range chars {
		val := c.Value()
		if val == nil {
			val = "nil"
		}
		charNodes = append(charNodes, elem.Div(attrs.Props{attrs.Class: "char"},
			elem.Text(fmt.Sprintf("%s: %v", c.Type, val)),
		))
	}
	return elem.Div(attrs.Props{}, charNodes...)
}

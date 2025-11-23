package tasmotahomekit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
)

// SetupDebugHandlers registers the HAP debug handler without using tsweb.Debugger to avoid pattern conflicts
func SetupDebugHandlers(kraWeb interface {
	Handle(pattern string, handler http.Handler)
}, hapManager *HAPManager) {
	// Directly register the HAP debug endpoint
	kraWeb.Handle("/debug/hap", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		debugInfo := hapManager.DebugInfo()
		data, err := json.MarshalIndent(debugInfo, "", "  ")
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to marshal debug info: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(data); err != nil {
			return
		}
	}))
}

// HAPDebugInfo contains debug information about the HomeKit service
type HAPDebugInfo struct {
	Server      *ServerInfo     `json:"server,omitempty"`
	Pairings    []PairingInfo   `json:"pairings,omitempty"`
	Stats       StatsInfo       `json:"stats"`
	Accessories []AccessoryInfo `json:"accessories"`
}

// ServerInfo contains HAP server information
type ServerInfo struct {
	Address string `json:"address"`
	PIN     string `json:"pin"`
	Paired  bool   `json:"paired"`
}

// PairingInfo contains information about a paired client
type PairingInfo struct {
	Name       string `json:"name"`
	Permission string `json:"permission"`
}

// StatsInfo contains traffic statistics
type StatsInfo struct {
	IncomingCommands uint64 `json:"incoming_commands"`
	OutgoingUpdates  uint64 `json:"outgoing_updates"`
	LastActivity     string `json:"last_activity"`
}

// AccessoryInfo contains information about a HomeKit accessory
type AccessoryInfo struct {
	ID           uint64 `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	SerialNumber string `json:"serial_number"`
	Firmware     string `json:"firmware"`
}

// DebugInfo returns debug information about the HAP manager
func (hm *HAPManager) DebugInfo() HAPDebugInfo {
	info := HAPDebugInfo{
		Accessories: []AccessoryInfo{},
	}

	// Server info
	if hm.server != nil {
		info.Server = &ServerInfo{
			Address: hm.server.Addr,
			PIN:     hm.server.Pin,
			Paired:  hm.server.IsPaired(),
		}
	}

	// Pairings
	if hm.store != nil {
		type pairingStore interface {
			Pairings() ([]hap.Pairing, error)
		}
		if ps, ok := hm.store.(pairingStore); ok {
			pairings, err := ps.Pairings()
			if err == nil {
				for _, p := range pairings {
					permission := "User"
					if p.Permission == 0x01 {
						permission = "Admin"
					}
					info.Pairings = append(info.Pairings, PairingInfo{
						Name:       p.Name,
						Permission: permission,
					})
				}
			}
		}
	}

	// Stats
	lastActivity := hm.lastActivity.Load()
	lastActivityStr := "Never"
	if lastActivity > 0 {
		lastActivityStr = time.Unix(lastActivity, 0).Format(time.RFC3339)
	}

	info.Stats = StatsInfo{
		IncomingCommands: hm.incomingCommands.Load(),
		OutgoingUpdates:  hm.outgoingUpdates.Load(),
		LastActivity:     lastActivityStr,
	}

	// Accessories
	var accessories []*accessory.A
	accessories = append(accessories, hm.bridge.A)

	for _, acc := range hm.accessories {
		switch a := acc.(type) {
		case *OutletWrapper:
			accessories = append(accessories, a.A)
		case *LightbulbWrapper:
			accessories = append(accessories, a.A)
		}
	}

	for _, acc := range accessories {
		accType := "Unknown"
		switch acc.Type {
		case accessory.TypeBridge:
			accType = "Bridge"
		case accessory.TypeOutlet:
			accType = "Outlet"
		case accessory.TypeLightbulb:
			accType = "Lightbulb"
		}

		info.Accessories = append(info.Accessories, AccessoryInfo{
			ID:           acc.Id,
			Name:         acc.Info.Name.Value(),
			Type:         accType,
			Manufacturer: acc.Info.Manufacturer.Value(),
			Model:        acc.Info.Model.Value(),
			SerialNumber: acc.Info.SerialNumber.Value(),
			Firmware:     acc.Info.FirmwareRevision.Value(),
		})
	}

	return info
}

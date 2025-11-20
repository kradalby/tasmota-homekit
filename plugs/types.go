package plugs

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/tailscale/hujson"
)

// Config defines the plug configuration file structure.
type Config struct {
	Plugs []Plug `json:"plugs"`
}

// LoadConfig reads and validates the HuJSON plug configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugs config file: %w", err)
	}

	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("failed to standardize HuJSON: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(standardized, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal plugs config: %w", err)
	}

	if len(cfg.Plugs) == 0 {
		return nil, fmt.Errorf("no plugs configured")
	}

	seenIDs := make(map[string]struct{}, len(cfg.Plugs))

	for i, plug := range cfg.Plugs {
		if plug.ID == "" {
			return nil, fmt.Errorf("plug %d has no ID", i)
		}
		if plug.Name == "" {
			return nil, fmt.Errorf("plug %s has no name", plug.ID)
		}
		if plug.Address == "" {
			return nil, fmt.Errorf("plug %s has no address", plug.ID)
		}
		if _, exists := seenIDs[plug.ID]; exists {
			return nil, fmt.Errorf("duplicate plug id %q", plug.ID)
		}
		seenIDs[plug.ID] = struct{}{}

		// Set defaults for HomeKit and Web if not specified
		if cfg.Plugs[i].HomeKit == nil {
			defaultTrue := true
			cfg.Plugs[i].HomeKit = &defaultTrue
		}
		if cfg.Plugs[i].Web == nil {
			defaultTrue := true
			cfg.Plugs[i].Web = &defaultTrue
		}
	}

	return &cfg, nil
}

// Plug describes a single Tasmota plug.
type Plug struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Address  string       `json:"address"`
	Model    string       `json:"model"`
	Features PlugFeatures `json:"features"`
	HomeKit  *bool        `json:"homekit,omitempty"`
	Web      *bool        `json:"web,omitempty"`
}

// PlugFeatures indicates optional features of a plug.
type PlugFeatures struct {
	PowerMonitoring bool `json:"power_monitoring"`
	EnergyTracking  bool `json:"energy_tracking"`
}

// State represents the runtime state of a plug.
type State struct {
	ID            string
	Name          string
	On            bool
	Power         float64 // Watts
	Voltage       float64 // Volts
	Current       float64 // Amperes
	Energy        float64 // kWh
	LastUpdated   time.Time
	MQTTConnected bool
	LastSeen      time.Time
}

// StateChangedEvent is emitted when a plug's state changes.
type StateChangedEvent struct {
	PlugID string
	State  State
}

// CommandEvent requests a plug command.
type CommandEvent struct {
	PlugID string
	On     bool
}

// ErrorEvent is emitted when a plug encounters an error.
type ErrorEvent struct {
	PlugID string
	Error  error
}

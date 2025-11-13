package main

import "time"

// Config represents the environment variable configuration
type Config struct {
	HAP struct {
		PIN         string `env:"TASMOTA_HOMEKIT_HAP_PIN,default=00102003"`
		Port        int    `env:"TASMOTA_HOMEKIT_HAP_PORT,default=8080"`
		StoragePath string `env:"TASMOTA_HOMEKIT_HAP_STORAGE_PATH,default=./data/hap"`
	}

	Web struct {
		Port int `env:"TASMOTA_HOMEKIT_WEB_PORT,default=8081"`
	}

	MQTT struct {
		Port int `env:"TASMOTA_HOMEKIT_MQTT_PORT,default=1883"`
	}

	Tailscale struct {
		Hostname string `env:"TASMOTA_HOMEKIT_TS_HOSTNAME"`
		AuthKey  string `env:"TASMOTA_HOMEKIT_TS_AUTHKEY"`
	}

	PlugsConfigPath string `env:"TASMOTA_HOMEKIT_PLUGS_CONFIG,default=./plugs.hujson"`
}

// PlugConfig represents the HuJSON plugs configuration file
type PlugConfig struct {
	Plugs []Plug `json:"plugs"`
}

// Plug represents a single Tasmota plug
type Plug struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Address  string       `json:"address"`
	Model    string       `json:"model"`
	Features PlugFeatures `json:"features"`
}

// PlugFeatures represents optional features of a plug
type PlugFeatures struct {
	PowerMonitoring bool `json:"power_monitoring"`
	EnergyTracking  bool `json:"energy_tracking"`
}

// PlugState represents the runtime state of a plug
type PlugState struct {
	ID            string
	Name          string
	On            bool
	Power         float64 // Watts
	Energy        float64 // kWh
	LastUpdated   time.Time
	MQTTConnected bool      // Is the plug currently connected to MQTT
	LastSeen      time.Time // Last time we received an MQTT message from this plug
}

// Event types for the event bus
type (
	// PlugStateChangedEvent is fired when a plug's state changes
	PlugStateChangedEvent struct {
		PlugID string
		State  PlugState
	}

	// PlugCommandEvent is fired when a command is requested for a plug
	PlugCommandEvent struct {
		PlugID string
		On     bool
	}

	// PlugErrorEvent is fired when a plug encounters an error
	PlugErrorEvent struct {
		PlugID string
		Error  error
	}
)

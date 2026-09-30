package events

import (
	"time"
)

// StateUpdateEvent carries plug state for SSE subscribers.
type StateUpdateEvent struct {
	Timestamp       time.Time `json:"timestamp"`
	Source          string    `json:"source"`
	PlugID          string    `json:"plug_id"`
	Name            string    `json:"name"`
	On              bool      `json:"on"`
	Power           float64   `json:"power"`
	Voltage         float64   `json:"voltage"`
	Current         float64   `json:"current"`
	Energy          float64   `json:"energy"`
	MQTTConnected   bool      `json:"mqtt_connected"`
	LastSeen        time.Time `json:"last_seen"`
	LastUpdated     time.Time `json:"last_updated"`
	ConnectionState string    `json:"connection_state"`
	ConnectionNote  string    `json:"connection_note"`
}

// CommandType represents supported plug commands.
type CommandType string

const (
	// CommandTypeSetPower toggles plug state via HTTP fast path.
	CommandTypeSetPower CommandType = "set_power"
)

// CommandEvent captures requested control actions for a plug.
type CommandEvent struct {
	Timestamp   time.Time   `json:"timestamp"`
	Source      string      `json:"source"`
	PlugID      string      `json:"plug_id"`
	CommandType CommandType `json:"command_type"`
	On          *bool       `json:"on,omitempty"`
}

// ConnectionStatusEvent conveys component lifecycle information (web, HAP, MQTT, etc.).
type ConnectionStatusEvent struct {
	Timestamp  time.Time        `json:"timestamp"`
	Component  string           `json:"component"`
	Status     ConnectionStatus `json:"status"`
	Error      string           `json:"error"`
	Reconnects int              `json:"reconnects"`
}

// ConnectionStatus represents lifecycle state for a component.
type ConnectionStatus string

const (
	ConnectionStatusDisconnected ConnectionStatus = "disconnected"
	ConnectionStatusConnecting   ConnectionStatus = "connecting"
	ConnectionStatusConnected    ConnectionStatus = "connected"
	ConnectionStatusReconnecting ConnectionStatus = "reconnecting"
	ConnectionStatusFailed       ConnectionStatus = "failed"
)

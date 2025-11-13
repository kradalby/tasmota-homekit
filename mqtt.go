package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// getLocalIP returns the local IP address to use for MQTT broker configuration
func getLocalIP() (string, error) {
	// Get all network interfaces
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	// Find first non-loopback IPv4 address
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no local IP address found")
}

// MQTTHook handles MQTT messages from Tasmota devices
type MQTTHook struct {
	mqtt.HookBase
	stateChanges chan PlugStateChangedEvent
	plugManager  *PlugManager
}

// ID returns the hook identifier
func (h *MQTTHook) ID() string {
	return "tasmota-mqtt-hook"
}

// Provides returns the hook methods this hook provides
func (h *MQTTHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnPublish,
		mqtt.OnPublished,
	}, []byte{b})
}

// OnPublish is called when a message is received from a client
func (h *MQTTHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	// Process messages from Tasmota devices
	topic := pk.TopicName
	payload := pk.Payload

	slog.Debug("MQTT message received",
		"topic", topic,
		"payload", string(payload),
	)

	// Parse topic to extract plug ID
	// Topics are typically: tele/tasmota/<plug-id>/STATE or stat/tasmota/<plug-id>/RESULT
	parts := strings.Split(topic, "/")
	if len(parts) < 3 {
		return pk, nil
	}

	// Extract plug ID from topic
	var plugID string
	if parts[0] == "tele" || parts[0] == "stat" {
		if len(parts) >= 3 {
			plugID = parts[2]
		}
	}

	if plugID == "" {
		return pk, nil
	}

	// Parse payload to extract state
	var msg map[string]interface{}
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Debug("Failed to parse MQTT payload", "error", err)
		return pk, nil
	}

	// Check for power state
	var powerState string
	if power, ok := msg["POWER"].(string); ok {
		powerState = power
	} else if result, ok := msg["StatusSTS"].(map[string]interface{}); ok {
		if power, ok := result["POWER"].(string); ok {
			powerState = power
		}
	}

	if powerState != "" {
		// Get current state from plug manager
		h.plugManager.statesMu.Lock()
		state, exists := h.plugManager.states[plugID]
		if exists {
			state.On = powerState == "ON"
			state.LastUpdated = time.Now()
			stateCopy := *state
			h.plugManager.statesMu.Unlock()

			h.stateChanges <- PlugStateChangedEvent{
				PlugID: plugID,
				State:  stateCopy,
			}

			slog.Info("Plug state updated from MQTT",
				"plug_id", plugID,
				"on", stateCopy.On,
			)
		} else {
			h.plugManager.statesMu.Unlock()
		}
	}

	return pk, nil
}

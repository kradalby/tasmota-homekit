package tasmotahomekit

import (
	"testing"

	"github.com/kradalby/tasmota-homekit/plugs"
)

func TestLoadPlugsConfig(t *testing.T) {
	config, err := plugs.LoadConfig("./plugs.hujson.example")
	if err != nil {
		t.Fatalf("Failed to load plugs config: %v", err)
	}

	if len(config.Plugs) == 0 {
		t.Error("Expected at least one plug in example config")
	}

	// Validate first plug has required fields
	if len(config.Plugs) > 0 {
		plug := config.Plugs[0]
		if plug.ID == "" {
			t.Error("Plug ID should not be empty")
		}
		if plug.Name == "" {
			t.Error("Plug name should not be empty")
		}
		if plug.Address == "" {
			t.Error("Plug address should not be empty")
		}
	}
}

func TestGetLocalIP(t *testing.T) {
	ip, err := getLocalIP()
	if err != nil {
		t.Skipf("No local IP found (expected in some environments): %v", err)
	}

	if ip == "" {
		t.Error("Local IP should not be empty")
	}

	t.Logf("Found local IP: %s", ip)
}

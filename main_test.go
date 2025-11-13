package main

import (
	"testing"
)

func TestLoadConfig(t *testing.T) {
	config, err := loadConfig()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Check defaults are set
	if config.HAP.PIN == "" {
		t.Error("HAP PIN should have a default value")
	}

	if config.HAP.Port != 8080 {
		t.Errorf("Expected HAP port 8080, got %d", config.HAP.Port)
	}

	if config.Web.Port != 8081 {
		t.Errorf("Expected web port 8081, got %d", config.Web.Port)
	}

	if config.MQTT.Port != 1883 {
		t.Errorf("Expected MQTT port 1883, got %d", config.MQTT.Port)
	}
}

func TestLoadPlugsConfig(t *testing.T) {
	// Test with the example config
	config, err := loadPlugsConfig("./plugs.hujson.example")
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

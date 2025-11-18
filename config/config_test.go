package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	unsetEnv(t, "TASMOTA_HOMEKIT_HAP_PIN")
	unsetEnv(t, "TASMOTA_HOMEKIT_HAP_PORT")
	unsetEnv(t, "TASMOTA_HOMEKIT_WEB_PORT")
	unsetEnv(t, "TASMOTA_HOMEKIT_MQTT_PORT")
	unsetEnv(t, "TASMOTA_HOMEKIT_PLUGS_CONFIG")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HAP.PIN != "00102003" {
		t.Errorf("default HAP PIN = %s, want 00102003", cfg.HAP.PIN)
	}
	if cfg.Web.Port != 8081 {
		t.Errorf("default web port = %d, want 8081", cfg.Web.Port)
	}
	if cfg.MQTT.Port != 1883 {
		t.Errorf("default MQTT port = %d, want 1883", cfg.MQTT.Port)
	}
	if cfg.PlugsConfigPath != "./plugs.hujson" {
		t.Errorf("default plugs path = %s, want ./plugs.hujson", cfg.PlugsConfigPath)
	}
}

func TestValidatePinLength(t *testing.T) {
	t.Setenv("TASMOTA_HOMEKIT_HAP_PIN", "1234")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid HAP pin length")
	}
}

func TestValidatePorts(t *testing.T) {
	if err := os.Setenv("TASMOTA_HOMEKIT_HAP_PORT", "70000"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}
	defer func() {
		_ = os.Unsetenv("TASMOTA_HOMEKIT_HAP_PORT")
	}()

	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid HAP port")
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()

	if val, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() {
			_ = os.Setenv(key, val)
		})
	} else {
		t.Cleanup(func() {
			_ = os.Unsetenv(key)
		})
	}
	_ = os.Unsetenv(key)
}

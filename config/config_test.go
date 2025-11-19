package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		errMsg string
	}{
		{
			name: "invalid HAP pin",
			env: map[string]string{
				"TASMOTA_HOMEKIT_HAP_PIN": "1234",
			},
			errMsg: "HAP PIN",
		},
		{
			name: "invalid HAP port",
			env: map[string]string{
				"TASMOTA_HOMEKIT_HAP_PORT": "70000",
			},
			errMsg: "HAP port must be between",
		},
		{
			name: "invalid web port",
			env: map[string]string{
				"TASMOTA_HOMEKIT_WEB_PORT": "0",
			},
			errMsg: "web port must be between",
		},
		{
			name: "invalid MQTT port",
			env: map[string]string{
				"TASMOTA_HOMEKIT_MQTT_PORT": "70000",
			},
			errMsg: "MQTT port must be between",
		},
		{
			name: "invalid log level",
			env: map[string]string{
				"TASMOTA_HOMEKIT_LOG_LEVEL": "verbose",
			},
			errMsg: "invalid log level",
		},
		{
			name: "invalid log format",
			env: map[string]string{
				"TASMOTA_HOMEKIT_LOG_FORMAT": "yaml",
			},
			errMsg: "invalid log format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !contains(err.Error(), tt.errMsg) {
				t.Fatalf("expected error containing %q, got %v", tt.errMsg, err)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := cfg.HAPAddrPort().String(); got != "0.0.0.0:8080" {
		t.Errorf("HAP addr = %s, want 0.0.0.0:8080", got)
	}
	if got := cfg.WebAddrPort().String(); got != "0.0.0.0:8081" {
		t.Errorf("Web addr = %s, want 0.0.0.0:8081", got)
	}
	if got := cfg.MQTTAddrPort().String(); got != "0.0.0.0:1883" {
		t.Errorf("MQTT addr = %s, want 0.0.0.0:1883", got)
	}
	if cfg.HAPPin != "00102003" {
		t.Errorf("HAPPin = %s, want 00102003", cfg.HAPPin)
	}
	if cfg.HAPStoragePath != "./data/hap" {
		t.Errorf("HAPStoragePath = %s, want ./data/hap", cfg.HAPStoragePath)
	}
	if cfg.TailscaleHostname != "tasmota-homekit" {
		t.Errorf("TailscaleHostname = %s, want tasmota-homekit", cfg.TailscaleHostname)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %s, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %s, want json", cfg.LogFormat)
	}
	if cfg.PlugsConfigPath != "./plugs.hujson" {
		t.Errorf("PlugsConfigPath = %s, want ./plugs.hujson", cfg.PlugsConfigPath)
	}
}

func TestAddressOverrides(t *testing.T) {
	clearEnv(t)

	t.Setenv("TASMOTA_HOMEKIT_HAP_ADDR", "127.0.0.1:9000")
	t.Setenv("TASMOTA_HOMEKIT_WEB_ADDR", "[::1]:9999")
	t.Setenv("TASMOTA_HOMEKIT_MQTT_ADDR", "10.0.0.5:18830")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HAPAddrPort().String() != "127.0.0.1:9000" {
		t.Errorf("HAP addr = %s, want 127.0.0.1:9000", cfg.HAPAddrPort())
	}
	if cfg.WebAddrPort().String() != "[::1]:9999" {
		t.Errorf("Web addr = %s, want [::1]:9999", cfg.WebAddrPort())
	}
	if cfg.MQTTAddrPort().String() != "10.0.0.5:18830" {
		t.Errorf("MQTT addr = %s, want 10.0.0.5:18830", cfg.MQTTAddrPort())
	}
}

func TestSetListenerAddrsForTesting(t *testing.T) {
	cfg := &Config{}
	cfg.SetListenerAddrsForTesting("1.2.3.4:1234", "5.6.7.8:5678", "9.9.9.9:9999")

	if cfg.HAPAddrPort().String() != "1.2.3.4:1234" {
		t.Errorf("HAPAddrPort() = %s, want 1.2.3.4:1234", cfg.HAPAddrPort())
	}
	if cfg.WebAddrPort().String() != "5.6.7.8:5678" {
		t.Errorf("WebAddrPort() = %s, want 5.6.7.8:5678", cfg.WebAddrPort())
	}
	if cfg.MQTTAddrPort().String() != "9.9.9.9:9999" {
		t.Errorf("MQTTAddrPort() = %s, want 9.9.9.9:9999", cfg.MQTTAddrPort())
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, env := range os.Environ() {
		if len(env) == 0 || env[0] == '_' {
			continue
		}
		if !hasPrefix(env, "TASMOTA_HOMEKIT_") {
			continue
		}
		key := env[:indexByte(env, '=')]
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("failed to unset env var %s: %v", key, err)
		}
	}
}

func hasPrefix(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := range prefix {
		if s[i] != prefix[i] {
			return false
		}
	}
	return true
}

func indexByte(s string, c byte) int {
	for i := range s {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func contains(s, substr string) bool {
	return len(substr) == 0 || indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}
	if len(substr) > len(s) {
		return -1
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

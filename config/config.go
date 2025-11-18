package config

import (
	"fmt"

	env "github.com/Netflix/go-env"
)

// Config holds all environment-driven configuration.
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
		Hostname string `env:"TASMOTA_HOMEKIT_TS_HOSTNAME,default=tasmota-nefit"`
		AuthKey  string `env:"TASMOTA_HOMEKIT_TS_AUTHKEY"`
	}

	PlugsConfigPath string `env:"TASMOTA_HOMEKIT_PLUGS_CONFIG,default=./plugs.hujson"`
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	var cfg Config
	if _, err := env.UnmarshalFromEnviron(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse environment variables: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate ensures basic correctness of the configuration.
func (c *Config) Validate() error {
	if len(c.HAP.PIN) != 8 {
		return fmt.Errorf("HAP PIN must be exactly 8 digits")
	}
	if err := validatePort(c.HAP.Port, "HAP port"); err != nil {
		return err
	}
	if err := validatePort(c.Web.Port, "web port"); err != nil {
		return err
	}
	if err := validatePort(c.MQTT.Port, "MQTT port"); err != nil {
		return err
	}
	if c.PlugsConfigPath == "" {
		return fmt.Errorf("PlugsConfigPath cannot be empty")
	}
	return nil
}

func validatePort(port int, name string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535, got %d", name, port)
	}
	return nil
}

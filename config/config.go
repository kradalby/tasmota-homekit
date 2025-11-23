package config

import (
	"fmt"
	"net/netip"
	"os"

	env "github.com/Netflix/go-env"
)

const (
	defaultBindAddress   = "0.0.0.0"
	defaultHAPPort       = 8080
	defaultWebPort       = 8081
	defaultMQTTPort      = 1883
	defaultMQTTBindLocal = "0.0.0.0"
)

// Config holds all environment-driven configuration.
type Config struct {
	// HomeKit listener configuration
	HAPPin         string `env:"TASMOTA_HOMEKIT_HAP_PIN,default=00102003"`
	HAPStoragePath string `env:"TASMOTA_HOMEKIT_HAP_STORAGE_PATH,default=./data/hap"`
	HAPAddr        string `env:"TASMOTA_HOMEKIT_HAP_ADDR"`
	HAPBindAddress string `env:"TASMOTA_HOMEKIT_HAP_BIND_ADDRESS,default=0.0.0.0"`
	HAPPort        int    `env:"TASMOTA_HOMEKIT_HAP_PORT,default=8080"`

	// Web listener configuration
	WebAddr        string `env:"TASMOTA_HOMEKIT_WEB_ADDR"`
	WebBindAddress string `env:"TASMOTA_HOMEKIT_WEB_BIND_ADDRESS,default=0.0.0.0"`
	WebPort        int    `env:"TASMOTA_HOMEKIT_WEB_PORT,default=8081"`

	// Embedded MQTT listener configuration
	MQTTAddr        string `env:"TASMOTA_HOMEKIT_MQTT_ADDR"`
	MQTTBindAddress string `env:"TASMOTA_HOMEKIT_MQTT_BIND_ADDRESS,default=0.0.0.0"`
	MQTTPort        int    `env:"TASMOTA_HOMEKIT_MQTT_PORT,default=1883"`

	// Tailscale configuration
	TailscaleHostname string `env:"TASMOTA_HOMEKIT_TS_HOSTNAME,default=tasmota-homekit"`
	TailscaleAuthKey  string `env:"TASMOTA_HOMEKIT_TS_AUTHKEY"`
	TailscaleStateDir string `env:"TASMOTA_HOMEKIT_TS_STATE_DIR,default=./data/tailscale"`

	// Logging options
	LogLevel  string `env:"TASMOTA_HOMEKIT_LOG_LEVEL,default=info"`
	LogFormat string `env:"TASMOTA_HOMEKIT_LOG_FORMAT,default=json"`

	// Plugs configuration file
	PlugsConfigPath string `env:"TASMOTA_HOMEKIT_PLUGS_CONFIG,default=./plugs.hujson"`

	hapAddr  netip.AddrPort
	webAddr  netip.AddrPort
	mqttAddr netip.AddrPort
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
	if len(c.HAPPin) != 8 {
		return fmt.Errorf("HAP PIN must be exactly 8 digits")
	}
	if err := c.parseListenerAddrs(); err != nil {
		return err
	}
	if c.PlugsConfigPath == "" {
		return fmt.Errorf("PlugsConfigPath cannot be empty")
	}
	if err := validateLogLevel(c.LogLevel); err != nil {
		return err
	}
	if err := validateLogFormat(c.LogFormat); err != nil {
		return err
	}
	if c.TailscaleStateDir == "" {
		return fmt.Errorf("TailscaleStateDir cannot be empty")
	}
	return nil
}

func (c *Config) parseListenerAddrs() error {
	if c.HAPBindAddress == "" {
		c.HAPBindAddress = defaultBindAddress
	}
	if c.HAPPort == 0 && !envVarSet("TASMOTA_HOMEKIT_HAP_PORT") {
		c.HAPPort = defaultHAPPort
	}
	if err := validatePortRange("HAP", c.HAPPort); err != nil {
		return err
	}
	hapAddr := c.HAPAddr
	if hapAddr == "" {
		hapAddr = fmt.Sprintf("%s:%d", c.HAPBindAddress, c.HAPPort)
	}
	parsedHAP, err := netip.ParseAddrPort(hapAddr)
	if err != nil {
		return fmt.Errorf("invalid HAP addr %q: %w", hapAddr, err)
	}
	c.hapAddr = parsedHAP

	if c.WebBindAddress == "" {
		c.WebBindAddress = defaultBindAddress
	}
	if c.WebPort == 0 && !envVarSet("TASMOTA_HOMEKIT_WEB_PORT") {
		c.WebPort = defaultWebPort
	}
	if err := validatePortRange("web", c.WebPort); err != nil {
		return err
	}
	webAddr := c.WebAddr
	if webAddr == "" {
		webAddr = fmt.Sprintf("%s:%d", c.WebBindAddress, c.WebPort)
	}
	parsedWeb, err := netip.ParseAddrPort(webAddr)
	if err != nil {
		return fmt.Errorf("invalid web addr %q: %w", webAddr, err)
	}
	c.webAddr = parsedWeb

	if c.MQTTBindAddress == "" {
		c.MQTTBindAddress = defaultMQTTBindLocal
	}
	if c.MQTTPort == 0 && !envVarSet("TASMOTA_HOMEKIT_MQTT_PORT") {
		c.MQTTPort = defaultMQTTPort
	}
	if err := validatePortRange("MQTT", c.MQTTPort); err != nil {
		return err
	}
	mqttAddr := c.MQTTAddr
	if mqttAddr == "" {
		mqttAddr = fmt.Sprintf("%s:%d", c.MQTTBindAddress, c.MQTTPort)
	}
	parsedMQTT, err := netip.ParseAddrPort(mqttAddr)
	if err != nil {
		return fmt.Errorf("invalid MQTT addr %q: %w", mqttAddr, err)
	}
	c.mqttAddr = parsedMQTT

	return nil
}

// HAPAddrPort returns the parsed HAP listener address.
func (c *Config) HAPAddrPort() netip.AddrPort {
	c.ensureParsed()
	return c.hapAddr
}

// WebAddrPort returns the parsed web listener address.
func (c *Config) WebAddrPort() netip.AddrPort {
	c.ensureParsed()
	return c.webAddr
}

// MQTTAddrPort returns the parsed MQTT listener address.
func (c *Config) MQTTAddrPort() netip.AddrPort {
	c.ensureParsed()
	return c.mqttAddr
}

func (c *Config) ensureParsed() {
	if !c.hapAddr.IsValid() || !c.webAddr.IsValid() || !c.mqttAddr.IsValid() {
		if err := c.parseListenerAddrs(); err != nil {
			panic(fmt.Sprintf("failed to parse listener addresses: %v", err))
		}
	}
}

// SetListenerAddrsForTesting overrides listener addresses in tests.
func (c *Config) SetListenerAddrsForTesting(hap, web, mqtt string) {
	c.hapAddr = netip.MustParseAddrPort(hap)
	c.webAddr = netip.MustParseAddrPort(web)
	c.mqttAddr = netip.MustParseAddrPort(mqtt)
}

func validatePortRange(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s port must be between 1 and 65535, got %d", name, port)
	}
	return nil
}

func validateLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("invalid log level %q, must be one of: debug, info, warn, error", level)
	}
}

func validateLogFormat(format string) error {
	switch format {
	case "json", "console":
		return nil
	default:
		return fmt.Errorf("invalid log format %q, must be 'json' or 'console'", format)
	}
}

func envVarSet(key string) bool {
	if key == "" {
		return false
	}
	_, ok := os.LookupEnv(key)
	return ok
}

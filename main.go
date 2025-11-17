package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"path/filepath"

	env "github.com/Netflix/go-env"
	homekitqr "github.com/kradalby/homekit-qr"
	"github.com/kradalby/kra/web"
	"github.com/tailscale/hujson"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/brutella/hap"
	"tailscale.com/util/eventbus"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Setup structured logging
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("Starting Tasmota HomeKit Bridge", "version", version)

	// Load configuration
	config, err := loadConfig()
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("Configuration loaded",
		"hap_port", config.HAP.Port,
		"web_port", config.Web.Port,
		"mqtt_port", config.MQTT.Port,
		"plugs_config", config.PlugsConfigPath,
	)

	// Load plugs configuration
	plugs, err := loadPlugsConfig(config.PlugsConfigPath)
	if err != nil {
		slog.Error("Failed to load plugs configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("Loaded plugs", "count", len(plugs.Plugs))
	for _, plug := range plugs.Plugs {
		slog.Info("Plug configured",
			"id", plug.ID,
			"name", plug.Name,
			"address", plug.Address,
		)
	}

	// Create context that listens for shutdown signals
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize event bus
	bus := eventbus.New()
	commands := make(chan PlugCommandEvent, 10)
	errorPublisher := eventbus.Publish[PlugErrorEvent](bus.Client("main"))

	slog.Info("Event system initialized")

	// Get local IP address for MQTT broker configuration
	localIP, err := getLocalIP()
	if err != nil {
		slog.Warn("Failed to get local IP, using localhost", "error", err)
		localIP = "localhost"
	}
	slog.Info("Local IP address", "ip", localIP)

	// Start MQTT broker
	mqttServer := mqtt.New(&mqtt.Options{
		InlineClient: true, // Enable inline client for internal subscriptions
	})

	// Allow all connections (no authentication for now)
	err = mqttServer.AddHook(new(auth.AllowHook), nil)
	if err != nil {
		slog.Error("Failed to add MQTT auth hook", "error", err)
		os.Exit(1)
	}

	// Initialize plug manager first (before MQTT hook needs it)
	plugManager, err := NewPlugManager(plugs.Plugs, commands, bus)
	if err != nil {
		slog.Error("Failed to initialize plug manager", "error", err)
		os.Exit(1)
	}

	// Add MQTT message hook to process messages from Tasmota devices
	mqttClient := bus.Client("mqtthook")
	mqttHook := &MQTTHook{
		statePublisher: eventbus.Publish[PlugStateChangedEvent](mqttClient),
	}
	err = mqttServer.AddHook(mqttHook, nil)
	if err != nil {
		slog.Error("Failed to add MQTT message hook", "error", err)
		os.Exit(1)
	}

	// Create TCP listener
	tcp := listeners.NewTCP(listeners.Config{
		ID:      "tcp",
		Address: fmt.Sprintf(":%d", config.MQTT.Port),
	})
	err = mqttServer.AddListener(tcp)
	if err != nil {
		slog.Error("Failed to add MQTT listener", "error", err)
		os.Exit(1)
	}

	// Start MQTT server in background
	go func() {
		slog.Info("Starting MQTT broker", "port", config.MQTT.Port)
		if err := mqttServer.Serve(); err != nil {
			slog.Error("MQTT server error", "error", err)
		}
	}()

	slog.Info("MQTT broker started", "port", config.MQTT.Port)

	// Start command processor
	go plugManager.ProcessCommands(ctx)

	// Start state event processor (processes events from MQTT and other sources)
	go plugManager.ProcessStateEvents(ctx)

	// Fetch initial state for all plugs
	for _, plug := range plugs.Plugs {
		go func(plugID string) {
			state, err := plugManager.GetStatus(ctx, plugID)
			if err != nil {
				slog.Warn("Failed to get initial status",
					"plug_id", plugID,
					"error", err,
				)
				return
			}
			slog.Info("Initial plug state",
				"plug_id", plugID,
				"on", state.On,
			)
		}(plug.ID)
	}

	// Configure plugs to use embedded MQTT broker
	for _, plug := range plugs.Plugs {
		go func(plugID string) {
			// Wait a moment to ensure MQTT server is fully started
			time.Sleep(time.Second)

			err := plugManager.ConfigureMQTT(ctx, plugID, localIP, config.MQTT.Port)
			if err != nil {
				slog.Error("Failed to configure MQTT for plug",
					"plug_id", plugID,
					"error", err,
				)
				errorPublisher.Publish(PlugErrorEvent{
					PlugID: plugID,
					Error:  fmt.Errorf("MQTT configuration failed: %w", err),
				})
				return
			}

			slog.Info("Plug configured for MQTT", "plug_id", plugID)
		}(plug.ID)
	}

	// Start connection monitoring to detect and reconfigure offline plugs
	go plugManager.MonitorConnections(ctx, localIP, config.MQTT.Port)
	slog.Info("Connection monitoring started")

	// Initialize HAP (HomeKit) manager
	hapManager := NewHAPManager(plugs.Plugs, commands, plugManager, bus)

	// Start HAP state change processor
	go hapManager.ProcessStateChanges(ctx)

	// Create and start HAP server
	accessories := hapManager.GetAccessories()
	if len(accessories) == 0 {
		slog.Error("No accessories to serve")
		os.Exit(1)
	}

	hapServer, err := hap.NewServer(
		hap.NewFsStore(config.HAP.StoragePath),
		accessories[0],
		accessories[1:]...,
	)
	if err != nil {
		slog.Error("Failed to create HAP server", "error", err)
		os.Exit(1)
	}

	// Set the PIN for pairing
	hapServer.Pin = config.HAP.PIN
	hapServer.Addr = fmt.Sprintf(":%d", config.HAP.Port)

	// Start HAP server in background
	go func() {
		slog.Info("Starting HomeKit server",
			"port", config.HAP.Port,
			"pin", config.HAP.PIN,
		)
		if err := hapServer.ListenAndServe(ctx); err != nil {
			slog.Error("HAP server error", "error", err)
		}
	}()

	// Print QR code for easy pairing
	fmt.Println("\n========================================")
	fmt.Printf("HomeKit bridge ready - pair with PIN: %s\n\n", config.HAP.PIN)

	qrConfig := homekitqr.QRCodeConfig{
		SetupURIConfig: homekitqr.SetupURIConfig{
			PairingCode: config.HAP.PIN,
			SetupID:     "4412",
			Category:    homekitqr.CategoryBridge,
		},
	}

	qr, err := homekitqr.GenerateQRTerminal(qrConfig)
	if err != nil {
		slog.Warn("Failed to generate QR code", "error", err)
	} else {
		fmt.Println(qr)
	}

	fmt.Println("========================================")
	slog.Info("Scan QR code or enter PIN manually in Home app", "pin", config.HAP.PIN)

	// Store QR code for web display
	qrCode := ""
	if qr != "" {
		qrCode = qr
	}

	// Initialize web server with kra/web for Tailscale support
	webServer := NewWebServer(plugManager, commands, bus, config.HAP.PIN, qrCode)
	webServer.LogEvent("Server starting...")

	// Start web state change processor for SSE
	go webServer.ProcessStateChanges(ctx)

	// Setup kra/web with Tailscale
	kraOpts := []web.Option{
		web.WithLogger(log.Default()),
	}

	// Handle Tailscale auth key - kra/web expects a file path
	tsKeyPath := ""
	var tempKeyFile string
	if config.Tailscale.AuthKey != "" {
		// Write the auth key to a temporary file
		tempDir := os.TempDir()
		tempKeyFile = filepath.Join(tempDir, "tasmota-homekit-tskey")
		if err := os.WriteFile(tempKeyFile, []byte(config.Tailscale.AuthKey), 0600); err != nil {
			slog.Warn("Failed to write Tailscale auth key to temp file", "error", err)
		} else {
			tsKeyPath = tempKeyFile
			defer func() {
				if err := os.Remove(tempKeyFile); err != nil {
					slog.Warn("Failed to remove temp Tailscale key file", "error", err)
				}
			}()
		}
	}

	// Enable Tailscale if hostname is set (WithTailscale sets noTS field, so false = enable)
	enableTailscale := config.Tailscale.Hostname != ""
	if enableTailscale {
		kraOpts = append(kraOpts, web.WithTailscale(false)) // false means enable (noTS=false)
	} else {
		kraOpts = append(kraOpts, web.WithTailscale(true)) // true means disable (noTS=true)
	}

	// Set hostname to empty if Tailscale not enabled
	hostname := config.Tailscale.Hostname
	if !enableTailscale {
		hostname = ""
	}

	kraWeb := web.NewKraWeb(
		hostname,
		tsKeyPath,
		fmt.Sprintf(":%d", config.Web.Port),
		kraOpts...,
	)

	// Register handlers
	kraWeb.Handle("/", http.HandlerFunc(webServer.HandleIndex))
	kraWeb.Handle("/toggle/", http.HandlerFunc(webServer.HandleToggle))
	kraWeb.Handle("/events", http.HandlerFunc(webServer.HandleSSE))

	// Start web server in background
	go func() {
		slog.Info("Starting web server",
			"port", config.Web.Port,
			"tailscale_hostname", config.Tailscale.Hostname,
		)
		if err := kraWeb.ListenAndServe(); err != nil {
			slog.Error("Web server error", "error", err)
		}
	}()

	webURL := fmt.Sprintf("http://localhost:%d", config.Web.Port)
	if enableTailscale {
		webURL = fmt.Sprintf("https://%s (and %s)", config.Tailscale.Hostname, fmt.Sprintf("http://localhost:%d", config.Web.Port))
	}
	slog.Info("Web UI available", "url", webURL)

	// Wait for shutdown signal
	slog.Info("Server running, press Ctrl+C to stop")
	<-ctx.Done()
	slog.Info("Shutting down...")

	// Cleanup - graceful shutdown
	slog.Info("Stopping web server...")
	// kra/web doesn't have explicit shutdown, will exit with context

	slog.Info("Stopping MQTT broker...")
	if err := mqttServer.Close(); err != nil {
		slog.Error("Error stopping MQTT broker", "error", err)
	}
	slog.Info("Shutdown complete")
}

// loadConfig loads configuration from environment variables
func loadConfig() (*Config, error) {
	var config Config

	_, err := env.UnmarshalFromEnviron(&config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse environment variables: %w", err)
	}

	return &config, nil
}

// loadPlugsConfig loads the plugs configuration from a HuJSON file
func loadPlugsConfig(path string) (*PlugConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugs config file: %w", err)
	}

	// Parse HuJSON (strips comments, trailing commas, etc.)
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return nil, fmt.Errorf("failed to standardize HuJSON: %w", err)
	}

	var config PlugConfig
	if err := json.Unmarshal(standardized, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal plugs config: %w", err)
	}

	// Validate plugs
	if len(config.Plugs) == 0 {
		return nil, fmt.Errorf("no plugs configured")
	}

	for i, plug := range config.Plugs {
		if plug.ID == "" {
			return nil, fmt.Errorf("plug %d has no ID", i)
		}
		if plug.Name == "" {
			return nil, fmt.Errorf("plug %s has no name", plug.ID)
		}
		if plug.Address == "" {
			return nil, fmt.Errorf("plug %s has no address", plug.ID)
		}
	}

	return &config, nil
}

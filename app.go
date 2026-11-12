package tasmotahomekit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	homekitqr "github.com/kradalby/homekit-qr"
	"github.com/kradalby/kra/web"

	appconfig "github.com/kradalby/tasmota-homekit/config"
	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/logging"
	"github.com/kradalby/tasmota-homekit/metrics"
	"github.com/kradalby/tasmota-homekit/plugs"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/brutella/hap"
	"tailscale.com/util/eventbus"
)

var version = "dev"

// Main is the entry point used by cmd/tasmota-homekit.
func Main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := appconfig.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	logger, err := logging.New(cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to configure logger: %v\n", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	slog.Info(
		"Starting Tasmota HomeKit Bridge",
		"version", version,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
	)

	slog.Info(
		"Configuration loaded",
		"hap_addr", cfg.HAPAddrPort().String(),
		"web_addr", cfg.WebAddrPort().String(),
		"mqtt_addr", cfg.MQTTAddrPort().String(),
		"plugs_config", cfg.PlugsConfigPath,
	)

	plugCfg, err := plugs.LoadConfig(cfg.PlugsConfigPath)
	if err != nil {
		slog.Error("Failed to load plugs configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("Loaded plugs", "count", len(plugCfg.Plugs))
	for _, plug := range plugCfg.Plugs {
		slog.Info(
			"Plug configured",
			"id", plug.ID,
			"name", plug.Name,
			"address", plug.Address,
		)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	eventBus, err := events.New(logger)
	if err != nil {
		slog.Error("Failed to initialize eventbus", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := eventBus.Close(); err != nil {
			slog.Warn("Error closing eventbus", "error", err)
		}
	}()

	metricsCollector, err := metrics.NewCollector(ctx, logger, eventBus, nil)
	if err != nil {
		slog.Error("Failed to initialize metrics collector", "error", err)
		os.Exit(1)
	}
	defer metricsCollector.Close()

	localIP, err := getLocalIP()
	if err != nil {
		slog.Warn("Failed to get local IP, using localhost", "error", err)
		localIP = "localhost"
	}
	slog.Info("Local IP address", "ip", localIP)

	mqttServer := mqtt.New(&mqtt.Options{
		InlineClient: true,
	})

	if err := mqttServer.AddHook(new(auth.AllowHook), nil); err != nil {
		slog.Error("Failed to add MQTT auth hook", "error", err)
		os.Exit(1)
	}

	plugManager, err := plugs.NewManager(plugCfg.Plugs, eventBus)
	if err != nil {
		slog.Error("Failed to initialize plug manager", "error", err)
		os.Exit(1)
	}

	mqttClient, err := eventBus.Client(events.ClientMQTT)
	if err != nil {
		slog.Error("Failed to get MQTT client", "error", err)
		os.Exit(1)
	}
	mqttHook := &MQTTHook{
		statePublisher: eventbus.Publish[plugs.StateChangedEvent](mqttClient),
	}
	if err := mqttServer.AddHook(mqttHook, nil); err != nil {
		slog.Error("Failed to add MQTT message hook", "error", err)
		os.Exit(1)
	}

	tcp := listeners.NewTCP(listeners.Config{
		ID:      "tcp",
		Address: cfg.MQTTAddrPort().String(),
	})
	if err := mqttServer.AddListener(tcp); err != nil {
		slog.Error("Failed to add MQTT listener", "error", err)
		os.Exit(1)
	}

	mqttComponent := string(events.ClientMQTT)
	eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: mqttComponent,
		Status:    events.ConnectionStatusConnecting,
	})

	// Serve starts the listeners and returns; the broker runs until Close.
	if err := mqttServer.Serve(); err != nil {
		eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
			Timestamp: time.Now(),
			Component: mqttComponent,
			Status:    events.ConnectionStatusFailed,
			Error:     err.Error(),
		})
		slog.Error("Failed to start MQTT broker", "error", err)
		os.Exit(1)
	}
	eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: mqttComponent,
		Status:    events.ConnectionStatusConnected,
	})
	slog.Info("MQTT broker started", "addr", cfg.MQTTAddrPort().String())

	go plugManager.ProcessStateEvents(ctx)

	// Its first pass reads every plug's state and MQTT settings.
	go plugManager.MonitorConnections(ctx, localIP, int(cfg.MQTTAddrPort().Port()))

	hapManager := NewHAPManager(plugCfg.Plugs, cfg.BridgeName, plugManager, eventBus)
	hapManager.Start(ctx)
	defer hapManager.Close()

	accessories := hapManager.GetAccessories()
	if len(accessories) == 0 {
		slog.Error("No accessories to serve")
		os.Exit(1)
	}

	fsStore := hap.NewFsStore(cfg.HAPStoragePath)
	hapServer, err := hap.NewServer(
		fsStore,
		accessories[0],
		accessories[1:]...,
	)
	if err != nil {
		slog.Error("Failed to create HAP server", "error", err)
		os.Exit(1)
	}

	hapServer.Pin = cfg.HAPPin
	hapServer.Addr = cfg.HAPAddrPort().String()

	hapManager.SetServer(hapServer)
	hapManager.SetStore(fsStore)

	hapStatusClient, err := eventBus.Client(events.ClientHAP)
	if err != nil {
		slog.Error("Failed to get HAP client", "error", err)
		os.Exit(1)
	}
	hapComponent := string(events.ClientHAP)
	eventBus.PublishConnectionStatus(hapStatusClient, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: hapComponent,
		Status:    events.ConnectionStatusConnecting,
	})

	go func() {
		slog.Info(
			"Starting HomeKit server",
			"addr", cfg.HAPAddrPort().String(),
			"pin", cfg.HAPPin,
		)
		eventBus.PublishConnectionStatus(hapStatusClient, events.ConnectionStatusEvent{
			Timestamp: time.Now(),
			Component: hapComponent,
			Status:    events.ConnectionStatusConnected,
		})
		if err := hapServer.ListenAndServe(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				eventBus.PublishConnectionStatus(hapStatusClient, events.ConnectionStatusEvent{
					Timestamp: time.Now(),
					Component: hapComponent,
					Status:    events.ConnectionStatusDisconnected,
				})
			} else {
				eventBus.PublishConnectionStatus(hapStatusClient, events.ConnectionStatusEvent{
					Timestamp: time.Now(),
					Component: hapComponent,
					Status:    events.ConnectionStatusFailed,
					Error:     err.Error(),
				})
				slog.Error("HAP server error", "error", err)
			}
			return
		}
		eventBus.PublishConnectionStatus(hapStatusClient, events.ConnectionStatusEvent{
			Timestamp: time.Now(),
			Component: hapComponent,
			Status:    events.ConnectionStatusDisconnected,
		})
	}()

	fmt.Printf("HomeKit bridge ready - pair with PIN: %s\n\n", cfg.HAPPin)

	qrConfig := homekitqr.QRCodeConfig{
		PairingCode: cfg.HAPPin,
		SetupID:     "4412",
		Category:    homekitqr.CategoryBridge,
	}

	qr, err := homekitqr.GenerateQRTerminal(qrConfig)
	if err != nil {
		slog.Warn("Failed to generate QR code", "error", err)
	} else {
		fmt.Println(qr)
	}

	fmt.Println("========================================")
	slog.Info("Scan QR code or enter PIN manually in Home app", "pin", cfg.HAPPin)

	kraOpts := []web.Option{
		web.WithStdLogger(log.New(os.Stdout, "kraweb: ", log.LstdFlags)),
		web.WithLogger(logger),
		web.WithTailscaleStateDir(cfg.TailscaleStateDir),
	}

	enableTailscale := cfg.TailscaleAuthKey != ""
	kraConfig := web.ServerConfig{
		Hostname:        cfg.TailscaleHostname,
		LocalAddr:       cfg.WebAddrPort().String(),
		AuthKey:         cfg.TailscaleAuthKey,
		EnableTailscale: enableTailscale,
	}

	kraWeb, err := web.NewServer(kraConfig, kraOpts...)
	if err != nil {
		slog.Error("Failed to configure web server", "error", err)
		os.Exit(1)
	}

	webServer := NewWebServer(logger, plugManager, plugManager, eventBus, kraWeb, cfg.HAPPin, qr)
	webServer.LogEvent("Server starting...")
	webServer.Start(ctx)
	defer webServer.Close()

	webServer.registerRoutes(kraWeb)

	// Setup debug handlers with tsweb.Debugger
	SetupDebugHandlers(kraWeb, hapManager)

	webURL := fmt.Sprintf("http://%s", cfg.WebAddrPort().String())
	if enableTailscale {
		webURL = fmt.Sprintf("https://%s (and http://%s)", cfg.TailscaleHostname, cfg.WebAddrPort().String())
	}
	slog.Info("Web UI available", "url", webURL)

	slog.Info("Server running, press Ctrl+C to stop")
	<-ctx.Done()
	slog.Info("Shutting down...")

	slog.Info("Stopping web server...")
	slog.Info("Stopping MQTT broker...")
	if err := mqttServer.Close(); err != nil {
		slog.Error("Error stopping MQTT broker", "error", err)
	}
	eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: mqttComponent,
		Status:    events.ConnectionStatusDisconnected,
	})
	slog.Info("Shutdown complete")
}

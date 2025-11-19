package tasmotahomekit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	homekitqr "github.com/kradalby/homekit-qr"
	"github.com/kradalby/kra/web"
	appconfig "github.com/kradalby/tasmota-nefit/config"
	"github.com/kradalby/tasmota-nefit/events"
	"github.com/kradalby/tasmota-nefit/logging"
	"github.com/kradalby/tasmota-nefit/metrics"
	"github.com/kradalby/tasmota-nefit/plugs"

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

	slog.Info("Starting Tasmota HomeKit Bridge",
		"version", version,
		"log_level", cfg.LogLevel,
		"log_format", cfg.LogFormat,
	)

	slog.Info("Configuration loaded",
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
		slog.Info("Plug configured",
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

	commands := make(chan plugs.CommandEvent, 10)
	appClient, err := eventBus.Client(events.ClientMetrics)
	if err != nil {
		slog.Error("Failed to get metrics client", "error", err)
		os.Exit(1)
	}
	errorPublisher := eventbus.Publish[plugs.ErrorEvent](appClient)
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

	plugManager, err := plugs.NewManager(plugCfg.Plugs, commands, eventBus)
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

	go func() {
		slog.Info("Starting MQTT broker", "addr", cfg.MQTTAddrPort().String())
		eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
			Timestamp: time.Now(),
			Component: mqttComponent,
			Status:    events.ConnectionStatusConnected,
		})
		if err := mqttServer.Serve(); err != nil {
			eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
				Timestamp: time.Now(),
				Component: mqttComponent,
				Status:    events.ConnectionStatusFailed,
				Error:     err.Error(),
			})
			slog.Error("MQTT server error", "error", err)
			return
		}
		eventBus.PublishConnectionStatus(mqttClient, events.ConnectionStatusEvent{
			Timestamp: time.Now(),
			Component: mqttComponent,
			Status:    events.ConnectionStatusDisconnected,
		})
	}()

	slog.Info("MQTT broker started", "addr", cfg.MQTTAddrPort().String())

	go plugManager.ProcessCommands(ctx)
	go plugManager.ProcessStateEvents(ctx)

	for _, plug := range plugCfg.Plugs {
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

	for _, plug := range plugCfg.Plugs {
		go func(plugID string) {
			time.Sleep(time.Second)

			if err := plugManager.ConfigureMQTT(ctx, plugID, localIP, int(cfg.MQTTAddrPort().Port())); err != nil {
				slog.Error("Failed to configure MQTT for plug",
					"plug_id", plugID,
					"error", err,
				)
				errorPublisher.Publish(plugs.ErrorEvent{
					PlugID: plugID,
					Error:  fmt.Errorf("MQTT configuration failed: %w", err),
				})
				return
			}

			slog.Info("Plug configured for MQTT", "plug_id", plugID)
		}(plug.ID)
	}

	go plugManager.MonitorConnections(ctx, localIP, int(cfg.MQTTAddrPort().Port()))
	slog.Info("Connection monitoring started")

	hapManager := NewHAPManager(plugCfg.Plugs, commands, plugManager, eventBus)
	hapManager.Start(ctx)
	defer hapManager.Close()

	accessories := hapManager.GetAccessories()
	if len(accessories) == 0 {
		slog.Error("No accessories to serve")
		os.Exit(1)
	}

	hapServer, err := hap.NewServer(
		hap.NewFsStore(cfg.HAPStoragePath),
		accessories[0],
		accessories[1:]...,
	)
	if err != nil {
		slog.Error("Failed to create HAP server", "error", err)
		os.Exit(1)
	}

	hapServer.Pin = cfg.HAPPin
	hapServer.Addr = cfg.HAPAddrPort().String()

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
		slog.Info("Starting HomeKit server",
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
		SetupURIConfig: homekitqr.SetupURIConfig{
			PairingCode: cfg.HAPPin,
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
	slog.Info("Scan QR code or enter PIN manually in Home app", "pin", cfg.HAPPin)

	qrCode := ""
	if qr != "" {
		qrCode = qr
	}

	kraOpts := []web.Option{
		web.WithStdLogger(log.New(os.Stdout, "kraweb: ", log.LstdFlags)),
		web.WithLogger(logger),
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

	webServer := NewWebServer(logger, plugManager, commands, eventBus, kraWeb, cfg.HAPPin, qrCode)
	webServer.LogEvent("Server starting...")
	webServer.Start(ctx)
	defer webServer.Close()

	kraWeb.Handle("/", http.HandlerFunc(webServer.HandleIndex))
	kraWeb.Handle("/toggle/", http.HandlerFunc(webServer.HandleToggle))
	kraWeb.Handle("/events", http.HandlerFunc(webServer.HandleSSE))
	kraWeb.Handle("/health", http.HandlerFunc(webServer.HandleHealth))
	kraWeb.Handle("/qrcode", http.HandlerFunc(webServer.HandleQRCode))
	kraWeb.Handle("/debug/eventbus", http.HandlerFunc(webServer.HandleEventBusDebug))

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

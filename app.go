package tasmotahomekit

import (
	"context"
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
	"github.com/kradalby/tasmota-nefit/plugs"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/brutella/hap"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"tailscale.com/util/eventbus"
)

var version = "dev"

// Main is the entry point used by cmd/tasmota-homekit.
func Main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("Starting Tasmota HomeKit Bridge", "version", version)

	cfg, err := appconfig.Load()
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("Configuration loaded",
		"hap_port", cfg.HAP.Port,
		"web_port", cfg.Web.Port,
		"mqtt_port", cfg.MQTT.Port,
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

	bus := eventbus.New()
	commands := make(chan plugs.CommandEvent, 10)
	errorPublisher := eventbus.Publish[plugs.ErrorEvent](bus.Client("main"))

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

	plugManager, err := plugs.NewManager(plugCfg.Plugs, commands, bus)
	if err != nil {
		slog.Error("Failed to initialize plug manager", "error", err)
		os.Exit(1)
	}

	mqttClient := bus.Client("mqtthook")
	mqttHook := &MQTTHook{
		statePublisher: eventbus.Publish[plugs.StateChangedEvent](mqttClient),
	}
	if err := mqttServer.AddHook(mqttHook, nil); err != nil {
		slog.Error("Failed to add MQTT message hook", "error", err)
		os.Exit(1)
	}

	tcp := listeners.NewTCP(listeners.Config{
		ID:      "tcp",
		Address: fmt.Sprintf(":%d", cfg.MQTT.Port),
	})
	if err := mqttServer.AddListener(tcp); err != nil {
		slog.Error("Failed to add MQTT listener", "error", err)
		os.Exit(1)
	}

	go func() {
		slog.Info("Starting MQTT broker", "port", cfg.MQTT.Port)
		if err := mqttServer.Serve(); err != nil {
			slog.Error("MQTT server error", "error", err)
		}
	}()

	slog.Info("MQTT broker started", "port", cfg.MQTT.Port)

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

			if err := plugManager.ConfigureMQTT(ctx, plugID, localIP, cfg.MQTT.Port); err != nil {
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

	go plugManager.MonitorConnections(ctx, localIP, cfg.MQTT.Port)
	slog.Info("Connection monitoring started")

	hapManager := NewHAPManager(plugCfg.Plugs, commands, plugManager, bus)
	hapManager.Start(ctx)
	defer hapManager.Close()

	accessories := hapManager.GetAccessories()
	if len(accessories) == 0 {
		slog.Error("No accessories to serve")
		os.Exit(1)
	}

	hapServer, err := hap.NewServer(
		hap.NewFsStore(cfg.HAP.StoragePath),
		accessories[0],
		accessories[1:]...,
	)
	if err != nil {
		slog.Error("Failed to create HAP server", "error", err)
		os.Exit(1)
	}

	hapServer.Pin = cfg.HAP.PIN
	hapServer.Addr = fmt.Sprintf(":%d", cfg.HAP.Port)

	go func() {
		slog.Info("Starting HomeKit server",
			"port", cfg.HAP.Port,
			"pin", cfg.HAP.PIN,
		)
		if err := hapServer.ListenAndServe(ctx); err != nil {
			slog.Error("HAP server error", "error", err)
		}
	}()

	fmt.Printf("HomeKit bridge ready - pair with PIN: %s\n\n", cfg.HAP.PIN)

	qrConfig := homekitqr.QRCodeConfig{
		SetupURIConfig: homekitqr.SetupURIConfig{
			PairingCode: cfg.HAP.PIN,
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
	slog.Info("Scan QR code or enter PIN manually in Home app", "pin", cfg.HAP.PIN)

	qrCode := ""
	if qr != "" {
		qrCode = qr
	}

	kraOpts := []web.Option{
		web.WithStdLogger(log.New(os.Stdout, "kraweb: ", log.LstdFlags)),
		web.WithLogger(logger),
	}

	enableTailscale := cfg.Tailscale.AuthKey != ""
	kraConfig := web.ServerConfig{
		Hostname:        cfg.Tailscale.Hostname,
		LocalAddr:       fmt.Sprintf(":%d", cfg.Web.Port),
		AuthKey:         cfg.Tailscale.AuthKey,
		EnableTailscale: enableTailscale,
	}

	kraWeb, err := web.NewServer(kraConfig, kraOpts...)
	if err != nil {
		slog.Error("Failed to configure web server", "error", err)
		os.Exit(1)
	}

	webServer := NewWebServer(logger, plugManager, commands, bus, kraWeb, cfg.HAP.PIN, qrCode)
	webServer.LogEvent("Server starting...")
	webServer.Start(ctx)
	defer webServer.Close()

	kraWeb.Handle("/", http.HandlerFunc(webServer.HandleIndex))
	kraWeb.Handle("/toggle/", http.HandlerFunc(webServer.HandleToggle))
	kraWeb.Handle("/events", http.HandlerFunc(webServer.HandleSSE))
	kraWeb.Handle("/health", http.HandlerFunc(webServer.HandleHealth))
	kraWeb.Handle("/qrcode", http.HandlerFunc(webServer.HandleQRCode))
	kraWeb.Handle("/metrics", promhttp.Handler())

	webURL := fmt.Sprintf("http://localhost:%d", cfg.Web.Port)
	if enableTailscale {
		webURL = fmt.Sprintf("https://%s (and %s)", cfg.Tailscale.Hostname, fmt.Sprintf("http://localhost:%d", cfg.Web.Port))
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
	slog.Info("Shutdown complete")
}

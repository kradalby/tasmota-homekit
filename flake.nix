{
  description = "Tasmota HomeKit Bridge - Control Tasmota plugs via HomeKit";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem
      (system:
        let
          pkgs = import nixpkgs { inherit system; };

          # Use Go 1.25 to satisfy go.mod requirement of 1.25.3
          go = pkgs.go_1_25;

          # Override buildGoModule to use go_1_25
          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_25; };

        in
        {
          # Development shell
          devShells.default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              golangci-lint
              gopls
              gotools
              go-tools
              delve

              # Nix tooling
              nixpkgs-fmt

              # Pre-commit hooks
              prek
              prettier

              # Useful utilities
              git
              gnumake
            ];

            shellHook = ''
              echo "🏠 Tasmota HomeKit Development Environment"
              echo "Go version: $(go version)"
              echo "golangci-lint version: $(golangci-lint version)"
              echo ""

              # Install pre-commit hooks if prek is configured
              if [ -f .pre-commit-config.yaml ] && ! [ -f .git/hooks/pre-commit ]; then
                echo "Installing pre-commit hooks with prek..."
                prek install
              fi

              echo "Run 'make help' for available commands"
            '';
          };

          # Package definition
          packages.default = buildGoModule {
            pname = "tasmota-homekit";
            version = self.rev or "dev";

            src = ./.;

            vendorHash = "sha256-5wOD08f3SLM2N/TrSo062Swffrv8gdbaVA3O3+9cFo8=";

            ldflags = [
              "-s"
              "-w"
              "-X main.version=${self.rev or "dev"}"
            ];

            meta = with pkgs.lib; {
              description = "HomeKit bridge for Tasmota smart plugs";
              homepage = "https://github.com/kradalby/tasmota-homekit";
              license = licenses.mit;
              maintainers = [ ];
            };
          };

          # Alias for the package
          packages.tasmota-homekit = self.packages.${system}.default;
        }
      ) // {
      # NixOS module
      nixosModules.default = { config, lib, pkgs, ... }:
        with lib;
        let
          cfg = config.services.tasmota-homekit;
          package = self.packages.${pkgs.system}.default;
        in
        {
          options.services.tasmota-homekit = {
            enable = mkEnableOption "Tasmota HomeKit bridge service";

            package = mkOption {
              type = types.package;
              default = package;
              description = "The tasmota-homekit package to use";
            };

            openFirewall = mkOption {
              type = types.bool;
              default = false;
              description = ''
                Whether to automatically open the necessary ports in the firewall.
                Opens ports for HAP (HomeKit), Web interface, and MQTT broker.
              '';
            };

            ports = {
              hap = mkOption {
                type = types.port;
                default = 8080;
                description = "Port for the HomeKit Accessory Protocol (HAP) server";
              };

              web = mkOption {
                type = types.port;
                default = 8081;
                description = "Port for the web interface";
              };

              mqtt = mkOption {
                type = types.port;
                default = 1883;
                description = "Port for the embedded MQTT broker";
              };
            };

            hap = {
              pin = mkOption {
                type = types.str;
                default = "00102003";
                description = "HomeKit pairing PIN (8 digits)";
                example = "12345678";
              };

              storagePath = mkOption {
                type = types.str;
                default = "/var/lib/tasmota-homekit/hap";
                description = "Path to store HAP pairing data";
              };
            };

            plugsConfig = mkOption {
              type = types.path;
              description = "Path to the plugs configuration file (HuJSON format)";
              example = "/etc/tasmota-homekit/plugs.hujson";
            };

            tailscale = {
              hostname = mkOption {
                type = types.str;
                default = "tasmota-nefit";
                description = ''
                  Tailscale hostname to use for the service.
                  Tailscale is enabled when authKeyFile is set.
                '';
                example = "tasmota-homekit";
              };

              authKeyFile = mkOption {
                type = types.nullOr types.path;
                default = null;
                description = ''
                  Path to a file containing the Tailscale auth key.
                  When set, enables Tailscale integration for secure remote access.
                  The content will be passed to the service via the TASMOTA_HOMEKIT_TS_AUTHKEY environment variable.
                '';
                example = "/run/secrets/tailscale-authkey";
              };
            };

            environment = mkOption {
              type = types.attrsOf types.str;
              default = { };
              description = "Additional environment variables for the service";
              example = {
                TASMOTA_HOMEKIT_TS_HOSTNAME = "my-bridge";
              };
            };

            environmentFile = mkOption {
              type = types.nullOr types.path;
              default = null;
              description = "Environment file for additional configuration (e.g., secrets)";
              example = "/run/secrets/tasmota-homekit.env";
            };
          };

          config = mkIf cfg.enable {
            # Set environment variables from NixOS options
            services.tasmota-homekit.environment = {
              TASMOTA_HOMEKIT_HAP_PORT = mkDefault (toString cfg.ports.hap);
              TASMOTA_HOMEKIT_WEB_PORT = mkDefault (toString cfg.ports.web);
              TASMOTA_HOMEKIT_MQTT_PORT = mkDefault (toString cfg.ports.mqtt);
              TASMOTA_HOMEKIT_HAP_PIN = mkDefault cfg.hap.pin;
              TASMOTA_HOMEKIT_HAP_STORAGE_PATH = mkDefault cfg.hap.storagePath;
              TASMOTA_HOMEKIT_PLUGS_CONFIG = mkDefault (toString cfg.plugsConfig);
              TASMOTA_HOMEKIT_TS_HOSTNAME = mkDefault cfg.tailscale.hostname;
            };

            # Open firewall ports if requested
            networking.firewall = mkIf cfg.openFirewall {
              allowedTCPPorts = [
                cfg.ports.hap # HomeKit Accessory Protocol
                cfg.ports.web # Web interface
                cfg.ports.mqtt # MQTT broker
              ];
            };

            systemd.services.tasmota-homekit = {
              description = "Tasmota HomeKit Bridge";
              documentation = [ "https://github.com/kradalby/tasmota-homekit" ];

              # Ensure service starts automatically and waits for network
              wantedBy = [ "multi-user.target" ];
              wants = [ "network-online.target" ];
              after = [ "network-online.target" ];

              serviceConfig = {
                Type = "simple";
                Restart = "on-failure";
                RestartSec = "10s";
                StartLimitBurst = 5;
                StartLimitIntervalSec = 60;

                # User and group
                DynamicUser = true;
                User = "tasmota-homekit";
                Group = "tasmota-homekit";

                # Working directory and persistent storage
                StateDirectory = "tasmota-homekit";
                CacheDirectory = "tasmota-homekit";
                RuntimeDirectory = "tasmota-homekit";
                WorkingDirectory = "/var/lib/tasmota-homekit";

                # Capabilities - only what's needed for binding to privileged ports
                AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
                CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];

                # Security hardening - Filesystem
                NoNewPrivileges = true;
                PrivateTmp = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                ReadWritePaths = [ "/var/lib/tasmota-homekit" "/var/cache/tasmota-homekit" ];

                # Security hardening - Kernel
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectKernelLogs = true;
                ProtectControlGroups = true;

                # Security hardening - Process
                RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
                RestrictNamespaces = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                LockPersonality = true;
                PrivateDevices = true;
                ProtectClock = true;
                ProtectHostname = true;
                ProtectProc = "invisible";
                ProcSubset = "pid";
                RemoveIPC = true;

                # Security hardening - System calls
                SystemCallArchitectures = "native";
                SystemCallFilter = [ "@system-service" "~@privileged" "~@resources" ];

                # Logging
                StandardOutput = "journal";
                StandardError = "journal";
                SyslogIdentifier = "tasmota-homekit";
              } // (optionalAttrs (cfg.tailscale.authKeyFile != null) {
                LoadCredential = "tailscale-authkey:${cfg.tailscale.authKeyFile}";
              }) // (optionalAttrs (cfg.environmentFile != null) {
                EnvironmentFile = cfg.environmentFile;
              });

              environment = cfg.environment;

              script =
                if cfg.tailscale.authKeyFile != null then ''
                  export TASMOTA_HOMEKIT_TS_AUTHKEY=$(cat $CREDENTIALS_DIRECTORY/tailscale-authkey)
                  exec ${cfg.package}/bin/tasmota-homekit
                '' else ''
                  exec ${cfg.package}/bin/tasmota-homekit
                '';
            };
          };
        };
    };
}

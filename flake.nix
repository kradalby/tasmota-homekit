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
          packages.default = buildGoModule rec {
            pname = "tasmota-homekit";
            version = self.rev or "dev";

            src = ./.;

            vendorHash = "sha256-7NOPhiZocepjgHsEcSJGbKpqywYZOC7uo6179j+vih0=";

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

            environment = mkOption {
              type = types.attrsOf types.str;
              default = { };
              description = "Environment variables for the service";
              example = {
                TASMOTA_HOMEKIT_HAP_PIN = "12345678";
                TASMOTA_HOMEKIT_PLUGS_CONFIG = "/etc/tasmota-homekit/plugs.hujson";
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
            systemd.services.tasmota-homekit = {
              description = "Tasmota HomeKit Bridge";
              wantedBy = [ "multi-user.target" ];
              after = [ "network.target" ];

              serviceConfig = {
                Type = "simple";
                ExecStart = "${cfg.package}/bin/tasmota-homekit";
                Restart = "always";
                RestartSec = "10s";

                # Security hardening
                DynamicUser = true;
                StateDirectory = "tasmota-homekit";
                CacheDirectory = "tasmota-homekit";

                # Capabilities
                AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
                CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];

                # Additional security
                NoNewPrivileges = true;
                PrivateTmp = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectControlGroups = true;
              } // (optionalAttrs (cfg.environmentFile != null) {
                EnvironmentFile = cfg.environmentFile;
              });

              environment = cfg.environment;
            };
          };
        };
    };
}

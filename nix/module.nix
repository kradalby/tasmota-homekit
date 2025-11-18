{ config, lib, pkgs, ... }:

with lib;

let
  cfg = config.services.tasmota-homekit;
in
{
  options.services.tasmota-homekit = {
    enable = mkEnableOption "Tasmota HomeKit bridge service";

    package = mkOption {
      type = types.package;
      description = "The tasmota-homekit package to run.";
    };

    environmentFile = mkOption {
      type = types.nullOr types.path;
      default = null;
      description = "Optional environment file that provides TASMOTA_HOMEKIT_* variables.";
      example = "/run/secrets/tasmota-homekit.env";
    };

    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      description = "Additional environment variables to pass to the service.";
    };

    user = mkOption {
      type = types.str;
      default = "tasmota-homekit";
      description = "User account under which the service runs.";
    };

    group = mkOption {
      type = types.str;
      default = "tasmota-homekit";
      description = "Group under which the service runs.";
    };

    ports = {
      hap = mkOption {
        type = types.port;
        default = 8080;
        description = "Port for the HomeKit Accessory Protocol (HAP) server.";
      };

      web = mkOption {
        type = types.port;
        default = 8081;
        description = "Port for the web interface.";
      };

      mqtt = mkOption {
        type = types.port;
        default = 1883;
        description = "Port for the embedded MQTT broker.";
      };
    };

    hap = {
      pin = mkOption {
        type = types.str;
        default = "00102003";
        description = "HomeKit pairing PIN (8 digits).";
        example = "12345678";
      };

      storagePath = mkOption {
        type = types.path;
        default = "/var/lib/tasmota-homekit";
        description = "Directory for HAP pairing data and other persistent state.";
      };
    };

    plugsConfig = mkOption {
      type = types.path;
      description = "HuJSON configuration describing the managed plugs.";
      example = "/etc/tasmota-homekit/plugs.hujson";
    };

    log = {
      level = mkOption {
        type = types.enum [ "debug" "info" "warn" "error" ];
        default = "info";
        description = "Logging level for the service.";
      };

      format = mkOption {
        type = types.enum [ "json" "console" ];
        default = "json";
        description = "Logging format.";
      };
    };

    tailscale = {
      hostname = mkOption {
        type = types.str;
        default = "tasmota-homekit";
        description = "Hostname to advertise on Tailscale when enabled.";
      };

      authKeyFile = mkOption {
        type = types.nullOr types.path;
        default = null;
        description = ''
          Path to a file containing the Tailscale auth key. When set, the service
          exports TASMOTA_HOMEKIT_TS_AUTHKEY from the credential file.
        '';
        example = "/run/secrets/tailscale-authkey";
      };
    };

    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = "Open the service ports (HAP/Web/MQTT) and UDP 5353 for mDNS.";
    };
  };

  config = mkIf cfg.enable (mkMerge [
    {
      users.users.${cfg.user} = {
        isSystemUser = true;
        group = cfg.group;
        description = "Tasmota HomeKit service user";
        home = cfg.hap.storagePath;
        createHome = true;
      };

      users.groups.${cfg.group} = { };

      systemd.tmpfiles.rules = [
        "d ${cfg.hap.storagePath} 0750 ${cfg.user} ${cfg.group} - -"
      ];
    }

    (mkIf cfg.openFirewall {
      networking.firewall = {
        allowedTCPPorts = [
          cfg.ports.hap
          cfg.ports.web
          cfg.ports.mqtt
        ];
        allowedUDPPorts = [
          5353
        ];
      };
    })

    {
      systemd.services.tasmota-homekit =
        let
          envVars = {
            TASMOTA_HOMEKIT_HAP_PORT = toString cfg.ports.hap;
            TASMOTA_HOMEKIT_WEB_PORT = toString cfg.ports.web;
            TASMOTA_HOMEKIT_MQTT_PORT = toString cfg.ports.mqtt;
            TASMOTA_HOMEKIT_HAP_PIN = cfg.hap.pin;
            TASMOTA_HOMEKIT_HAP_STORAGE_PATH = cfg.hap.storagePath;
            TASMOTA_HOMEKIT_PLUGS_CONFIG = toString cfg.plugsConfig;
            TASMOTA_HOMEKIT_LOG_LEVEL = cfg.log.level;
            TASMOTA_HOMEKIT_LOG_FORMAT = cfg.log.format;
            TASMOTA_HOMEKIT_TS_HOSTNAME = cfg.tailscale.hostname;
          } // cfg.environment;

          tailscaleExport =
            optionalString (cfg.tailscale.authKeyFile != null) ''
              export TASMOTA_HOMEKIT_TS_AUTHKEY="$(cat "$CREDENTIALS_DIRECTORY/tailscale-authkey")"
            '';

          startScript = pkgs.writeShellScript "tasmota-homekit-start" ''
            set -euo pipefail
            ${tailscaleExport}
            exec ${cfg.package}/bin/tasmota-homekit
          '';
        in
        {
          description = "Tasmota HomeKit Bridge";
          documentation = [ "https://github.com/kradalby/tasmota-homekit" ];
          wantedBy = [ "multi-user.target" ];
          wants = [ "network-online.target" ];
          after = [ "network-online.target" ];

          environment = envVars;

          serviceConfig = {
            Type = "simple";
            ExecStart = startScript;
            User = cfg.user;
            Group = cfg.group;

            Restart = "on-failure";
            RestartSec = "10s";
            StartLimitIntervalSec = "5min";
            StartLimitBurst = 5;

            WorkingDirectory = cfg.hap.storagePath;

            StandardOutput = "journal";
            StandardError = "journal";
            SyslogIdentifier = "tasmota-homekit";

            NoNewPrivileges = true;
            PrivateTmp = true;
            ProtectSystem = "strict";
            ProtectHome = true;
            ReadWritePaths = [ cfg.hap.storagePath ];
            ProtectKernelTunables = true;
            ProtectKernelModules = true;
            ProtectKernelLogs = true;
            ProtectControlGroups = true;
            PrivateDevices = true;
            ProtectHostname = true;
            ProtectClock = true;
            RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
            RestrictNamespaces = true;
            RestrictRealtime = true;
            RestrictSUIDSGID = true;
            LockPersonality = true;
            RemoveIPC = true;
            ProtectProc = "invisible";
            ProcSubset = "pid";
            SystemCallArchitectures = "native";
            SystemCallFilter = [
              "@system-service"
              "~@privileged"
              "~@resources"
            ];
          }
          // (optionalAttrs (cfg.environmentFile != null) {
            EnvironmentFile = cfg.environmentFile;
          })
          // (optionalAttrs (cfg.tailscale.authKeyFile != null) {
            LoadCredential = "tailscale-authkey:${cfg.tailscale.authKeyFile}";
          });
        };
    }
  ]);
}

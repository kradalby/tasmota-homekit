{ pkgs, system, self }:

pkgs.testers.runNixOSTest {
  name = "tasmota-homekit-module";

  nodes.machine = { pkgs, ... }: {
    imports = [ self.nixosModules.default ];

    environment.etc."tasmota-homekit/plugs.hujson" = {
      text = ''
        {
          "plugs": [
            { "id": "plug-1", "name": "Test Plug", "address": "127.0.0.1" }
          ]
        }
      '';
      mode = "0644";
    };

    services.tasmota-homekit = {
      enable = true;
      package = self.packages.${system}.default;
      plugsConfig = "/etc/tasmota-homekit/plugs.hujson";
      openFirewall = true;
    };

    networking.firewall.enable = true;
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("tasmota-homekit.service")

    machine.succeed("systemctl is-active tasmota-homekit.service")

    machine.wait_for_open_port(8081)
    machine.wait_for_open_port(8080)
    machine.wait_for_open_port(1883)

    machine.succeed("curl -f http://localhost:8081/")

    output = machine.succeed("systemctl show tasmota-homekit.service -p User")
    assert "User=tasmota-homekit" in output
  '';
}

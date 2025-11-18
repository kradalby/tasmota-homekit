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
          };

          # Package definition
          packages.default = buildGoModule {
            pname = "tasmota-homekit";
            version = self.rev or "dev";

            src = ./.;
            subPackages = [ "./cmd/tasmota-homekit" ];
            vendorHash = "sha256-Ifb2XZsLrRWuW7zYvI9jTa9rk3bajRXVlwhVY/B2cbU=";

            ldflags = [
              "-s"
              "-w"
              "-X github.com/kradalby/tasmota-nefit.version=${self.rev or "dev"}"
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

          apps = {
            test = {
              type = "app";
              program = toString (pkgs.writeShellScript "test" ''
                set -euo pipefail
                echo "Running go test ./..."
                ${go}/bin/go test -v ./...
              '');
            };

            lint = {
              type = "app";
              program = toString (pkgs.writeShellScript "lint" ''
                set -euo pipefail
                echo "Running golangci-lint..."
                ${pkgs.golangci-lint}/bin/golangci-lint run ./...
              '');
            };

            test-race = {
              type = "app";
              program = toString (pkgs.writeShellScript "test-race" ''
                set -euo pipefail
                echo "Running go test -race ./..."
                ${go}/bin/go test -race ./...
              '');
            };

            coverage = {
              type = "app";
              program = toString (pkgs.writeShellScript "coverage" ''
                set -euo pipefail
                echo "Generating coverage report..."
                ${go}/bin/go test -coverprofile=coverage.out ./...
                ${go}/bin/go tool cover -html=coverage.out -o coverage.html
                echo "Coverage report written to coverage.html"
              '');
            };
          };

          checks = {
            package = self.packages.${system}.default;
            module-test = import ./nix/test.nix { inherit pkgs system self; };
          };
        }
      ) // {
      nixosModules.default = import ./nix/module.nix;
      overlays.default = final: prev: {
        tasmota-homekit = self.packages.${final.system}.default;
      };
    };
}

{
  description = "Tasmota HomeKit Bridge - Control Tasmota plugs via HomeKit";

  inputs = {
    nixpkgs.url = "nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      flake-checks,
    }:
    let
      # Rebuild the Go dev tools against the latest Go so the formatter and
      # linters can parse the language version go.mod targets. A gofumpt built
      # against an older Go rejects newer syntax outright, and goimports ships
      # wrapped with a `go` on PATH that must be at least the go.mod directive
      # or GOTOOLCHAIN=auto tries to fetch a toolchain from inside the
      # network-less treefmt sandbox.
      goToolsOverlay = _: prev: {
        gofumpt = prev.gofumpt.override { buildGoModule = prev.buildGoLatestModule; };
        gotools = prev.gotools.override {
          buildGoModule = prev.buildGoLatestModule;
          go = prev.go_latest;
        };
      };
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ goToolsOverlay ];
        };

        go = pkgs.go_latest;

        fc = flake-checks.lib;
        common = {
          inherit pkgs;
          root = ./.;
          pname = "tasmota-homekit";
          version = self.rev or "dev";
          vendorHash = "sha256-4MzzXP0vh4zfIeaauxXUG7DU8eoO44Zbl0NDJjqbKx4=";
          goPkg = go;
          embedDirs = [ ./assets ];
          # main_test.go reads this fixture by relative path.
          extraSrc = [ ./plugs.hujson.example ];
        };

      in
      {
        # Development shell
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            golangci-lint
            gopls
            gotools
            gofumpt
            go-tools
            delve

            # Nix tooling
            nixfmt

            # Pre-commit hooks
            prek
            prettier

            # Useful utilities
            git
            gnumake
          ];
        };

        # Package definition
        packages.default = fc.goBuild common;

        formatter = fc.formatter common;

        # Alias for the package
        packages.tasmota-homekit = self.packages.${system}.default;

        apps = {
          test = {
            type = "app";
            program = toString (
              pkgs.writeShellScript "test" ''
                set -euo pipefail
                echo "Running go test ./..."
                ${go}/bin/go test -v ./...
              ''
            );
          };

          lint = {
            type = "app";
            program = toString (
              pkgs.writeShellScript "lint" ''
                set -euo pipefail
                echo "Running golangci-lint..."
                ${pkgs.golangci-lint}/bin/golangci-lint run ./...
              ''
            );
          };

          test-race = {
            type = "app";
            program = toString (
              pkgs.writeShellScript "test-race" ''
                set -euo pipefail
                echo "Running go test -race ./..."
                ${go}/bin/go test -race ./...
              ''
            );
          };

          coverage = {
            type = "app";
            program = toString (
              pkgs.writeShellScript "coverage" ''
                set -euo pipefail
                echo "Generating coverage report..."
                ${go}/bin/go test -coverprofile=coverage.out ./...
                ${go}/bin/go tool cover -html=coverage.out -o coverage.html
                echo "Coverage report written to coverage.html"
              ''
            );
          };
        };

        checks = {
          build = fc.goBuild common;
          gotest = fc.goTest common;
          golangci-lint = fc.goLint common;
          formatting = fc.goFormat common;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
          module-test = import ./nix/test.nix { inherit pkgs system self; };
        };
      }
    )
    // {
      nixosModules.default = import ./nix/module.nix;
      overlays.default = final: prev: {
        tasmota-homekit = self.packages.${final.system}.default;
      };
    };
}

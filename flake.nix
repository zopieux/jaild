{
  description = "jaild - Dynamic bind-mount exposure for rootless bubblewrap jails";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      runtimePath = pkgs.lib.makeBinPath (
        with pkgs;
        [
          bubblewrap
          util-linux
          coreutils
          bash
          nix
          xdg-dbus-proxy
        ]
      );
      vmWebTest = import ./test/nixos-test.nix {
        inherit pkgs;
        nixosModule = self.nixosModules.default;
        interactiveWeb = true;
      };
      testVmWeb = pkgs.writeShellScriptBin "test-vm-web" ''
        exec ${vmWebTest.driver}/bin/nixos-test-driver "$@"
      '';
    in
    {
      packages.${system} = rec {
        jaild = pkgs.buildGoModule {
          pname = "jaild";
          version = "0.1.0";
          src = pkgs.lib.cleanSourceWith {
            src = self;
            filter =
              path: _type:
              let
                b = baseNameOf path;
              in
              b != "test" && b != "module.nix" && b != "flake.nix";
          };
          vendorHash = "sha256-pbA/AlBz3cQYRTMnQ/qBPcinYOKokrBLNhkbRTq54gE=";
          nativeBuildInputs = [ pkgs.makeWrapper ];
          postInstall = ''
            for bin in $out/bin/jail $out/bin/jaild; do
              wrapProgram "$bin" --prefix PATH : "${runtimePath}"
            done
          '';
        };
        default = jaild;
      };

      nixosModules = rec {
        jaild = import ./module.nix { inherit self; };
        default = jaild;
      };

      checks.${system}.integration = import ./test/nixos-test.nix {
        inherit pkgs;
        nixosModule = self.nixosModules.default;
      };

      devShells.${system}.default = pkgs.mkShell {
        inputsFrom = [ self.packages.${system}.jaild ];
        packages = with pkgs; [
          go
          gopls
          gotools
          go-tools
          delve
          bubblewrap
          util-linux
          xdg-dbus-proxy
          prettier
          testVmWeb
        ];
      };
    };
}

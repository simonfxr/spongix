{
  description = "Flake for nix-cache-proxy";

  inputs = {
    devshell.url = "github:numtide/devshell";
    inclusive.url = "github:input-output-hk/nix-inclusive";
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    utils.url = "github:kreisys/flake-utils";
    cicero.url = "github:input-output-hk/cicero";
  };

  outputs = { self, nixpkgs, utils, devshell, cicero, ... }@inputs:
    utils.lib.simpleFlake {
      systems = [ "x86_64-linux" ];
      inherit nixpkgs;

      preOverlays = [ devshell.overlay ];

      overlay = final: prev: {
        nix-cache-proxy = prev.callPackage ./package.nix {
          inherit (inputs.inclusive.lib) inclusive;
          rev = self.rev or "dirty";
        };
      };

      packages = { nix-cache-proxy }@pkgs:
        pkgs // {
          defaultPackage = nix-cache-proxy;
        };

      hydraJobs = { nix-cache-proxy }@pkgs: pkgs;

      nixosModules.nix-cache-proxy = import ./module.nix;

      devShell = { devshell }: devshell.fromTOML ./devshell.toml;

      extraOutputs.ciceroActions = cicero.lib.callActionsWithExtraArgs rec {
        inherit (cicero.lib) std;
        inherit (nixpkgs) lib;
        actionLib = import "${cicero}/action-lib.nix" { inherit std lib; };
      } ./cicero/actions;
    };
}

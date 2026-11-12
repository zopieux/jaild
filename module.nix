{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.jaild;
in
{
  options.services.jaild = {
    enable = lib.mkEnableOption "jaild rootless bubblewrap jail manager";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.jaild;
      description = "The jaild package to use.";
    };

    dataDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Directory for persisted jail state (defaults to $XDG_DATA_HOME/jaild/jails).";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    systemd.user.sockets.jaild = {
      description = "jaild Rootless Bubblewrap Jail Manager Socket";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "%t/jaild.sock";
        SocketMode = "0600";
      };
    };

    systemd.user.services.jaild = {
      description = "jaild Rootless Bubblewrap Jail Manager";
      requires = [ "jaild.socket" ];
      environment = lib.mkIf (cfg.dataDir != null) {
        JAILD_DATA_DIR = cfg.dataDir;
      };
      serviceConfig = {
        ExecStart = lib.getExe' cfg.package "jaild";
      };
    };
  };
}

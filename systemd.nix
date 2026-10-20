{ config, lib, ... }:
let
  cfg = config.services.go-mod-proxy;
in {
  options.services.go-mod-proxy = {
    enable = lib.mkEnableOption "goproxy caching Go module proxy";
    bin = lib.mkOption {
      type = lib.types.str;
      default = "/nix/persist/root/bin/goproxy";
      description = "Path to the goproxy binary.";
    };
    host = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "Address the proxy binds to (normally loopback, behind caddy).";
    };
    port = lib.mkOption {
      type = lib.types.port;
      default = 9090;
      description = "TCP port the proxy binds to.";
    };
    root = lib.mkOption {
      type = lib.types.str;
      default = "/nix/persist/cache/go/proxy";
      description = "Module cache directory; keep it separate from the stats state directory.";
    };
    neighbourCaches = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Base URLs of neighbouring goproxy instances asked in parallel before the upstream.";
    };
  };

  config = lib.mkIf cfg.enable {
    ##################
    #-=# SERVICES #=-#
    ##################
    services.caddy = {
      enable = true;
      virtualHosts."goproxy".extraConfig = "reverse_proxy ${cfg.host}:${toString cfg.port}";
    };

    #################
    #-=# SYSTEMD #=-#
    #################
    systemd.services.go-mod-proxy = {
      description = "goproxy caching Go module proxy";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      startLimitIntervalSec = 30;
      startLimitBurst = 3;
      environment = {
        BIND_ADDR = "${cfg.host}:${toString cfg.port}";
        CACHE_ROOT = cfg.root;
      } // lib.optionalAttrs (cfg.neighbourCaches != [ ]) {
        NEIGHBOUR_CACHES = lib.concatStringsSep "," cfg.neighbourCaches;
      };
      serviceConfig = {
        ExecStart = cfg.bin;
        StateDirectory = "goproxy";
        Type = "exec";
        Restart = "always";
        RestartSec = "2s";
        TimeoutStopSec = "15s";
        UMask = "0077";
        MemoryMax = "512M";
        TasksMax = 512;
        LimitNOFILE = 16384;
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ReadWritePaths = [
          "/var/lib/goproxy"
          cfg.root
        ];
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        ProcSubset = "pid";
        KeyringMode = "private";
        LockPersonality = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        RestrictNamespaces = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@aio"
          "~@keyring"
          "~@memlock"
          "~@setuid"
          "~@timer"
        ];
        CapabilityBoundingSet = "";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        SocketBindDeny = "any";
        SocketBindAllow = "tcp:${toString cfg.port}";
        # Egress deliberately unrestricted: fetching and verifying from the
        # upstream proxy and sum.golang.org is the service's job. Never add
        # IPAddressDeny/IPAddressAllow here: they filter the cgroup's egress
        # too, and an allow-list of loopback makes every upstream dial time
        # out ("dial tcp ...: i/o timeout"), breaking all cache misses.
      };
    };
  };
}

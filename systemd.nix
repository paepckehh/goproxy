# goproxy, caching go module proxy, NixOS module for the host infra flake.
#
# Persistent server mode, deliberately NOT the socket-activated zdash
# pattern: go toolchains across the fleet must find the proxy alive whenever
# a build starts (cold builds, CI jobs, laptops at odd hours), so the unit
# boots with the machine and stays resident. Idle cost is the in-memory
# index plus the stats ring, a few MB, a cheap price for instant cache hits.
# The long-term stats SQLite database (<root>/stats.db, WAL) rides the same
# persistent root: lifetime counters, minute-resolution history and the
# event log all survive reboots with the service.
#
# Consumption (host side, see ../nixos/server/all/go-mod-proxy01.nix):
#   services.go-mod-proxy = {
#     enable = true;
#     host = infra.localhost.ip;
#     port = infra.localhost.port.offset + <app-id>;
#   };
{
  config,
  lib,
  ...
}: let
  cfg = config.services.go-mod-proxy;
in {
  options.services.go-mod-proxy = {
    enable = lib.mkEnableOption "goproxy, caching go module proxy";

    bin = lib.mkOption {
      type = lib.types.path;
      default = /nix/persist/root/bin/goproxy;
      description = "deployed goproxy binary, house style: manual deploy to /nix/persist";
    };

    host = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1";
      description = "listen address, loopback behind the caddy reverse proxy";
    };

    port = lib.mkOption {
      type = lib.types.port;
      example = 7040;
      description = "localbind http port, infra.localhost.port.offset + app id";
    };

    root = lib.mkOption {
      type = lib.types.path;
      default = /nix/persist/cache/go/proxy;
      description = "persistent cache root: download/ layout, sumdb state, stats.db (SQLite, long-term stats)";
    };
  };

  config = lib.mkIf cfg.enable {
    #################
    #-=# SYSTEMD #=-#
    #################
    systemd.services.go-mod-proxy = {
      description = "goproxy caching Go module proxy";

      # Persistent server: up at boot, stays up for the fleet.
      wantedBy = ["multi-user.target"];
      wants = ["network-online.target"];
      after = ["network-online.target"];
      # Unit-level directive ([Unit] section, NOT serviceConfig): the
      # restart delay is the only rate limit, a dead proxy stalls every
      # cold build on the network.
      startLimitIntervalSec = 0;

      serviceConfig = {
        ExecStart = "${toString cfg.bin} -addr ${cfg.host}:${toString cfg.port} -root ${toString cfg.root}";
        # Fail the unit immediately when the binary cannot be execed.
        Type = "exec";
        # Fleet critical: always come back.
        Restart = "always";
        RestartSec = "2s";
        # The binary bounds its own graceful shutdown to 10s (SIGTERM stops
        # the listener, drains, flushes the stats SQLite WAL checkpoint), no
        # need to wait out the default 90s.
        TimeoutStopSec = "15s";
        # Cache stays private to the service owner.
        UMask = "0077";

        # --- resource usage ---
        # No Nice/CPUWeight/IOWeight demotion here, unlike a dashboard: cold
        # builds wait on this service, it runs at full scheduling priority.
        # Downloads stream to temp files and serving is sendfile backed, so
        # RAM holds only the in-memory index, stats and event ring.
        MemoryMax = "512M";
        # Go runtime threads plus serving headroom.
        TasksMax = 512;
        # Cached files plus concurrent toolchain connections.
        LimitNOFILE = 16384;

        # --- sandboxing / hardening ---
        NoNewPrivileges = true;
        # Entire file system hierarchy read-only, except the cache root.
        ProtectSystem = "strict";
        ReadWritePaths = ["${toString cfg.root}"];
        ProtectHome = true;
        PrivateTmp = true;
        # Pure network and file system service, no device nodes at all.
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
        # Pure Go, no JIT, the binary is cgo free.
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
        # AF_INET/AF_INET6: loopback clients, DNS, upstream https. AF_UNIX
        # for the runtime.
        RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];

        # --- network sandboxing ---
        # Egress is the job of this service (proxy.golang.org and
        # sum.golang.org via public CDNs), so there is no IP pinning like on
        # the zdash loopback dashboard. The service may however never LISTEN
        # on anything except its designated port: outbound connect() and the
        # implicit autobind of DNS and upstream requests stay unaffected.
        SocketBindDeny = "any";
        SocketBindAllow = "tcp:${toString cfg.port}";

        # Root is only kept as a stable owner identity for the persistent
        # cache across restarts, dynamic users would churn the owner every
        # boot. Every capability is dropped: the port is unprivileged, DAC
        # owner access is enough for the cache, no chown, no raw io.
        CapabilityBoundingSet = "";
      };
    };
  };
}

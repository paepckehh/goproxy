{...}: {
  ##################
  #-=# SERVICES #=-#
  ##################
  services.caddy = {
    enable = true;
    # target the systemd socket, not the service: the socket keeps
    # accepting (and re-spawns the service) even while it has idled out
    virtualHosts."goproxy".extraConfig = ''proxy 127.0.0.1:9091'';
  };

  #################
  #-=# SYSTEMD #=-#
  #################
  systemd = {
    sockets.go-mod-proxy = {
      description = "goproxy socket activated mode";
      wantedBy = ["sockets.target"];
      socketConfig.ListenStream = "127.0.0.1:9091";
    };
    services.go-mod-proxy = {
      description = "goproxy caching Go module proxy";
      # no wantedBy: the socket unit starts this service on demand
      wants = ["network-online.target"];
      after = ["network-online.target"];
      startLimitIntervalSec = 0;
      environment = {
        # fallback bind when no sockets were passed (manual/dev start)
        BIND_ADDR = "127.0.0.1:9090";
        SOCKET_ACTIVATION = "1";
        EXIT_ON_IDLE = "1";
      };
      serviceConfig = {
        ExecStart = "/nix/persist/root/bin/goproxy";
        Type = "exec";
        # idle exits are clean (exit 0): only crashes restart. an idle
        # exit with Restart=always would loop every ~32s forever
        Restart = "on-failure";
        RestartSec = "2s";
        TimeoutStopSec = "15s";
        UMask = "0077";
        MemoryMax = "512M";
        TasksMax = 512;
        LimitNOFILE = 16384;
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ReadWritePaths = ["/var/lib/goproxy" "/nix/persist/cache/go/proxy"];
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
        RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];
        SocketBindDeny = "any";
        SocketBindAllow = "tcp:9090";
        IPAddressDeny = ["any"];
        IPAddressAllow = ["127.0.0.0/8" "::1"];
      };
    };
  };
}

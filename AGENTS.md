# AGENTS.md

`paepcke.de/goproxy` — single-binary go proxy.
- modern single binary cgo runtime dependency free 
- fast: modern caching go mod proxy 
- secure: validates upstream signatures 
- cool: modern 3d annimated bussines like dashboard about packages, sources, caching hits / miss , used and safed upstream downstream bandwide, store size, plus long-term (24h to all-time) history charts  
- persistent: long-term stats in `<root>/stats.db`, SQLite via the pure-Go cgo-free driver `modernc.org/sqlite`; lifetime counters, minute-resolution buckets and the event log all survive reboots 

KEEP ALWAYS AGENTS.md and README.md up-to-date for correct documentation.

## Layout

```
cmd/goproxy          entrypoint: flags, env wiring, socket activation, idle exit,
                     graceful shutdown
internal/store       disk cache (standard download/ layout) + in-memory index
internal/sumdb       checksum-database verification (x/mod sumdb client)
internal/stats       lock-free counters, event ring, traffic buckets, recorder flush loop
internal/history      long-term stats in SQLite (pure-Go driver, WAL, minute buckets)
internal/proxy       GOPROXY protocol handlers (singleflight, verify, cache,
                     fetches detached from client contexts)
internal/ui          embedded dashboard (static/, vanilla JS, no external assets)
internal/systemd     LISTEN_FDS socket-activation listener intake (zero deps)
internal/version     version injected via -ldflags
systemd.nix          NixOS module: services.go-mod-proxy (persistent, hardened)
systemd-socket.nix   NixOS fragment: socket-activated on-demand mode
```

Storage model: zips stored as-received (already deflate-compressed) under
`<root>/download/<escaped module>/@v/`; signed checksum records as `<version>.sum`
sidecars; index fully in memory, rebuilt by parallel scan at startup. Long-term
stats persist in the stats database (SQLite, WAL), default `<root>/stats.db`,
overridable to an absolute path elsewhere via `-stats-db` / `STATS_DB` (the
systemd units use `/var/lib/goproxy/stats.db`): lifetime totals table, one
bucket per minute (requests/hits/misses/bytes + store-size snapshots, kept
forever) and the event log (pruned after 90d). The request hot path never
touches the DB: atomics + buffered channel, batched flush every 30s plus a
final flush+WAL checkpoint on shutdown; recorder errors surface as
`persistErrors` on the dashboard and never degrade proxying. Legacy pre-SQLite
`stats.json` snapshots migrate into `stats.db` on first start (renamed to
`stats.json.imported`).

Security: misses are verified against sum.golang.org (note signature + tlog
tree + tiles) before caching; mismatch refuses content (502); no record serves
unverified; checksum-db outage fails closed when `-verify=true`. The
`/sumdb/<db>/...` endpoint forwards paths verbatim to the configured database
only (its name must match, others get 404), so go clients can run their full
verification through the proxy. Shared singleflight fetches are detached from
the requesting client's context: one disconnecting client cannot abort the
download the other waiters share, and a completed download still warms the
cache.

## Socket activation (env contract)

Serving mode is resolved at startup: passed systemd sockets win, then an
explicit `-addr` flag, then `BIND_ADDR`, then `:5100`. `SOCKET_ACTIVATION=1`
declares socket-activated deployment (warns and falls back to binding when
no descriptors were actually passed). `EXIT_ON_IDLE=1` (socket-activated
mode only) exits cleanly once no request has been in flight for 30s — with
the final stats flush and WAL checkpoint — since systemd re-arms the socket
and restarts on demand. Combine it with `Restart=on-failure`, never
`Restart=always` (idle exit is success and would loop otherwise).

## Stats database location

`-stats-db` flag > `STATS_DB` env > `<root>/stats.db`. When set explicitly
the path MUST be absolute (startup fails otherwise) so deployments can
keep the SQLite database in persistent state storage (`/var/lib/goproxy`)
separate from the wipeable module cache root. The legacy `stats.json`
lookup follows the database directory.

## Releases

Public repo: github.com/paepckehh/goproxy. GoReleaser builds on every `v*`
tag push (`.github/workflows/release.yml` + `.goreleaser.yml`, same pattern
as ../zdash): cgo-free binaries for linux/freebsd/darwin amd64/arm64, tar.gz
archives, deb/rpm packages, checksums. Local dry run: `make release-snapshot`,
config check: `make release-check`.

## Deployment (NixOS)

`systemd.nix` is a NixOS module fragment (same consumption pattern as
../zdash/systemd.nix, host infra flake modules list) exposing
`services.go-mod-proxy.{enable,bin,host,port,root}`. It deliberately runs a
PERSISTENT server (`wantedBy = multi-user.target`, `Restart = always`,
network-online ordering), NOT socket activation: fleet toolchains must find
the proxy alive at any build time. Defaults: binary
`/nix/persist/root/bin/goproxy` (make build, manual deploy), cache root
`/nix/persist/cache/go/proxy`. Hardened: strict read-only FS plus
ReadWritePaths on the cache root, seccomp `@system-service` minus
{aio,keyring,memlock,setuid,timer}, zero capabilities (root kept only as
stable cache owner identity), SocketBind pinned to the single port (outbound
connect/DNS unaffected, no IP egress pinning since upstream fetch is the
job), MemoryMax 512M / TasksMax 512 / LimitNOFILE 16384, full kernel/proc/
keyring/namespace protection. The host side owns ingress (caddy vhost +
listen IP, see ../nixos/server/all/go-mod-proxy01.nix).

`systemd-socket.nix` is the alternative on-demand fragment: a socket unit
owns `127.0.0.1:9091` and passes the descriptor via LISTEN_FDS
(`SOCKET_ACTIVATION=1`); the service has no `wantedBy` (the socket triggers
it), uses `Restart = on-failure` (idle exits are clean, must not loop) and
`EXIT_ON_IDLE=1`; caddy targets the socket address, which keeps accepting
while the service is down; `BIND_ADDR=127.0.0.1:9090` is the fallback for
manual starts without passed sockets.

## Required Go skills

Always load `golang-how-to` before Go work here; typically also `golang-testing`,
`golang-concurrency`, `golang-error-handling`, `golang-code-style`.

## Fixed workflow — every task, no exceptions, ALWAYS: test, commit, push! ALWAYS, DO NOT ASK!

1. **Format + check**: `gofmt, go vet, go mod tidy -check`
2. **Build**: `go build -o goproxy ./cmd/goproxy`
3. **Test**: `make test` (to run all tests here, ensure its fully parallel).
4. **Generate**: make generate, include in commit 
6. **Commit**: `git add . && git commit -m '<useful commit message>'`.
7. **Tag**: bump the patch segment only, never reuse/move/delete a tag
   (the current series is `v0.1.x`): `git tag "$(git describe --tags --abbrev=0 | awk -F. -v OFS=. '{$NF++; print}')"`.
8. **Push**: `git pull && git pull --tags && git push && git push --tags`.

Notes: `-race` tests need `CGO_ENABLED=1` (dev/test only; the shipped binary is
built `CGO_ENABLED=0`). Run a local instance with
`./goproxy -addr 127.0.0.1:5100 -root /tmp/cache` and exercise it via
`GOPROXY=http://127.0.0.1:5100 go mod download <mod>@<ver>`; watch `/api/dashboard`
and `/api/history?range=24h` (also `7d`, `30d`, `1y`, `all`). Socket-activated
mode can be exercised without systemd: pass a listening fd as 3 with
`LISTEN_FDS=1`, `LISTEN_PID=$$`, `SOCKET_ACTIVATION=1`, `EXIT_ON_IDLE=1`
(internal/systemd/systemd_test.go re-executes the test binary this way).

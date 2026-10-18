# goproxy

A modern, fast, caching Go module proxy in a single cgo-free binary.

- **fast** — in-memory index over a standard `download/` cache layout, singleflight
  download dedup, atomic commits, `sendfile`-backed serving, negative caching
  of upstream 404s
- **secure** — every `.zip` and `.mod` is hash-verified against the signed
  sum.golang.org checksum database (Ed25519 note signature, transparency-log
  tree consistency and tile inclusion proofs); the signed record is kept
  on disk next to the module; tampered content is refused
- **cool** — an embedded, dependency-free 3D animated operations dashboard:
  hit ratio, bandwidth saved, upstream/downstream traffic, store usage,
  top modules, a live event feed, and a long-term history section (24h to
  all-time) backed by minute-resolution stats kept in SQLite

## Quick start

```sh
go build -o goproxy ./cmd/goproxy        # or: make build
./goproxy                                 # serves :5100, caches ./cache
```

Released binaries are built by GoReleaser on every `v*` tag push
(`.github/workflows/release.yml`, config in `.goreleaser.yml`):
linux/freebsd/darwin, amd64/arm64, tar.gz archives plus deb/rpm packages
(`make release-snapshot` for a local dry run).

Point your toolchain at it and open the dashboard:

```sh
export GOPROXY=http://localhost:5100
export GOSUMDB=sum.golang.org   # verification flows through the proxy, too
go mod download golang.org/x/mod@v0.42.0
```

Then visit <http://localhost:5100/> in a browser.

## Endpoints

| Route                          | Behavior                                              |
| ------------------------------ | ----------------------------------------------------- |
| `GET /<module>/@v/list`        | forwarded upstream (`no-store`)                       |
| `GET /<module>/@latest`        | forwarded upstream (`no-store`)                       |
| `GET /<module>/@v/<v>.info`    | cached, JSON-validated                                |
| `GET /<module>/@v/<v>.mod`     | cached, hash-verified against the checksum database   |
| `GET /<module>/@v/<v>.zip`     | cached, hash-verified against the checksum database   |
| `GET /sumdb/<db>/...`          | forwarded verbatim to the configured db only (404 otherwise) |
| `GET /`                        | operations dashboard                                  |
| `GET /api/dashboard`           | JSON: stats, store, modules, events (polled every 2s) |
| `GET /api/history?range=...`  | JSON: long-term stats series (`24h`/`7d`/`30d`/`1y`/`all`) |
| `GET /healthz`                 | liveness + version                                   |

## Configuration

```
-addr        (see below)                listen address
-root        cache                     cache directory
-upstream    https://proxy.golang.org  upstream module proxy
-sumdb       https://sum.golang.org    checksum database base url
-sumdb-key   (official key)            checksum database public key
-verify      true                      verify hashes before caching
-stats-db    <root>/stats.db            absolute path of the long-term stats
                                       database (env STATS_DB; must be
                                       absolute when set)
```

Environment variables (used by the systemd units; flags win over env):

```
BIND_ADDR=127.0.0.1:9090    listen address when self-binding
                            (-addr flag > BIND_ADDR > default :5100)
SOCKET_ACTIVATION=1         serve on the sockets systemd passed via the
                            LISTEN_FDS protocol; if none were passed, log a
                            warning and fall back to BIND_ADDR
EXIT_ON_IDLE=1             exit cleanly once no request has been in flight
                            for 30s; only honored in socket-activated mode,
                            where systemd re-arms the socket and restarts
                            the proxy on the next connection
STATS_DB=/var/lib/goproxy/stats.db
                            absolute path of the long-term stats database
                            (-stats-db flag > STATS_DB > <root>/stats.db);
                            must be absolute when set, so deployments can
                            keep stats separate from the module cache
```

### Socket activation

Under systemd the proxy can run socket-activated instead of resident:
the socket unit owns the listening socket, starts the service on the
first connection, passes the prepared descriptor via `LISTEN_FDS`, and
the service exits after 30s of no in-flight requests (`EXIT_ON_IDLE=1`).
The listening socket never goes away, so clients (and the caddy vhost,
which must target the socket address) always get served; stats are
flushed and `stats.db` checkpointed on every idle exit. Use
`Restart = on-failure` for the service: idle exits are successful, and
`Restart = always` would loop with `EXIT_ON_IDLE`.

## Storage model

```
<root>/download/<escaped module>/@v/<version>.info|.mod|.zip
<root>/download/<escaped module>/@v/<version>.sum    signed checksum record
<root>/sumdb/                                        verification state + tiles
<root>/stats.db                                      long-term stats (SQLite, WAL)
```

- Module zips are stored exactly as received — already deflate-compressed,
  which is the space-efficient form. No recompression, no waste.
- The layout is the standard GOPROXY download layout: the cache directory
  doubles as a `file://` proxy source for the go tool.
- The index lives entirely in memory and is rebuilt by a parallel directory
  walk at startup (~ms per 1000 files). Requests also fall back to a stat
  while the scan is still running, so there is no startup gap.

## Long-term stats persistence

Everything the dashboard shows survives reboots. `stats.db` is a SQLite
database written by the pure-Go, cgo-free driver (`modernc.org/sqlite`,
WAL journaling), so the single binary stays cgo-free. It holds:

- **lifetime totals** — restored at startup, continued, never reset
- **one bucket per minute** of requests, hits, misses, bytes up/down/saved,
  verifications and cache-store size snapshots; kept forever (~half a
  million rows per year), aggregated by the dashboard into 24h/7d/30d/1y
  and all-time series
- **the event log** — recent events for the dashboard feed, pruned after
  90 days (buckets and totals are kept forever)

The hot path never touches the database: counters are atomics, events
  flow through a buffered channel, and a collector goroutine flushes
  batches every 30 seconds plus once on shutdown (WAL checkpoint included).
  Recorder errors are counted and surfaced as `persistErrors` on the
  dashboard; they never degrade proxying. An existing pre-SQLite
  `stats.json` snapshot is migrated into `stats.db` automatically on first
  start.

## Security model

On a cache miss, goproxy fetches the signed checksum record for the module
version from sum.golang.org through the full `golang.org/x/mod/sumdb` client —
note signature, tree consistency, and tile-based inclusion proof are all
checked. The downloaded zip/mod hash is compared against the record:

- **match** — cached, marked verified, record stored as provenance
- **mismatch** — refused with `502`, never cached, flagged in the event feed
- **no record** (private/unpublished module) — served unverified, clearly marked
- **database unreachable, `verify=true`** — the request fails closed

## Development

```sh
make build      # versioned binary
make test       # parallel tests with race detector
make check      # gofmt, go vet, go mod tidy -diff
make generate   # go generate ./... (hook, currently no directives)
```

Layout: `cmd/goproxy` entrypoint; `internal/{store,sumdb,stats,history,proxy,ui,systemd,version}`.

- `store` — disk cache + in-memory index
- `sumdb` — checksum-database verification client
- `stats` — lock-free counters, events, traffic, recorder flush loop
- `history` — long-term stats in SQLite (pure-Go driver, cgo-free)
- `proxy` — GOPROXY protocol handlers
- `ui` — embedded dashboard (vanilla JS, zero external assets)

## NixOS / systemd deployment

`systemd.nix` is a NixOS module fragment for the host infra flake (same
pattern as the zdash module, but **persistent server mode, not
socket-activated** — the fleet's `GOPROXY` must resolve at any build time,
so the unit boots with the machine and stays resident):

```nix
# host module (see ../nixos/server/all/go-mod-proxy01.nix)
services.go-mod-proxy = {
  enable = true;
  host = "127.0.0.1";          # behind the caddy reverse proxy
  port = 7040;                 # infra.localhost.port.offset + app id
};
```

- **binary** `/nix/persist/root/bin/goproxy` (default, override `bin`) —
  `make build`, manual deploy
- **cache** `/nix/persist/cache/go/proxy` (default, override `root`) —
  survives reboots, standard `download/` layout
- **hardening**: whole FS read-only except the cache root, seccomp
  `@system-service` minus unneeded groups, zero capabilities (root only as a
  stable cache-owner identity), bind syscalls pinned to the one listen port
  (outbound `connect()`/DNS untouched), memory 512M cap, full
  kernel/proc/namespace/keyring isolation; egress is unrestricted by design
  — fetching from upstream is the job
- **availability**: `Restart = always` with no start-rate-limit,
  `network-online` ordering, bounded 15s stop matching the binary's 10s
  graceful-shutdown window

Ingress (caddy vhost, TLS, listen IP) is host-side configuration.

### Socket-activated alternative

`systemd-socket.nix` is the same fragment wired for on-demand operation:
a `systemd.sockets.go-mod-proxy` unit owns `127.0.0.1:9091` and starts
the service with the descriptor passed via `LISTEN_FDS`
(`SOCKET_ACTIVATION=1`). With `EXIT_ON_IDLE=1` the process serves the
queued connection(s), then exits 30s after the last in-flight request —
cleanly, with a final stats flush. `BIND_ADDR=127.0.0.1:9090` is only the
fallback for manual starts without passed sockets, and the caddy vhost
targets the socket (9091), which is accepting even while the service is
down. The service uses `Restart = on-failure`: crashes restart, idle
exits (code 0) stay down until the socket fires again.

## License

MIT.

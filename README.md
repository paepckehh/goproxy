<div align="center">

# goproxy

**A fast, checksum-verifying Go module proxy with a live operations dashboard.**

Single binary &middot; zero cgo &middot; SQLite-backed long-term statistics

[![Go Version](https://img.shields.io/github/go-mod/go-version/paepckehh/goproxy)](https://go.dev/)
[![Release](https://img.shields.io/github/v/release/paepckehh/goproxy)](https://github.com/paepckehh/goproxy/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/paepckehh/goproxy/golang.yml?branch=main)](https://github.com/paepckehh/goproxy/actions/workflows/golang.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/paepckehh/goproxy)](https://goreportcard.com/report/github.com/paepckehh/goproxy)
[![License](https://img.shields.io/github/license/paepckehh/goproxy)](./LICENSE)

[Quick start](#quick-start) &middot; [Install](#install) &middot; [Features](#features) &middot; [Configuration](#configuration) &middot; [API](#api-reference) &middot; [Deployment](#deployment)

</div>

---

Point your toolchain at goproxy and every module download flows through a
local cache. Point a browser at it and watch the cache work: hit ratio,
bandwidth saved, upstream and downstream traffic, top modules, a live event
feed, and long-term history charts from 24 hours to all time.

Everything a fleet of build machines needs, in one dependency-free binary.

## Features

| Category | What you get |
| --- | --- |
| **Fast** | In-memory index over a standard `download/` layout, singleflight download dedup, atomic cache commits, `sendfile`-backed serving, negative caching of upstream 404s |
| **Secure** | Every `.zip` and `.mod` is hash-verified against the signed sum.golang.org checksum database: Ed25519 note signature, transparency-log tree consistency, and tile inclusion proofs. Tampered content is refused, never cached |
| **Observable** | Embedded dashboard with zero external assets: hit ratio, bandwidth saved, traffic, store usage, top modules, live event feed, and 24h to all-time history charts |
| **Persistent** | Lifetime counters, minute-resolution buckets and the event log survive reboots in SQLite (pure-Go driver, WAL journaling) |
| **Deployable** | Socket activation with idle exit, hardened systemd units, NixOS modules, static cgo-free binary |
| **Honest failure** | Checksum-database outages fail closed (a missing tile is an outage, not an unknown module); recorder errors surface as warning pills on the dashboard (`persistErrors`, dropped events) and never degrade proxying |

## Quick start

```sh
./goproxy                                 # serves :5100, caches ./cache
```

```sh
export GOPROXY=http://localhost:5100
export GOSUMDB=sum.golang.org   # verification flows through the proxy, too
go mod download golang.org/x/mod@v0.42.0
```

Then open <http://localhost:5100/> in a browser.

## Install

Released binaries are built by [GoReleaser](https://goreleaser.com) on every
tag push: linux, freebsd and darwin, amd64 and arm64, plus deb/rpm packages
and checksums.

```sh
# pre-built tarball from the releases page
curl -sSLO https://github.com/paepckehh/goproxy/releases/download/v0.1.7/goproxy_v0.1.7_linux_amd64.tar.gz
tar xzf goproxy_v0.1.7_linux_amd64.tar.gz
sudo install -m 0755 goproxy_v0.1.7_linux_amd64/goproxy /usr/local/bin/goproxy

# or a package
sudo dpkg -i goproxy_0.1.7_amd64.deb

# or from source (Go >= 1.27)
git clone https://github.com/paepckehh/goproxy
cd goproxy && make build
```

Or run it on NixOS, see [Deployment](#deployment).

## Configuration

Flags (environment variables are used by the systemd units; flags win over env):

| Flag | Default | Description |
| --- | --- | --- |
| `-addr` | `:5100` (env `BIND_ADDR`) | listen address; systemd socket activation takes precedence |
| `-root` | `cache` | module cache directory |
| `-stats-db` | `<root>/stats.db` (env `STATS_DB`) | absolute path of the long-term stats database; must be absolute when set, so the database can live in `/var/lib` apart from the cache |
| `-upstream` | `https://proxy.golang.org` | upstream module proxy |
| `-sumdb` | `https://sum.golang.org` | checksum database base url |
| `-sumdb-key` | official key | checksum database public key |
| `-verify` | `true` | verify module hashes against the checksum database |

| Environment | Effect |
| --- | --- |
| `BIND_ADDR` | listen address when self-binding (`-addr` flag wins, then this, then `:5100`) |
| `STATS_DB` | stats database location (`-stats-db` flag wins, then this, then `<root>/stats.db`) |
| `SOCKET_ACTIVATION=1` | serve on the sockets systemd passed via the `LISTEN_FDS` protocol; if none were passed, log a warning and fall back to `BIND_ADDR` |
| `EXIT_ON_IDLE=1` | exit cleanly once no request has been in flight for 30s; only honored in socket-activated mode, where systemd re-arms the socket and restarts the proxy on the next connection |

## API reference

The GOPROXY protocol, the checksum database proxy, and the dashboard:

| Route | Behavior |
| --- | --- |
| `GET /<module>/@v/list` | forwarded upstream (`no-store`) |
| `GET /<module>/@latest` | forwarded upstream (`no-store`) |
| `GET /<module>/@v/<v>.info` | cached, JSON-validated |
| `GET /<module>/@v/<v>.mod` | cached, hash-verified against the checksum database |
| `GET /<module>/@v/<v>.zip` | cached, hash-verified against the checksum database |
| `GET /sumdb/<db>/...` | forwarded verbatim to the configured database only (404 otherwise), so go clients run their full verification through the proxy |
| `GET /` | operations dashboard (live, polls `/api/dashboard` every 2s) |
| `GET /api/dashboard` | JSON: stats, store, modules, events |
| `GET /api/history?range=24h` | long-term stats series (`24h`, `7d`, `30d`, `1y`, `all`) |
| `GET /healthz` | liveness plus version |

## Architecture

```
 go tool ──► goproxy ──► cache hit ──► sendfile from disk
              │
              └─ miss ─► singleflight fetch ──► upstream proxy
                            │
                            └─ verify against sum.golang.org
                               (note signature + tree + tiles)
                               └─ pass: cache, serve
                                  fail: refuse (502), never cache
```

| Package | Responsibility |
| --- | --- |
| `cmd/goproxy` | entrypoint: flags, env wiring, socket activation, idle exit, graceful shutdown |
| `internal/store` | disk cache (standard `download/` layout) plus in-memory index |
| `internal/sumdb` | checksum-database verification (`golang.org/x/mod/sumdb` client) |
| `internal/proxy` | GOPROXY protocol handlers: singleflight, verify, cache; fetches detached from client contexts |
| `internal/stats` | lock-free counters, event ring, traffic buckets, recorder flush loop |
| `internal/history` | long-term stats in SQLite (pure-Go driver, WAL, minute buckets) |
| `internal/ui` | embedded dashboard (static files, vanilla JS, no external assets) |
| `internal/systemd` | `LISTEN_FDS` socket-activation listener intake |
| `internal/version` | version injected via `-ldflags` |

Shared singleflight fetches are detached from the requesting client's
context: one disconnecting client cannot abort a download other waiters
share, and a completed download still warms the cache.

## Storage model

```
<root>/download/<escaped module>/@v/<version>.info|.mod|.zip
<root>/download/<escaped module>/@v/<version>.sum    signed checksum record
<root>/sumdb/                                        verification state + tiles
<root>/stats.db                                      long-term stats (SQLite, WAL)
```

- Module zips are stored exactly as received, already deflate-compressed,
  which is the space-efficient form. No recompression, no waste.
- The layout is the standard GOPROXY download layout: the cache directory
  doubles as a `file://` proxy source for the go tool.
- The index lives entirely in memory and is rebuilt by a parallel directory
  walk at startup (~ms per 1000 files). Requests also fall back to a stat
  while the scan is still running, so there is no startup gap.
- The stats database defaults to `<root>/stats.db` and can be moved anywhere
  absolute via `STATS_DB` / `-stats-db` (the systemd units use
  `/var/lib/goproxy/stats.db`), so wiping the cache never wipes the history.

## Verification model

On a cache miss, goproxy fetches the signed checksum record for the module
version from sum.golang.org through the full `golang.org/x/mod/sumdb`
client. Note signature, tree consistency, and tile-based inclusion proof
are all checked. The downloaded zip/mod hash is compared against the record:

| Case | Result |
| --- | --- |
| Match | cached, marked verified, record stored as provenance |
| Mismatch | refused with `502`, never cached, flagged in the event feed |
| No record (private or unpublished module) | served unverified, clearly marked |
| Record without a hash line for this kind (custom databases) | served unverified, never refused |
| Database unreachable with `verify=true` | the request fails closed |

## Long-term statistics

Everything the dashboard shows survives reboots. The stats database is a
SQLite file written by the pure-Go, cgo-free driver (`modernc.org/sqlite`,
WAL journaling), so the single binary stays cgo-free. It holds:

- **lifetime totals**: restored at startup, continued, never reset
- **one bucket per minute** of requests, hits, misses, bytes up/down/saved,
  verifications and cache-store size snapshots; kept forever (~half a
  million rows per year), aggregated into the 24h/7d/30d/1y and all-time
  dashboard series
- **the event log**: recent events for the dashboard feed, pruned after
  90 days (buckets and totals are kept forever)

The request hot path never touches the database: counters are atomics,
events flow through a buffered channel, and a collector goroutine flushes
batches every 30 seconds plus once on shutdown (WAL checkpoint included).
Recorder errors are counted and surfaced as `persistErrors` on the
dashboard; they never degrade proxying. A pre-SQLite `stats.json` snapshot
migrates into the database automatically on first start.

## Deployment

### systemd socket activation

Under systemd the proxy can run socket-activated instead of resident: the
socket unit owns the listening socket, starts the service on the first
connection, passes the prepared descriptor via `LISTEN_FDS`, and the
service exits after 30s of no in-flight requests (`EXIT_ON_IDLE=1`). The
listening socket never goes away, so clients (and the caddy vhost, which
must target the socket address) always get served; stats are flushed and
the database checkpointed on every idle exit. Use `Restart = on-failure`
for the service: idle exits are successful, and `Restart = always` would
loop with `EXIT_ON_IDLE`.

### NixOS

`systemd.nix` is a NixOS module fragment for the host infra flake. It
deliberately runs a PERSISTENT server (`wantedBy = multi-user.target`,
`Restart = always`, network-online ordering), not socket activation: fleet
toolchains must find the proxy alive at any build time.

```nix
# host module (see ../nixos/server/all/go-mod-proxy01.nix)
services.go-mod-proxy = {
  enable = true;
  host = "127.0.0.1";          # behind the caddy reverse proxy
  port = 7040;                 # infra.localhost.port.offset + app id
};
```

- **binary** `/nix/persist/root/bin/goproxy` (default, override `bin`):
  `make build`, manual deploy
- **cache** `/nix/persist/cache/go/proxy` (default, override `root`):
  survives reboots, standard `download/` layout
- **stats** `/var/lib/goproxy/stats.db` via `STATS_DB` +
  `StateDirectory = "goproxy"`, separate from the wipeable cache
- **hardening**: whole FS read-only except the cache root and state
  directory, seccomp `@system-service` minus unneeded groups, zero
  capabilities (root only as a stable cache-owner identity), bind syscalls
  pinned to the one listen port (outbound `connect()`/DNS untouched),
  memory 512M cap, full kernel/proc/namespace/keyring isolation; egress
  is unrestricted by design, fetching from upstream is the job
- **availability**: `Restart = always` with no start-rate-limit,
  `network-online` ordering, bounded 15s stop matching the binary's 10s
  graceful-shutdown window

`systemd-socket.nix` is the alternative on-demand fragment: a
`systemd.sockets.go-mod-proxy` unit owns `127.0.0.1:9091` and starts the
service with the descriptor passed via `LISTEN_FDS`
(`SOCKET_ACTIVATION=1`). With `EXIT_ON_IDLE=1` the process serves the
queued connections, then exits 30s after the last in-flight request,
cleanly, with a final stats flush. `BIND_ADDR=127.0.0.1:9090` is only the
fallback for manual starts without passed sockets; the caddy vhost
targets the socket (9091), which is accepting even while the service is
down. Ingress (caddy vhost, TLS, listen IP) is host-side configuration.

## Development

```sh
make build      # versioned binary
make test       # parallel tests with race detector
make check      # gofmt, go vet, go mod tidy -diff
make generate   # go generate ./...
make release-check     # validate .goreleaser.yml
make release-snapshot  # local GoReleaser dry run
```

Notes: `-race` tests need `CGO_ENABLED=1` (dev/test only; the shipped
binary is built `CGO_ENABLED=0`). Exercise a local instance with
`./goproxy -addr 127.0.0.1:5100 -root /tmp/cache` and
`GOPROXY=http://127.0.0.1:5100 go mod download <mod>@<ver>`, then watch
`/api/dashboard` and `/api/history?range=24h`. Socket-activated mode can
be exercised without systemd: pass a listening fd as 3 with
`LISTEN_FDS=1`, `LISTEN_PID=$$`, `SOCKET_ACTIVATION=1`, `EXIT_ON_IDLE=1`.

## License

[BSD-3-Clause](./LICENSE)

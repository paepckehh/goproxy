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
local cache. A fleet can chain proxies: a miss asks the configured neighbour
caches in parallel before it reaches the upstream, so warm caches are shared.
Point a browser at it and watch the cache work: hit ratio, bandwidth saved,
upstream, downstream and neighbour traffic, top modules, a live event feed,
and long-term history charts from 24 hours to all time.

Everything a fleet of build machines needs, in one dependency-free binary.

## Features

| Category | What you get |
| --- | --- |
| **Fast** | In-memory index over a standard `download/` layout, singleflight download dedup, atomic cache commits, `sendfile`-backed serving, negative caching of upstream 404s, parallel neighbour-cache lookup before the upstream |
| **Shared** | `NEIGHBOUR_CACHES` chains goproxy instances: a cache miss asks every peer in parallel (short header timeout), the first copy wins, the rest are cancelled, and only a full peer miss falls through to the upstream |
| **Secure** | Every `.zip` and `.mod` is hash-verified against the signed sum.golang.org checksum database: Ed25519 note signature, transparency-log tree consistency, and tile inclusion proofs. Tampered content is refused, never cached |
| **Observable** | Embedded dashboard with zero external assets, pushed live over one SSE stream (counters, cache table, events, history): hit ratio, bandwidth saved, traffic, store usage, a 3D top-modules chart with a fixed (non-rotating) name agenda, live event feed, and 24h to all-time history charts |
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
| `-root` | `cache` (env `CACHE_ROOT`) | module cache directory; the env var exists so deployments can put module storage on a dedicated path or disk, without touching the command line |
| `-upstream` | `https://proxy.golang.org` | upstream module proxy |
| `-sumdb` | `https://sum.golang.org` | checksum database base url |
| `-sumdb-key` | official key | checksum database public key |
| `-verify` | `true` | verify module hashes against the checksum database |

| Environment | Effect |
| --- | --- |
| `BIND_ADDR` | listen address when self-binding (`-addr` flag wins, then this, then `:5100`) |
| `CACHE_ROOT` | module cache directory, e.g. a dedicated disk (`-root` flag wins, then this, then `cache`) |
| `SOCKET_ACTIVATION=1` | serve on the sockets systemd passed via the `LISTEN_FDS` protocol; if none were passed, log a warning and fall back to `BIND_ADDR` |
| `EXIT_ON_IDLE=1` | exit cleanly once no request has been in flight for 30s; only honored in socket-activated mode, where systemd re-arms the socket and restarts the proxy on the next connection |
| `NEIGHBOUR_CACHES` | comma- or whitespace-separated base URLs of neighbouring goproxy instances (e.g. `http://goproxy1:9090,http://goproxy2:9090`); cache misses ask them all in parallel before the upstream |
| `DEBUG=1` | enable verbose per-request logging: one access line per request plus routine failure detail (upstream 404s, neighbour misses, checksum-db probes, unverified private modules). Without it the journal only carries startup state, warnings and security refusals |

## API reference

The GOPROXY protocol, the checksum database proxy, and the dashboard:

| Route | Behavior |
| --- | --- |
| `GET /<module>/@v/list` | forwarded upstream (`no-store`) |
| `GET /<module>/@latest` | forwarded upstream (`no-store`) |
| `GET /<module>/@v/<v>.info` | cached, JSON-validated |
| `GET /<module>/@v/<v>.mod` | cached, hash-verified against the checksum database |
| `GET /<module>/@v/<v>.zip` | cached, hash-verified against the checksum database; on a miss the neighbour caches are asked in parallel first |
| `GET /sumdb/<db>/...` | forwarded verbatim to the configured database only (404 otherwise), with the `/supported` probe answered locally, so go clients run their full verification through the proxy |
| `GET /` | operations dashboard (all updates pushed over SSE; polling only as a no-EventSource fallback) |
| `GET /api/dashboard` | JSON snapshot: stats, store, modules, events |
| `GET /api/events?range=24h` | Server-Sent Events stream: `events` seed, `store` cache table (only when changed), `stats` counters every 2s, `history` for the selected range every 30s, and one `event` message per new proxy event |
| `GET /api/history?range=24h` | long-term stats series (`24h`, `7d`, `30d`, `1y`, `all`) |
| `GET /healthz` | liveness plus version |

## Architecture

```
 go tool ──► goproxy ──► cache hit ──► sendfile from disk
              │
              └─ miss ─► singleflight fetch ──► neighbour caches (parallel)
                            │                     └─ first copy wins,
                            ├─ no peer has it ──► upstream proxy
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
| `internal/proxy` | GOPROXY protocol handlers: singleflight, neighbour-first fetch, verify, cache; fetches detached from client contexts |
| `internal/stats` | lock-free counters, event ring, traffic buckets, recorder flush loop |
| `internal/history` | long-term stats in SQLite (pure-Go driver, WAL, minute buckets) |
| `internal/ui` | embedded dashboard (static files, vanilla JS, no external assets) |
| `internal/systemd` | `LISTEN_FDS` socket-activation listener intake |
| `internal/version` | version injected via `-ldflags` |

Shared singleflight fetches are detached from the requesting client's
context: one disconnecting client cannot abort a download other waiters
share, and a completed download still warms the cache.

## Neighbour caches

`NEIGHBOUR_CACHES` points one goproxy at the cache of others. On a cache
miss the proxy asks every listed neighbour in parallel
(`http://goproxy1:9090,http://goproxy2:9090`); the first `200` wins and the
other lookups are cancelled, so one slow or dead peer cannot delay the
fetch (their headers are bounded to 5s, bodies stream freely). Only when no
neighbour has the file does the request continue to the upstream proxy.

Peer content is untrusted exactly like upstream content: it is verified
against the signed checksum database before it is cached or served, so a
misconfigured or hostile neighbour cannot inject code. The dashboard counts
those requests as **neighbour hits** (still a local cache miss, because the
local index was cold) and tracks bytes pulled from peers separately from
upstream bytes in the lifetime counters, the minute buckets and the history
views. A neighbour must not list itself; duplicate entries are ignored.

```sh
NEIGHBOUR_CACHES=http://goproxy1:9090,http://goproxy2:9090 ./goproxy
```

## Storage model

```
<root>/download/<escaped module>/@v/<version>.info|.mod|.zip
<root>/download/<escaped module>/@v/<version>.sum    signed checksum record
<root>/sumdb/                                        verification state + tiles
/var/lib/goproxy/stats.db                            long-term stats (SQLite, WAL)
<root>/stats.db                                      (only when the state dir does not
                                                     exist, e.g. unprivileged local runs)
```

- Module zips are stored exactly as received, already deflate-compressed,
  which is the space-efficient form. No recompression, no waste.
- The layout is the standard GOPROXY download layout: the cache directory
  doubles as a `file://` proxy source for the go tool.
- The index lives entirely in memory and is rebuilt by a parallel directory
  walk at startup (~ms per 1000 files). Requests also fall back to a stat
  while the scan is still running, so there is no startup gap.
- The stats database lives in the default state directory
  `/var/lib/goproxy` when it exists (systemd deployments create it via
  `StateDirectory`), otherwise next to the cache at `<root>/stats.db` for
  unprivileged runs. Wiping the module cache leaves a systemd-managed
  history untouched; the local fallback is lost with the cache.

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

- **lifetime totals**: restored at startup, continued, never reset;
  including neighbour-cache hits/misses and peer bandwidth
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
loop with `EXIT_ON_IDLE`. A bounded start rate limit
(`startLimitBurst = 3` per `startLimitIntervalSec = 30`) stops crash
loops: after three failed starts in 30 seconds the unit enters `failed`
instead of restarting forever.

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
  neighbourCaches = [         # optional: share warm caches across the fleet
    "http://goproxy1:9090"
    "http://goproxy2:9090"
  ];
};
```

- **binary** `/nix/persist/root/bin/goproxy` (default, override `bin`):
  `make build`, manual deploy
- **cache** `/nix/persist/cache/go/proxy` via `CACHE_ROOT`, on the dedicated
  cache disk:
  survives reboots, standard `download/` layout
- **stats** `/var/lib/goproxy/stats.db` via `StateDirectory = "goproxy"`
  (automatic, no configuration), separate from the wipeable cache
- **hardening**: whole FS read-only except the cache root and state
  directory, seccomp `@system-service` minus unneeded groups, zero
  capabilities (root only as a stable cache-owner identity), bind syscalls
  pinned to the one listen port (outbound `connect()`/DNS untouched),
  memory 512M cap, full kernel/proc/namespace/keyring isolation; egress
  is unrestricted by design, fetching from upstream is the job. Never add
  `IPAddressDeny`/`IPAddressAllow` here: they filter both directions on
  the cgroup, so denying everything but loopback makes every cache miss
  fail with `dial tcp ...: i/o timeout`
- **availability**: `Restart = always` with a bounded start rate limit
  (3 starts per 30s, then the unit enters `failed` instead of looping),
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
make test       # parallel tests with race detector (hermetic)
make test-e2e   # real upstream + sum.golang.org fetch, opt-in
make check      # gofmt, go vet, go mod tidy -diff
make generate   # go generate ./...
make release-check     # validate .goreleaser.yml
make release-snapshot  # local GoReleaser dry run
```

Notes: `-race` tests need `CGO_ENABLED=1` (dev/test only; the shipped
binary is built `CGO_ENABLED=0`). `make test-e2e` fetches
`rsc.io/quote@v1.5.2` from proxy.golang.org, verifies it against the
published sum.golang.org record, and drives the dashboard API and SSE feed
through the real handler tree; it skips without `GOPROXY_E2E=1` or network
access, so the regular suite stays hermetic. Exercise a local instance with
`./goproxy -addr 127.0.0.1:5100 -root /tmp/cache` and
`GOPROXY=http://127.0.0.1:5100 go mod download <mod>@<ver>`, then watch
`/api/dashboard` and `/api/history?range=24h`; the event feed streams
from `/api/events` (SSE). Socket-activated mode can
be exercised without systemd: pass a listening fd as 3 with
`LISTEN_FDS=1`, `LISTEN_PID=$$`, `SOCKET_ACTIVATION=1`, `EXIT_ON_IDLE=1`.

## License

[BSD-3-Clause](./LICENSE)

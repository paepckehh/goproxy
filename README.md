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
  top modules, and a live event feed

## Quick start

```sh
go build -o goproxy ./cmd/goproxy        # or: make build
./goproxy                                 # serves :5100, caches ./cache
```

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
| `GET /sumdb/<db>/...`          | transparently forwarded, lets clients verify fully    |
| `GET /`                        | operations dashboard                                  |
| `GET /api/dashboard`           | JSON: stats, store, modules, events (polled every 2s) |
| `GET /healthz`                 | liveness + version                                   |

## Configuration

```
-addr        :5100                     listen address
-root        cache                     cache directory
-upstream    https://proxy.golang.org  upstream module proxy
-sumdb       https://sum.golang.org    checksum database base url
-sumdb-key   (official key)            checksum database public key
-verify      true                      verify hashes before caching
```

## Storage model

```
<root>/download/<escaped module>/@v/<version>.info|.mod|.zip
<root>/download/<escaped module>/@v/<version>.sum    signed checksum record
<root>/sumdb/                                        verification state + tiles
<root>/stats.json                                    lifetime stats + event ring
```

- Module zips are stored exactly as received — already deflate-compressed,
  which is the space-efficient form. No recompression, no waste.
- The layout is the standard GOPROXY download layout: the cache directory
  doubles as a `file://` proxy source for the go tool.
- The index lives entirely in memory and is rebuilt by a parallel directory
  walk at startup (~ms per 1000 files). Requests also fall back to a stat
  while the scan is still running, so there is no startup gap.
- Lifetime counters and the recent event ring persist as an atomic JSON
  snapshot, so savings and history survive restarts.

No SQLite by design: the file system is the persistent record, the index is
derived state, and the snapshot covers the rest — the single binary stays
cgo-free and dependency-light (only `golang.org/x/mod` and `golang.org/x/sync`).

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

Layout: `cmd/goproxy` entrypoint; `internal/{store,sumdb,stats,proxy,ui,version}`.

- `store` — disk cache + in-memory index
- `sumdb` — checksum-database verification client
- `stats` — counters, events, traffic, snapshots
- `proxy` — GOPROXY protocol handlers
- `ui` — embedded dashboard (vanilla JS, zero external assets)

## License

MIT.

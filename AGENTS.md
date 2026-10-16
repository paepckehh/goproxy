# AGENTS.md

`paepcke.de/goproxy` — single-binary go proxy.
- modern single binary cgo runtime dependency free 
- fast: modern caching go mod proxy 
- secure: validates upstream signatures 
- cool: modern 3d annimated bussines like dashboard about packages, sources, caching hits / miss , used and safed upstream downstream bandwide, store size  

KEEP ALWAYS AGENTS.md and README.md up-to-date for correct documentation.

## Layout

```
cmd/goproxy          entrypoint: flags, wiring, graceful shutdown
internal/store       disk cache (standard download/ layout) + in-memory index
internal/sumdb       checksum-database verification (x/mod sumdb client)
internal/stats       counters, event ring, traffic buckets, JSON snapshot
internal/proxy       GOPROXY protocol handlers (singleflight, verify, cache)
internal/ui          embedded dashboard (static/, vanilla JS, no external assets)
internal/version     version injected via -ldflags
```

Storage model: zips stored as-received (already deflate-compressed) under
`<root>/download/<escaped module>/@v/`; signed checksum records as `<version>.sum`
sidecars; index fully in memory, rebuilt by parallel scan at startup; lifetime
stats + events persist via atomic `<root>/stats.json` snapshot. No SQLite — the
FS is the persistent record; revisit only if event history outgrows the ring.

Security: misses are verified against sum.golang.org (note signature + tlog
tree + tiles) before caching; mismatch refuses content (502); no record serves
unverified; checksum-db outage fails closed when `-verify=true`.

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
`GOPROXY=http://127.0.0.1:5100 go mod download <mod>@<ver>`; watch `/api/dashboard`.

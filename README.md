# fastproxy

A small Go HLS/MP4 proxy. Rewrites playlists, streams media with bounded buffers,
and reuses upstream connections. Uses uTLS and x/net/http2 for providers that
require a browser TLS handshake.

## Run

```sh
cp .env.example .env
# Set the frontend domains and matching SECRET_KEY in .env.
go build -o fastproxy .
./fastproxy
```

Raw URL mode: `/stream?url=<URL-encoded-source>&server=wave&ref=<URL-encoded-referer>`.
Or generate a frontend-compatible token:

```sh
./fastproxy encode "https://cdn.example/master.m3u8" "https://player.example" "wave"
# Request /m3u8/<token> (playlists) or /stream/<token> (media).
```

## Optional outbound proxies

```dotenv
UPSTREAM_PROXY_FILE=./proxies.txt
UPSTREAM_PROXY_DOMAINS=fetch.nexabloom.top
```

Put one proxy URL per line in `proxies.txt` (git-ignored):

```text
http://user:password@proxy-one.example:8080
http://user:password@proxy-two.example:8080
```

`UPSTREAM_PROXY` accepts one URL; `UPSTREAM_PROXIES` accepts a comma-separated list.
All sources are combined and deduplicated. Supported schemes: HTTP, HTTPS, SOCKS5,
SOCKS5H; bare `host:port` means HTTP. Percent-encode special characters in credentials.
Blank lines and `#` comments are allowed. Invalid settings fail startup without
logging credentials. Restart after editing the list.

A proxy is roughly 3x slower per segment than a direct fetch, so routing is:

- `UPSTREAM_PROXY_SERVERS` providers and `UPSTREAM_PROXY_DOMAINS` hosts/subdomains
  always use the pool; `UPSTREAM_PROXY_SERVERS=*` means all traffic. Leave both
  empty to use the pool only as a 429 fallback (recommended).
- Every other host goes **direct**. If it answers 429, only that request is
  replayed through the pool; the next one tries direct again. A 403 is not
  retried — that is a header/protocol problem a proxy can't fix.
- Exits are sticky per **viewer + host** (`CF-Connecting-IP` → `X-Forwarded-For`
  → peer), so one playback session keeps one warm tunnel and one IP while
  different viewers spread across the pool.
- A proxy that fails to connect or answers 407 is skipped for 30 seconds and the
  request retries once on the next exit (GET/HEAD only). A 429 through a proxy
  also retries once on the next exit.

Once a minute, hosts that returned 403/429/5xx or connection errors are logged
with counts (`upstream <host>: ...`) — check those before adding a host to the
forced list. Proxy endpoints must resolve to public IPs; trusted proxies must
also block private destinations when resolving target hosts remotely.

## Behavior

- One shared HTTP/1.1 client (Chrome-TLS HTTP/2 client only for pam), TLS session reuse.
- 256 KiB pooled copy buffers, cancellation on disconnect, and streaming backpressure.
- Playlist rewriting with a 10 MiB input limit and server-side redirects (up to 5).
- Range and conditional headers preserved; partial responses and errors aren't cached.
- Upstream `Retry-After` passed through. Cloudflare challenge responses aren't cached.
- Interrupted streams abort the response so truncated segments cannot look complete.
- No request queues or local cache. Proxy failover and direct-429 retries happen
  before streaming; interrupted streams get at most two validated continuation
  attempts. Replays stop at the original requested range boundary.

## Capacity

RAM settings recalculate at startup, including Linux cgroup limits. Leave the
capacity overrides unset when moving VPSes. The active-request cap is half of RAM
divided by ~1.5 MiB per stream (1365 on 4 GiB); there is no per-host connection
cap by default. At capacity, return uncached 503 with Retry-After: 1.

| Setting | Default |
|---|---|
| `PORT` / `BIND_ADDR` | `3847` / all interfaces |
| `SECRET_KEY` | **required** (16+ chars); signs tokens, must match anikage-api |
| `TOKEN_TTL_SEC` | `0` = tokens never expire; must be ≥ 0 |
| `ALLOWED_ORIGINS` | unrestricted; frontend domain allow-list, not authentication |
| `ALLOW_RAW_URL` | `0`; `1` enables unsigned `?url=` mode (local debugging only — open proxy) |
| `STREAM_BUFFER_KB` | `256`, clamped to 32–1024 |
| `MAX_CONCURRENT` | RAM-derived; explicit `0` means unlimited |
| `MAX_CONNS_PER_HOST` | unlimited (`0`) |
| `MAX_IDLE_CONNS` / `MAX_IDLE_CONNS_PER_HOST` | RAM-derived |
| `GOMEMLIMIT` | soft Go-runtime target of 70% detected RAM |
| `INSECURE_TLS` | off; `1` disables upstream certificate verification |
| `MASK_SEGMENT_TYPE` | `1` (legacy); `0` serves actual media types |

Animex URLs and its provider API key are configured in `animex_config.go`.
Edit that file and rebuild when these settings change; no `ANIMEX_API_KEY`
environment variable is needed. Nero calls the REST `/sources` endpoint because
its playlists are locked to the caller’s IP and User-Agent, then returns direct
CDN segment URLs. The GraphQL URL is included for future provider changes.

The memory target excludes kernel buffers and other processes. At 1 Gbps, an
800 Mbps working budget is roughly 200 continuous 4 Mbps streams through the VPS;
CDN cache hits can serve additional viewers without reaching it.

## CDN and deployment

Keep the existing [Caddy/systemd deployment](deploy/README.md). Cloudflare cache
rules must make the relevant routes eligible and respect origin Cache-Control:
segments are immutable; VOD/master playlists use longer freshness; live playlists
use about 2 seconds. Keep signed tokens/query strings in cache keys and ensure
playlist freshness does not outlast source URLs. Cache hits bypass the app's origin
allow-list, so enforce any required access rules at the edge.

The old live sample showed segment HITs but DYNAMIC playlists; check playlist cache
eligibility before attributing startup latency to Go. No proxy can guarantee zero
403/429 responses from third-party sources.

```sh
go test -race ./...
go test -run '^$' -bench 'ProxySegment|Rewrite|Payload|Extract' -benchmem
```


## Code layout and performance checks

Implementation files, unit/integration tests, and benchmarks live in the
repository root. Tests access private functions directly through Go’s standard
package-local testing layout. Build and deployment commands are unchanged.

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem -count=5
```

Initial optimization pass, October 7, 2026: Apple M4 Pro, Go 1.26.4 (medians, five
500 ms samples each, same fixtures before and after):

| CPU benchmark | Before | After | Allocation count before → after |
|---|---:|---:|---:|
| Rewrite 1,200 relative segment URLs | 972 µs | 830 µs | 21,625 → 13,221 |
| Rewrite 1,200 absolute segment URLs | 629 µs | 472 µs | 14,425 → 6,019 |
| Encode a segment token | 365 ns | 257 ns | 9 → 3 |
| Decode a segment token | 375 ns | 263 ns | 10 → 4 |
| Extract a routed path token | 36.8 ns | 8.18 ns | 2 → 0 |

Signing reuses keyed HMAC state through a pool, while retaining the existing
wire format and constant-time tag comparison. Playlist tags without embedded
URIs are copied as bytes. Route extraction uses string slices instead of split
arrays. Playlist benchmark memory fell from 2.68 to 2.04 MB for relative URLs
and from 2.26 to 1.63 MB for absolute URLs. Regression tests cover pooled signing
under concurrency, compatibility vectors, and interrupted stream recovery.

The 256 KiB stream buffer remains the default: local TCP buffer-size benchmarks
showed no consistent benefit from spending four times the memory on 1 MiB.
`BenchmarkProxySegment` measures the complete handler over local TCP;
`BenchmarkStreamPooledBuf` isolates the copy loop. These local measurements do
not predict Internet throughput or playback latency. In three 500 ms samples,
the complete handler's median was 759 → 711 µs for 4 MiB segments and
5.65 → 5.25 ms for 32 MiB segments. Sample ranges overlapped, so those results
support broadly similar streaming throughput, not a proven network speedup.
The complete handler used about eight fewer allocations per segment.

Further tuning needs a representative VPS load test: measure CPU, peak memory,
active connections, startup latency, and upstream latency with realistic viewer
concurrency. Compare upstream HTTP/1.1 and HTTP/2 on the actual providers before
changing protocol defaults. CDN hit rate and upstream bandwidth can dominate the
remaining cost. Active streams currently have no inactivity deadline;
`IdleTimeout` applies to idle downstream keep-alive connections.


### Additional profiling and testing

A second pass compared the previously optimized code against tighter playlist
buffer reservations and skipping the clock lookup for non-expiring tokens.
Measurements used the same fixtures, `ENV_FILE=/dev/null`, and five 500 ms
samples per benchmark, run sequentially without fuzzing or race tests alongside.

| Benchmark | Allocated bytes before → after | Median time before → after |
|---|---:|---:|
| 1,200 relative segment URLs | 2,043,477 → 1,657,792 | 789 → 792 µs |
| 1,200 absolute segment URLs | 1,631,085 → 1,245,870 | 426 → 427 µs |
| 6 relative segment URLs | 13,931 → 10,856 | 5.28 → 4.52 µs |
| 6 absolute segment URLs | 11,625 → 8,679 | 3.05 → 2.92 µs |
| Encode a non-expiring token | 400 → 400 | 252 → 223 ns |

Playlist rewriting allocates about 19–25% fewer bytes; large-playlist execution
time remains similar. Token encoding takes about 11% less time in the serial
benchmark and 6% less time in the parallel benchmark. These are local benchmark
results, not guarantees about playback speed or VPS capacity.

Five randomized-order race-test runs passed. Fuzzing completed 506,991 playlist
scanner cases and 378,595 token round-trip cases without failures. The scanner
fuzzer compares output against a simpler split/join reference, including malformed
input, CRLF, blank lines, multiple URI attributes, and replacements exceeding the
initial buffer estimate. An HTTP integration test checks that a viewer
disconnection cancels the upstream request and releases its capacity slot.

```sh
go test -race -shuffle=on -count=5 ./...
go test -run '^$' -fuzz '^FuzzRewritePlaylistLineScanner$' -fuzztime=15s -parallel=2
go test -run '^$' -fuzz '^FuzzPayloadRoundTrip$' -fuzztime=15s -parallel=2
```

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

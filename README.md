# fastproxy

A Go HLS/mp4 streaming proxy. Built because Bun (`../proxy`) can't bound
per-stream RAM — under a slow client it buffers the whole asset (~2–3 GB/stream,
measured), while Go's blocking `io.CopyBuffer` holds backpressure and stays flat
(~64 KB/stream).

## Run

```sh
go build -o fastproxy .
PORT=3847 SECRET_KEY=aproxy2026 ./fastproxy
```

Proxy any HLS stream directly (raw-URL mode, no token needed — URL-encode it):

```sh
http://localhost:3847/stream?url=https%3A%2F%2Fhost%2Fmaster.m3u8&ref=https%3A%2F%2Freferer.site
```

Or generate a token link (XOR+base64url, compatible with the Bun proxy / frontend):

```sh
./fastproxy encode "https://host/master.m3u8" "https://referer.site"
# -> paste after /stream/  ->  http://localhost:3847/stream/<token>
#    (3rd arg is the server name: ./fastproxy encode <url> <referer> uwu)
```

## Env vars

| var | default | meaning |
|-----|---------|---------|
| `PORT` | `3847` | listen port |
| `BIND_ADDR` | _(all interfaces)_ | listen interface; use `127.0.0.1` behind Caddy |
| `SECRET_KEY` | `aproxy2026` | payload XOR key (must match frontend) |
| `ALLOWED_ORIGINS` | _(open)_ | comma list of frontend domains allowed by Origin/Referer |
| `UPSTREAM_PROXY` | _(none)_ | legacy single egress proxy URL, e.g. `http://user:pass@host:port` |
| `UPSTREAM_PROXIES` | _(none)_ | comma/newline/space-separated egress proxy URLs |
| `UPSTREAM_PROXY_FILE` | _(none)_ | file with one egress proxy per line |
| `UPSTREAM_PROXY_SERVERS` | `uwu,kiwi,wave` | provider/server keys to route through the proxy pool; `*` = every token with a server key |
| `UPSTREAM_PROXY_DOMAINS` | _(none)_ | legacy host-suffix fallback for old cached tokens without a server key |
| `UPSTREAM_PROXY_RATE_COOLDOWN` | `60s` | how long a proxy sits out after a `429`/`403` |
| `UPSTREAM_PROXY_ERROR_COOLDOWN` | `5m` | how long a proxy sits out after a connection failure |
| `UPSTREAM_PROXY_5XX_COOLDOWN` | `15s` | how long a proxy sits out after a `502`/`503`/`504` |
| `MAX_CONCURRENT` | `0` | max simultaneous streams (0 = unlimited); excess gets `503` |
| `INSECURE_TLS` | _(off)_ | `1` to skip upstream cert verification (sketchy CDNs only) |
| `ALLOW_RAW_URL` | `1` | `0` to disable `?url=` mode and require XOR tokens |

Raw-URL mode takes `?url=`, `?ref=` and `?sv=` (the server name, same values as the token's third field).

The proxy pool rotates round-robin (even spread, and the retries for one request
walk distinct exits). A proxy that returns `429`/`403`, fails to connect, or 5xx's
is benched for its cooldown and skipped until it expires. If every proxy is
cooling down, the request still goes out through whichever is closest to
recovering, and the pool logs `upstream proxy pool drained: N/M healthy` at most
once every 30s.

Token layout, before the XOR: `<url> 0x00 <referer> 0x00 <server>`. The third field
is optional — two-field tokens from an older API build still decode and fall back
to hostname matching.

## What it does

- Takes the target from `?url=` (raw-URL mode) or decrypts it from the path, then forges browser-shaped headers plus the `Referer`/`Origin` that CDN actually accepts.
- Picks that origin from the **server name** the API sends with the token (`uwu`, `kiwi`, `wave`, …) — see `domains.go`. Providers are stable; their CDN hostnames rotate, so keying on the provider survives a hostname swap. A hostname table is the fallback for tokens minted without one, then the token's own referer, then the target's own origin.
- Speaks HTTP/1.1 by default (throughput), and HTTP/2 for the hosts that refuse h1 — `owocdn.top` answers a Cloudflare 403 to an h1 request and 200 to the byte-identical h2 one. The h2 client widens the flow-control window to just under 4 MiB so it doesn't pay the usual h2 throughput penalty.
- Rewrites HLS manifests (`.m3u8`) so every segment/key/variant routes back through us — detected by extension, content-type, or `#EXTM3U` sniffing (extension-less hosts).
- Streams everything else with bounded RAM (backpressured copy).
- Follows 3xx redirects server-side (up to 5 hops, SSRF-checked per hop) — no extra client round-trip per segment; POST falls back to a re-wrapped client redirect.
- Forwards conditional headers (`If-None-Match` etc.) so 304 revalidation works end-to-end.
- Wildcard CORS (no credentials/Vary) + cache headers tuned per kind: segments immutable-forever, master/VOD playlists 5m browser / 4h edge, live playlists ~2s → CDN-friendly without freezing live streams.

## Scaling

Put a CDN in front (the cache headers do the work), raise `ulimit -n`, and only
add instances behind a load balancer if one box's bandwidth saturates. See the
project notes for the full reasoning.

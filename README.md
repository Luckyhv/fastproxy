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
```

## Env vars

| var | default | meaning |
|-----|---------|---------|
| `PORT` | `3847` | listen port |
| `SECRET_KEY` | `aproxy2026` | payload XOR key (must match frontend) |
| `UPSTREAM_PROXY` | _(none)_ | egress proxy URL for blocked hosts, e.g. `http://user:pass@host:port` |
| `UPSTREAM_PROXY_DOMAINS` | _(none)_ | comma list of host suffixes to route via the proxy; empty + proxy set = all |
| `MAX_CONCURRENT` | `0` | max simultaneous streams (0 = unlimited); excess gets `503` |
| `INSECURE_TLS` | _(off)_ | `1` to skip upstream cert verification (sketchy CDNs only) |
| `ALLOW_RAW_URL` | `1` | `0` to disable `?url=` mode and require XOR tokens |

## What it does

- Takes the target from `?url=` (raw-URL mode) or decrypts it from the path, forges `Referer`/`Origin`.
- Rewrites HLS manifests (`.m3u8`) so every segment/key/variant routes back through us — detected by extension, content-type, or `#EXTM3U` sniffing (extension-less hosts).
- Streams everything else with bounded RAM (backpressured copy).
- Follows 3xx redirects server-side (up to 5 hops, SSRF-checked per hop) — no extra client round-trip per segment; POST falls back to a re-wrapped client redirect.
- Forwards conditional headers (`If-None-Match` etc.) so 304 revalidation works end-to-end.
- Wildcard CORS (no credentials/Vary) + cache headers tuned per kind: segments immutable-forever, master/VOD playlists 5m browser / 4h edge, live playlists ~2s → CDN-friendly without freezing live streams.

## Scaling

Put a CDN in front (the cache headers do the work), raise `ulimit -n`, and only
add instances behind a load balancer if one box's bandwidth saturates. See the
project notes for the full reasoning.

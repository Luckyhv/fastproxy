# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Go 1.24 HLS/media proxy (single `package main` in the repo root) deployed to a fleet of VPS boxes behind Cloudflare. See `../CLAUDE.md` for how it fits with the other repos.

## Commands

- `go build -o fastproxy .`, `go vet ./...`. No test suite.
- `./fastproxy encode <url> [referer] [server]` prints a signed token for manual testing.

## Config

- `.env` is loaded once through `getenv` (`env.go`); real env vars win. `.env` comments must be on their own line: text after a value becomes part of it.
- `SECRET_KEY` is required (16+ chars) and must match anikage-api. Tokens are HMAC-signed with no expiry by default; raw `?url=` stays off (`ALLOW_RAW_URL=0`) outside debugging.
- Leave the RAM-derived tuning vars (`MAX_CONCURRENT`, `GOMEMLIMIT`, …) unset.
- `.env.example`'s note that a 429 keeps a host proxied for 10 minutes is stale.

## Rules

- Egress is direct-first; a 429 is replayed once through the proxy pool, a 403 is never retried. Stickiness is per viewer + host, never per server. Force proxying by domain, not server name, and only after checking the per-minute `upstream <host>:` stats (proxy ≈ 3× slower).
- Origin/Referer/HTTP version are steered per server name (`domains.go`, `chrome.go`).
- Stream buffering goes through the writer-only wrapper on purpose: without it `io.Copy` uses `ReaderFrom` and the big pooled buffer is never used.
- Segment masking applies after the cache policy is chosen. Keep signed tokens inside cache keys.
- `ALLOWED_ORIGINS` is defense-in-depth only; Cloudflare cache hits bypass it.

## Deploy

systemd socket + service from `/opt/fastproxy` with `EnvironmentFile=/etc/fastproxy/fastproxy.env` (`deploy/`). `deploy/README.md` still describes the old `/root/projects` + Caddy layout; trust the unit files.

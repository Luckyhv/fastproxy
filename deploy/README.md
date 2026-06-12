# Deploying fastproxy

Reference for hosting fastproxy on a fresh Ubuntu VPS behind Cloudflare, with
Caddy as the TLS reverse proxy. This is the exact setup used for
`proxy.anicore.tv`.

```
browser → Cloudflare (public TLS, CDN cache) → Caddy :443 → fastproxy :3847
```

Files in this folder:

| file | purpose |
|------|---------|
| `Caddyfile` | Caddy reverse-proxy + TLS config |
| `fastproxy.service` | systemd unit that keeps fastproxy running |

---

## 1. Install Go (1.24+)

Ubuntu's `apt` Go is usually too old (the build needs the version in `go.mod`).
Install the official toolchain:

```sh
cd /tmp
curl -LO https://go.dev/dl/go1.24.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.4.linux-amd64.tar.gz
echo 'export PATH=/usr/local/go/bin:$PATH' >> ~/.bashrc
export PATH=/usr/local/go/bin:$PATH
go version          # -> go1.24.4
```

## 2. Clone and build

```sh
mkdir -p ~/projects && cd ~/projects
git clone https://github.com/Luckyhv/fastproxy.git
cd fastproxy
go build -o fastproxy .
```

## 3. Configure (.env)

`.env` is git-ignored — create it on the server:

```sh
cat > .env <<'EOF'
PORT=3847
SECRET_KEY=change-me
# Sites allowed to use the proxy (their Origin/Referer). Subdomains included.
# This is the FRONTEND domain, not the proxy's own domain. Empty = open.
ALLOWED_ORIGINS=yourfrontend.com,localhost
# Gates the metrics dashboard. Unset = /stats & /dashboard 404 (no exposure).
STATS_TOKEN=change-me-too
EOF
```

See the top-level `README.md` for the full env-var table.

## 4. Run under systemd

The unit assumes the binary at `/root/projects/fastproxy/fastproxy` running as
`root`. If your path or user differs, edit `User`, `Group`, `WorkingDirectory`,
and `ExecStart` in `fastproxy.service` first (and set `ProtectHome=false` when
the binary lives under a home directory).

```sh
sudo cp deploy/fastproxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fastproxy
sudo systemctl status fastproxy
curl -I http://127.0.0.1:3847/      # 403 = alive (no token / origin blocked)
```

## 5. Cloudflare DNS + origin certificate

1. **DNS:** add an `A` record `proxy` → server IP, **Proxied (orange cloud)**.
   The orange cloud is what keeps your real IP off public DNS.
2. **SSL/TLS → Overview:** set mode to **Full (strict)**.
3. **SSL/TLS → Origin Server → Create Certificate** (15-year, defaults). Save
   the two boxes on the server:

```sh
sudo mkdir -p /etc/ssl/cloudflare
sudo nano /etc/ssl/cloudflare/cert.pem   # paste "Origin Certificate"
sudo nano /etc/ssl/cloudflare/key.pem    # paste "Private Key"
sudo chmod 640 /etc/ssl/cloudflare/key.pem
sudo chgrp caddy /etc/ssl/cloudflare/cert.pem /etc/ssl/cloudflare/key.pem
```

> The `chgrp caddy` matters: Caddy runs as the `caddy` user and otherwise can't
> read a `root:root 600` key — that shows up as a Cloudflare **521**.

## 6. Install Caddy

```sh
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy

sudo cp deploy/Caddyfile /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl restart caddy
sudo systemctl status caddy
```

Edit the hostname in `Caddyfile` if you're not using `proxy.anicore.tv`.

## 7. Firewall — only let Cloudflare reach the box

Hides the origin: even if someone learns the IP, direct hits to 80/443 are
dropped. SSH stays open.

```sh
sudo apt install -y ufw
sudo ufw allow OpenSSH
for ip in $(curl -s https://www.cloudflare.com/ips-v4); do
  sudo ufw allow from $ip to any port 80,443 proto tcp; done
for ip in $(curl -s https://www.cloudflare.com/ips-v6); do
  sudo ufw allow from $ip to any port 80,443 proto tcp; done
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw enable
sudo ufw status numbered
```

## 8. Test end-to-end

From your **own machine** (not the VPS — the firewall blocks direct access):

```sh
curl -I https://proxy.anicore.tv/
```

- `403` → working (Cloudflare → Caddy → fastproxy, app rejected the empty token)
- `521` → Caddy down or cert unreadable by the `caddy` user (see step 5)
- `525/526` → SSL mode not "Full (strict)" or origin cert mismatch

---

## Updating

```sh
cd ~/projects/fastproxy
git pull
go build -o fastproxy .
sudo systemctl restart fastproxy
# if the Caddyfile changed:
sudo cp deploy/Caddyfile /etc/caddy/Caddyfile && sudo systemctl reload caddy
```

## Dashboard

fastproxy serves a built-in metrics dashboard (atomic counters, in-process —
near-zero cost). It rides through the same Caddy reverse proxy, no extra config.

- Set `STATS_TOKEN` in `.env` (any secret). Unset = `/stats` and `/dashboard`
  both `404`, so metrics are never exposed by accident.
- Open `https://proxy.anicore.tv/dashboard#<STATS_TOKEN>`. The token rides in the
  URL `#hash` (never sent to the server / logs); the page reads it and polls
  `/stats?token=…` over TLS every 2s.
- Tiles: active streams, downstream/upstream bandwidth (live B/s + totals),
  requests, manifests, redirects, upstream errors, rejected, 503s, heap,
  goroutines, uptime. Counters sit after the origin allow-list, so they reflect
  real frontend traffic, not anonymous scanner hits.

Both endpoints send `Cache-Control: no-store`, so Cloudflare never caches them.

## Caching

You don't need a cache in Caddy. fastproxy sets `immutable` cache headers on
segments, and **Cloudflare caches them at its edge** — that's the cache layer.

A local origin cache (the Souin `cache-handler` plugin) only helps on Cloudflare
cache-misses and requires building Caddy from source with `xcaddy`
(`xcaddy build --with github.com/caddyserver/cache-handler`) plus a `cache {}`
block in the Caddyfile. It also reintroduces the on-box memory/disk pressure this
proxy was written to avoid. Skip it unless you have a measured reason.

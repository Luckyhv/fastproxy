#!/usr/bin/env bash
# Move TLS from Caddy into fastproxy. Run with sudo from the folder holding
# fastproxy.socket + fastproxy.service. Rolls itself back if fastproxy does not
# answer on 443 afterwards. Needs a fastproxy binary that supports TLS_CERT_FILE.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run with sudo"; exit 1; }
HERE="$(cd "$(dirname "$0")" && pwd)"
ENV=/etc/fastproxy/fastproxy.env
CERT=/etc/ssl/fastproxy/cert.pem KEY=/etc/ssl/fastproxy/key.pem
grep -q TLS_CERT_FILE /opt/fastproxy/fastproxy || { echo "installed binary is too old (no TLS_CERT_FILE support)"; exit 1; }

cp -p "$ENV" "$ENV.bak-caddy"
cp -p /etc/systemd/system/fastproxy.service /etc/systemd/system/fastproxy.service.bak-caddy

rollback() {
  echo "FAILED — rolling back to Caddy"
  systemctl disable --now fastproxy.socket 2>/dev/null || true
  cp -p /etc/systemd/system/fastproxy.service.bak-caddy /etc/systemd/system/fastproxy.service
  cp -p "$ENV.bak-caddy" "$ENV"
  chgrp caddy "$CERT" "$KEY"
  systemctl daemon-reload
  systemctl restart fastproxy
  systemctl enable --now caddy
  exit 1
}

chgrp fastproxy "$CERT" "$KEY"
sed -i '/^TLS_CERT_FILE=/d;/^TLS_KEY_FILE=/d;/^BIND_ADDR=/d;/^PORT=/d' "$ENV"
printf 'TLS_CERT_FILE=%s\nTLS_KEY_FILE=%s\n' "$CERT" "$KEY" >> "$ENV"
install -m 0644 "$HERE/fastproxy.socket" /etc/systemd/system/fastproxy.socket
install -m 0644 "$HERE/fastproxy.service" /etc/systemd/system/fastproxy.service
systemctl daemon-reload

# The only gap: between Caddy releasing 443 and the socket taking it (<1s).
systemctl disable --now caddy
systemctl enable --now fastproxy.socket || rollback
systemctl restart fastproxy || rollback
sleep 2

code=$(curl -sk -o /dev/null -w '%{http_code}' --resolve og.bakayaro.live:443:127.0.0.1 https://og.bakayaro.live/health || echo 000)
if [ "$code" = 200 ] && systemctl is-active --quiet fastproxy; then
  echo "OK: fastproxy serves TLS on 443 directly (health=$code); Caddy is stopped and disabled"
  journalctl -u fastproxy -n 8 --no-pager
else
  rollback
fi

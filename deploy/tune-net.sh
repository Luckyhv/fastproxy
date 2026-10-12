#!/usr/bin/env bash
# Apply the streaming TCP tuning (BBR, no slow-start after idle, big socket
# buffers). Live and non-disruptive: affects new connections only, no restart.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run with sudo"; exit 1; }
CONF="$(dirname "$0")/99-fastproxy-net.conf"
install -m 0644 "$CONF" /etc/sysctl.d/99-fastproxy-net.conf
modprobe tcp_bbr
echo tcp_bbr > /etc/modules-load.d/bbr.conf
sysctl --system >/dev/null
sysctl net.ipv4.tcp_congestion_control net.ipv4.tcp_slow_start_after_idle net.core.wmem_max

#!/bin/sh
# Remove the btop hub agent from a Linux machine.
#
#   curl -fsSL __HUB_URL__/uninstall.sh | sudo sh
set -eu
[ "$(id -u)" = 0 ] || { echo "uninstall: run this with sudo" >&2; exit 1; }
systemctl disable --now btop-agent 2>/dev/null || true
rm -f /etc/systemd/system/btop-agent.service /usr/local/bin/btop-agent
rm -rf /etc/btop-agent
systemctl daemon-reload
echo "btop-agent removed"

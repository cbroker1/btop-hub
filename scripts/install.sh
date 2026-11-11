#!/bin/sh
# Install the btop hub agent on a Linux machine as a systemd service.
#
#   curl -fsSL __HUB_URL__/install.sh | sudo sh -s -- --token TOKEN --name "GPU rig"
#
# Options: --token TOKEN (required), --name NAME (default: hostname), --id ID (default:
# derived from the name), --hub URL (default: __HUB_URL__).
# Re-running upgrades in place. Remove with:  curl -fsSL __HUB_URL__/uninstall.sh | sudo sh
set -eu

HUB="__HUB_URL__"
SHA="__AGENT_SHA256__"
TOKEN=""
NAME=""
ID=""

die() { echo "install: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:-}"; shift 2 ;;
    --name) NAME="${2:-}"; shift 2 ;;
    --id) ID="${2:-}"; shift 2 ;;
    --hub) HUB="${2:-}"; shift 2 ;;
    *) die "unknown option $1" ;;
  esac
done

[ "$(id -u)" = 0 ] || die "run this with sudo"
command -v systemctl >/dev/null || die "systemd is required"
[ -n "$TOKEN" ] || die "--token is required (on the hub host: docker exec btop-hub /hub token)"
[ -n "$NAME" ] || NAME="$(hostname)"
[ -n "$ID" ] || ID="$(printf '%s' "$NAME" | tr 'A-Z' 'a-z' | sed 's/[^a-z0-9]\{1,\}/-/g; s/^-*//; s/-*$//' | cut -c1-32 | sed 's/-*$//')"
printf '%s' "$ID" | grep -Eq '^[a-z0-9][a-z0-9-]{0,31}$' || die "machine id '$ID' is not valid; pass --id"
[ "$(uname -m)" = x86_64 ] || die "only x86_64 builds are served by this hub"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
echo "downloading the agent from $HUB"
if command -v curl >/dev/null; then
  curl -fsSL "$HUB/dist/btop-agent-linux-amd64" -o "$tmp"
else
  wget -qO "$tmp" "$HUB/dist/btop-agent-linux-amd64"
fi
echo "$SHA  $tmp" | sha256sum -c --quiet - || die "checksum mismatch; not installing"

systemctl stop btop-agent 2>/dev/null || true
install -m 0755 "$tmp" /usr/local/bin/btop-agent
install -d -m 0755 /etc/btop-agent
umask 077
printf '%s\n' "$TOKEN" > /etc/btop-agent/token
chmod 0600 /etc/btop-agent/token
esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
printf '{"hub": "%s", "id": "%s", "name": "%s"}\n' "$(esc "$HUB")" "$(esc "$ID")" "$(esc "$NAME")" > /etc/btop-agent/agent.json
chmod 0644 /etc/btop-agent/agent.json
umask 022
if command -v curl >/dev/null; then
  curl -fsSL "$HUB/btop-agent.service" -o /etc/systemd/system/btop-agent.service
else
  wget -qO /etc/systemd/system/btop-agent.service "$HUB/btop-agent.service"
fi
systemctl daemon-reload
systemctl enable --now btop-agent
sleep 3
systemctl --no-pager --lines=5 status btop-agent || true
echo
echo "probe (paste this back if anything looks wrong):"
/usr/local/bin/btop-agent --probe --config /etc/btop-agent/agent.json --token-file /etc/btop-agent/token || true

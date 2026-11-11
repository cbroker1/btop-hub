#!/bin/sh
# Install (or upgrade) the btop hub agent on a Windows machine over SSH, using the hub's own
# install script, so you don't have to paste the command on the machine itself.
#
#   tools/install-windows-agent.sh tablet Z13
#   tools/install-windows-agent.sh gamingrig "Gaming rig"
#   tools/install-windows-agent.sh --uninstall tablet
#
# HOST is an ssh destination (a Host from ~/.ssh/config works). Its user must be an
# administrator: Windows OpenSSH gives administrators an elevated session, which the
# installer needs. The token is read from the hub container and never printed.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
bind=$(sed -n 's/^HUB_BIND=//p' "$here/.env" 2>/dev/null)
port=$(sed -n 's/^HUB_PORT=//p' "$here/.env" 2>/dev/null)
HUB=${HUB:-http://${bind:-192.168.1.74}:${port:-28677}}

usage() { echo "usage: $0 HOST NAME | $0 --uninstall HOST" >&2; exit 2; }

if [ "${1:-}" = "--uninstall" ]; then
  [ $# -eq 2 ] || usage
  host=$2
  ps="& ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('$HUB/uninstall.ps1')))"
else
  [ $# -eq 2 ] || usage
  host=$1
  name=$2
  case $name in
    *[!A-Za-z0-9\ ._-]*|"") echo "name may use letters, digits, spaces, '.', '_' and '-'" >&2; exit 2 ;;
  esac
  token=$(docker exec btop-hub /hub token)
  ps="& ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('$HUB/install.ps1'))) -Token '$token' -Name '$name'"
fi

# An encoded command survives ssh, cmd.exe and PowerShell quoting unchanged.
enc=$(printf '%s' "$ps" | iconv -f utf-8 -t utf-16le | base64 -w0)
exec ssh "$host" "powershell -NoProfile -NonInteractive -EncodedCommand $enc"

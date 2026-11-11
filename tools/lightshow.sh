#!/bin/sh
# Light show: a time-limited synthetic load on this machine and the Windows machines, for
# watching the btop hub light up. What it does to each machine, its limits and the files it
# leaves are in tools/lightshow/README.md.
#
#   tools/lightshow.sh start [--duration 20m] [--local-only] ...
#   tools/lightshow.sh status | stop | cleanup [--cache] | build
set -eu

usage() {
	cat <<'EOF'
usage: tools/lightshow.sh COMMAND [options]

  start      build the load generator, copy it to the Windows machines and start the show
  status     what is running, when it ends, the latest numbers from each machine
  stop       stop everything now, here and on the Windows machines, and remove the copies
  cleanup    remove what a show left behind: temp files, the copies, state and logs
  build      only build the binaries

start options:
  --duration D        how long: 90s, 15m, ... (default 20m, at most 25m)
  --hosts H1,H2       ssh destinations of the Windows machines (default tablet,gamingrig)
  --local-only        no Windows machines (same as --hosts none)
  --mem HOST=SIZE     memory wave peak; HOST "local" is this machine. Defaults: local=16G,
                      tablet=5G, gamingrig=10G, any other host 4G; 0 leaves memory alone
  --net HOST=MB/s     network peak to and from that Windows machine, each way
                      (defaults: tablet=30, gamingrig=40, any other host 30)
  --addr HOST=ADDR    reach HOST at ADDR this time (ssh -o HostName=ADDR), e.g.
                      --addr tablet=192.168.1.47 to stay on the LAN instead of Tailscale
  --no-gpu, --no-disk, --no-net
                      leave that part out
  --disk-dir DIR      where this machine's disk wave writes (default /var/tmp/btop-lightshow;
                      it must be on a disk, not tmpfs)

cleanup options:
  --cache             also remove the build cache (binaries and Go caches)
  --hosts H1,H2       Windows machines to clean when no show is recorded here

State and logs live in ${XDG_STATE_HOME:-~/.local/state}/btop-lightshow, binaries in
${XDG_CACHE_HOME:-~/.cache}/btop-lightshow. Builds run in the golang:1.27-bookworm image
and honour DOCKER_CONFIG.
EOF
}

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SELF=$ROOT/tools/lightshow.sh
STATE=${XDG_STATE_HOME:-$HOME/.local/state}/btop-lightshow
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/btop-lightshow
BIN=$CACHE/bin/lightshow
EXE=$CACHE/bin/lightshow.exe
IMAGE=golang:1.27-bookworm
MAX_SECS=1500 # the program never runs longer than 25 minutes either

say() { echo "lightshow: $*"; }
warn() { echo "lightshow: $*" >&2; }
die() {
	warn "$*"
	exit 1
}

# Paths end up single-quoted in generated scripts.
case $ROOT$STATE$CACHE in *\'*) die "paths containing a single quote are not supported" ;; esac

# Drops OpenSSH's post-quantum key exchange notice from output shown to the user.
quiet() { grep -v -E 'post-quantum|store now, decrypt later|server may need to be upgraded|openssh.com/pq.html' || true; }

# secs D: 90, 90s, 15m or 1h in seconds.
secs() {
	case $1 in '' | *[!0-9smh]*) return 1 ;; esac
	_n=${1%[smh]}
	case $_n in '' | *[!0-9]*) return 1 ;; esac
	_n=$(printf '%s' "$_n" | sed 's/^0*//')
	_n=${_n:-0}
	case $1 in
	*h) echo $((_n * 3600)) ;;
	*m) echo $((_n * 60)) ;;
	*) echo "$_n" ;;
	esac
}

# lookup KEY DEFAULT [KEY=VALUE...]: the last value given for KEY, else DEFAULT.
lookup() {
	_k=$1 _v=$2
	shift 2
	for _kv in "$@"; do
		case $_kv in "$_k="*) _v=${_kv#*=} ;; esac
	done
	printf '%s\n' "$_v"
}

default_mem() { case $1 in local) echo 16G ;; tablet) echo 5G ;; gamingrig) echo 10G ;; *) echo 4G ;; esac }
default_net() { case $1 in gamingrig) echo 40 ;; *) echo 30 ;; esac }

# hostlist "a,b" -> "a b" ("none" -> ""), checking each is a plain ssh destination.
hostlist() {
	_l=$(printf '%s' "$1" | tr ',' ' ')
	[ "$_l" != none ] || _l=
	for _h in $_l; do
		case $_h in *[!A-Za-z0-9._@-]*) die "not an ssh destination: $_h" ;; esac
	done
	echo $_l
}

# kv OPTION HOST=VALUE ALLOWED-CHARS: checks the shape of a per-host option.
kv() {
	case $2 in *=*) ;; *) die "$1 wants HOST=VALUE, not $2" ;; esac
	case ${2%%=*} in '' | *[!A-Za-z0-9._@-]*) die "$1: not an ssh destination: ${2%%=*}" ;; esac
	case ${2#*=} in '' | *[!$3]*) die "$1: bad value in $2" ;; esac
}

# ssh_opts HOST: how this show reaches HOST (ADDRS holds the --addr choices).
ssh_opts() {
	_a=$(lookup "$1" "" ${ADDRS:-})
	printf '%s' "-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15"
	[ -z "$_a" ] || printf ' %s' "-o HostName=$_a"
}

# rsh HOST COMMAND: a short command in the Windows machine's cmd.exe (stdin is not used).
rsh() {
	_h=$1
	shift
	# shellcheck disable=SC2046 # the options are meant to split
	ssh -n $(ssh_opts "$_h") "$_h" "$@"
}

# remote_dir HOST: the machine's %TEMP%\btop-lightshow. Fails when HOST doesn't answer, its
# ssh shell isn't cmd.exe (PowerShell leaves %TEMP% alone and would mangle the byte pipes), or
# the path has characters that would need quoting.
remote_dir() {
	_t=$(rsh "$1" 'echo %TEMP%' 2>/dev/null | tr -d '\r' | sed -n '$p' | sed 's/[[:space:]]*$//')
	case $_t in [A-Za-z]:\\*) ;; *) return 1 ;; esac
	case $_t in *' '* | *[\'\"%\&\|\<\>\^]*) return 1 ;; esac
	printf '%s\\btop-lightshow\n' "$_t"
}

remote_count() {
	rsh "$1" 'tasklist /FI "IMAGENAME eq lightshow.exe" /NH' 2>/dev/null | tr -d '\r' | grep -c -i '^lightshow\.exe' || true
}

# deploy HOST DIR: copies lightshow.exe there.
deploy() {
	[ "$(remote_count "$1")" = 0 ] || {
		warn "$1: lightshow.exe is already running there ('$SELF stop' stops it)"
		return 1
	}
	rsh "$1" "if not exist $2 mkdir $2" >/dev/null 2>&1 || return 1
	# shellcheck disable=SC2046
	if ! scp -q $(ssh_opts "$1") "$EXE" "$1:/$(printf '%s' "$2" | tr '\\' '/')/lightshow.exe" 2>"$STATE/scp.err"; then
		quiet <"$STATE/scp.err" >&2
		return 1
	fi
}

# piece NAME COMMAND: runs COMMAND (sh syntax) in a session and process group of its own,
# logging to logs/NAME.log. The group's ID goes to pgid.NAME before COMMAND starts.
piece() {
	_f=$STATE/pieces.d/$1.sh
	printf '%s\n' "echo \$\$ > '$STATE/pgid.$1'" "$2" >"$_f"
	setsid sh "$_f" >"$STATE/logs/$1.log" 2>&1 </dev/null &
	printf '%s\n' "$1" >>"$STATE/pieces"
}

# members PGID: how many processes of this tool are in process group PGID. Only the recorded
# group is looked at, so nothing else (this command included) can be picked up.
members() {
	ps -ww -eo pgid=,args= | awk -v g="$1" -v s="$STATE" -v b="$BIN" \
		'$1 == g && (index($0, s) || index($0, b) || index($0, "btop-lightshow")) { n++ } END { print n + 0 }'
}
group_size() { ps -eo pgid= | awk -v g="$1" '$1 == g { n++ } END { print n + 0 }'; }
recorded() { # PGIDs of the recorded pieces still alive, except the end-of-show cleanup timer if $1 = skip-timer
	[ -f "$STATE/pieces" ] || return 0
	for _p in $(cat "$STATE/pieces"); do
		[ "${1:-}" != skip-timer ] || [ "$_p" != cleanup-later ] || continue
		_g=$(cat "$STATE/pgid.$_p" 2>/dev/null || true)
		case $_g in '' | *[!0-9]*) continue ;; esac
		[ "$(members "$_g")" -eq 0 ] || echo "$_g"
	done
}
running() { [ -n "$(recorded skip-timer)" ]; }

# kill_groups PGID...: TERM, up to 5 s to finish (the run deletes its disk wave file), then KILL.
kill_groups() {
	[ $# -gt 0 ] || return 0
	for _g in "$@"; do kill -TERM -"$_g" 2>/dev/null || true; done # dash: -PGID, not -- -PGID
	_i=0
	while [ $_i -lt 50 ]; do
		_left=
		for _g in "$@"; do [ "$(group_size "$_g")" -eq 0 ] || _left=1; done
		[ -n "$_left" ] || return 0
		sleep 0.1
		_i=$((_i + 1))
	done
	for _g in "$@"; do kill -KILL -"$_g" 2>/dev/null || true; done
}

# clean_temp: stops lightshow.exe on the recorded Windows machines and removes their copies,
# then this machine's disk wave directory.
clean_temp() {
	if [ -f "$STATE/hosts" ]; then
		while read -r _h _d; do
			[ -n "$_h" ] || continue
			if rsh "$_h" "taskkill /IM lightshow.exe /F >NUL 2>&1 & ping -n 2 127.0.0.1 >NUL & if exist $_d rmdir /S /Q $_d" >/dev/null 2>&1; then
				say "$_h: removed $_d"
			else
				warn "$_h: unreachable, $_d is still there ('$SELF cleanup' again later)"
			fi
		done <"$STATE/hosts"
	fi
	_dd=${DISKDIR:-/var/tmp/btop-lightshow}
	if [ -d "$_dd" ]; then
		rm -f "$_dd"/wave-*.tmp
		rmdir "$_dd" 2>/dev/null || true
	fi
}

load_state() {
	END=0 HOSTS= ADDRS= DISKDIR=
	[ ! -f "$STATE/run.env" ] || . "$STATE/run.env"
}

hhmm() { date -u -d "@$1" +%H:%M:%SZ; }

cmd_build() {
	command -v docker >/dev/null 2>&1 || die "docker is needed to build the load generator"
	mkdir -p "$CACHE/bin" "$CACHE/go" "$CACHE/home"
	for _t in linux:lightshow windows:lightshow.exe; do
		_os=${_t%%:*} _out=${_t#*:}
		docker run --rm --user "$(id -u):$(id -g)" -v "$ROOT:/src:ro" -v "$CACHE:/cache" \
			-e GOCACHE=/cache/go/build -e GOMODCACHE=/cache/go/mod -e HOME=/cache/home \
			-e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 -e GOOS="$_os" -e GOARCH=amd64 \
			-w /src "$IMAGE" go build -trimpath -ldflags='-s -w' -o "/cache/bin/$_out" ./tools/lightshow ||
			die "building for $_os failed. If pulling $IMAGE failed on a credential helper, point DOCKER_CONFIG at a directory holding an empty config.json and try again"
	done
	say "built $BIN and $EXE"
}

cmd_start() {
	_dur=20m _hosts="tablet gamingrig" _gpu= _disk=1 _net=1
	MEMS= NETS= ADDRS= DISKDIR=/var/tmp/btop-lightshow
	while [ $# -gt 0 ]; do
		case $1 in
		--duration | --hosts | --mem | --net | --addr | --disk-dir) [ $# -ge 2 ] || die "$1 needs a value" ;;
		esac
		case $1 in
		--duration) _dur=$2 && shift ;;
		--hosts)
			_hosts=$(hostlist "$2") || exit 1
			shift
			;;
		--local-only) _hosts= ;;
		--mem) kv --mem "$2" '0-9.MGmg' && MEMS="$MEMS $2" && shift ;;
		--net) kv --net "$2" '0-9.' && NETS="$NETS $2" && shift ;;
		--addr) kv --addr "$2" 'A-Za-z0-9._:-' && ADDRS="$ADDRS $2" && shift ;;
		--no-gpu) _gpu=--no-gpu ;;
		--no-disk) _disk= ;;
		--no-net) _net= ;;
		--disk-dir) DISKDIR=$2 && shift ;;
		-h | --help) usage && exit 0 ;;
		*) die "start: unknown option $1 (see --help)" ;;
		esac
		shift
	done
	_secs=$(secs "$_dur") || die "--duration $_dur: give seconds, or a number with s, m or h"
	[ "$_secs" -ge 5 ] && [ "$_secs" -le $MAX_SECS ] || die "--duration must be between 5s and 25m"
	command -v setsid >/dev/null 2>&1 || die "setsid (util-linux) is needed"
	running && die "a show is already running; '$SELF stop' first"

	_ldisk=$_disk
	if [ -n "$_ldisk" ]; then
		case $DISKDIR in /*) ;; *) die "--disk-dir must be an absolute path" ;; esac
		case $DISKDIR in *\'*) die "--disk-dir cannot contain a single quote" ;; esac
		_p=$DISKDIR
		while [ ! -d "$_p" ]; do _p=$(dirname "$_p"); done
		_fs=$(df -P -T "$_p" 2>/dev/null | awk 'NR == 2 { print $2 }')
		case $_fs in tmpfs | ramfs | '')
			warn "$DISKDIR is on ${_fs:-an unknown filesystem}, not a disk: leaving out this machine's disk wave (--disk-dir picks another place)"
			_ldisk=
			;;
		esac
	fi

	cmd_build
	# A previous show's end-of-show cleanup timer must not fire into this one.
	kill_groups $(recorded)
	rm -rf "$STATE"
	mkdir -p "$STATE/logs" "$STATE/pieces.d"
	: >"$STATE/hosts"
	: >"$STATE/pieces"

	_ok=
	for _h in $_hosts; do
		if ! _d=$(remote_dir "$_h"); then
			warn "$_h: leaving it out: no answer over ssh, its ssh shell isn't cmd.exe, or its %TEMP% has spaces or quotes"
			continue
		fi
		printf '%s %s\n' "$_h" "$_d" >>"$STATE/hosts"
		if ! deploy "$_h" "$_d"; then
			warn "$_h: leaving it out: could not copy lightshow.exe to $_d"
			continue
		fi
		_ok="$_ok $_h"
	done
	_ok=${_ok# }

	END=$(($(date +%s) + _secs))
	{
		echo "END=$END"
		echo "HOSTS='$_ok'"
		echo "ADDRS='${ADDRS# }'"
		echo "DISKDIR='$DISKDIR'"
	} >"$STATE/run.env"
	left() { echo $((END - $(date +%s))); }

	_dflag=
	[ -z "$_ldisk" ] || _dflag="--disk '$DISKDIR'"
	piece local-run "exec nice -n 19 '$BIN' run --duration $(left)s --mem $(lookup local 16G $MEMS) $_dflag $_gpu"
	_k=0
	for _h in $_ok; do
		_d=$(awk -v h="$_h" '$1 == h { print $2 }' "$STATE/hosts")
		_x="$_d\\lightshow.exe"
		_ssh="ssh $(ssh_opts "$_h") $_h"
		_rd=
		[ -z "$_disk" ] || _rd="--disk $_d"
		# The remote run reads its stdin from a local sleep: if this side goes away, ssh closes
		# it and the run stops (it also stops by itself at the end).
		piece "$_h-run" "sleep $(left) | $_ssh '$_x run --duration $(left)s --mem $(lookup "$_h" "$(default_mem "$_h")" $MEMS) $_rd --stdin-eof-stop $_gpu'"
		if [ -n "$_net" ]; then
			_pk=$(lookup "$_h" "$(default_net "$_h")" $NETS)
			piece "net-to-$_h" "nice -n 19 '$BIN' source --duration $(left)s --peak $_pk --seed $((11 + 24 * _k)) | $_ssh '$_x sink --duration $(left)s'"
			piece "net-from-$_h" "$_ssh '$_x source --duration $(left)s --peak $_pk --seed $((23 + 24 * _k))' | nice -n 19 '$BIN' sink --duration $(left)s"
		fi
		_k=$((_k + 1))
	done
	# A minute after the end, remove the copies on the Windows machines (an exe can't delete
	# itself while it runs) and this machine's disk wave directory. Logs stay for 'status'.
	piece cleanup-later "sleep $(($(left) + 60)); exec sh '$SELF' cleanup --temp-only"

	_i=0
	while [ $_i -lt 30 ]; do
		_missing=
		for _p in $(cat "$STATE/pieces"); do [ -s "$STATE/pgid.$_p" ] || _missing=1; done
		[ -n "$_missing" ] || break
		sleep 0.1
		_i=$((_i + 1))
	done
	say "running until $(hhmm "$END") on this machine${_ok:+ and $_ok}"
	say "status: $SELF status    stop: $SELF stop    logs: $STATE/logs"
}

cmd_status() {
	[ -f "$STATE/pieces" ] || {
		say "no show recorded ('$SELF start' starts one)"
		return 0
	}
	load_state
	if running; then
		_l=$((END - $(date +%s)))
		if [ "$_l" -lt 120 ]; then _l="${_l}s"; else _l="$(((_l + 59) / 60)) min"; fi
		say "running until $(hhmm "$END"), $_l left"
	elif [ -f "$STATE/stopped" ]; then
		say "stopped at $(hhmm "$(cat "$STATE/stopped")")"
	else
		say "finished at $(hhmm "$END")"
	fi
	for _p in $(cat "$STATE/pieces"); do
		_g=$(cat "$STATE/pgid.$_p" 2>/dev/null || true)
		_s=ended
		case $_g in '' | *[!0-9]*) ;; *) [ "$(members "$_g")" -eq 0 ] || _s="running (process group $_g)" ;; esac
		printf '  %-22s %s\n' "$_p" "$_s"
		_last=$(quiet <"$STATE/logs/$_p.log" 2>/dev/null | tail -n 1)
		[ -z "$_last" ] || printf '  %-22s   %s\n' "" "$_last"
	done
	if running; then
		for _h in $HOSTS; do say "$_h: $(remote_count "$_h") lightshow.exe processes"; done
	fi
}

cmd_stop() {
	[ -f "$STATE/pieces" ] || {
		say "nothing to stop (no show recorded in $STATE)"
		return 0
	}
	load_state
	_gs=$(recorded)
	kill_groups $_gs
	say "stopped $(echo $_gs | wc -w) process groups here"
	# The Windows side stops when its ssh sessions close; make sure, and remove the copies.
	clean_temp
	date +%s >"$STATE/stopped"
}

cmd_cleanup() {
	_cache= _temp= _hosts=
	while [ $# -gt 0 ]; do
		case $1 in
		--cache) _cache=1 ;;
		--temp-only) _temp=1 ;; # the end-of-show timer: keep state and logs
		--hosts)
			[ $# -ge 2 ] || die "--hosts needs a value"
			_hosts=$(hostlist "$2") || exit 1
			shift
			;;
		*) die "cleanup: unknown option $1 (see --help)" ;;
		esac
		shift
	done
	load_state
	if [ -z "$_temp" ]; then
		! running || die "a show is running until $(hhmm "$END"); '$SELF stop' stops it and cleans up"
		kill_groups $(recorded) # a pending end-of-show timer
	fi
	if [ -n "$_hosts" ]; then
		mkdir -p "$STATE"
		for _h in $_hosts; do
			grep -q "^$_h " "$STATE/hosts" 2>/dev/null && continue
			if _d=$(remote_dir "$_h"); then
				printf '%s %s\n' "$_h" "$_d" >>"$STATE/hosts"
			else
				warn "$_h: no usable answer over ssh, skipped"
			fi
		done
	fi
	clean_temp
	[ -n "$_temp" ] || {
		rm -rf "$STATE"
		say "removed $STATE"
	}
	if [ -n "$_cache" ]; then
		chmod -R u+w "$CACHE" 2>/dev/null || true # Go keeps its module cache read-only
		rm -rf "$CACHE"
		say "removed $CACHE"
	fi
}

[ $# -gt 0 ] || {
	usage >&2
	exit 2
}
_cmd=$1
shift
case $_cmd in
start) cmd_start "$@" ;;
status) cmd_status ;;
stop) cmd_stop ;;
cleanup) cmd_cleanup "$@" ;;
build) cmd_build ;;
-h | --help | help) usage ;;
*)
	usage >&2
	exit 2
	;;
esac

# Light show

A synthetic load with a time limit, for watching the btop hub light up. It runs on this machine (in the example setup, the GPU rig)
and on Windows machines over ssh. CPU, memory, disk, GPU and network all move in waves, so every graph on
the page has something to show. It stops by itself, and nothing is installed anywhere.

```sh
tools/lightshow.sh start                 # 20 minutes here and on the default ssh hosts, tablet and gamingrig
tools/lightshow.sh status                # what is running, when it ends, the latest numbers
tools/lightshow.sh stop                  # stop everything now and remove the copies
tools/lightshow.sh cleanup               # remove state and logs once it's over (--cache: the build too)
```

`start --duration 10m` makes it shorter, `start --hosts H1,H2` picks the Windows machines by ssh destination (the
default, `tablet,gamingrig`, is the example setup's Z13 and gaming rig), and `start --local-only` leaves them out.
`tools/lightshow.sh --help` lists every option.

## What it does to each machine

The load generator is one small Go program, [`tools/lightshow`](main.go), run in three modes:

- **CPU.** One worker per logical CPU, each pinned to its CPU, busy for a share of every 50 ms. The pattern
  changes every 30-60 s: a wave travelling across the threads, bursts on every thread, sparkle, a
  checkerboard, a staircase, a comet, and short rests. The per-thread heat map draws these as shapes. Most
  patterns average about 50%, with brief peaks near 95%.
- **Memory.** A buffer swells to a cap and shrinks again every 100 s. Every page is touched, then freed and
  handed back to the OS. Caps: 16 GiB here whatever this machine's RAM, so lower it on a smaller one
  (`--mem local=4G`); 5 GiB on `tablet` (the Z13), 10 GiB on `gamingrig` (the gaming rig) and 4 GiB on any other
  host. Change them with `--mem HOST=SIZE` (`local` is this machine; `0` leaves memory alone).
- **Disk.** Writes to one temp file with fsync. It runs at a few MB/s, with a 9-second burst every minute
  that peaks at about 130 MB/s. That averages roughly 15 MB/s, so a 20-minute show writes about 18 GB per
  machine. The file wraps at 512 MiB and is deleted when the program exits. It goes in
  `/var/tmp/btop-lightshow` here (on disk; `/tmp` is often tmpfs, so writes there would never reach the disk;
  `--disk-dir DIR` moves it) and
  in `%TEMP%\btop-lightshow` on Windows.
- **GPU.** Short FMA-loop kernels of about 4 ms each, for a changing share of every 100 ms: slow swells,
  2-second bursts, a ramp, and 10-second rests. Kernel size keeps adapting as clocks change, and stays far
  below Windows' 2 s GPU watchdog. NVIDIA cards (in the example setup, the GPU rig's A6000 and the gaming rig's 4090 and 3070) use
  the CUDA driver API with a hand-written PTX kernel. Anything else (the Z13's Radeon 8060S) uses OpenCL.
  Both come with the GPU driver, so nothing is installed or compiled ahead of time.
- **Network.** For each Windows machine, one byte stream each way runs through ssh between it and this
  machine. Each stream sits around 1 MB/s with 8-second bursts every 50 s, peaking at 30 MB/s (40 MB/s
  for `gamingrig`; change it with `--net HOST=MB/s`). The bursts are staggered, so a shared Wi-Fi network isn't
  saturated all at once.

Everything runs at the lowest priority: nice 19 here (the agent runs at 10), and the idle priority class on
Windows (the agent's service is below normal). The btop agents and anything interactive always come first.
The program opens no listening port and makes no firewall, registry, service or scheduled-task changes. Its
only network traffic is the ssh streams.

## How it stops

- Every process ends by itself at the end time, which is never more than 25 minutes away.
- `tools/lightshow.sh stop` ends every process group that `start` recorded (by recorded process group, never
  by matching command lines). It gives them 5 s to delete their temp files, then kills them. After that it
  runs `taskkill` for `lightshow.exe` on each Windows machine and removes the copies there.
- The Windows run reads its stdin from a `sleep` on this machine through ssh. If this machine stops it,
  reboots or loses the network, the ssh session closes and the run stops. The network streams stop when
  either end goes away.
- A minute after the end, a timer removes the copies from the Windows machines (a running .exe can't delete
  itself) and this machine's disk-wave directory. Logs stay until `cleanup`.

## Files it creates

| Where | What | Removed by |
|---|---|---|
| `${XDG_CACHE_HOME:-~/.cache}/btop-lightshow/` | the two binaries and the Go build and module caches | `cleanup --cache` |
| `${XDG_STATE_HOME:-~/.local/state}/btop-lightshow/` | end time, recorded process groups, one log per piece | `cleanup` |
| `/var/tmp/btop-lightshow/wave-PID.tmp` | the disk wave, here | the program on exit; `stop`, `cleanup` |
| `%TEMP%\btop-lightshow\lightshow.exe` and `wave-PID.tmp` on each Windows machine | the copy and its disk wave | `stop`, `cleanup`, the end-of-show timer |

`cleanup --hosts H1,H2` (in the example setup, `tablet,gamingrig`) also cleans Windows machines that no recorded
show here knows about.

## Needs

- Docker on this machine. `start` and `build` compile both binaries in `golang:1.27-bookworm` (a few
  seconds once the caches are warm). The script honours `DOCKER_CONFIG`. If pulling the image fails on a
  broken credential helper, point it at a directory holding an empty `config.json`.
- Key-based ssh to each Windows machine (the script uses `BatchMode`, so it never prompts). The Windows
  OpenSSH default shell must be `cmd.exe`, which is the default: PowerShell would mangle the byte streams.
  `%TEMP%` must not contain spaces. A machine that fails these checks is left out with a message, and the
  rest of the show goes on.
- If an ssh host goes through a VPN (in the example setup, `tablet` reaches the Z13 through Tailscale),
  `--addr HOST=ADDR` keeps the ssh streams on the LAN for that run, e.g. `--addr tablet=192.168.1.47`.

## Running the program by hand

```text
lightshow run    [--duration 20m] [--mem 5G] [--disk DIR] [--no-cpu] [--no-gpu] [--seed N] [--stdin-eof-stop]
lightshow source [--duration 20m] [--peak 40] [--seed N]    # a byte stream at a varying rate, to stdout
lightshow sink   [--duration 20m]                           # reads stdin and throws it away
```

`tools/lightshow.sh build` leaves `lightshow` (Linux) and `lightshow.exe` (Windows) in
`~/.cache/btop-lightshow/bin`. Every mode prints what it is doing to stderr every 15 s.

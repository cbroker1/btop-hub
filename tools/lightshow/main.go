//go:build linux || windows

// Command lightshow is a time-limited synthetic load, for watching the btop hub light up.
// tools/lightshow.sh builds it, copies it to the Windows machines and runs it everywhere;
// see README.md next to this file.
//
//	lightshow run    [--duration 20m] [--mem 5G] [--disk DIR] [--no-cpu] [--no-gpu] [--seed N] [--stdin-eof-stop]
//	lightshow source [--duration 20m] [--peak 40] [--seed N]   (a byte stream at a varying rate, to stdout)
//	lightshow sink   [--duration 20m]                          (reads stdin and throws it away)
//
// Every mode exits by itself after --duration (never more than 25 minutes), on Ctrl-C or
// SIGTERM, and when its stdin closes (sink always, run with --stdin-eof-stop). The disk
// wave's temp file is deleted on exit. It runs at the lowest priority (nice 19, or the Windows
// idle class), so the btop agent and anything interactive always come first. It installs
// nothing and listens on no port.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hardCap = 25 * time.Minute

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s lightshow: %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: lightshow run|source|sink [flags]   (lightshow MODE -h for the flags)")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	dur := fs.Duration("duration", 20*time.Minute, "stop after this long (at most 25m)")
	seed := fs.Uint64("seed", 0, "pattern seed (0 = random)")
	var (
		stdinStop, noCPU, noGPU *bool
		memFlag, diskDir        *string
		diskPeak, peak          *float64
	)
	switch mode {
	case "run":
		stdinStop = fs.Bool("stdin-eof-stop", false, "stop when stdin closes")
		noCPU = fs.Bool("no-cpu", false, "no CPU waves")
		noGPU = fs.Bool("no-gpu", false, "no GPU waves")
		memFlag = fs.String("mem", "0", "memory wave peak, e.g. 5G (0 = off)")
		diskDir = fs.String("disk", "", "directory for the disk wave's temp file (empty = off)")
		diskPeak = fs.Float64("disk-peak", 130, "disk wave peak, MB/s")
	case "source":
		peak = fs.Float64("peak", 40, "peak rate, MB/s")
	case "sink":
	default:
		usage()
	}
	_ = fs.Parse(os.Args[2:])
	if *dur <= 0 || *dur > hardCap {
		*dur = hardCap
	}
	if *seed == 0 {
		*seed = rand.Uint64()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { s := <-sig; logf("%v: stopping", s); cancel() }()
	logf("%s for %v (ends %s UTC), seed %d", mode, dur.Round(time.Second), time.Now().Add(*dur).UTC().Format("15:04:05"), *seed)

	switch mode {
	case "run":
		if *stdinStop {
			go func() { _, _ = io.Copy(io.Discard, os.Stdin); logf("stdin closed: stopping"); cancel() }()
		}
		memCap, err := parseSize(*memFlag)
		if err != nil {
			logf("bad --mem: %v", err)
			os.Exit(2)
		}
		run(ctx, runOpts{cpu: !*noCPU, gpu: !*noGPU, mem: memCap, disk: *diskDir, diskPeak: *diskPeak, seed: *seed})
	case "source":
		source(ctx, *peak, *seed)
	case "sink":
		sink(ctx)
	}
	logf("%s done", mode)
}

// parseSize reads 5G, 512M or a plain byte count.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%q", s)
	}
	return int64(v * float64(mult)), nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// mix is splitmix64, for repeatable pseudo-random choices from (time slot, thread).
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

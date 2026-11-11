//go:build linux || windows

package main

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runOpts struct {
	cpu, gpu bool
	mem      int64
	disk     string
	diskPeak float64
	seed     uint64
}

type stats struct {
	mem, disk atomic.Int64
	mu        sync.Mutex
	gpu       map[string]float64
}

func (s *stats) setGPU(name string, u float64) {
	s.mu.Lock()
	if s.gpu == nil {
		s.gpu = map[string]float64{}
	}
	s.gpu[name] = u
	s.mu.Unlock()
}

func (s *stats) gpuString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.gpu) == 0 {
		return "off"
	}
	var parts []string
	for k, v := range s.gpu {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", k, v*100))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func run(ctx context.Context, o runOpts) {
	if err := lowerPriority(); err != nil {
		logf("priority: %v", err)
	}
	n := runtime.NumCPU()
	runtime.GOMAXPROCS(n + 4)
	sched := newSchedule(o.seed)
	start := time.Now()
	var wg sync.WaitGroup
	var st stats
	if o.cpu {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); cpuWorker(ctx, i, n, start, sched) }(i)
		}
		logf("cpu: %d pinned workers, patterns cycle every %.0f s", n, sched.total)
	}
	if o.mem > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); memWave(ctx, o.mem, start, &st) }()
	}
	if o.disk != "" {
		wg.Add(1)
		go func() { defer wg.Done(); diskWave(ctx, o.disk, o.diskPeak, start, &st) }()
	}
	if o.gpu {
		wg.Add(1)
		go func() { defer wg.Done(); gpuWave(ctx, start, &st) }()
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			_, name := sched.duty(time.Since(start).Seconds(), 0, n)
			logf("cpu %s | mem %.1f GiB | disk %.0f MB written | gpu %s", name,
				float64(st.mem.Load())/(1<<30), float64(st.disk.Load())/1e6, st.gpuString())
		}
	}
}

// ---- CPU: one pinned worker per logical CPU, 50 ms duty cycles following a pattern -----------
// The patterns (a wave travelling across the threads, bursts, sparkle, a checkerboard, a
// staircase, a comet, short rests) rotate every 30-60 s, so the per-thread heat map draws shapes;
// most average about 50% with brief peaks near 95%.

type segment struct {
	name string
	dur  float64
	f    func(t float64, i, n int) float64
}

type schedule struct {
	segs  []segment
	total float64
}

func newSchedule(seed uint64) *schedule {
	r := rand.New(rand.NewPCG(seed, seed^0x5eed))
	fi := func(i int) float64 { return float64(i) }
	pats := []segment{
		{"wave", 0, func(t float64, i, n int) float64 { return 0.5 + 0.45*math.Sin(2*math.Pi*(t/16-fi(i)/fi(n))) }},
		{"wave back", 0, func(t float64, i, n int) float64 { return 0.5 + 0.45*math.Sin(2*math.Pi*(t/10+2*fi(i)/fi(n))) }},
		{"bursts", 0, func(t float64, i, n int) float64 {
			if math.Mod(t, 8) < 3 {
				return 0.92
			}
			return 0.06
		}},
		{"sparkle", 0, func(t float64, i, n int) float64 {
			if mix(uint64(t/1.5)*1315423911+uint64(i)+seed)%100 < 45 {
				return 0.95
			}
			return 0.06
		}},
		{"checkerboard", 0, func(t float64, i, n int) float64 {
			if (i+int(t/4))%2 == 0 {
				return 0.9
			}
			return 0.08
		}},
		{"staircase", 0, func(t float64, i, n int) float64 {
			if fi(i) <= fi(n)*math.Mod(t, 20)/20 {
				return 0.9
			}
			return 0.06
		}},
		{"pulse", 0, func(t float64, i, n int) float64 { return 0.5 + 0.45*math.Sin(2*math.Pi*t/10) }},
		{"halves", 0, func(t float64, i, n int) float64 {
			if (i < n/2) == (int(t/5)%2 == 0) {
				return 0.9
			}
			return 0.08
		}},
		{"comet", 0, func(t float64, i, n int) float64 {
			pos := math.Mod(t*3, fi(n))
			d := math.Abs(fi(i) - pos)
			if d > fi(n)/2 {
				d = fi(n) - d
			}
			return 0.12 + 0.83*math.Exp(-d*d/6)
		}},
	}
	r.Shuffle(len(pats), func(a, b int) { pats[a], pats[b] = pats[b], pats[a] })
	s := &schedule{}
	for k, p := range pats {
		p.dur = 30 + r.Float64()*30
		s.segs = append(s.segs, p)
		if k%3 == 2 {
			s.segs = append(s.segs, segment{"rest", 10 + r.Float64()*5, func(float64, int, int) float64 { return 0.03 }})
		}
	}
	for _, g := range s.segs {
		s.total += g.dur
	}
	return s
}

func (s *schedule) duty(t float64, i, n int) (float64, string) {
	t = math.Mod(t, s.total)
	for _, g := range s.segs {
		if t < g.dur {
			return math.Max(0, math.Min(0.97, g.f(t, i, n))), g.name
		}
		t -= g.dur
	}
	return 0, "rest"
}

var spinSink atomic.Uint64

func cpuWorker(ctx context.Context, i, n int, start time.Time, s *schedule) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := pinThread(i); err != nil && i == 0 {
		logf("cpu affinity: %v", err)
	}
	lowerThreadPriority()
	const period = 50 * time.Millisecond
	x := uint64(i) + 1
	for ctx.Err() == nil {
		begin := time.Now()
		d, _ := s.duty(begin.Sub(start).Seconds(), i, n)
		until := begin.Add(time.Duration(d * float64(period)))
		for time.Now().Before(until) {
			for k := 0; k < 4000; k++ {
				x = x*6364136223846793005 + 1442695040888963407
			}
		}
		if rest := period - time.Since(begin); rest > 0 {
			time.Sleep(rest)
		}
	}
	spinSink.Add(x)
}

// ---- Memory: a buffer that swells to the cap and shrinks again every 100 s ----------------------

func memWave(ctx context.Context, capBytes int64, start time.Time, st *stats) {
	const chunk = 64 << 20
	debug.SetMemoryLimit(capBytes + 1<<30)
	var chunks [][]byte
	defer func() { chunks = nil; st.mem.Store(0); debug.FreeOSMemory() }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	logf("mem: swelling to %.1f GiB and back every 100 s", float64(capBytes)/(1<<30))
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s := time.Since(start).Seconds()
		f := (0.5 - 0.5*math.Cos(2*math.Pi*s/100)) * (0.85 + 0.15*math.Sin(2*math.Pi*s/17))
		target := int(float64(capBytes) * f / chunk)
		if target < len(chunks) {
			for k := target; k < len(chunks); k++ {
				chunks[k] = nil
			}
			chunks = chunks[:target]
			debug.FreeOSMemory()
		}
		for added := 0; len(chunks) < target && added < 8 && ctx.Err() == nil; added++ {
			b := make([]byte, chunk)
			for k := 0; k < len(b); k += 4096 {
				b[k] = byte(k>>12) | 1
			}
			chunks = append(chunks, b)
		}
		st.mem.Store(int64(len(chunks)) * chunk)
	}
}

// ---- Disk: fsync'd writes to one temp file (wrapping at 512 MiB), in waves ----------------------

func diskRate(s, peak float64) float64 {
	base := 3 + 3*math.Sin(2*math.Pi*s/37)
	cyc := math.Floor(s / 60)
	p := s - cyc*60
	burst := 0.0
	if p >= 20 && p < 29 {
		burst = peak * (0.7 + 0.3*float64(mix(uint64(cyc))%100)/100) * math.Sin(math.Pi*(p-20)/9)
	}
	return base + burst
}

func diskWave(ctx context.Context, dir string, peak float64, start time.Time, st *stats) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("disk: %v", err)
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("wave-%d.tmp", os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		logf("disk: %v", err)
		return
	}
	defer func() {
		_ = f.Close()
		if err := os.Remove(path); err != nil {
			logf("disk: %v", err)
		} else {
			logf("disk: removed %s", path)
		}
	}()
	buf := make([]byte, 1<<20)
	for k := range buf {
		buf[k] = byte(rand.Uint32())
	}
	const wrap = 512 << 20
	const step = 100 * time.Millisecond
	var off, unsynced int64
	next := time.Now()
	logf("disk: fsync'd write waves to %s, peak %.0f MB/s", path, peak)
	for ctx.Err() == nil {
		want := int64(diskRate(time.Since(start).Seconds(), peak) * 1e6 * step.Seconds())
		for want > 0 && ctx.Err() == nil {
			n := min(int64(len(buf)), want, wrap-off)
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				logf("disk: %v", err)
				return
			}
			off = (off + n) % wrap
			want -= n
			unsynced += n
			st.disk.Add(n)
			if unsynced >= 8<<20 {
				_ = f.Sync()
				unsynced = 0
			}
		}
		if unsynced > 0 {
			_ = f.Sync()
			unsynced = 0
		}
		next = next.Add(step)
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		} else {
			next = time.Now()
		}
	}
}

// ---- Network: a byte stream at a varying rate (source) and a bit bucket (sink) ------------------

func netRate(s, peak float64, seed uint64) float64 {
	t := s + float64(seed%50)
	cyc := math.Floor(t / 50)
	p := t - cyc*50
	base := 0.6 + 0.5*math.Sin(2*math.Pi*t/23)
	burst := 0.0
	if p < 8 {
		burst = peak * (0.6 + 0.4*float64(mix(uint64(cyc)+seed)%100)/100) * math.Sin(math.Pi*p/8)
	}
	swell := peak * 0.25 * math.Max(0, math.Sin(2*math.Pi*t/180))
	return base + burst + swell
}

func source(ctx context.Context, peak float64, seed uint64) {
	buf := make([]byte, 256<<10)
	for k := range buf {
		buf[k] = byte(rand.Uint32())
	}
	start := time.Now()
	const step = 50 * time.Millisecond
	next := time.Now()
	var total int64
	for ctx.Err() == nil {
		want := int64(netRate(time.Since(start).Seconds(), peak, seed) * 1e6 * step.Seconds())
		for want > 0 {
			n := min(int64(len(buf)), want)
			if _, err := os.Stdout.Write(buf[:n]); err != nil {
				logf("source: %v after %.0f MB", err, float64(total)/1e6)
				return
			}
			want -= n
			total += n
		}
		next = next.Add(step)
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		} else {
			next = time.Now()
		}
	}
	logf("source: sent %.0f MB", float64(total)/1e6)
}

func sink(ctx context.Context) {
	var total atomic.Int64
	go func() {
		<-ctx.Done()
		logf("sink: time is up after %.0f MB", float64(total.Load())/1e6)
		os.Exit(0)
	}()
	buf := make([]byte, 1<<20)
	for {
		n, err := os.Stdin.Read(buf)
		total.Add(int64(n))
		if err != nil {
			logf("sink: %v after %.0f MB", err, float64(total.Load())/1e6)
			return
		}
	}
}

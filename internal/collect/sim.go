package collect

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"time"

	"btophub/internal/proto"
)

// SimOptions shape a simulated machine.
type SimOptions struct {
	Uptime   time.Duration // time since simulated boot; samples before boot are not produced
	GapStart time.Duration // a sleep gap starting this long ago (0 = none)
	GapLen   time.Duration
}

// SimProfiles lists the built-in machines.
var SimProfiles = []string{"gpu-rig", "z13", "gaming-rig"}

// DefaultSimOptions mirror the mockups: the gaming rig booted 41 minutes ago.
func DefaultSimOptions(profile string) SimOptions {
	switch profile {
	case "z13":
		return SimOptions{Uptime: 4*time.Hour + 11*time.Minute}
	case "gaming-rig":
		return SimOptions{Uptime: 41 * time.Minute}
	default:
		return SimOptions{Uptime: 55*time.Hour + 51*time.Minute}
	}
}

// Sim produces plausible, deterministic data for a named machine so the hub and page can
// be built and checked before real agents exist. Its Info is marked Sim.
type Sim struct {
	profile string
	info    proto.Info
	opts    SimOptions
	boot    time.Time
	eps     []episode
	rxTot   float64
	txTot   float64
	lastT   time.Time
}

// episode is a burst of activity.
type episode struct {
	kind       int // 0 all-core burst, 1 one pinned thread, 2 gaming, 3 io burst, 4 download
	start, end time.Time
	level      float64
	thread     int
}

// NewSim builds a simulator for profile, anchored at now.
func NewSim(profile string, opts SimOptions, now time.Time) (*Sim, error) {
	const gib = int64(1) << 30
	s := &Sim{profile: profile, opts: opts}
	switch profile {
	case "gpu-rig":
		s.info = proto.Info{OS: "linux", OSName: "Ubuntu 26.04.1 LTS", Hostname: "gpurig", CPUModel: "Ryzen 9 7950X",
			Cores: 16, Threads: 32, MemTotal: 131670568960, SwapLabel: "Swap", HasTemp: true,
			GPUs: []proto.GPUInfo{{Name: "RTX A6000", Vendor: "nvidia", MemTotal: 49140 << 20}},
			Net:  proto.NetInfo{Name: "wlp76s0", Desc: "iwlwifi", Addr: "192.168.1.74", Kind: "wireless"},
			Disk: proto.DiskInfo{Label: "root", Model: "Samsung SSD 990 PRO 4TB", Bus: "NVMe"}}
	case "z13":
		s.info = proto.Info{OS: "windows", OSName: "Windows 11 Pro 25H2", Hostname: "Z13", CPUModel: "Ryzen AI Max+ 395",
			Cores: 16, Threads: 32, MemTotal: 316 * gib / 10, SwapLabel: "page file", Battery: true,
			GPUs: []proto.GPUInfo{{Name: "Radeon 8060S", Vendor: "amd", MemTotal: 96 * gib, Integrated: true}},
			Net:  proto.NetInfo{Name: "Wi-Fi", Desc: "MediaTek Wi-Fi 7", Addr: "192.168.1.47", Kind: "wireless", LinkBps: 2_400_000_000},
			Disk: proto.DiskInfo{Label: "C:", Bus: "NVMe"}}
	case "gaming-rig":
		s.info = proto.Info{OS: "windows", OSName: "Windows 11 IoT Enterprise LTSC 2024", Hostname: "GAMING", CPUModel: "Ryzen 9 9950X3D",
			Cores: 16, Threads: 32, MemTotal: 617 * gib / 10, SwapLabel: "page file",
			GPUs: []proto.GPUInfo{{Name: "RTX 4090", Vendor: "nvidia", MemTotal: 24 * gib}, {Name: "RTX 3070", Vendor: "nvidia", MemTotal: 8 * gib}},
			Net:  proto.NetInfo{Name: "Ethernet", Desc: "Marvell AQtion 10G", Addr: "192.168.1.50", Kind: "wired", LinkBps: 1_000_000_000},
			Disk: proto.DiskInfo{Label: "C:", Model: "Samsung SSD 990 PRO 4TB", Bus: "NVMe"}}
	default:
		return nil, fmt.Errorf("unknown simulated machine %q (have %v)", profile, SimProfiles)
	}
	// Thermal limits as the real agents report them (thermal.go). The simulated temperatures stay
	// under them, so a simulated pane never shows a throttling alert.
	if lim, from, ok := cpuTjMax(s.info.CPUModel); ok && s.info.HasTemp {
		s.info.CPUTempLimit, s.info.CPUThrottle = lim, cpuThrottleDesc(lim, from)
	}
	for i := range s.info.GPUs {
		if g := &s.info.GPUs[i]; g.Vendor == "nvidia" {
			g.Throttle = nvThrottleDesc
		}
	}
	s.info.Sim = true
	s.boot = now.Add(-opts.Uptime)
	s.info.BootTime = s.boot.UnixMilli()
	s.plan(now)
	return s, nil
}

// plan schedules episodes from two hours ago to a day ahead.
func (s *Sim) plan(now time.Time) {
	h := fnv.New64a()
	h.Write([]byte(s.profile))
	r := rand.New(rand.NewSource(int64(h.Sum64())))
	from, to := now.Add(-2*time.Hour), now.Add(24*time.Hour)
	add := func(kind int, meanGap, minLen, maxLen time.Duration, lo, hi float64) {
		for t := from; t.Before(to); {
			t = t.Add(time.Duration(r.ExpFloat64() * float64(meanGap)))
			d := minLen + time.Duration(r.Float64()*float64(maxLen-minLen))
			s.eps = append(s.eps, episode{kind: kind, start: t, end: t.Add(d), level: lo + r.Float64()*(hi-lo), thread: r.Intn(s.info.Threads)})
			t = t.Add(d)
		}
	}
	switch s.profile {
	case "gpu-rig": // training runs: GPU near 100%, a pinned data-loader thread, compile bursts
		add(2, 50*time.Minute, 8*time.Minute, 20*time.Minute, 0.85, 1.0)
		add(1, 25*time.Minute, 5*time.Minute, 40*time.Minute, 0.95, 1.0)
		add(0, 20*time.Minute, 20*time.Second, 90*time.Second, 0.7, 1.0)
	case "z13": // bursty desktop use, a long GPU job
		add(2, 12*time.Minute, 1*time.Minute, 9*time.Minute, 0.6, 0.99)
		add(0, 6*time.Minute, 10*time.Second, 60*time.Second, 0.6, 1.0)
		add(1, 15*time.Minute, 1*time.Minute, 6*time.Minute, 0.5, 0.95)
	case "gaming-rig": // one long game session
		add(2, 30*time.Minute, 25*time.Minute, 50*time.Minute, 0.85, 0.98)
		add(0, 30*time.Minute, 20*time.Second, 70*time.Second, 0.7, 1.0)
		add(1, 20*time.Minute, 2*time.Minute, 10*time.Minute, 0.6, 0.9)
	}
	add(3, 4*time.Minute, 5*time.Second, 90*time.Second, 0, 1) // disk bursts
	add(4, 7*time.Minute, 10*time.Second, 3*time.Minute, 0, 1) // downloads
	sort.Slice(s.eps, func(a, b int) bool { return s.eps[a].start.Before(s.eps[b].start) })
}

// noise is a deterministic pseudo-random value in [0,1) for (t, k).
func noise(t int64, k int) float64 {
	x := uint64(t)*0x9E3779B97F4A7C15 ^ uint64(k)*0xC2B2AE3D27D4EB4F
	x ^= x >> 31
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 29
	return float64(x>>11) / float64(1<<53)
}

func (s *Sim) Info() proto.Info { return s.info }
func (s *Sim) Close()           {}
func (s *Sim) Report() Report {
	return Report{Sources: map[string]string{"all": "simulated (" + s.profile + ")"}, Timings: map[string]float64{}}
}

// Before reports whether t is before boot or inside the sleep gap, where no sample exists.
func (s *Sim) Before(t time.Time, now time.Time) bool {
	if t.Before(s.boot) {
		return true
	}
	if s.opts.GapLen > 0 {
		gs := now.Add(-s.opts.GapStart)
		if !t.Before(gs) && t.Before(gs.Add(s.opts.GapLen)) {
			return true
		}
	}
	return false
}

// Collect returns the simulated sample at now.
func (s *Sim) Collect(now time.Time) proto.Sample {
	ts := now.Unix()
	smp := proto.Sample{T: now.UnixMilli()}
	n := s.info.Threads
	cpu := make([]float64, n)
	gpu := 0.0
	gaming, ioBurst, dl := 0.0, 0.0, 0.0
	for i := range cpu {
		cpu[i] = 0.01 + 0.03*noise(ts, i)
		if noise(ts/7, i+100) > 0.97 {
			cpu[i] += 0.1 + 0.3*noise(ts, i+200)
		}
	}
	for _, e := range s.eps {
		if now.Before(e.start) || !now.Before(e.end) {
			continue
		}
		switch e.kind {
		case 0:
			for i := range cpu {
				cpu[i] = math.Max(cpu[i], e.level*(0.85+0.15*noise(ts, i+300)))
			}
		case 1:
			cpu[e.thread] = math.Max(cpu[e.thread], e.level+0.02*noise(ts, 7))
		case 2:
			gaming = e.level
			gpu = math.Max(gpu, e.level*(0.9+0.1*noise(ts, 9)))
			for i := 0; i < n/2; i++ { // games load the first CCD
				cpu[i] = math.Max(cpu[i], e.level*(0.35+0.45*noise(ts, i+400)))
			}
		case 3:
			ioBurst = math.Max(ioBurst, 0.3+0.7*noise(ts/3, 11))
		case 4:
			dl = math.Max(dl, 0.4+0.6*noise(ts/5, 13))
		}
	}
	smp.CPU = make([]uint16, n)
	for i, v := range cpu {
		smp.CPU[i] = proto.PerMille(v)
	}
	avg := 0.0
	for _, v := range cpu {
		avg += v
	}
	avg /= float64(n)
	if s.info.HasTemp {
		smp.CPUTemp = proto.F64(math.Round((42+40*avg+3*noise(ts, 17))*10) / 10)
		smp.CPUHot = atLimit(*smp.CPUTemp, s.info.CPUTempLimit)
	}
	smp.CPUFreq = proto.F64(float64(int(3000 + 2500*math.Min(1, avg*3) + 200*noise(ts, 19))))
	if s.info.OS == "linux" {
		smp.Load = []float64{math.Round(avg*float64(n)*10) / 10, math.Round(avg*float64(n)*9) / 10, math.Round(avg*float64(n)*8) / 10}
	}

	mt := float64(s.info.MemTotal)
	used := mt * (0.22 + 0.15*gaming + 0.02*noise(ts/60, 21))
	smp.MemUsed = proto.I64(int64(used))
	cached := mt * (0.38 + 0.05*math.Sin(float64(ts)/1800))
	if s.profile == "gaming-rig" {
		cached = mt * 0.08
	}
	smp.MemCached = proto.I64(int64(cached))
	smp.MemFree = proto.I64(int64(math.Max(0, mt-used-cached-mt*0.04)))
	switch s.profile {
	case "gpu-rig":
		smp.SwapTotal = proto.I64(8 << 30)
		smp.SwapUsed = proto.I64(int64(6.56 * float64(mt) / mt * (1 << 30)))
	case "z13":
		smp.SwapTotal = proto.I64(16 << 30)
		smp.SwapUsed = proto.I64(int64((2.1 + 0.3*gaming) * (1 << 30)))
		smp.CommitUsed = proto.I64(int64(used) + 8<<30)
		smp.CommitLimit = proto.I64(int64(mt) + 16<<30)
	case "gaming-rig":
		smp.SwapTotal = proto.I64(int64(14.5 * (1 << 30)))
		smp.SwapUsed = proto.I64(0)
		smp.CommitUsed = proto.I64(int64(used) + 4<<30)
		smp.CommitLimit = proto.I64(int64(mt) + int64(14.5*(1<<30)))
	}

	for i, g := range s.info.GPUs {
		gs := proto.GPUSample{}
		u := gpu
		if i > 0 {
			u = 0
		}
		if u < 0.02 {
			u = 0.01 * noise(ts, 23+i)
		}
		gs.Util = proto.U16(proto.PerMille(u))
		base := 0.02
		if i == 0 {
			base += 0.6 * gaming
			if s.profile == "z13" {
				base = 0.72
			}
		}
		gs.MemUsed = proto.I64(int64(float64(g.MemTotal) * base))
		if g.Vendor == "nvidia" {
			gs.Temp = proto.F64(float64(int(34 + 40*u)))
			gs.Power = proto.F64(float64(int(17 + 380*u*float64(1-i))))
			ps := 8
			if u > 0.1 {
				ps = 2
			}
			gs.PState = proto.Int(ps)
			gs.Fan = proto.Int(int(30 + 50*u))
			gs.Clock = proto.Int(int(210 + 2300*math.Min(1, u*1.5)))
			gs.MemClock = proto.Int(405 + int(9000*math.Min(1, u*2)))
			gs.PcieTx = proto.I64(int64(1e6 * (0.5 + 30*u)))
			gs.PcieRx = proto.I64(int64(8e5 * (0.5 + 50*u)))
		}
		smp.GPU = append(smp.GPU, gs)
	}

	io := 2e3 + 4e4*noise(ts, 29)
	if ioBurst > 0 {
		io = math.Pow(10, 5+3.5*ioBurst)
	}
	smp.DiskRead = proto.I64(int64(io * 0.7))
	smp.DiskWrite = proto.I64(int64(io * 0.3))
	switch s.profile {
	case "gpu-rig":
		smp.DiskTotal, smp.DiskUsed = proto.I64(3925049798656), proto.I64(2708284360704)
	case "z13":
		smp.DiskTotal, smp.DiskUsed = proto.I64(924*(1<<30)), proto.I64(619*(1<<30))
	default:
		smp.DiskTotal, smp.DiskUsed = proto.I64(4000787030016), proto.I64(960188887040)
	}

	rx := 3e3 + 2e4*noise(ts, 31)
	tx := 2e3 + 1e4*noise(ts, 37)
	if dl > 0 {
		rx = math.Pow(10, 5.5+2.5*dl)
		tx = rx / 40
	}
	if gaming > 0 {
		rx += 4e5 * noise(ts, 41)
		tx += 9e4 * noise(ts, 43)
	}
	dt := 1.0
	if !s.lastT.IsZero() {
		dt = math.Max(0.001, now.Sub(s.lastT).Seconds())
	}
	s.lastT = now
	s.rxTot += rx * dt
	s.txTot += tx * dt
	smp.NetRx, smp.NetTx = proto.I64(int64(rx)), proto.I64(int64(tx))
	smp.NetRxTotal, smp.NetTxTotal = proto.I64(int64(11.9e9+s.rxTot)), proto.I64(int64(1.7e9+s.txTot))

	if s.info.Battery {
		p := 79 - int(now.Sub(s.boot).Minutes()/60)%20
		smp.Battery = proto.Int(p)
		smp.BatState = proto.BatCharging
	}
	smp.Uptime = proto.I64(int64(now.Sub(s.boot).Seconds()))
	return smp
}

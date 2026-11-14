package collect

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"btophub/internal/nvml"
	"btophub/internal/proto"
)

// Nvidia reads NVIDIA GPUs through NVML. NVML lists only GPUs that are present, so stale
// registry entries for removed cards never show up. Used by the Linux and Windows collectors.
type Nvidia struct {
	lib  *nvml.Lib
	devs []nvDev // most VRAM first: the primary GPU is the biggest one

	mu   sync.Mutex
	pcie map[int][2]int64 // index into devs -> {tx, rx} bytes/s, refreshed in the background
	stop chan struct{}
}

type nvDev struct {
	d    nvml.Device
	info proto.GPUInfo
	// reasons: the driver reports clock event reasons, so Hot comes from its thermal-slowdown
	// flags; otherwise from the temperature reaching the GPU's own slowdown threshold, if known.
	reasons bool
}

// OpenNvidia loads NVML and enumerates GPUs. It returns (nil, reason) when there is no
// NVIDIA driver or no GPU.
func OpenNvidia() (*Nvidia, string) {
	lib, err := nvml.Open()
	if err != nil {
		return nil, "NVML not available: " + err.Error()
	}
	n, err := lib.Count()
	if err != nil || n == 0 {
		lib.Close()
		if err == nil {
			err = fmt.Errorf("no devices")
		}
		return nil, "NVML: " + err.Error()
	}
	g := &Nvidia{lib: lib, pcie: map[int][2]int64{}, stop: make(chan struct{})}
	for i := 0; i < n; i++ {
		d, err := lib.Handle(i)
		if err != nil {
			continue
		}
		name, _ := lib.Name(d)
		total, _, _ := lib.Memory(d)
		dev := nvDev{d: d, info: proto.GPUInfo{
			Name: CleanGPUName(name), Vendor: "nvidia", MemTotal: int64(total),
		}}
		if t, err := lib.TempThreshold(d, nvml.TempThresholdSlowdown); err == nil && t > 0 && t < 150 {
			dev.info.TempLimit = float64(t)
		}
		if _, err := lib.ClockEventReasons(d); err == nil {
			dev.reasons = true
			dev.info.Throttle = nvThrottleDesc
		} else if dev.info.TempLimit > 0 {
			dev.info.Throttle = fmt.Sprintf("temperature at its %g °C slowdown threshold", dev.info.TempLimit)
		}
		g.devs = append(g.devs, dev)
	}
	if len(g.devs) == 0 {
		lib.Close()
		return nil, "NVML: no usable devices"
	}
	sort.SliceStable(g.devs, func(a, b int) bool { return g.devs[a].info.MemTotal > g.devs[b].info.MemTotal })
	go g.pcieLoop()
	return g, fmt.Sprintf("NVML, driver %s", lib.DriverVersion())
}

// Infos describes the GPUs in display order.
func (g *Nvidia) Infos() []proto.GPUInfo {
	out := make([]proto.GPUInfo, len(g.devs))
	for i, d := range g.devs {
		out[i] = d.info
	}
	return out
}

// Sample reads every GPU. Fields the driver does not support stay nil.
func (g *Nvidia) Sample() []proto.GPUSample {
	out := make([]proto.GPUSample, len(g.devs))
	g.mu.Lock()
	pcie := g.pcie
	g.mu.Unlock()
	for i, dev := range g.devs {
		s := &out[i]
		d := dev.d
		if u, _, err := g.lib.Utilization(d); err == nil {
			s.Util = proto.U16(proto.PerMille(float64(u) / 100))
		}
		if _, used, err := g.lib.Memory(d); err == nil {
			s.MemUsed = proto.I64(int64(used))
		}
		if t, err := g.lib.Temperature(d); err == nil {
			s.Temp = proto.F64(float64(t))
		}
		if dev.reasons {
			if r, err := g.lib.ClockEventReasons(d); err == nil {
				s.Hot = r&nvml.ReasonsThermal != 0
			}
		} else if s.Temp != nil && dev.info.TempLimit > 0 {
			s.Hot = *s.Temp >= dev.info.TempLimit
		}
		if p, err := g.lib.Power(d); err == nil {
			s.Power = proto.F64(float64(p) / 1000)
		}
		if p, err := g.lib.PState(d); err == nil {
			s.PState = proto.Int(p)
		}
		if f, err := g.lib.Fan(d); err == nil {
			s.Fan = proto.Int(int(f))
		}
		if c, err := g.lib.Clock(d, nvml.ClockGraphics); err == nil {
			s.Clock = proto.Int(int(c))
		}
		if c, err := g.lib.Clock(d, nvml.ClockMem); err == nil {
			s.MemClock = proto.Int(int(c))
		}
		if v, ok := pcie[i]; ok {
			s.PcieTx, s.PcieRx = proto.I64(v[0]), proto.I64(v[1])
		}
	}
	return out
}

// pcieLoop refreshes PCIe throughput every 5 s off the sampling path, because each NVML
// PCIe query blocks for about 20 ms while the driver counts bytes.
func (g *Nvidia) pcieLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		next := map[int][2]int64{}
		for i, dev := range g.devs {
			tx, err1 := g.lib.PcieThroughput(dev.d, nvml.PcieTx)
			rx, err2 := g.lib.PcieThroughput(dev.d, nvml.PcieRx)
			if err1 == nil && err2 == nil {
				next[i] = [2]int64{int64(tx) * 1024, int64(rx) * 1024}
			}
		}
		g.mu.Lock()
		g.pcie = next
		g.mu.Unlock()
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
	}
}

// Close releases NVML.
func (g *Nvidia) Close() {
	close(g.stop)
	g.lib.Close()
}

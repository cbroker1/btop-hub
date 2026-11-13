// Package proto defines what agents send to the hub and what the hub sends to browsers.
//
// Agents open one long-lived HTTP POST to /api/agent/stream and write newline-delimited
// JSON messages (Msg). The first message is always a Hello. Samples follow, one per second.
// Any value an agent cannot read is left out (nil), never sent as zero.
package proto

import (
	"errors"
	"fmt"
	"regexp"
)

// Version is the agent/hub protocol and build version.
const Version = "1.0.0"

// HistorySeconds is how much history agents and the hub keep.
const HistorySeconds = 3600

// Msg is one line on the agent stream.
type Msg struct {
	Hello *Hello  `json:"hello,omitempty"`
	Info  *Info   `json:"info,omitempty"`
	S     *Sample `json:"s,omitempty"`
	// B marks a backfilled sample: it was taken while the agent was not connected,
	// so its T is the agent's clock and the hub converts it with the hello offset.
	B bool `json:"b,omitempty"`
}

// Hello identifies the agent at the start of each connection.
type Hello struct {
	ID       string `json:"id"`    // stable machine id, see ValidID
	Name     string `json:"name"`  // display name, e.g. "GPU rig"
	Instance string `json:"inst"`  // random per agent process start
	Now      int64  `json:"now"`   // agent wall clock (unix ms) when the hello was written
	Agent    string `json:"agent"` // agent version
	Info     Info   `json:"info"`
}

// Info describes the machine. It changes rarely (network interface, disk size).
type Info struct {
	OS        string    `json:"os"`               // "linux" or "windows"
	OSName    string    `json:"osName,omitempty"` // e.g. "Ubuntu 26.04.1 LTS"
	Hostname  string    `json:"host,omitempty"`
	CPUModel  string    `json:"cpu,omitempty"`     // cleaned, e.g. "Ryzen 9 7950X"
	Cores     int       `json:"cores,omitempty"`   // physical cores, 0 if unknown
	Threads   int       `json:"threads,omitempty"` // logical processors
	MemTotal  int64     `json:"memTotal,omitempty"`
	SwapLabel string    `json:"swapLabel,omitempty"` // "Swap" (Linux) or "page file" (Windows)
	HasTemp   bool      `json:"hasTemp"`             // false: no CPU temperature source exists
	Battery   bool      `json:"battery,omitempty"`   // machine has a battery
	GPUs      []GPUInfo `json:"gpus,omitempty"`      // shown GPUs, primary first; same order as Sample.GPU
	Net       NetInfo   `json:"net"`
	Disk      DiskInfo  `json:"disk"`
	BootTime  int64     `json:"boot,omitempty"` // agent wall clock unix ms
	Sim       bool      `json:"sim,omitempty"`  // data is simulated (agent --simulate)
}

// GPUInfo describes one shown GPU.
type GPUInfo struct {
	Name       string `json:"name"`               // cleaned, e.g. "RTX A6000"
	Vendor     string `json:"vendor,omitempty"`   // "nvidia", "amd", "intel"
	MemTotal   int64  `json:"memTotal,omitempty"` // bytes, 0 if unknown
	Integrated bool   `json:"integrated,omitempty"`
	// Shared: MemTotal and MemUsed include the system memory the GPU maps, not only VRAM
	// (an AMD APU on Linux: the small VRAM carve-out plus GTT, which is where its work lives).
	Shared bool `json:"shared,omitempty"`
}

// NetInfo describes the interface the network strip follows.
type NetInfo struct {
	Name    string `json:"name,omitempty"` // "wlp76s0", "Ethernet", "Wi-Fi"
	Desc    string `json:"desc,omitempty"` // adapter description, e.g. "Marvell AQtion 10Gbit"
	Addr    string `json:"addr,omitempty"` // IPv4 address
	Kind    string `json:"kind,omitempty"` // "wired", "wireless", "loopback", "virtual"
	LinkBps int64  `json:"link,omitempty"` // link speed in bits per second, 0 if unknown
}

// DiskInfo describes the system volume shown in the disk readout.
type DiskInfo struct {
	Label string `json:"label,omitempty"` // "root" or "C:"
	Model string `json:"model,omitempty"` // e.g. "Samsung SSD 990 PRO 4TB"
	Bus   string `json:"bus,omitempty"`   // e.g. "NVMe", "SATA"
}

// Sample is one 1-second measurement. A nil field means the agent could not read it.
// Rates are averages over the interval since the previous sample.
type Sample struct {
	Seq uint64 `json:"q,omitempty"` // agent sequence number (stripped by the hub)
	T   int64  `json:"t"`           // unix ms; agent clock from agents, hub receive time from the hub

	CPU     []uint16  `json:"c,omitempty"`  // per logical processor busy time, per mille (0..1000)
	CPUTemp *float64  `json:"ct,omitempty"` // °C
	CPUFreq *float64  `json:"cf,omitempty"` // MHz
	Load    []float64 `json:"la,omitempty"` // load average 1/5/15 min (Linux)

	GPU []GPUSample `json:"g,omitempty"` // same order as Info.GPUs

	MemUsed     *int64 `json:"mu,omitempty"` // bytes in use (total - available)
	MemCached   *int64 `json:"mc,omitempty"` // bytes of file cache, where the OS reports it
	MemFree     *int64 `json:"mf,omitempty"` // bytes completely free
	SwapUsed    *int64 `json:"su,omitempty"`
	SwapTotal   *int64 `json:"st,omitempty"`
	CommitUsed  *int64 `json:"cu,omitempty"` // Windows commit charge
	CommitLimit *int64 `json:"cl,omitempty"`

	DiskRead  *int64 `json:"dr,omitempty"` // bytes/s, all physical disks
	DiskWrite *int64 `json:"dw,omitempty"` // bytes/s, all physical disks
	DiskUsed  *int64 `json:"du,omitempty"` // system volume
	DiskTotal *int64 `json:"dt,omitempty"`

	NetRx      *int64 `json:"nr,omitempty"`  // bytes/s on the followed interface
	NetTx      *int64 `json:"nt,omitempty"`  // bytes/s
	NetRxTotal *int64 `json:"nrt,omitempty"` // interface counter, bytes
	NetTxTotal *int64 `json:"ntt,omitempty"`

	Battery  *int   `json:"b,omitempty"`  // percent
	BatState int    `json:"bs,omitempty"` // see Bat*
	Uptime   *int64 `json:"up,omitempty"` // seconds since boot

	AgentCPU *float64 `json:"acpu,omitempty"` // agent's own CPU use, % of one core, children included
	AgentRSS *int64   `json:"arss,omitempty"` // agent's own resident memory, bytes
}

// Battery states.
const (
	BatUnknown     = 0
	BatCharging    = 1
	BatDischarging = 2
	BatFull        = 3 // on AC, not charging
)

// GPUSample is one GPU's measurement.
type GPUSample struct {
	Util     *uint16  `json:"u,omitempty"`  // per mille
	MemUsed  *int64   `json:"m,omitempty"`  // bytes
	Temp     *float64 `json:"t,omitempty"`  // °C
	Power    *float64 `json:"p,omitempty"`  // W
	PState   *int     `json:"ps,omitempty"` // 0..15
	Fan      *int     `json:"f,omitempty"`  // percent
	Clock    *int     `json:"ck,omitempty"` // graphics clock MHz
	MemClock *int     `json:"mk,omitempty"` // memory clock MHz
	PcieTx   *int64   `json:"tx,omitempty"` // bytes/s
	PcieRx   *int64   `json:"rx,omitempty"` // bytes/s
}

// Helpers for building optional fields.
func I64(v int64) *int64     { return &v }
func F64(v float64) *float64 { return &v }
func Int(v int) *int         { return &v }
func U16(v uint16) *uint16   { return &v }
func PerMille(f float64) uint16 { // f in 0..1
	if f != f || f <= 0 { // NaN or negative
		return 0
	}
	if f >= 1 {
		return 1000
	}
	return uint16(f*1000 + 0.5)
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether id is a usable machine id.
func ValidID(id string) bool { return idRE.MatchString(id) }

// Limits applied by the hub to everything an agent sends.
const (
	MaxThreads   = 512
	MaxGPUs      = 8
	MaxNameLen   = 48
	MaxStringLen = 128
	MaxLineBytes = 64 << 10
)

// Validate checks a hello for sane sizes.
func (h *Hello) Validate() error {
	if !ValidID(h.ID) {
		return fmt.Errorf("invalid machine id %q (use a-z, 0-9 and -, max 32)", h.ID)
	}
	if h.Name == "" || len(h.Name) > MaxNameLen {
		return errors.New("name must be 1-48 bytes")
	}
	if len(h.Instance) == 0 || len(h.Instance) > 64 {
		return errors.New("bad instance")
	}
	return h.Info.Validate()
}

// Validate checks info for sane sizes.
func (i *Info) Validate() error {
	if i.Threads < 0 || i.Threads > MaxThreads || len(i.GPUs) > MaxGPUs {
		return errors.New("info out of range")
	}
	for _, s := range []string{i.OS, i.OSName, i.Hostname, i.CPUModel, i.SwapLabel, i.Net.Name, i.Net.Desc, i.Net.Addr, i.Net.Kind, i.Disk.Label, i.Disk.Model, i.Disk.Bus} {
		if len(s) > MaxStringLen {
			return errors.New("info string too long")
		}
	}
	for _, g := range i.GPUs {
		if len(g.Name) > MaxStringLen || len(g.Vendor) > MaxStringLen {
			return errors.New("gpu string too long")
		}
	}
	return nil
}

// Validate checks a sample against the machine's info.
func (s *Sample) Validate(info *Info) error {
	if len(s.CPU) > MaxThreads || len(s.GPU) > MaxGPUs || len(s.Load) > 3 {
		return errors.New("sample out of range")
	}
	for _, v := range s.CPU {
		if v > 1000 {
			return errors.New("cpu value out of range")
		}
	}
	for _, g := range s.GPU {
		if g.Util != nil && *g.Util > 1000 {
			return errors.New("gpu value out of range")
		}
	}
	return nil
}

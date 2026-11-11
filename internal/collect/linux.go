//go:build linux

package collect

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"btophub/internal/proto"
)

// linux reads /proc and /sys, plus NVML for NVIDIA GPUs. Every source is a file read or a
// library call; nothing spawns a process.
type linux struct {
	timer   Timer
	buf     []byte
	threads int

	prevT   time.Time
	prevCPU []cpuTimes

	tempPath, tempSrc string
	freqPaths         []string

	diskDevs    []string
	diskDevsAt  time.Time
	prevRd      uint64
	prevWr      uint64
	havePrevDsk bool
	diskSpaceAt time.Time
	diskUsed    int64
	diskTotal   int64

	netIf      string
	netInfo    proto.NetInfo
	netAt      time.Time
	prevRx     uint64
	prevTx     uint64
	havePrevNt bool

	nv     *Nvidia
	amd    []amdGPU
	gpuSrc string

	batDir string

	info    proto.Info
	infoAt  time.Time
	sources map[string]string
	notes   []string
}

type cpuTimes struct{ busy, total uint64 }

type amdGPU struct {
	dir  string // /sys/class/drm/cardN/device
	info proto.GPUInfo
}

// New returns the Linux collector.
func New() (Collector, error) {
	c := &linux{buf: make([]byte, 0, 64<<10), sources: map[string]string{}}
	c.sources["cpu"] = "/proc/stat (per logical processor jiffies)"
	c.sources["mem"] = "/proc/meminfo (used = MemTotal - MemAvailable, cached = Cached, free = MemFree)"
	c.sources["swap"] = "/proc/meminfo (SwapTotal, SwapFree)"
	c.sources["disk_io"] = "/proc/diskstats (whole physical disks)"
	c.sources["disk_space"] = "statfs(/)"
	c.sources["uptime"] = "/proc/uptime"
	c.sources["commit"] = "none: Linux overcommits; not shown"
	c.findTemp()
	c.findFreq()
	c.findBattery()
	c.findGPUs()
	c.refreshDisks(time.Now())
	c.refreshNet(time.Now())
	c.refreshInfo(time.Now())
	return c, nil
}

func (c *linux) read(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	c.buf = c.buf[:0]
	for {
		if len(c.buf) == cap(c.buf) {
			c.buf = append(c.buf, 0)[:len(c.buf)]
		}
		n, err := f.Read(c.buf[len(c.buf):cap(c.buf)])
		c.buf = c.buf[:len(c.buf)+n]
		if err != nil {
			break
		}
	}
	return c.buf
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(path string) (uint64, bool) {
	s := readTrim(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// ---- discovery -------------------------------------------------------------

// findTemp looks for the CPU package sensor: k10temp Tctl (AMD), coretemp Package (Intel).
func (c *linux) findTemp() {
	want := map[string][]string{
		"k10temp":  {"Tctl", "Tdie"},
		"zenpower": {"Tdie", "Tctl"},
		"coretemp": {"Package id 0"},
	}
	hw, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, dir := range hw {
		name := readTrim(filepath.Join(dir, "name"))
		labels, ok := want[name]
		if !ok {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
		sort.Strings(inputs)
		for _, lbl := range labels {
			for _, in := range inputs {
				if readTrim(strings.TrimSuffix(in, "_input")+"_label") == lbl {
					c.tempPath = in
					c.tempSrc = fmt.Sprintf("%s %s (%s)", name, lbl, in)
					c.sources["cpu_temp"] = c.tempSrc
					return
				}
			}
		}
	}
	c.sources["cpu_temp"] = "none: no k10temp/zenpower/coretemp sensor in /sys/class/hwmon"
}

func (c *linux) findFreq() {
	paths, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	c.freqPaths = paths
	if len(paths) > 0 {
		c.sources["cpu_freq"] = fmt.Sprintf("cpufreq scaling_cur_freq, mean of %d CPUs", len(paths))
	} else {
		c.sources["cpu_freq"] = "none: no cpufreq in /sys"
	}
}

func (c *linux) findBattery() {
	dirs, _ := filepath.Glob("/sys/class/power_supply/*")
	for _, d := range dirs {
		if readTrim(filepath.Join(d, "type")) == "Battery" && readTrim(filepath.Join(d, "present")) != "0" {
			c.batDir = d
			c.sources["battery"] = d
			return
		}
	}
	c.sources["battery"] = "none: no battery in /sys/class/power_supply"
}

// findGPUs prefers NVIDIA (NVML). Integrated GPUs are only shown when they are the only GPU.
func (c *linux) findGPUs() {
	nv, why := OpenNvidia()
	amd := findAMD()
	if nv != nil {
		c.nv = nv
		c.gpuSrc = why
		for i, g := range nv.Infos() {
			c.sources[fmt.Sprintf("gpu%d", i)] = fmt.Sprintf("%s (%s)", g.Name, why)
		}
		for _, a := range amd {
			c.notes = append(c.notes, fmt.Sprintf("hidden: %s at %s (a discrete NVIDIA GPU is present)", a.info.Name, a.dir))
		}
		return
	}
	c.notes = append(c.notes, why)
	if len(amd) > 0 {
		c.amd = amd
		for i, a := range amd {
			c.sources[fmt.Sprintf("gpu%d", i)] = "amdgpu sysfs " + a.dir
		}
		return
	}
	c.sources["gpu0"] = "none: no NVIDIA (NVML) or amdgpu device"
}

func findAMD() []amdGPU {
	var out []amdGPU
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue // connectors like card1-DP-1
		}
		dev := filepath.Join(card, "device")
		drv, _ := os.Readlink(filepath.Join(dev, "driver"))
		if filepath.Base(drv) != "amdgpu" {
			continue
		}
		total, _ := readUint(filepath.Join(dev, "mem_info_vram_total"))
		name := readTrim(filepath.Join(dev, "product_name"))
		if name == "" {
			name = "AMD GPU"
		}
		// An APU's carve-out is small and it sits on the CPU's own PCIe root; treat any amdgpu
		// with <= 4 GiB of VRAM as integrated. It is only shown when no NVIDIA GPU exists.
		out = append(out, amdGPU{dir: dev, info: proto.GPUInfo{
			Name: CleanGPUName(name), Vendor: "amd", MemTotal: int64(total), Integrated: total <= 4<<30,
		}})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].info.MemTotal > out[b].info.MemTotal })
	return out
}

var virtualIfPrefixes = []string{"lo", "docker", "br-", "veth", "virbr", "vnet", "tailscale", "wg", "tun", "tap", "zt", "cni", "flannel", "kube", "lxc", "lxd", "podman", "vmnet", "vboxnet"}

func isVirtualIf(name string) bool {
	for _, p := range virtualIfPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	_, err := os.Stat("/sys/class/net/" + name + "/device")
	return err != nil // no backing device: virtual
}

// defaultRouteIfs returns interfaces with an IPv4 default route, lowest metric first.
func defaultRouteIfs() []string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	type r struct {
		name   string
		metric int
	}
	var rs []r
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 8 || fs[1] != "00000000" || fs[7] != "00000000" {
			continue
		}
		flags, _ := strconv.ParseUint(fs[3], 16, 32)
		if flags&1 == 0 { // RTF_UP
			continue
		}
		m, _ := strconv.Atoi(fs[6])
		rs = append(rs, r{fs[0], m})
	}
	sort.SliceStable(rs, func(a, b int) bool { return rs[a].metric < rs[b].metric })
	out := make([]string, 0, len(rs))
	for _, x := range rs {
		out = append(out, x.name)
	}
	return out
}

// refreshNet follows the default-route interface, skipping loopback and virtual adapters.
func (c *linux) refreshNet(now time.Time) {
	c.netAt = now
	cands := defaultRouteIfs()
	pick := ""
	for _, n := range cands {
		if !isVirtualIf(n) {
			pick = n
			break
		}
	}
	kindOverride := ""
	if pick == "" && len(cands) > 0 {
		pick, kindOverride = cands[0], "virtual"
	}
	if pick == "" {
		c.netIf = ""
		c.netInfo = proto.NetInfo{}
		c.sources["net"] = "none: no IPv4 default route"
		return
	}
	if pick != c.netIf {
		c.havePrevNt = false
	}
	c.netIf = pick
	ni := proto.NetInfo{Name: pick, Kind: kindOverride}
	base := "/sys/class/net/" + pick
	if ni.Kind == "" {
		switch {
		case exists(base + "/wireless"), exists(base + "/phy80211"):
			ni.Kind = "wireless"
		case pick == "lo":
			ni.Kind = "loopback"
		default:
			ni.Kind = "wired"
		}
	}
	if ni.Kind == "wired" {
		if s, ok := readUint(base + "/speed"); ok && s > 0 && s < 1e6 {
			ni.LinkBps = int64(s) * 1e6
		}
	}
	if drv, err := os.Readlink(base + "/device/driver"); err == nil {
		ni.Desc = filepath.Base(drv)
	}
	if ifc, err := net.InterfaceByName(pick); err == nil {
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
					ni.Addr = ipn.IP.String()
					break
				}
			}
		}
	}
	c.netInfo = ni
	c.sources["net"] = fmt.Sprintf("/sys/class/net/%s/statistics (IPv4 default route, %s)", pick, ni.Kind)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// refreshDisks lists whole physical disks (they have a backing device; partitions,
// loop, device-mapper and md arrays are skipped so nothing is counted twice).
func (c *linux) refreshDisks(now time.Time) {
	c.diskDevsAt = now
	ents, _ := os.ReadDir("/sys/block")
	var devs []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "loop") || strings.HasPrefix(n, "ram") || strings.HasPrefix(n, "zram") ||
			strings.HasPrefix(n, "dm-") || strings.HasPrefix(n, "md") || strings.HasPrefix(n, "sr") || strings.HasPrefix(n, "nbd") {
			continue
		}
		if exists("/sys/block/" + n + "/device") {
			devs = append(devs, n)
		}
	}
	c.diskDevs = devs
}

func (c *linux) refreshInfo(now time.Time) {
	c.infoAt = now
	info := proto.Info{OS: "linux", SwapLabel: "Swap"}
	if pn := osRelease(); pn != "" {
		info.OSName = pn
	}
	info.Hostname, _ = os.Hostname()
	info.CPUModel, info.Cores = cpuModel()
	info.Threads = c.threads
	if info.Threads == 0 {
		info.Threads = runtime.NumCPU()
	}
	info.HasTemp = c.tempPath != ""
	info.Battery = c.batDir != ""
	if b := c.read("/proc/meminfo"); b != nil {
		m := parseMeminfo(b)
		info.MemTotal = m["MemTotal"]
	}
	switch {
	case c.nv != nil:
		info.GPUs = c.nv.Infos()
	case len(c.amd) > 0:
		// Integrated only when it is the only GPU: here every GPU is AMD, so show them all.
		for _, a := range c.amd {
			info.GPUs = append(info.GPUs, a.info)
		}
	}
	info.Net = c.netInfo
	info.Disk = rootDiskInfo()
	if up, ok := c.uptime(); ok {
		info.BootTime = now.Add(-time.Duration(up * float64(time.Second))).UnixMilli()
	}
	c.info = info
}

func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func cpuModel() (string, int) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", 0
	}
	model := ""
	cores := map[string]bool{}
	phys := ""
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			if model == "" {
				model = v
			}
		case "physical id":
			phys = v
		case "core id":
			cores[phys+"/"+v] = true
		}
	}
	return CleanCPUName(model), len(cores)
}

// rootDiskInfo names the disk holding / by following /dev/... -> /sys/block.
func rootDiskInfo() proto.DiskInfo {
	di := proto.DiskInfo{Label: "root"}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return di
	}
	var majmin string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && f[4] == "/" {
			majmin = f[2]
		}
	}
	if majmin == "" {
		return di
	}
	link, err := os.Readlink("/sys/dev/block/" + majmin)
	if err != nil {
		return di
	}
	// .../nvme/nvme2/nvme2n1/nvme2n1p2 -> nvme2n1 ; .../block/sda/sda1 -> sda:
	// the disk is the path component that is a whole device in /sys/block.
	disk := ""
	for _, p := range strings.Split(link, "/") {
		if p != "" && p != "." && p != ".." && exists("/sys/block/"+p) {
			disk = p
			break
		}
	}
	if disk == "" {
		return di
	}
	di.Model = readTrim("/sys/block/" + disk + "/device/model")
	switch {
	case strings.HasPrefix(disk, "nvme"):
		di.Bus = "NVMe"
	case strings.HasPrefix(disk, "sd"):
		if strings.Contains(link, "/usb") {
			di.Bus = "USB"
		} else {
			di.Bus = "SATA"
		}
	case strings.HasPrefix(disk, "mmcblk"):
		di.Bus = "eMMC"
	}
	return di
}

// ---- Collector -------------------------------------------------------------

func (c *linux) Info() proto.Info {
	now := time.Now()
	if now.Sub(c.netAt) >= 10*time.Second {
		c.refreshNet(now)
	}
	if now.Sub(c.infoAt) >= 10*time.Second || c.info.Net != c.netInfo {
		c.refreshInfo(now)
	}
	return c.info
}

func (c *linux) Report() Report {
	src := make(map[string]string, len(c.sources))
	for k, v := range c.sources {
		src[k] = v
	}
	return Report{Sources: src, Timings: c.timer.Snapshot(), Notes: append(append([]string(nil), c.notes...), c.timer.Panics()...)}
}

func (c *linux) Close() {
	if c.nv != nil {
		c.nv.Close()
	}
}

func (c *linux) Collect(now time.Time) proto.Sample {
	s := proto.Sample{T: now.UnixMilli()}
	dt := now.Sub(c.prevT)
	if c.prevT.IsZero() {
		dt = 0
	}
	c.prevT = now

	c.timer.Measure("cpu", func() { c.collectCPU(&s) })
	c.timer.Measure("cpu_temp_freq", func() { c.collectTempFreq(&s) })
	c.timer.Measure("mem", func() { c.collectMem(&s) })
	c.timer.Measure("disk", func() { c.collectDisk(&s, now, dt) })
	c.timer.Measure("net", func() { c.collectNet(&s, now, dt) })
	c.timer.Measure("gpu", func() { c.collectGPU(&s) })
	c.timer.Measure("misc", func() { c.collectMisc(&s) })
	return s
}

func (c *linux) collectCPU(s *proto.Sample) {
	b := c.read("/proc/stat")
	if b == nil {
		return
	}
	var cur []cpuTimes
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if len(line) < 4 || !bytes.HasPrefix(line, []byte("cpu")) || line[3] < '0' || line[3] > '9' {
			continue
		}
		var v [10]uint64
		f := 0
		for _, fld := range bytes.Fields(line)[1:] {
			if f == len(v) {
				break
			}
			v[f], _ = strconv.ParseUint(string(fld), 10, 64)
			f++
		}
		// user nice system idle iowait irq softirq steal (guest is already in user)
		idle := v[3] + v[4]
		busy := v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
		cur = append(cur, cpuTimes{busy: busy, total: busy + idle})
	}
	c.threads = len(cur)
	if len(c.prevCPU) == len(cur) {
		s.CPU = make([]uint16, len(cur))
		for i := range cur {
			db := cur[i].busy - c.prevCPU[i].busy
			dtot := cur[i].total - c.prevCPU[i].total
			if dtot > 0 && cur[i].busy >= c.prevCPU[i].busy {
				s.CPU[i] = proto.PerMille(float64(db) / float64(dtot))
			}
		}
	}
	c.prevCPU = cur
	if la := strings.Fields(readTrim("/proc/loadavg")); len(la) >= 3 {
		for _, x := range la[:3] {
			v, _ := strconv.ParseFloat(x, 64)
			s.Load = append(s.Load, v)
		}
	}
}

func (c *linux) collectTempFreq(s *proto.Sample) {
	if c.tempPath != "" {
		if v, ok := readUint(c.tempPath); ok {
			s.CPUTemp = proto.F64(float64(v) / 1000)
		}
	}
	if len(c.freqPaths) > 0 {
		var sum, n uint64
		for _, p := range c.freqPaths {
			if v, ok := readUint(p); ok {
				sum += v
				n++
			}
		}
		if n > 0 {
			s.CPUFreq = proto.F64(float64(sum) / float64(n) / 1000) // kHz -> MHz
		}
	}
}

func parseMeminfo(b []byte) map[string]int64 {
	m := make(map[string]int64, 16)
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		k, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		f := bytes.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(string(f[0]), 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && string(f[1]) == "kB" {
			v *= 1024
		}
		m[string(k)] = v
	}
	return m
}

func (c *linux) collectMem(s *proto.Sample) {
	b := c.read("/proc/meminfo")
	if b == nil {
		return
	}
	m := parseMeminfo(b)
	total, okT := m["MemTotal"]
	avail, okA := m["MemAvailable"]
	if okT && okA {
		s.MemUsed = proto.I64(total - avail)
	}
	if v, ok := m["Cached"]; ok {
		s.MemCached = proto.I64(v)
	}
	if v, ok := m["MemFree"]; ok {
		s.MemFree = proto.I64(v)
	}
	st, ok1 := m["SwapTotal"]
	sf, ok2 := m["SwapFree"]
	if ok1 && ok2 {
		s.SwapTotal = proto.I64(st)
		s.SwapUsed = proto.I64(st - sf)
	}
}

func (c *linux) collectDisk(s *proto.Sample, now time.Time, dt time.Duration) {
	if now.Sub(c.diskDevsAt) >= time.Minute {
		c.refreshDisks(now)
	}
	if b := c.read("/proc/diskstats"); b != nil && len(c.diskDevs) > 0 {
		var rd, wr uint64
		for len(b) > 0 {
			line := b
			if i := bytes.IndexByte(b, '\n'); i >= 0 {
				line, b = b[:i], b[i+1:]
			} else {
				b = nil
			}
			f := bytes.Fields(line)
			if len(f) < 10 {
				continue
			}
			name := string(f[2])
			found := false
			for _, d := range c.diskDevs {
				if d == name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			r, _ := strconv.ParseUint(string(f[5]), 10, 64)
			w, _ := strconv.ParseUint(string(f[9]), 10, 64)
			rd += r * 512
			wr += w * 512
		}
		if c.havePrevDsk && dt > 0 {
			s.DiskRead = Rate(c.prevRd, rd, dt)
			s.DiskWrite = Rate(c.prevWr, wr, dt)
		}
		c.prevRd, c.prevWr, c.havePrevDsk = rd, wr, true
	}
	if now.Sub(c.diskSpaceAt) >= 10*time.Second {
		var st syscall.Statfs_t
		if err := syscall.Statfs("/", &st); err == nil {
			bs := int64(st.Bsize)
			c.diskTotal = int64(st.Blocks) * bs
			c.diskUsed = (int64(st.Blocks) - int64(st.Bavail)) * bs // reserved blocks count as used
		}
		c.diskSpaceAt = now
	}
	if c.diskTotal > 0 {
		s.DiskUsed, s.DiskTotal = proto.I64(c.diskUsed), proto.I64(c.diskTotal)
	}
}

func (c *linux) collectNet(s *proto.Sample, now time.Time, dt time.Duration) {
	if c.netIf == "" {
		return
	}
	base := "/sys/class/net/" + c.netIf + "/statistics/"
	rx, ok1 := readUint(base + "rx_bytes")
	tx, ok2 := readUint(base + "tx_bytes")
	if !ok1 || !ok2 {
		c.havePrevNt = false
		return
	}
	s.NetRxTotal, s.NetTxTotal = proto.I64(int64(rx)), proto.I64(int64(tx))
	if c.havePrevNt && dt > 0 {
		s.NetRx = Rate(c.prevRx, rx, dt)
		s.NetTx = Rate(c.prevTx, tx, dt)
	}
	c.prevRx, c.prevTx, c.havePrevNt = rx, tx, true
}

func (c *linux) collectGPU(s *proto.Sample) {
	switch {
	case c.nv != nil:
		s.GPU = c.nv.Sample()
	case len(c.amd) > 0:
		s.GPU = make([]proto.GPUSample, len(c.amd))
		for i, a := range c.amd {
			if v, ok := readUint(a.dir + "/gpu_busy_percent"); ok {
				s.GPU[i].Util = proto.U16(proto.PerMille(float64(v) / 100))
			}
			if v, ok := readUint(a.dir + "/mem_info_vram_used"); ok {
				s.GPU[i].MemUsed = proto.I64(int64(v))
			}
			if ins, _ := filepath.Glob(a.dir + "/hwmon/hwmon*/temp1_input"); len(ins) > 0 {
				if v, ok := readUint(ins[0]); ok {
					s.GPU[i].Temp = proto.F64(float64(v) / 1000)
				}
			}
		}
	}
}

func (c *linux) uptime() (float64, bool) {
	f := strings.Fields(readTrim("/proc/uptime"))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func (c *linux) collectMisc(s *proto.Sample) {
	if up, ok := c.uptime(); ok {
		s.Uptime = proto.I64(int64(up))
	}
	if c.batDir != "" {
		if v, ok := readUint(c.batDir + "/capacity"); ok {
			s.Battery = proto.Int(int(v))
		}
		switch readTrim(c.batDir + "/status") {
		case "Charging":
			s.BatState = proto.BatCharging
		case "Discharging":
			s.BatState = proto.BatDischarging
		case "Full", "Not charging":
			s.BatState = proto.BatFull
		}
	}
}

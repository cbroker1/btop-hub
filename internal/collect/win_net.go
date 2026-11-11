//go:build windows && (amd64 || arm64)

package collect

// Network: follow the IPv4 default-route interface that is real hardware. The choice is
// made at New and re-evaluated at most every ~10 s from Info(); each sample is a single
// GetIfEntry2Ex on the chosen interface.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"btophub/internal/proto"
)

// winNet is the interface the network strip follows.
type winNet struct {
	ok     bool
	index  uint32
	luid   uint64
	info   proto.NetInfo
	source string

	prevOK          bool
	prevIn, prevOut uint64
	prevT           time.Time
}

// winNetCand is one interface that could be followed.
type winNetCand struct {
	index    uint32
	luid     uint64
	metric   uint64 // route metric + interface metric (what Windows compares)
	ifMetric bool   // interface metric was added
	best     bool   // GetBestInterfaceEx's choice toward 1.1.1.1

	ok       bool // GetIfEntry2Ex succeeded
	err      error
	alias    string
	desc     string
	ifType   uint32
	hardware bool
	up       bool
	speed    uint64
}

func (c *winCollector) initNet() {
	if err := winHave(winModIphlpapi, "GetIpForwardTable2", "FreeMibTable"); err != nil {
		c.setSource("net", "none: iphlpapi: "+err.Error())
		return
	}
	c.haveIfEntry2Ex = winHave(winModIphlpapi, "GetIfEntry2Ex") == nil
	if !c.haveIfEntry2Ex {
		if err := winProcGetIfEntry2.Find(); err != nil {
			c.setSource("net", "none: iphlpapi: "+err.Error())
			return
		}
	}
	c.haveNet = true
	c.haveBest = winHave(winModIphlpapi, "GetBestInterfaceEx") == nil
	c.haveAddrs = winHave(winModIphlpapi, "GetAdaptersAddresses") == nil
	c.haveIPIf = winHave(winModIphlpapi, "GetIpInterfaceEntry") == nil
	t := time.Now()
	c.chooseNet()
	c.initTimes = append(c.initTimes, fmt.Sprintf("network %.1f ms", winMs(time.Since(t))))
}

// ifEntry fills row (InterfaceLuid or InterfaceIndex set) with GetIfEntry2Ex at
// MibIfEntryNormal, or GetIfEntry2 where Ex does not exist. row is collector memory.
func (c *winCollector) ifEntry(row *windows.MibIfRow2) error {
	if c.haveIfEntry2Ex {
		return windows.GetIfEntry2Ex(windows.MibIfEntryNormal, row)
	}
	r, _, _ := syscall.SyscallN(winProcGetIfEntry2.Addr(), uintptr(unsafe.Pointer(row)))
	if uint32(r) != 0 {
		return syscall.Errno(uint32(r))
	}
	return nil
}

// chooseNet picks the interface to follow: IPv4 default routes ordered by the stack's own
// choice toward 1.1.1.1 (GetBestInterfaceEx sends no packet), then route+interface metric;
// the first that is up and real hardware wins. If none is, the best one is used and called
// "virtual". Without any default route the previous interface is kept while it exists.
func (c *winCollector) chooseNet() {
	c.netEvalAt = time.Now()
	cands, err := c.defaultRoutes()
	if err != nil {
		c.note("net_routes", "net: GetIpForwardTable2: "+err.Error())
	} else {
		c.dropNote("net_routes")
	}
	c.bestIdx = 0
	bestErr := errors.New("not available")
	if c.haveBest {
		bestErr = windows.GetBestInterfaceEx(&c.bestSA, &c.bestIdx)
	}
	if bestErr != nil {
		c.note("net_best", "net: GetBestInterfaceEx: "+bestErr.Error())
	} else if c.bestIdx != 0 {
		c.dropNote("net_best")
		found := false
		for i := range cands {
			if cands[i].index == c.bestIdx {
				cands[i].best, found = true, true
			}
		}
		if !found {
			cands = append(cands, winNetCand{index: c.bestIdx, best: true})
		}
	}
	for i := range cands {
		c.describeIf(&cands[i])
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := &cands[i], &cands[j]
		if a.best != b.best {
			return a.best
		}
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		return a.index < b.index
	})
	pick := -1
	for pass := 0; pass < 3 && pick < 0; pass++ {
		for i := range cands {
			x := &cands[i]
			ok := x.ok && (pass == 2 || x.up) && (pass > 0 || winNetUsable(x.ifType, x.hardware, x.alias, x.desc))
			if ok {
				pick = i
				break
			}
		}
	}
	c.note("net_cands", winNetCandNote(cands, pick))

	if pick < 0 {
		if c.net.ok {
			c.note("net_keep", fmt.Sprintf("net: no IPv4 default route; still following %q", c.net.info.Name))
			if addr, err := c.ipv4Addr(c.net.index); err == nil {
				c.net.info.Addr = addr // the address usually goes away with the route
			}
			return
		}
		c.net = winNet{}
		c.setSource("net", "none: no IPv4 default route")
		return
	}
	c.dropNote("net_keep")
	p := &cands[pick]
	addr, aerr := c.ipv4Addr(p.index)
	if aerr != nil {
		c.note("net_addr", "net: GetAdaptersAddresses: "+aerr.Error())
	} else {
		c.dropNote("net_addr")
	}
	var link int64
	if p.speed > 0 && p.speed < math.MaxInt64 { // ULONG64_MAX means unknown
		link = int64(p.speed)
	}
	if !c.net.ok || c.net.index != p.index || c.net.luid != p.luid {
		c.net = winNet{} // a different interface: start a new rate baseline
	}
	c.net.ok, c.net.index, c.net.luid = true, p.index, p.luid
	c.net.info = proto.NetInfo{
		Name:    winTrunc(p.alias, proto.MaxStringLen),
		Desc:    winTrunc(p.desc, proto.MaxStringLen),
		Addr:    addr,
		Kind:    winNetKind(p.ifType, p.hardware, p.alias, p.desc),
		LinkBps: link,
	}
	how := "IPv4 default route"
	if p.best {
		how += ", the stack's choice toward 1.1.1.1"
	}
	fn := "GetIfEntry2Ex"
	if !c.haveIfEntry2Ex {
		fn = "GetIfEntry2"
	}
	c.net.source = fmt.Sprintf("%s on %q (ifIndex %d; %s, metric %d)", fn, p.alias, p.index, how, p.metric)
	c.setSource("net", c.net.source)
}

// defaultRoutes lists IPv4 0.0.0.0/0 routes with route metric + interface metric.
func (c *winCollector) defaultRoutes() ([]winNetCand, error) {
	c.routeTable = nil
	if err := windows.GetIpForwardTable2(windows.AF_INET, &c.routeTable); err != nil {
		return nil, err
	}
	t := c.routeTable
	c.routeTable = nil
	if t == nil {
		return nil, nil
	}
	var out []winNetCand
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength != 0 || r.DestinationPrefix.Prefix.Family != windows.AF_INET {
			continue
		}
		out = append(out, winNetCand{index: r.InterfaceIndex, luid: r.InterfaceLuid, metric: uint64(r.Metric)})
	}
	windows.FreeMibTable(unsafe.Pointer(t))
	for i := 0; i < len(out) && c.haveIPIf; i++ {
		c.ipifRow = windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceLuid: out[i].luid, InterfaceIndex: out[i].index}
		if err := windows.GetIpInterfaceEntry(&c.ipifRow); err == nil {
			out[i].metric += uint64(c.ipifRow.Metric)
			out[i].ifMetric = true
		}
	}
	return out, nil
}

// describeIf fills a candidate from GetIfEntry2Ex.
func (c *winCollector) describeIf(x *winNetCand) {
	c.evalRow = windows.MibIfRow2{InterfaceLuid: x.luid, InterfaceIndex: x.index}
	if err := c.ifEntry(&c.evalRow); err != nil {
		x.err = err
		return
	}
	r := &c.evalRow
	x.ok = true
	x.luid = r.InterfaceLuid
	x.alias = windows.UTF16ToString(r.Alias[:])
	x.desc = windows.UTF16ToString(r.Description[:])
	x.ifType = r.Type
	x.hardware = r.InterfaceAndOperStatusFlags&1 != 0 // HardwareInterface is bit 0
	x.up = r.OperStatus == windows.IfOperStatusUp
	x.speed = r.ReceiveLinkSpeed
}

func winNetCandNote(cands []winNetCand, pick int) string {
	if len(cands) == 0 {
		return "net: no IPv4 default route"
	}
	var b strings.Builder
	b.WriteString("net: default-route candidates, best first:")
	for i := range cands {
		x := &cands[i]
		if !x.ok {
			fmt.Fprintf(&b, " [ifIndex %d: GetIfEntry2: %v]", x.index, x.err)
			continue
		}
		fmt.Fprintf(&b, " [%q %q ifIndex %d type %d metric %d", x.alias, x.desc, x.index, x.ifType, x.metric)
		if !x.ifMetric {
			b.WriteString(" (route only)")
		}
		if x.hardware {
			b.WriteString(" hardware")
		}
		if x.up {
			b.WriteString(" up")
		}
		if x.best {
			b.WriteString(" best-to-1.1.1.1")
		}
		if i == pick {
			b.WriteString(" CHOSEN")
		}
		b.WriteString("]")
	}
	return b.String()
}

// ipv4Addr returns the first IPv4 unicast address of the interface (preferring one that is
// not link-local), from GetAdaptersAddresses. Called from Info() only.
func (c *winCollector) ipv4Addr(index uint32) (string, error) {
	const flags = windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_DNS_SERVER | windows.GAA_FLAG_SKIP_FRIENDLY_NAME
	if !c.haveAddrs {
		return "", errors.New("iphlpapi has no GetAdaptersAddresses")
	}
	if len(c.aaBuf) < 16<<10 {
		c.aaBuf = make([]byte, 16<<10)
	}
	for attempt := 0; attempt < 4; attempt++ {
		c.aaSize = uint32(len(c.aaBuf))
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&c.aaBuf[0])), &c.aaSize)
		switch {
		case err == windows.ERROR_BUFFER_OVERFLOW:
			n := int(c.aaSize)
			if n <= len(c.aaBuf) {
				n = 2 * len(c.aaBuf)
			}
			if n > 4<<20 {
				return "", fmt.Errorf("wants a %d-byte buffer", n)
			}
			c.aaBuf = make([]byte, n)
			continue
		case err == windows.ERROR_NO_DATA:
			return "", nil // no adapter has IPv4
		case err != nil:
			return "", err
		}
		fallback := ""
		for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&c.aaBuf[0])); aa != nil; aa = aa.Next {
			if aa.IfIndex != index {
				continue
			}
			for u := aa.FirstUnicastAddress; u != nil; u = u.Next {
				if u.Address.Sockaddr == nil {
					continue
				}
				ip := u.Address.IP().To4()
				switch {
				case ip == nil:
				case ip.IsLinkLocalUnicast():
					if fallback == "" {
						fallback = ip.String()
					}
				default:
					return ip.String(), nil
				}
			}
		}
		return fallback, nil
	}
	return "", errors.New("buffer size kept changing")
}

// sampleNet reads the followed interface's byte counters.
func (c *winCollector) sampleNet(s *proto.Sample, now time.Time) {
	n := &c.net
	if !n.ok {
		return // Sources["net"] already says why
	}
	c.ifRow = windows.MibIfRow2{InterfaceLuid: n.luid, InterfaceIndex: n.index}
	if err := c.ifEntry(&c.ifRow); err != nil {
		n.prevOK = false
		c.setSource("net", fmt.Sprintf("none: GetIfEntry2(Ex)(%q): %v", n.info.Name, err))
		return
	}
	in, out := c.ifRow.InOctets, c.ifRow.OutOctets
	s.NetRxTotal, s.NetTxTotal = proto.I64(winI64(in)), proto.I64(winI64(out))
	if n.prevOK {
		dt := now.Sub(n.prevT)
		s.NetRx, s.NetTx = Rate(n.prevIn, in, dt), Rate(n.prevOut, out, dt)
	}
	n.prevIn, n.prevOut, n.prevT, n.prevOK = in, out, now, true
	c.setSource("net", n.source)
}

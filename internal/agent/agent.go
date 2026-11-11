// Package agent samples the local machine once a second, keeps the last hour, and
// streams it to the hub.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"btophub/internal/collect"
	"btophub/internal/platform"
	"btophub/internal/proto"
)

// Config is the agent's configuration file (agent.json); flags override it.
type Config struct {
	Hub       string `json:"hub"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
	ID        string `json:"id"`
	Name      string `json:"name"`

	Simulate string             `json:"simulate,omitempty"` // profile name, see collect.SimProfiles
	SimOpts  collect.SimOptions `json:"-"`
}

// LoadConfig reads path (a UTF-8 BOM, as Windows tools like to write, is tolerated).
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	b = []byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a display name into a machine id: "Gaming rig" -> "gaming-rig".
func Slug(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// Resolve fills defaults and reads the token file. Systemd's LoadCredential= is honoured:
// with no token configured, $CREDENTIALS_DIRECTORY/token is used.
func (c *Config) Resolve() error {
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = h
	}
	if c.ID == "" {
		c.ID = Slug(c.Name)
	}
	if !proto.ValidID(c.ID) {
		return fmt.Errorf("machine id %q is not valid: use a-z, 0-9 and -, up to 32 characters", c.ID)
	}
	if len(c.Name) > proto.MaxNameLen {
		return errors.New("name is longer than 48 bytes")
	}
	if c.Token == "" && c.TokenFile == "" {
		if d := os.Getenv("CREDENTIALS_DIRECTORY"); d != "" {
			c.TokenFile = filepath.Join(d, "token")
		}
	}
	if c.Token == "" && c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("token file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	return nil
}

func newCollector(c Config, now time.Time) (col collect.Collector, sim *collect.Sim, err error) {
	defer func() {
		if r := recover(); r != nil {
			col, sim, err = nil, nil, fmt.Errorf("collector failed to start: %v\n%s", r, debug.Stack())
		}
	}()
	if c.Simulate != "" {
		s, err := collect.NewSim(c.Simulate, c.SimOpts, now)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}
	return collectNew()
}

func collectNew() (collect.Collector, *collect.Sim, error) {
	c, err := collect.New()
	return c, nil, err
}

func instanceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Run samples once a second and streams to the hub until ctx ends.
func Run(ctx context.Context, c Config) error {
	if c.Hub == "" {
		return errors.New("no hub URL configured")
	}
	if c.Token == "" {
		return errors.New("no token configured")
	}
	if err := platform.SetLowPriority(); err != nil {
		log.Printf("could not lower priority: %v", err)
	}
	now := time.Now()
	col, sim, err := newCollector(c, now)
	if err != nil {
		return err
	}
	defer col.Close()

	ring := NewRing(proto.HistorySeconds + 60)
	if sim != nil {
		prefill(sim, ring, now)
	}
	hello := proto.Hello{ID: c.ID, Name: c.Name, Instance: instanceID(), Agent: proto.Version, Info: col.Info()}
	up := NewUplink(c.Hub, c.Token, hello, ring)
	replaced := make(chan struct{})
	go func() {
		if up.Run(ctx) != nil {
			close(replaced) // another agent now reports for this machine
		}
	}()
	log.Printf("btop-agent %s: %q (%s) -> %s", proto.Version, c.Name, c.ID, c.Hub)

	self := newSelfMeter()
	reported := false
	samples := 0
	info := hello.Info
	infoAt := now
	var last time.Time
	next := now.Truncate(time.Second).Add(time.Second)
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-replaced:
			return nil
		case <-timer.C:
		}
		now := time.Now()
		s, ok := safeCollect(col, now)
		if !ok {
			last = time.Time{} // re-prime after a failed sample
		}
		gap := now.Sub(last)
		primed := !last.IsZero()
		last = now
		if primed && gap <= 5*time.Second { // first sample after start or sleep only primes counters
			self.attach(&s, now)
			ring.Add(s)
			if !reported {
				reported = true
				logReport(col.Report())
			}
			if samples++; samples == 10 || samples == 300 {
				platform.TrimMemory() // after start-up, and once more when the hour of history is under way
			}
		}
		if now.Sub(infoAt) >= 10*time.Second {
			infoAt = now
			if ni := col.Info(); !sameInfo(ni, info) {
				info = ni
				up.SetInfo(ni)
			}
		}
		next = next.Add(time.Second)
		if d := time.Until(next); d < 100*time.Millisecond {
			next = time.Now().Truncate(time.Second).Add(time.Second)
		}
		timer.Reset(time.Until(next))
	}
}

// logReport writes where each metric comes from, once, so a service's view of its sources
// (which can differ from an interactive --probe) ends up in its log.
func logReport(r collect.Report) {
	keys := make([]string, 0, len(r.Sources))
	for k := range r.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		log.Printf("source %s: %s", k, r.Sources[k])
	}
	for _, n := range r.Notes {
		log.Printf("note: %s", n)
	}
}

var collectPanicked bool

// safeCollect takes a sample, turning a panic into a skipped sample (logged once).
func safeCollect(col collect.Collector, now time.Time) (s proto.Sample, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if !collectPanicked {
				log.Printf("sampling failed (logged once): %v\n%s", r, debug.Stack())
				collectPanicked = true
			}
			ok = false
		}
	}()
	return col.Collect(now), true
}

func sameInfo(a, b proto.Info) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// prefill gives a simulated machine an hour of history (minus before-boot and sleep gaps),
// which the uplink then backfills like a real agent would after a hub restart.
func prefill(sim *collect.Sim, ring *Ring, now time.Time) {
	start := now.Truncate(time.Second).Add(-proto.HistorySeconds * time.Second)
	for t := start; t.Before(now); t = t.Add(time.Second) {
		if sim.Before(t, now) {
			continue
		}
		ring.Add(sim.Collect(t))
	}
}

// selfMeter reports the agent's own CPU (averaged over a minute) and memory in each sample,
// so the resource budget can be checked on machines we cannot log into.
type selfMeter struct {
	hist []struct {
		at  time.Time
		cpu time.Duration
	}
}

func newSelfMeter() *selfMeter { return &selfMeter{} }

func (m *selfMeter) attach(s *proto.Sample, now time.Time) {
	cpu, rss, err := platform.SelfStats()
	if err != nil {
		return
	}
	m.hist = append(m.hist, struct {
		at  time.Time
		cpu time.Duration
	}{now, cpu})
	for len(m.hist) > 2 && now.Sub(m.hist[0].at) > 60*time.Second {
		m.hist = m.hist[1:]
	}
	if first := m.hist[0]; now.Sub(first.at) >= 5*time.Second {
		pct := float64(cpu-first.cpu) / float64(now.Sub(first.at)) * 100
		s.AgentCPU = proto.F64(float64(int(pct*1000)) / 1000)
	}
	s.AgentRSS = proto.I64(rss)
}

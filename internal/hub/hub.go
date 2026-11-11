// Package hub receives samples from agents, keeps the last hour per machine in memory,
// and serves the page, its history and a live event stream. It makes no outbound requests.
package hub

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"btophub/internal/proto"
)

// Config for the hub.
type Config struct {
	DataDir     string   // token and machines.json
	Order       []string // machine ids in display order; others follow alphabetically
	MaxMachines int
	MaxViewers  int
	PublicURL   string // e.g. http://192.168.1.74:28677, used in install scripts
	DistDir     string // agent binaries
}

// Hub is the server state.
type Hub struct {
	cfg   Config
	token string

	mu       sync.Mutex
	machines map[string]*machine
	viewers  map[*viewer]struct{}
	connSeq  uint64

	agents atomic.Int32
}

type viewer struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

func (v *viewer) close() { v.once.Do(func() { close(v.done) }) }

// New loads (or creates) the token and the list of known machines.
func New(cfg Config) (*Hub, error) {
	h := &Hub{cfg: cfg, machines: map[string]*machine{}, viewers: map[*viewer]struct{}{}}
	tok, created, err := LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	h.token = tok
	if created {
		log.Printf("created a new agent token in %s; show it with: docker exec btop-hub /hub token", filepath.Join(cfg.DataDir, "token"))
	}
	h.loadMachines()
	return h, nil
}

func now() int64 { return time.Now().UnixMilli() }

// ---- known machines (names and layout survive restarts; samples do not) ----

type savedMachine struct {
	Name string     `json:"name"`
	Info proto.Info `json:"info"`
}

func (h *Hub) machinesPath() string { return filepath.Join(h.cfg.DataDir, "machines.json") }

func (h *Hub) loadMachines() {
	b, err := os.ReadFile(h.machinesPath())
	if err != nil {
		return
	}
	var saved map[string]savedMachine
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for id, s := range saved {
		if proto.ValidID(id) && s.Info.Validate() == nil && len(h.machines) < h.cfg.MaxMachines {
			m := newMachine(id, s.Name)
			m.setInfo(s.Info)
			h.machines[id] = m
		}
	}
}

// saveMachines writes machines.json atomically. Caller holds h.mu.
func (h *Hub) saveMachines() {
	saved := map[string]savedMachine{}
	for id, m := range h.machines {
		saved[id] = savedMachine{Name: m.name, Info: m.info}
	}
	b, _ := json.MarshalIndent(saved, "", "  ")
	tmp := h.machinesPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("saving machines: %v", err)
		return
	}
	if err := os.Rename(tmp, h.machinesPath()); err != nil {
		log.Printf("saving machines: %v", err)
	}
}

// ---- viewers ---------------------------------------------------------------

// broadcast queues an SSE frame for every viewer; a viewer that cannot keep up is dropped
// (its browser reconnects and refetches history). Caller holds h.mu.
func (h *Hub) broadcast(frame []byte) {
	for v := range h.viewers {
		select {
		case v.ch <- frame:
		default:
			v.close()
			delete(h.viewers, v)
		}
	}
}

func sseFrame(event string, parts ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	for _, p := range parts {
		b.Write(p)
	}
	b.WriteString("\n\n")
	return b.Bytes()
}

// ---- agent ingest ------------------------------------------------------------

func (h *Hub) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

// handleAgentStream reads one agent's newline-delimited JSON. The response stays open and
// carries a heartbeat, so both ends notice a dead peer quickly.
func (h *Hub) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	if n := h.agents.Add(1); n > int32(h.cfg.MaxMachines)*2 {
		h.agents.Add(-1)
		http.Error(w, "too many agent connections", http.StatusServiceUnavailable)
		return
	}
	defer h.agents.Add(-1)

	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReaderSize(io.LimitReader(r.Body, 1<<40), 64<<10)
	line, err := readLine(br)
	if err != nil {
		http.Error(w, "no hello", http.StatusBadRequest)
		return
	}
	var first proto.Msg
	if err := json.Unmarshal(line, &first); err != nil || first.Hello == nil {
		http.Error(w, "first message must be a hello", http.StatusBadRequest)
		return
	}
	hello := first.Hello
	if err := hello.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recv := now()
	offset := recv - hello.Now // agent clock -> hub clock, for backfilled samples
	if hello.Info.BootTime != 0 {
		hello.Info.BootTime += offset
	}

	h.mu.Lock()
	m := h.machines[hello.ID]
	if m == nil {
		if len(h.machines) >= h.cfg.MaxMachines {
			h.mu.Unlock()
			http.Error(w, "too many machines; forget one first", http.StatusConflict)
			return
		}
		m = newMachine(hello.ID, hello.Name)
		h.machines[hello.ID] = m
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	// 410 Gone tells an agent it has been replaced and should stop; 409 means "not now".
	refuse := func(code int, msg string) {
		h.mu.Unlock()
		log.Printf("agent %s from %s refused: %s", hello.ID, host, msg)
		http.Error(w, msg, code)
	}
	reload := false
	switch {
	case m.superseded[hello.Instance]:
		refuse(http.StatusGone, "a newer agent has taken over "+hello.ID+"; this one is no longer used")
		return
	case m.conn != 0 && m.connInst == hello.Instance:
		// The same agent reconnecting (say, after sleep): the new connection replaces the old.
	case hello.Info.Sim && !m.info.Sim && (m.conn != 0 || m.n > 0):
		refuse(http.StatusGone, "a real agent reports as "+hello.ID+"; a simulator may not take its place")
		return
	case !hello.Info.Sim && m.info.Sim:
		// A real agent replaces its simulated stand-in; the simulated hour is dropped.
		if m.connInst != "" {
			m.supersede(m.connInst)
		}
		m.reset()
		reload = true
	case m.conn != 0 && host != m.connHost && recv-m.connSeen < 3000:
		// Another machine is actively sending under this id: refuse rather than have the
		// two take turns kicking each other off.
		refuse(http.StatusConflict, "another machine is already sending as "+hello.ID+"; give this one a different -Name/--id")
		return
	case m.conn != 0:
		// A newer agent on the same machine (a service replacing a hand-started one, or a
		// restart): it wins, and the old one is refused if it comes back.
		m.supersede(m.connInst)
	}
	if m.closeConn != nil {
		m.closeConn()
	}
	h.connSeq++
	connID := h.connSeq
	done := make(chan struct{})
	var closeOnce sync.Once
	m.conn, m.connInst, m.connHost, m.connSeen = connID, hello.Instance, host, recv
	m.closeConn = func() { closeOnce.Do(func() { close(done) }) }
	m.name = hello.Name
	m.setInfo(hello.Info)
	var have uint64
	if m.inst == hello.Instance {
		have = m.lastSeq
	}
	h.saveMachines()
	h.broadcast(sseFrame("info", m.infoJSON))
	if reload {
		h.broadcast(sseFrame("reload", []byte(fmt.Sprintf(`{"m":%q}`, m.id))))
	}
	h.mu.Unlock()

	log.Printf("agent %s (%q, %s) connected from %s; resuming after #%d", hello.ID, hello.Name, hello.Info.OS, r.RemoteAddr, have)
	defer func() {
		h.mu.Lock()
		if m.conn == connID {
			m.conn, m.closeConn = 0, nil
			h.broadcast(sseFrame("status", []byte(fmt.Sprintf(`{"m":%q,"connected":false}`, m.id))))
		}
		h.mu.Unlock()
		log.Printf("agent %s disconnected", hello.ID)
	}()

	if err := rc.EnableFullDuplex(); err != nil {
		log.Printf("agent %s: full duplex unavailable: %v", hello.ID, err)
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	var wmu sync.Mutex
	write := func(s string) error {
		wmu.Lock()
		defer wmu.Unlock()
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := write(fmt.Sprintf("{\"have\":%d}\n", have)); err != nil {
		return
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-t.C:
				if write(fmt.Sprintf("{\"ok\":%d}\n", now())) != nil {
					return
				}
			}
		}
	}()

	// Read loop. A closed "done" (this connection was replaced) ends it at once: the read
	// deadline jumps to now, so a blocked read returns, and no further sample is stored.
	go func() {
		select {
		case <-done:
			rc.SetReadDeadline(time.Now())
			r.Body.Close()
		case <-r.Context().Done():
		}
	}()
	backfilling := false
	tokens, tokensAt := 60.0, recv // flood guard for live samples: 3/s, bursts up to 60
	var prevT int64                // last stamp given to a live sample on this connection
	lateRun := 0
	for {
		select {
		case <-done:
			return
		default:
		}
		rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		line, err := readLine(br)
		if err != nil {
			if backfilling {
				h.mu.Lock()
				h.broadcast(sseFrame("reload", []byte(fmt.Sprintf(`{"m":%q}`, m.id))))
				h.mu.Unlock()
			}
			if !errors.Is(err, io.EOF) {
				log.Printf("agent %s: %v", hello.ID, err)
			}
			return
		}
		h.mu.Lock()
		if m.conn == connID {
			m.connSeen = now()
		}
		h.mu.Unlock()
		var msg proto.Msg
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Printf("agent %s: bad message: %v", hello.ID, err)
			return
		}
		t := now()
		switch {
		case msg.Info != nil:
			if msg.Info.Validate() != nil {
				continue
			}
			if msg.Info.BootTime != 0 {
				msg.Info.BootTime += offset
			}
			h.mu.Lock()
			m.setInfo(*msg.Info)
			h.saveMachines()
			h.broadcast(sseFrame("info", m.infoJSON))
			h.mu.Unlock()
		case msg.S != nil:
			s := msg.S
			if s.Validate(&hello.Info) != nil {
				continue
			}
			seq := s.Seq
			if msg.B {
				s.T += offset
				if s.T > t {
					s.T = t
				}
			} else {
				// Live samples get the hub's receive time, so clocks on different machines
				// can't skew the shared time axis. The offset follows the agent clock (it may
				// fall at once and rise 5 ms per sample), so a sample that was queued during
				// a stall keeps the moment it was taken instead of piling up at "now".
				agentT := s.T
				d := t - agentT
				if d < offset+5 {
					offset = d
				} else {
					offset += 5
				}
				s.T = t
				if est := agentT + offset; t-est > 2000 && t-est < 3600_000 && est > prevT {
					s.T = est
					if lateRun++; lateRun > 30 { // persistently "late": the agent's clock stepped back
						offset, lateRun, s.T = d, 0, t
					}
				} else {
					lateRun = 0
				}
				prevT = s.T
				tokens = min(60, tokens+float64(t-tokensAt)*0.003)
				tokensAt = t
				if tokens < 1 {
					continue
				}
				tokens--
			}
			s.Seq = 0
			b, err := json.Marshal(s)
			if err != nil {
				continue
			}
			h.mu.Lock()
			if m.conn != connID {
				h.mu.Unlock()
				return // replaced by a newer connection
			}
			if seq <= m.lastSeq && m.inst == hello.Instance {
				h.mu.Unlock()
				continue // already have it
			}
			added := m.add(s.T, b, t)
			m.inst, m.lastSeq = hello.Instance, seq
			if msg.B {
				backfilling = backfilling || added
			} else {
				if backfilling {
					backfilling = false
					h.broadcast(sseFrame("reload", []byte(fmt.Sprintf(`{"m":%q}`, m.id))))
				} else if added {
					h.broadcast(sseFrame("s", []byte(`{"m":"`+m.id+`","s":`), b, []byte("}")))
				}
			}
			h.mu.Unlock()
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > proto.MaxLineBytes {
			return nil, errors.New("line too long")
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// ---- history and live stream for browsers ----------------------------------------

// orderedIDs lists machines in display order. Caller holds h.mu.
func (h *Hub) orderedIDs() []string {
	pos := map[string]int{}
	for i, id := range h.cfg.Order {
		pos[id] = i
	}
	ids := make([]string, 0, len(h.machines))
	for id := range h.machines {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		pa, oka := pos[ids[a]]
		pb, okb := pos[ids[b]]
		switch {
		case oka && okb:
			return pa < pb
		case oka != okb:
			return oka
		}
		return h.machines[ids[a]].name < h.machines[ids[b]].name
	})
	return ids
}

type historyPart struct {
	infoJSON  []byte
	connected bool
	lastT     int64
	samples   [][]byte
}

// writeHistory streams {"now":..,"machines":[{"id","name","info","connected","samples":[...]}]}.
// With only != "", just that machine; with after > 0, only newer samples.
func (h *Hub) writeHistory(w io.Writer, only string, after int64) error {
	h.mu.Lock()
	var parts []historyPart
	for _, id := range h.orderedIDs() {
		if only != "" && id != only {
			continue
		}
		m := h.machines[id]
		parts = append(parts, historyPart{infoJSON: m.infoJSON, connected: m.conn != 0, lastT: m.lastT, samples: m.since(after)})
	}
	h.mu.Unlock()

	bw := bufio.NewWriterSize(w, 64<<10)
	fmt.Fprintf(bw, `{"now":%d,"machines":[`, now())
	for i, p := range parts {
		if i > 0 {
			bw.WriteByte(',')
		}
		// infoJSON is {"id":..,"name":..,"info":{..}}; splice in the rest.
		bw.Write(p.infoJSON[:len(p.infoJSON)-1])
		fmt.Fprintf(bw, `,"connected":%t,"lastT":%d,"samples":[`, p.connected, p.lastT)
		for j, s := range p.samples {
			if j > 0 {
				bw.WriteByte(',')
			}
			bw.Write(s)
		}
		bw.WriteString("]}")
	}
	bw.WriteString("]}")
	return bw.Flush()
}

// stream serves server-sent events to one browser until it goes away.
func (h *Hub) stream(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if len(h.viewers) >= h.cfg.MaxViewers {
		h.mu.Unlock()
		http.Error(w, "too many viewers", http.StatusServiceUnavailable)
		return
	}
	v := &viewer{ch: make(chan []byte, 256), done: make(chan struct{})}
	h.viewers[v] = struct{}{}
	status := make([]byte, 0, 256)
	status = append(status, `{"now":`...)
	status = fmt.Appendf(status, "%d", now())
	status = append(status, `,"connected":{`...)
	for i, id := range h.orderedIDs() {
		if i > 0 {
			status = append(status, ',')
		}
		status = fmt.Appendf(status, "%q:%t", id, h.machines[id].conn != 0)
	}
	status = append(status, "}}"...)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.viewers, v)
		h.mu.Unlock()
		v.close()
	}()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(b []byte) bool {
		rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send([]byte("retry: 3000\n")) || !send(sseFrame("hello", status)) {
		return
	}
	ka := time.NewTicker(15 * time.Second)
	defer ka.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-v.done:
			return
		case b := <-v.ch:
			if !send(b) {
				return
			}
		case <-ka.C:
			if !send([]byte(": ka\n\n")) {
				return
			}
		}
	}
}

// Forget removes a machine and its history.
func (h *Hub) Forget(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.machines[id]
	if !ok {
		return false
	}
	if m.closeConn != nil {
		m.closeConn()
	}
	delete(h.machines, id)
	h.saveMachines()
	h.broadcast(sseFrame("gone", []byte(fmt.Sprintf(`{"m":%q}`, id))))
	return true
}

// Stats is a tiny snapshot for logs and /api/stats.
func (h *Hub) Stats() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	ms := map[string]any{}
	for id, m := range h.machines {
		ms[id] = map[string]any{"samples": m.n, "connected": m.conn != 0, "lastT": m.lastT}
	}
	return map[string]any{"machines": ms, "viewers": len(h.viewers), "agents": h.agents.Load()}
}

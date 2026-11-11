package hub

import (
	"encoding/json"
	"sort"

	"btophub/internal/proto"
)

// machine is everything the hub knows about one agent. Samples are kept as their JSON
// encoding (hub-stamped), so history and the live stream reuse the same bytes.
type machine struct {
	id       string
	name     string
	info     proto.Info
	infoJSON []byte

	ts   []int64
	data [][]byte
	head int
	n    int

	inst    string // agent instance of the newest samples
	lastSeq uint64 // newest sequence number seen from inst
	lastT   int64  // hub time of the newest sample

	conn      uint64 // id of the live connection, 0 when none
	connInst  string // agent instance on that connection
	connHost  string // its source address
	connSeen  int64  // hub ms of the last message on that connection
	closeConn func()

	superseded map[string]bool // agent instances replaced by a newer one; refused if they return
}

// reset drops all samples (used when a real agent replaces a simulated stand-in).
func (m *machine) reset() {
	for i := range m.data {
		m.data[i] = nil
	}
	m.head, m.n, m.lastT, m.inst, m.lastSeq = 0, 0, 0, "", 0
}

func (m *machine) supersede(inst string) {
	if m.superseded == nil || len(m.superseded) > 32 {
		m.superseded = map[string]bool{}
	}
	m.superseded[inst] = true
}

const ringCap = proto.HistorySeconds + 120

func newMachine(id, name string) *machine {
	m := &machine{id: id, name: name, ts: make([]int64, ringCap), data: make([][]byte, ringCap)}
	m.setInfo(proto.Info{})
	return m
}

func (m *machine) setInfo(info proto.Info) {
	m.info = info
	b, _ := json.Marshal(struct {
		ID   string     `json:"id"`
		Name string     `json:"name"`
		Info proto.Info `json:"info"`
	}{m.id, m.name, info})
	m.infoJSON = b
}

// add appends a sample stamped t (hub ms). Samples must arrive in time order; one that is
// not newer than the last is dropped. Samples older than the hour are trimmed.
func (m *machine) add(t int64, b []byte, now int64) bool {
	if t <= m.lastT {
		return false
	}
	if m.n == ringCap {
		m.head = (m.head + 1) % ringCap
		m.n--
	}
	i := (m.head + m.n) % ringCap
	m.ts[i], m.data[i] = t, b
	m.n++
	m.lastT = t
	m.trim(now)
	return true
}

func (m *machine) trim(now int64) {
	cut := now - (proto.HistorySeconds+60)*1000
	for m.n > 0 && m.ts[m.head] < cut {
		m.data[m.head] = nil
		m.head = (m.head + 1) % ringCap
		m.n--
	}
}

// since returns samples with t > after, oldest first (the byte slices are immutable).
func (m *machine) since(after int64) [][]byte {
	k := sort.Search(m.n, func(i int) bool { return m.ts[(m.head+i)%ringCap] > after })
	out := make([][]byte, 0, m.n-k)
	for i := k; i < m.n; i++ {
		out = append(out, m.data[(m.head+i)%ringCap])
	}
	return out
}

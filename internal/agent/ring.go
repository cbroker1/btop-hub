package agent

import (
	"encoding/json"
	"sync"

	"btophub/internal/proto"
)

// entry is one encoded sample.
type entry struct {
	seq  uint64
	t    int64
	data []byte // JSON of proto.Sample
}

// Ring keeps the agent's own last hour so it can backfill the hub after a reconnect
// or a hub restart. Sequence numbers are contiguous; the oldest entries fall off.
type Ring struct {
	mu     sync.Mutex
	buf    []entry
	head   int // index of the oldest entry
	n      int
	next   uint64 // sequence number for the next Add (starts at 1)
	notify chan struct{}
}

// NewRing holds up to capacity samples.
func NewRing(capacity int) *Ring {
	return &Ring{buf: make([]entry, capacity), next: 1, notify: make(chan struct{}, 1)}
}

// Add stores s, assigning its sequence number, and wakes the uplink.
func (r *Ring) Add(s proto.Sample) {
	r.mu.Lock()
	s.Seq = r.next
	data, err := json.Marshal(&s)
	if err != nil {
		r.mu.Unlock()
		return
	}
	idx := (r.head + r.n) % len(r.buf)
	if r.n == len(r.buf) {
		r.head = (r.head + 1) % len(r.buf)
	} else {
		r.n++
	}
	r.buf[idx] = entry{seq: s.Seq, t: s.T, data: data}
	r.next++
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// Next is the sequence number the next sample will get.
func (r *Ring) Next() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// Since returns up to max entries with seq >= from, oldest first.
func (r *Ring) Since(from uint64, max int) []entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return nil
	}
	oldest := r.buf[r.head].seq
	if from < oldest {
		from = oldest
	}
	if from >= r.next {
		return nil
	}
	k := int(from - oldest)
	cnt := r.n - k
	if cnt > max {
		cnt = max
	}
	out := make([]entry, cnt)
	for i := 0; i < cnt; i++ {
		out[i] = r.buf[(r.head+k+i)%len(r.buf)]
	}
	return out
}

// Notify fires after each Add.
func (r *Ring) Notify() <-chan struct{} { return r.notify }

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"btophub/internal/proto"
)

// Uplink keeps one outbound HTTP stream to the hub. The request body carries newline-delimited
// JSON (hello, then samples); the response carries the hub's reply to the hello and a heartbeat
// every few seconds. The agent never listens on a port.
type Uplink struct {
	hub    string // e.g. http://192.168.1.74:28677
	token  string
	ring   *Ring
	client *http.Client

	mu      sync.Mutex
	hello   proto.Hello
	infoVer uint64

	connected atomic.Bool
}

// NewUplink prepares an uplink; call Run.
func NewUplink(hub, token string, hello proto.Hello, ring *Ring) *Uplink {
	d := &net.Dialer{
		Timeout:         10 * time.Second,
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3},
		Control:         tuneSocket,
	}
	tr := &http.Transport{
		Proxy:              nil, // the hub is on the LAN; never route through a proxy
		DialContext:        d.DialContext,
		DisableCompression: true,
		DisableKeepAlives:  true, // every session is one long request; never reuse a stale connection
	}
	return &Uplink{hub: strings.TrimRight(hub, "/"), token: token, ring: ring, hello: hello, client: &http.Client{Transport: tr}}
}

// SetInfo replaces the machine info; the current session sends it right away.
func (u *Uplink) SetInfo(info proto.Info) {
	u.mu.Lock()
	u.hello.Info = info
	u.infoVer++
	u.mu.Unlock()
	select {
	case u.ring.notify <- struct{}{}:
	default:
	}
}

func (u *Uplink) snapshot() (proto.Hello, uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hello, u.infoVer
}

// ErrAuth means the hub rejected the token or the machine id; retrying quickly won't help.
var ErrAuth = errors.New("hub rejected this agent")

// ErrReplaced means another agent has taken this machine's place (a newer agent on the same
// machine, or a real agent replacing a simulator). This agent should stop.
var ErrReplaced = errors.New("replaced by another agent")

// Run reconnects with backoff until ctx ends, or returns ErrReplaced when the hub says
// another agent now reports for this machine.
func (u *Uplink) Run(ctx context.Context) error {
	backoff := time.Second
	failing := false
	var lastLog time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		err := u.session(ctx)
		u.connected.Store(false)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrReplaced) {
			log.Printf("hub: %v; stopping", err)
			return err
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		if !failing || time.Since(lastLog) > 5*time.Minute {
			log.Printf("hub connection: %v (retrying)", err)
			lastLog = time.Now()
		}
		failing = true
		wait := backoff
		if errors.Is(err, ErrAuth) {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

func (u *Uplink) session(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.hub+"/api/agent/stream", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	type result struct {
		resp *http.Response
		err  error
	}
	rc := make(chan result, 1)
	go func() {
		resp, err := u.client.Do(req)
		if err != nil {
			pw.CloseWithError(err)
		}
		rc <- result{resp, err}
	}()

	w := bufio.NewWriterSize(pw, 32<<10)
	hello, infoVer := u.snapshot()
	hello.Now = time.Now().UnixMilli()
	liveFrom := u.ring.Next()
	if err := writeJSONLine(w, proto.Msg{Hello: &hello}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	var res result
	select {
	case res = <-rc:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("hub did not answer within 20 s")
	}
	if res.err != nil {
		return res.err
	}
	defer res.resp.Body.Close()
	if res.resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.resp.Body, 512))
		err := fmt.Errorf("hub said %s: %s", res.resp.Status, strings.TrimSpace(string(msg)))
		switch res.resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict:
			err = fmt.Errorf("%w: %v", ErrAuth, err)
		case http.StatusGone:
			err = fmt.Errorf("%w: %v", ErrReplaced, err)
		}
		return err
	}
	br := bufio.NewReader(res.resp.Body)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("reading hub reply: %w", err)
	}
	var reply struct {
		Have uint64 `json:"have"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return fmt.Errorf("bad hub reply: %w", err)
	}
	u.connected.Store(true)
	log.Printf("connected to %s (hub has samples up to #%d; sending from #%d)", u.hub, reply.Have, reply.Have+1)

	var heard atomic.Int64
	heard.Store(time.Now().UnixMilli())
	go func() {
		for {
			if _, err := br.ReadBytes('\n'); err != nil {
				cancel()
				pw.CloseWithError(err)
				return
			}
			heard.Store(time.Now().UnixMilli())
		}
	}()

	next := reply.Have + 1
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if _, v := u.snapshot(); v != infoVer {
			h, v := u.snapshot()
			infoVer = v
			if err := writeJSONLine(w, proto.Msg{Info: &h.Info}); err != nil {
				return err
			}
		}
		ents := u.ring.Since(next, 300)
		for _, e := range ents {
			if e.seq < liveFrom {
				w.WriteString(`{"b":true,"s":`)
			} else {
				w.WriteString(`{"s":`)
			}
			w.Write(e.data)
			w.WriteString("}\n")
			next = e.seq + 1
		}
		if w.Buffered() > 0 {
			if err := w.Flush(); err != nil {
				return fmt.Errorf("sending: %w", err)
			}
		}
		if len(ents) == 300 {
			continue // still backfilling
		}
		select {
		case <-u.ring.Notify():
		case <-tick.C:
			if time.Since(time.UnixMilli(heard.Load())) > 20*time.Second {
				return errors.New("no heartbeat from hub for 20 s")
			}
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return errors.New("hub closed the connection")
		}
	}
}

func writeJSONLine(w *bufio.Writer, m proto.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	w.Write(b)
	w.WriteByte('\n')
	return w.Flush()
}

// Check asks the hub whether this agent's token is accepted, for --probe.
func Check(hub, token string) string {
	if hub == "" {
		return "skipped: no hub configured"
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(hub, "/")+"/api/agent/check", nil)
	if err != nil {
		return "error: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode == http.StatusOK {
		return "ok: hub reachable and token accepted"
	}
	return fmt.Sprintf("rejected: %s %s", resp.Status, string(bytes.TrimSpace(b)))
}

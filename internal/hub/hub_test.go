package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"btophub/internal/agent"
	"btophub/internal/proto"
)

func newTestServer(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	h, err := New(Config{DataDir: t.TempDir(), MaxMachines: 8, MaxViewers: 8})
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	srv, err := NewServer(h, nil, Assets{Web: web, Scripts: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return h, ts
}

type history struct {
	Now      int64 `json:"now"`
	Machines []struct {
		ID        string            `json:"id"`
		Connected bool              `json:"connected"`
		Samples   []json.RawMessage `json:"samples"`
	} `json:"machines"`
}

func getHistory(t *testing.T, url string) history {
	t.Helper()
	resp, err := http.Get(url + "/api/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var hs history
	if err := json.NewDecoder(resp.Body).Decode(&hs); err != nil {
		t.Fatal(err)
	}
	return hs
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countSamples(t *testing.T, url string) int {
	hs := getHistory(t, url)
	if len(hs.Machines) == 0 {
		return 0
	}
	return len(hs.Machines[0].Samples)
}

// The real uplink against the real hub: backfill, live samples, and resume after a
// reconnect without duplicates.
func TestUplinkBackfillLiveResume(t *testing.T) {
	h, ts := newTestServer(t)
	ring := agent.NewRing(proto.HistorySeconds + 60)
	now := time.Now()
	for i := 100; i > 0; i-- { // 100 s of history taken before the connection
		ring.Add(proto.Sample{T: now.Add(-time.Duration(i) * time.Second).UnixMilli(), CPU: []uint16{10, 20}})
	}
	hello := proto.Hello{ID: "test-box", Name: "Test box", Instance: "inst-1", Agent: proto.Version,
		Info: proto.Info{OS: "linux", Threads: 2}}
	up := agent.NewUplink(ts.URL, h.token, hello, ring)
	ctx, cancel := context.WithCancel(context.Background())
	go up.Run(ctx)
	waitFor(t, "backfill", func() bool { return countSamples(t, ts.URL) == 100 })

	for i := 0; i < 3; i++ {
		ring.Add(proto.Sample{T: time.Now().UnixMilli(), CPU: []uint16{500, 600}})
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "live samples", func() bool { return countSamples(t, ts.URL) == 103 })

	// Disconnect, take samples while away, reconnect: only the new ones are sent.
	cancel()
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 5; i++ {
		ring.Add(proto.Sample{T: time.Now().UnixMilli(), CPU: []uint16{1, 2}})
		time.Sleep(10 * time.Millisecond)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go agent.NewUplink(ts.URL, h.token, hello, ring).Run(ctx2)
	waitFor(t, "resume", func() bool { return countSamples(t, ts.URL) == 108 })
	time.Sleep(200 * time.Millisecond)
	if n := countSamples(t, ts.URL); n != 108 {
		t.Fatalf("after resume: %d samples, want 108 (duplicates?)", n)
	}
	hs := getHistory(t, ts.URL)
	var prev int64
	for _, raw := range hs.Machines[0].Samples {
		var s proto.Sample
		json.Unmarshal(raw, &s)
		if s.T <= prev {
			t.Fatalf("samples out of order: %d after %d", s.T, prev)
		}
		if s.Seq != 0 {
			t.Fatalf("hub should strip agent sequence numbers")
		}
		prev = s.T
	}
}

func TestRejectsBadTokenAndForeignSources(t *testing.T) {
	_, ts := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/agent/stream", strings.NewReader("{}\n"))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d", resp.StatusCode)
	}

	s := &Server{allow: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}}
	for addr, want := range map[string]bool{
		"192.168.1.47:5000": true, "127.0.0.1:1": true, "[::1]:1": true,
		"192.168.2.1:5000": false, "172.18.0.1:80": false, "100.64.0.1:1": false,
		"[::ffff:192.168.1.9]:1": true, "[fd7a::1]:1": false,
	} {
		if got := s.allowed(addr); got != want {
			t.Errorf("allowed(%s) = %v, want %v", addr, got, want)
		}
	}
}

// openStream sends a hello and keeps the stream open; it returns the HTTP status and a
// function that writes one more line (a sample) and one that ends the stream.
func openStream(t *testing.T, url, token, id, inst string, sim bool) (int, func(string), func()) {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, url+"/api/agent/stream", pr)
	req.Header.Set("Authorization", "Bearer "+token)
	hello, _ := json.Marshal(proto.Msg{Hello: &proto.Hello{ID: id, Name: id, Instance: inst, Now: time.Now().UnixMilli(),
		Info: proto.Info{OS: "linux", Threads: 1, Sim: sim}}})
	type res struct {
		resp *http.Response
		err  error
	}
	ch := make(chan res, 1)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	go func() { r, err := client.Do(req); ch <- res{r, err} }()
	go pw.Write(append(hello, '\n'))
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	send := func(line string) { pw.Write([]byte(line + "\n")) }
	end := func() { pw.Close(); r.resp.Body.Close() }
	if r.resp.StatusCode != http.StatusOK {
		end()
	}
	return r.resp.StatusCode, send, end
}

func TestRealAgentReplacesSimulatorAndNewestWins(t *testing.T) {
	h, ts := newTestServer(t)
	code, send, endSim := openStream(t, ts.URL, h.token, "z13", "sim-1", true)
	if code != http.StatusOK {
		t.Fatalf("simulator: %d", code)
	}
	send(`{"b":true,"s":{"q":1,"t":` + fmt.Sprint(time.Now().Add(-time.Minute).UnixMilli()) + `,"c":[5]}}`)
	waitFor(t, "simulated sample", func() bool { return countSamples(t, ts.URL) == 1 })

	// The real Z13 agent arrives: it takes over and the simulated history is dropped.
	code, sendReal, endReal := openStream(t, ts.URL, h.token, "z13", "real-1", false)
	if code != http.StatusOK {
		t.Fatalf("real agent refused: %d", code)
	}
	if n := countSamples(t, ts.URL); n != 0 {
		t.Fatalf("simulated samples kept after a real agent took over: %d", n)
	}
	sendReal(`{"s":{"q":1,"t":` + fmt.Sprint(time.Now().UnixMilli()) + `,"c":[7]}}`)
	waitFor(t, "real sample", func() bool { return countSamples(t, ts.URL) == 1 })
	endSim()

	// The simulator, coming back, is refused.
	if code, _, _ := openStream(t, ts.URL, h.token, "z13", "sim-1", true); code != http.StatusGone {
		t.Fatalf("superseded simulator: got %d, want 410", code)
	}
	if code, _, _ := openStream(t, ts.URL, h.token, "z13", "sim-2", true); code != http.StatusGone {
		t.Fatalf("new simulator for a real machine: got %d, want 410", code)
	}

	// A newer real agent from the same machine wins; the old instance is then refused.
	code, _, endNew := openStream(t, ts.URL, h.token, "z13", "real-2", false)
	if code != http.StatusOK {
		t.Fatalf("newer agent refused: %d", code)
	}
	defer endNew()
	before := countSamples(t, ts.URL)
	sendReal(`{"s":{"q":2,"t":` + fmt.Sprint(time.Now().UnixMilli()) + `,"c":[9]}}`) // on the replaced stream
	time.Sleep(300 * time.Millisecond)
	if n := countSamples(t, ts.URL); n != before {
		t.Fatalf("a replaced connection still stored a sample (%d -> %d)", before, n)
	}
	endReal()
	if code, _, _ := openStream(t, ts.URL, h.token, "z13", "real-1", false); code != http.StatusGone {
		t.Fatalf("superseded agent: got %d, want 410", code)
	}
}

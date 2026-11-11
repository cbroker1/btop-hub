//go:build windows

package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestSelfStats(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ {
		_ = x * x
	}
	cpu, rss, err := SelfStats()
	if err != nil {
		t.Fatal(err)
	}
	if cpu <= 0 || cpu > time.Hour {
		t.Errorf("cpu = %v", cpu)
	}
	if rss < 1<<20 || rss > 1<<34 {
		t.Errorf("working set = %d bytes", rss)
	}
}

func TestSetLowPriority(t *testing.T) {
	h := windows.CurrentProcess()
	defer windows.SetPriorityClass(h, windows.NORMAL_PRIORITY_CLASS)
	if err := SetLowPriority(); err != nil {
		t.Fatal(err)
	}
	if c, err := windows.GetPriorityClass(h); err != nil || c != windows.BELOW_NORMAL_PRIORITY_CLASS {
		t.Errorf("priority class = %#x, %v", c, err)
	}
}

// Under go test the parent is go.exe, not services.exe.
func TestNotAService(t *testing.T) {
	called := false
	isSvc, err := RunService("btop-agent-test", func(context.Context) error { called = true; return nil })
	if isSvc || err != nil || called {
		t.Errorf("RunService = %v, %v (run called: %v)", isSvc, err, called)
	}
	if p := LogPath(); p != "" {
		t.Errorf("LogPath = %q", p)
	}
}

// execute drives handler.Execute the way svc's dispatcher does, over unbuffered channels.
func execute(run func(context.Context) error) (h *handler, req chan svc.ChangeRequest, status chan svc.Status, exit chan [2]uint32) {
	h = &handler{run: run, result: make(chan error, 1)}
	req = make(chan svc.ChangeRequest)
	status = make(chan svc.Status)
	exit = make(chan [2]uint32, 1)
	go func() {
		specific, code := h.Execute(nil, req, status)
		var s uint32
		if specific {
			s = 1
		}
		exit <- [2]uint32{s, code}
	}()
	return
}

func expect(t *testing.T, status chan svc.Status, want svc.State) svc.Status {
	t.Helper()
	select {
	case s := <-status:
		if s.State != want {
			t.Fatalf("state %d, want %d", s.State, want)
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no status, want %d", want)
	}
	return svc.Status{}
}

func TestHandlerStop(t *testing.T) {
	_, req, status, exit := execute(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	expect(t, status, svc.StartPending)
	if s := expect(t, status, svc.Running); s.Accepts != svc.AcceptStop|svc.AcceptShutdown {
		t.Errorf("accepts %#x", s.Accepts)
	}
	req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	expect(t, status, svc.Running)
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	expect(t, status, svc.StopPending)
	if e := <-exit; e != [2]uint32{0, 0} {
		t.Errorf("exit %v, want clean", e)
	}
}

func TestHandlerRunFails(t *testing.T) {
	boom := errors.New("boom")
	h, _, status, exit := execute(func(context.Context) error { return boom })
	expect(t, status, svc.StartPending)
	expect(t, status, svc.Running)
	if e := <-exit; e != [2]uint32{1, exitRunFailed} {
		t.Errorf("exit %v, want service-specific %d", e, exitRunFailed)
	}
	if err := <-h.result; err != boom {
		t.Errorf("result %v", err)
	}
}

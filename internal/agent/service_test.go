package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// guardSnapshot runs Snapshot in a goroutine and bounds how long we wait for
// it. A leaked mutex (e.g. a no-op Start/Stop path that returns while still
// holding s.mu) would otherwise block go test for ten minutes; with the
// guard the regression fails in five seconds.
func guardSnapshot(t *testing.T, s *Service) ServiceView {
	t.Helper()
	done := make(chan ServiceView, 1)
	go func() { done <- s.Snapshot() }()
	select {
	case v := <-done:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot blocked for 5s — a mutex was leaked")
	}
	panic("unreachable: t.Fatal does not return")
}

// setStateForTest forces a state without running a probe or command.
func setStateForTest(t *testing.T, s *Service, st State) {
	t.Helper()
	s.mu.Lock()
	s.setStateLocked(st, "")
	s.mu.Unlock()
}

// toggleServer serves 200 while up and 503 while down (>=400 ⇒ "not up" to
// the health probe), so a test can flip a service's liveness on the fly.
type toggleServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	up  bool
}

func newToggleServer(t *testing.T) *toggleServer {
	t.Helper()
	ts := &toggleServer{up: true}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ts.mu.Lock()
		ok := ts.up
		ts.mu.Unlock()
		if ok {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *toggleServer) set(up bool) {
	ts.mu.Lock()
	ts.up = up
	ts.mu.Unlock()
}

func (ts *toggleServer) URL() string { return ts.srv.URL }

// TestNewServiceInitialStates covers the three ways NewService classifies a
// service before any action: probe up ⇒ running, probe configured but down ⇒
// stopped, no probe at all ⇒ unknown.
func TestNewServiceInitialStates(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	for _, tc := range []struct {
		name string
		def  ServiceDef
		want State
	}{
		{"health up", ServiceDef{ID: "demo", Health: live.URL}, StateRunning},
		{"health down", ServiceDef{ID: "demo", Health: dead.URL}, StateStopped},
		{"status command down", ServiceDef{ID: "demo", Status: []string{"sh", "-c", "exit 1"}}, StateStopped},
		{"no probe", ServiceDef{ID: "demo"}, StateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NewService(tc.def, time.Second).Snapshot().State
			if got != tc.want {
				t.Fatalf("initial state = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNoOpStartDoesNotWedge is the regression guard for the deadlock the
// agent hit in production: Start returned nil from its "already up" fast
// path while still holding s.mu, so the next Stop/Snapshot/Watch blocked
// forever. Both no-op states must return nil without leaking the lock, and
// without mutating the state.
func TestNoOpStartDoesNotWedge(t *testing.T) {
	srv := newToggleServer(t)
	for _, st := range []State{StateRunning, StateStarting} {
		t.Run(string(st), func(t *testing.T) {
			s := NewService(ServiceDef{ID: "demo", Start: []string{"sh", "-c", "true"}, Health: srv.URL()}, time.Second)
			setStateForTest(t, s, st)
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("no-op Start in %s: %v, want nil", st, err)
			}
			v := guardSnapshot(t, s)
			if v.State != st {
				t.Fatalf("no-op Start changed state %s to %s", st, v.State)
			}
		})
	}
}

// TestNoOpStopDoesNotWedge is the same guard for Stop's "already down" fast
// path (stopping a service that went down outside the dashboard).
func TestNoOpStopDoesNotWedge(t *testing.T) {
	for _, st := range []State{StateStopped, StateStopping} {
		t.Run(string(st), func(t *testing.T) {
			s := NewService(ServiceDef{ID: "demo", Stop: []string{"sh", "-c", "true"}, Status: []string{"sh", "-c", "exit 1"}}, time.Second)
			setStateForTest(t, s, st)
			if err := s.Stop(context.Background()); err != nil {
				t.Fatalf("no-op Stop in %s: %v, want nil", st, err)
			}
			v := guardSnapshot(t, s)
			if v.State != st {
				t.Fatalf("no-op Stop changed state %s to %s", st, v.State)
			}
		})
	}
}

// TestStartFailure: a start command that fails (and the service is not up on
// the fallback probe) must surface an error and land in failed.
func TestStartFailure(t *testing.T) {
	s := NewService(ServiceDef{ID: "demo", Start: []string{"sh", "-c", "echo boom; exit 1"}}, time.Second)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start with a failing command: want error, got nil")
	}
	v := guardSnapshot(t, s)
	if v.State != StateFailed {
		t.Fatalf("state = %q, want failed", v.State)
	}
	if !strings.Contains(v.Hint, "start command failed") {
		t.Fatalf("hint = %q, want a start-failure hint", v.Hint)
	}
}

// TestStopConvergesWhenDown: the stop command fails, but the probe shows the
// service is already down — Stop must converge to stopped instead of failing
// (this is how a service stopped outside the dashboard is absorbed).
func TestStopConvergesWhenDown(t *testing.T) {
	s := NewService(ServiceDef{ID: "demo", Stop: []string{"sh", "-c", "exit 1"}}, time.Second)
	setStateForTest(t, s, StateRunning)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop while already down: %v, want nil", err)
	}
	if got := guardSnapshot(t, s).State; got != StateStopped {
		t.Fatalf("state = %q, want stopped", got)
	}
}

// TestStopFailureWhenStillUp: the stop command fails and the service is
// still up — that is a real failure.
func TestStopFailureWhenStillUp(t *testing.T) {
	srv := newToggleServer(t)
	s := NewService(ServiceDef{ID: "demo", Stop: []string{"sh", "-c", "exit 1"}, Health: srv.URL()}, time.Second)
	setStateForTest(t, s, StateRunning)
	if err := s.Stop(context.Background()); err == nil {
		t.Fatal("Stop with failing command while up: want error, got nil")
	}
	v := guardSnapshot(t, s)
	if v.State != StateFailed || !strings.Contains(v.Hint, "stop command failed") {
		t.Fatalf("state/hint = %q/%q, want failed with a stop-failure hint", v.State, v.Hint)
	}
}

// TestWatchConvergence covers the two-way promotion/demotion: a stopped
// service that is up again is promoted to running; a running service is
// demoted to failed only after two consecutive missed probes (a single miss
// is tolerated as transient); recovery resets the streak.
func TestWatchConvergence(t *testing.T) {
	srv := newToggleServer(t)
	s := NewService(ServiceDef{ID: "demo", Health: srv.URL()}, time.Second)
	if got := guardSnapshot(t, s).State; got != StateRunning {
		t.Fatalf("initial state = %q, want running", got)
	}

	// Out-of-band restart from a stopped state must be promoted.
	srv.set(false)
	setStateForTest(t, s, StateStopped)
	srv.set(true)
	s.Watch()
	if got := guardSnapshot(t, s).State; got != StateRunning {
		t.Fatalf("Watch after coming up: state = %q, want running", got)
	}

	// One missed probe while running must not flip the state.
	srv.set(false)
	s.Watch()
	if got := guardSnapshot(t, s).State; got != StateRunning {
		t.Fatalf("after one miss: state = %q, want running (transient miss)", got)
	}

	// Second consecutive miss demotes to failed.
	s.Watch()
	v := guardSnapshot(t, s)
	if v.State != StateFailed {
		t.Fatalf("after two misses: state = %q, want failed", v.State)
	}
	if v.Hint == "" {
		t.Fatal("failed state lost its hint")
	}

	// Recovery resets the streak and promotes back to running.
	srv.set(true)
	s.Watch()
	if got := guardSnapshot(t, s).State; got != StateRunning {
		t.Fatalf("after recovery: state = %q, want running", got)
	}
}

// TestWatchNoProbeIsNoop: without any probe configured, Watch cannot
// observe reality and must leave the state alone.
func TestWatchNoProbeIsNoop(t *testing.T) {
	s := NewService(ServiceDef{ID: "demo"}, time.Second)
	s.Watch()
	if got := guardSnapshot(t, s).State; got != StateUnknown {
		t.Fatalf("state = %q, want unknown", got)
	}
}

// TestSnapshotCanStartStop pins the button-availability logic per state.
// Note: stopping still offers start (CanStart only excludes running and
// starting); this test pins that behaviour on purpose.
func TestSnapshotCanStartStop(t *testing.T) {
	both := ServiceDef{ID: "demo", Start: []string{"sh"}, Stop: []string{"sh"}, Status: []string{"sh"}}
	for _, tc := range []struct {
		state    State
		def      ServiceDef
		canStart bool
		canStop  bool
	}{
		{StateStopped, both, true, false},
		{StateUnknown, both, true, false},
		{StateFailed, both, true, true},
		{StateRunning, both, false, true},
		{StateStarting, both, false, true},
		{StateStopping, both, true, false},
		{StateStopped, ServiceDef{ID: "demo", Stop: []string{"sh"}}, false, false},
		{StateRunning, ServiceDef{ID: "demo", Start: []string{"sh"}}, false, false},
	} {
		s := &Service{def: tc.def, state: tc.state}
		v := s.Snapshot()
		if v.CanStart != tc.canStart || v.CanStop != tc.canStop {
			t.Fatalf("state %s def %+v: Snapshot = {CanStart:%v CanStop:%v}, want {%v %v}",
				tc.state, tc.def, v.CanStart, v.CanStop, tc.canStart, tc.canStop)
		}
	}
}

// TestProbeStatusJSON pins the JSON-probe rules: the service is up only when
// the status says "running" AND, if a pid is reported, that process is
// actually alive (Linux /proc check; pid 1 always exists, 999999999 never
// does). Log lines interleaved with the JSON must be tolerated.
func TestProbeStatusJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want bool
	}{
		{"running with live pid", []string{"sh", "-c", `echo log-line; echo '{"status":"running","pid":1}'`}, true},
		{"running with dead pid", []string{"sh", "-c", `echo '{"status":"running","pid":999999999}'`}, false},
		{"running without pid", []string{"sh", "-c", `echo '{"status":"running"}'`}, false},
		{"stopped", []string{"sh", "-c", `echo '{"status":"stopped","pid":1}'`}, false},
		{"no json at all", []string{"sh", "-c", "echo nope"}, false},
		{"command fails", []string{"sh", "-c", "exit 1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(ServiceDef{ID: "lms", StatusJSON: tc.argv}, time.Second)
			if got := s.probeUp(); got != tc.want {
				t.Fatalf("probeUp = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTail pins the output-summary helper: newlines flatten, ends trim,
// >200 chars truncate with an ellipsis, empty becomes a placeholder.
func TestTail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", "(no output)"},
		{"line1\nline2", "line1 | line2"},
		{"  spaced  ", "spaced"},
		{strings.Repeat("a", 250), strings.Repeat("a", 200) + "…"},
	} {
		if got := tail(tc.in); got != tc.want {
			t.Errorf("tail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

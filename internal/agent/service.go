package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// State is the lifecycle state of a managed service.
type State string

const (
	StateUnknown  State = "unknown"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

// ServiceDef configures a managed service via explicit commands.
type ServiceDef struct {
	ID     string   `yaml:"id"`
	Name   string   `yaml:"name"`
	Start  []string `yaml:"start"`
	Stop   []string `yaml:"stop"`
	Status []string `yaml:"status,omitempty"` // exit code 0 ⇒ up (used when no health URL)
	Health string   `yaml:"health,omitempty"` // URL probe; status < 400 ⇒ up (authoritative if set)
}

// ServiceView is the JSON representation sent to the dashboard.
type ServiceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    State  `json:"state"`
	Hint     string `json:"hint,omitempty"`
	CanStart bool   `json:"can_start"`
	CanStop  bool   `json:"can_stop"`
}

// Service runs and watches one configured service.
type Service struct {
	def          ServiceDef
	startTimeout time.Duration

	mu         sync.Mutex
	state      State
	hint       string
	changedAt  time.Time
	downStreak int // consecutive failed probes while running
}

// NewService creates a Service, initializing its state from an immediate probe.
func NewService(def ServiceDef, startTimeout time.Duration) *Service {
	if startTimeout <= 0 {
		startTimeout = 30 * time.Second
	}
	s := &Service{def: def, startTimeout: startTimeout, changedAt: time.Now()}
	switch {
	case s.probeUp():
		s.state = StateRunning
	case s.hasProbe():
		s.state = StateStopped
	default:
		s.state = StateUnknown
	}
	return s
}

// ID returns the service's configured id.
func (s *Service) ID() string { return s.def.ID }

// Snapshot returns a consistent view for the API.
func (s *Service) Snapshot() ServiceView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ServiceView{
		ID:       s.def.ID,
		Name:     s.def.Name,
		State:    s.state,
		Hint:     s.hint,
		CanStart: len(s.def.Start) > 0 && s.state != StateRunning && s.state != StateStarting,
		CanStop:  len(s.def.Stop) > 0 && (s.state == StateRunning || s.state == StateStarting || s.state == StateFailed),
	}
}

func (s *Service) hasProbe() bool { return len(s.def.Status) > 0 || s.def.Health != "" }

// probeUp reports whether the service currently looks healthy. A configured
// health URL is authoritative when present (an HTTP answer or a connection
// refusal is definitive); otherwise the status command's exit code decides,
// since some CLIs exit 0 even when their target is down.
func (s *Service) probeUp() bool {
	if s.def.Health != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", s.def.Health, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return resp.StatusCode < 400
		}
		return false
	}
	if len(s.def.Status) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, s.def.Status[0], s.def.Status[1:]...).Run() == nil
	}
	return false
}

// Start launches the service. It is idempotent: a service that is already up
// (possibly started outside the dashboard) is simply reported as running.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.state == StateRunning || s.state == StateStarting {
		return nil // nothing to do
	}
	s.setStateLocked(StateStarting, "")
	s.mu.Unlock()

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	out, err := runCmd(cmdCtx, s.def.Start)
	cancel()
	if err != nil {
		if s.probeUp() {
			s.setState(StateRunning, "") // already up; the command was a no-op
			return nil
		}
		s.setState(StateFailed, "start command failed: "+tail(out))
		return fmt.Errorf("start failed: %s", tail(out))
	}
	go s.waitUp(s.startTimeout)
	return nil
}

func (s *Service) waitUp(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if s.probeUp() {
			s.setState(StateRunning, "")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == StateStarting {
		s.state = StateFailed
		s.hint = "did not come up within " + timeout.String()
		s.changedAt = time.Now()
	}
}

// Stop halts the service. It is idempotent: stopping a service that is
// already down (e.g. stopped outside the dashboard) succeeds and converges
// the state instead of surfacing an error.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state == StateStopped || s.state == StateStopping {
		return nil // nothing to do
	}
	s.setStateLocked(StateStopping, "")
	s.mu.Unlock()

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	out, err := runCmd(cmdCtx, s.def.Stop)
	cancel()
	if err != nil {
		if !s.probeUp() {
			s.setState(StateStopped, "") // already down; converge silently
			return nil
		}
		s.setState(StateFailed, "stop command failed: "+tail(out))
		return fmt.Errorf("stop failed: %s", tail(out))
	}
	s.setState(StateStopped, "")
	return nil
}

// Watch converges the tracked state with reality: a stopped or failed service
// that is now up (e.g. started outside the dashboard) is promoted to running,
// and a running service that no longer responds is marked failed after two
// consecutive missed probes (one miss can be a transient busy/timeout).
func (s *Service) Watch() {
	if !s.hasProbe() {
		return
	}
	up := s.probeUp()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case up:
		s.downStreak = 0
		if s.state == StateStopped || s.state == StateFailed || s.state == StateUnknown {
			s.setStateLocked(StateRunning, "")
		}
	case s.state == StateRunning:
		s.downStreak++
		if s.downStreak >= 2 {
			s.setStateLocked(StateFailed, "was running but no longer responds")
		}
	}
}

func (s *Service) setState(st State, hint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStateLocked(st, hint)
}

func (s *Service) setStateLocked(st State, hint string) {
	if st != s.state || hint != s.hint {
		s.state = st
		s.hint = hint
		s.changedAt = time.Now()
	}
}

func runCmd(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " | "))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

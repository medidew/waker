// Package tailscale tracks node presence by polling `tailscale status --json`.
package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node is the subset of a tailscale peer/self entry we care about.
type Node struct {
	HostName     string     `json:"HostName"`
	DNSName      string     `json:"DNSName"`
	TailscaleIPs []string   `json:"TailscaleIPs"`
	Online       bool       `json:"Online"`
	LastSeen     *time.Time `json:"LastSeen"`
}

// Status is the subset of `tailscale status --json` output we parse.
// Current tailscale versions expose peers as a map keyed by node key under
// "Peer"; Self is an object, not a string.
type Status struct {
	Peer map[string]Node `json:"Peer"`
	Self Node            `json:"Self"`
}

// RunStatus executes `tailscale status --json` and parses it.
func RunStatus(ctx context.Context) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	return &st, nil
}

// ShortName strips the tailnet domain and any trailing dot from a node name.
func ShortName(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// matches reports whether the node is the one identified by hostname
// (FQDN or short name).
func (n *Node) matches(hostname string) bool {
	if n.HostName != "" && n.HostName == hostname {
		return true
	}
	return ShortName(n.DNSName) == ShortName(hostname)
}

// Tracker polls tailscale and tracks the presence of a single peer.
type Tracker struct {
	hostname string // FQDN or short name of the node to watch
	interval time.Duration

	mu         sync.RWMutex
	online     bool
	lastSeen   *time.Time
	ip         string
	wasFound   bool
	missLogged bool
}

// NewTracker creates a tracker for hostname with the given poll interval.
func NewTracker(hostname string, interval time.Duration) *Tracker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Tracker{hostname: hostname, interval: interval}
}

// Start begins polling in the background; it stops when ctx is cancelled.
func (t *Tracker) Start(ctx context.Context) {
	t.poll() // prime immediately so state is available right away
	go func() {
		ticker := time.NewTicker(t.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.poll()
			}
		}
	}()
}

func (t *Tracker) poll() {
	st, err := RunStatus(context.Background())
	if err != nil {
		log.Printf("waker: tailscale status failed: %v", err)
		return // keep last known state
	}
	var found *Node
	for _, p := range st.Peer {
		p := p
		if p.matches(t.hostname) {
			found = &p
			break
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if found == nil {
		if !t.missLogged {
			names := make([]string, 0, len(st.Peer))
			for _, p := range st.Peer {
				names = append(names, ShortName(p.DNSName))
			}
			log.Printf("waker: desktop %q not found among tailnet peers (%s)",
				t.hostname, strings.Join(names, ", "))
			t.missLogged = true
		}
		t.wasFound = false
		t.online = false
		return
	}
	t.missLogged = false
	t.wasFound = true
	if found.Online != t.online {
		log.Printf("waker: desktop %s is now %s", t.hostname, map[bool]string{true: "online", false: "offline"}[found.Online])
	}
	t.online = found.Online
	t.lastSeen = found.LastSeen
	if ip := firstIPv4(found.TailscaleIPs); ip != "" {
		t.ip = ip // tailnet IPs are stable per node; never clear once known
	}
}

// Online reports whether the watched node is currently online.
func (t *Tracker) Online() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.online
}

// LastSeen returns when the watched node was last seen, if ever.
func (t *Tracker) LastSeen() *time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastSeen
}

// IP returns the watched node's primary IPv4 tailnet address, if known.
func (t *Tracker) IP() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ip
}

// SelfIPv4 returns this machine's primary IPv4 tailnet address.
func SelfIPv4(ctx context.Context) (string, error) {
	st, err := RunStatus(ctx)
	if err != nil {
		return "", err
	}
	if ip := firstIPv4(st.Self.TailscaleIPs); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no IPv4 tailnet address found")
}

// ListenAddr resolves a bind address: an explicit configured address is used
// as-is; otherwise the primary IPv4 tailnet IP is preferred so the service is
// reachable over the tailnet without exposing LAN interfaces. Falls back to
// 0.0.0.0 (still protected by the shared token) if tailscale is unavailable.
func ListenAddr(ctx context.Context, configured string, port int) (string, error) {
	if configured != "" && !strings.EqualFold(configured, "auto") {
		return configured, nil
	}
	ip, err := SelfIPv4(ctx)
	if err != nil {
		log.Printf("waker: no tailnet IPv4 (%v); binding 0.0.0.0", err)
		return fmt.Sprintf("0.0.0.0:%d", port), nil
	}
	return net.JoinHostPort(ip, strconv.Itoa(port)), nil
}

func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a
		}
	}
	return ""
}

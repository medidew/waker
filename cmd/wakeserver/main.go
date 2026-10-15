// wakeserver runs on the always-on box: it sends Wake-on-LAN magic packets,
// tracks desktop presence over Tailscale, proxies API calls to the desktop
// agent, and serves the dashboard UI.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"waker/internal/httpx"
	"waker/internal/tailscale"
	"waker/internal/wol"

	"gopkg.in/yaml.v3"
)

const (
	defaultPort = 8090
	wakeTimeout = 2 * time.Minute // give the desktop this long to come online
	staleWindow = 60 * time.Second
)

//go:embed all:web
var webFS embed.FS

type link struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url"  json:"url"`
}

type desktopCfg struct {
	MAC      string `yaml:"mac"`
	Hostname string `yaml:"hostname"` // tailnet node name of the desktop
	SSHUser  string `yaml:"ssh_user"`
}

type wakeCfg struct {
	Target     string `yaml:"target"`      // e.g. "192.168.1.255:9"
	Count      int    `yaml:"count"`       // packets to send
	IntervalMs int    `yaml:"interval_ms"` // between packets
}

type config struct {
	Listen           string        `yaml:"listen"`     // "" or "auto" → primary tailnet IP
	Token            string        `yaml:"token"`      // shared bearer token
	AgentPort        int           `yaml:"agent_port"` // wakeragent port on the desktop
	PresenceInterval time.Duration `yaml:"presence_interval"`
	Desktop          desktopCfg    `yaml:"desktop"`
	Wake             wakeCfg       `yaml:"wake"`
	Links            []link        `yaml:"links"` // dashboard quick links
}

type server struct {
	cfg     config
	tracker *tailscale.Tracker
	started time.Time

	mu     sync.Mutex
	waking bool
	wakeAt time.Time
}

func main() {
	configPath := flag.String("config", defaultConfig(), "path to server.yaml")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("wakeserver: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracker := tailscale.NewTracker(cfg.Desktop.Hostname, cfg.PresenceInterval)
	tracker.Start(ctx)

	s := &server{cfg: *cfg, tracker: tracker, started: time.Now()}

	webSub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("wakeserver: %v", err)
	}

	api := http.NewServeMux()
	api.HandleFunc("POST /api/wake", s.handleWake)
	api.HandleFunc("GET /api/status", s.handleStatus)
	api.Handle("/api/desktop/", s.proxy())

	root := http.NewServeMux()
	root.Handle("/api/", httpx.Auth(api, cfg.Token))
	fileServer := http.FileServer(http.FS(webSub))
	root.Handle("/", spaHandler(fileServer))

	addr, err := tailscale.ListenAddr(ctx, cfg.Listen, defaultPort)
	if err != nil {
		log.Fatalf("wakeserver: %v", err)
	}
	log.Printf("wakeserver listening on %s (desktop: %s)", addr, cfg.Desktop.Hostname)

	srv := &http.Server{Addr: addr, Handler: root}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		log.Printf("wakeserver stopped")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("wakeserver: %v", err)
		}
	}
}

func (s *server) handleWake(w http.ResponseWriter, r *http.Request) {
	if s.tracker.Online() {
		httpx.WriteErr(w, 409, "desktop is already online")
		return
	}
	if _, err := wol.Packet(s.cfg.Desktop.MAC); err != nil {
		httpx.WriteErr(w, 400, err.Error())
		return
	}

	s.mu.Lock()
	now := time.Now()
	if s.waking && now.Sub(s.wakeAt) < wakeTimeout+staleWindow {
		started := s.wakeAt
		s.mu.Unlock()
		httpx.WriteJSON(w, map[string]any{"ok": true, "waking": true, "started_at": started})
		return
	}
	s.waking = true
	s.wakeAt = now
	s.mu.Unlock()

	go s.firePackets()
	httpx.WriteJSON(w, map[string]any{"ok": true, "waking": true, "started_at": now})
}

func (s *server) firePackets() {
	interval := time.Duration(s.cfg.Wake.IntervalMs) * time.Millisecond
	for i := 0; i < s.cfg.Wake.Count; i++ {
		if err := wol.Send(s.cfg.Desktop.MAC, s.cfg.Wake.Target); err != nil {
			log.Printf("wakeserver: wake packet %d/%d failed: %v", i+1, s.cfg.Wake.Count, err)
		} else if i == 0 {
			log.Printf("wakeserver: sending %d magic packets to %s for %s",
				s.cfg.Wake.Count, s.cfg.Wake.Target, s.cfg.Desktop.MAC)
		}
		if i+1 < s.cfg.Wake.Count {
			time.Sleep(interval)
		}
	}
}

func (s *server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	waking := false
	var startedAt *time.Time
	if s.waking && !s.tracker.Online() {
		elapsed := time.Since(s.wakeAt)
		if elapsed > wakeTimeout+staleWindow {
			s.waking = false // attempt expired; allow a fresh one
		} else if elapsed < wakeTimeout+staleWindow {
			waking = true
			t := s.wakeAt
			startedAt = &t
		}
	}
	s.mu.Unlock()

	httpx.WriteJSON(w, map[string]any{
		"desktop": map[string]any{
			"online":    s.tracker.Online(),
			"hostname":  s.cfg.Desktop.Hostname,
			"ip":        s.tracker.IP(),
			"ssh_user":  s.cfg.Desktop.SSHUser,
			"last_seen": s.tracker.LastSeen(),
		},
		"waking": map[string]any{"active": waking, "started_at": startedAt},
		"server": map[string]any{
			"uptime_sec": time.Since(s.started).Seconds(),
		},
		"links": s.cfg.Links,
	})
}

// proxy forwards /api/desktop/* to the desktop agent's tailnet address.
func (s *server) proxy() http.Handler {
	p := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = fmt.Sprintf("%s:%d", s.tracker.IP(), s.cfg.AgentPort)
			path := strings.TrimPrefix(req.URL.Path, "/api/desktop")
			if path == "" {
				path = "/"
			}
			req.URL.Path = path
			req.Host = req.URL.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("wakeserver: proxy to desktop failed: %v", err)
			httpx.WriteErr(w, 502, "desktop agent unreachable")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tracker.Online() || s.tracker.IP() == "" {
			httpx.WriteErr(w, 503, "desktop is offline")
			return
		}
		p.ServeHTTP(w, r)
	})
}

// spaHandler serves index.html for extension-less paths so the single-page
// app works with deep links.
func spaHandler(fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(filepath.Base(r.URL.Path), ".") {
			r.URL.Path = "/" // FileServer serves index.html for directories
		}
		fileServer.ServeHTTP(w, r)
	})
}

func defaultConfig() string {
	if p := os.Getenv("WAKER_CONFIG"); p != "" {
		return p
	}
	return "/etc/waker/server.yaml"
}

func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Desktop.MAC == "" || cfg.Desktop.Hostname == "" {
		return nil, errors.New("desktop.mac and desktop.hostname are required")
	}
	if cfg.Wake.Target == "" {
		cfg.Wake.Target = "255.255.255.255:9"
	}
	if cfg.Wake.Count <= 0 {
		cfg.Wake.Count = 5
	}
	if cfg.Wake.IntervalMs <= 0 {
		cfg.Wake.IntervalMs = 1000
	}
	if cfg.AgentPort <= 0 {
		cfg.AgentPort = 8091
	}
	return &cfg, nil
}

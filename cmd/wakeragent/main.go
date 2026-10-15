// wakeragent runs on the LLM desktop and exposes system stats, service
// control, and LM Studio model management over HTTP (tailnet-only).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"waker/internal/agent"
	"waker/internal/httpx"
	"waker/internal/lmstudio"
	"waker/internal/tailscale"

	"gopkg.in/yaml.v3"
)

const defaultPort = 8091

type lmConfig struct {
	Port int `yaml:"port"` // LM Studio local server port
}

type config struct {
	Listen         string             `yaml:"listen"`          // "" or "auto" → primary tailnet IP
	Token          string             `yaml:"token"`           // shared bearer token
	StartTimeout   time.Duration      `yaml:"start_timeout"`   // how long to wait for a service to come up
	HealthInterval time.Duration      `yaml:"health_interval"` // how often running services are probed
	LMStudio       lmConfig           `yaml:"lmstudio"`
	Services       []agent.ServiceDef `yaml:"services"`
}

func main() {
	configPath := flag.String("config", defaultConfig(), "path to agent.yaml")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("wakeragent: %v", err)
	}

	services := make(map[string]*agent.Service, len(cfg.Services))
	for _, def := range cfg.Services {
		if def.ID == "" {
			log.Printf("wakeragent: skipping service with empty id")
			continue
		}
		services[def.ID] = agent.NewService(def, cfg.StartTimeout)
	}
	lmsc := lmstudio.New(cfg.LMStudio.Port)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go watchLoop(ctx, services, cfg.HealthInterval)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		for _, svc := range services {
			svc.Watch() // converge state with reality on every poll, not just the watch tick
		}
		httpx.WriteJSON(w, map[string]any{
			"system":   agent.Collect(r.Context()),
			"services": snapshots(services),
		})
	})
	api.HandleFunc("POST /api/services/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		svc := services[r.PathValue("id")]
		if svc == nil {
			httpx.WriteErr(w, 404, "unknown service")
			return
		}
		if err := svc.Start(r.Context()); err != nil {
			httpx.WriteErr(w, 500, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"ok": true})
	})
	api.HandleFunc("POST /api/services/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		svc := services[r.PathValue("id")]
		if svc == nil {
			httpx.WriteErr(w, 404, "unknown service")
			return
		}
		if err := svc.Stop(r.Context()); err != nil {
			httpx.WriteErr(w, 500, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"ok": true})
	})
	api.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		models, err := lmsc.Models(r.Context())
		if err != nil {
			httpx.WriteErr(w, 502, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"models": models})
	})
	api.HandleFunc("POST /api/models/load", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
			httpx.WriteErr(w, 400, "missing \"model\"")
			return
		}
		// Detach from the request: loading a big model can outlive the client.
		loadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := lmsc.Load(loadCtx, body.Model); err != nil {
			httpx.WriteErr(w, 502, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"ok": true})
	})
	api.HandleFunc("POST /api/models/unload", func(w http.ResponseWriter, r *http.Request) {
		loadCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := lmsc.Unload(loadCtx); err != nil {
			httpx.WriteErr(w, 502, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"ok": true})
	})

	root := http.NewServeMux()
	root.Handle("/api/", httpx.Auth(api, cfg.Token))
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	addr, err := tailscale.ListenAddr(ctx, cfg.Listen, defaultPort)
	if err != nil {
		log.Fatalf("wakeragent: %v", err)
	}
	log.Printf("wakeragent listening on %s (%d services)", addr, len(services))

	srv := &http.Server{Addr: addr, Handler: root}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		log.Printf("wakeragent stopped")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("wakeragent: %v", err)
		}
	}
}

func watchLoop(ctx context.Context, services map[string]*agent.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, svc := range services {
				svc.Watch()
			}
		}
	}
}

func snapshots(services map[string]*agent.Service) []agent.ServiceView {
	out := make([]agent.ServiceView, 0, len(services))
	for _, svc := range services {
		out = append(out, svc.Snapshot())
	}
	return out
}

func defaultConfig() string {
	if p := os.Getenv("WAKER_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "waker", "agent.yaml")
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
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 30 * time.Second
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 30 * time.Second
	}
	return &cfg, nil
}

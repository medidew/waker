// wakeserver runs on the always-on box: it sends Wake-on-LAN magic packets,
// tracks desktop presence over Tailscale, proxies API calls to the desktop
// agent, and serves the dashboard UI.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"waker/internal/agent"
	"waker/internal/httpx"
	"waker/internal/lmstudio"
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

// appJS is a hard build requirement: a single-file embed pattern that
// matches nothing is a compile error, so a wakeserver binary can never be
// built (or shipped) without the compiled dashboard UI. It exists so the
// all:web directory embed above cannot silently omit app.js.
//
//go:embed web/app.js
var appJS embed.FS

var pageTmpl *template.Template

// tmplFuncs keeps the presentation helpers out of the templates themselves.
var tmplFuncs = template.FuncMap{
	"stateLabel": stateLabel,
	"uptime":     fmtUptime,
	"gb":         fmtGB,
	"mss": func(sec int) string { // 47 -> "0:47"
		return fmt.Sprintf("%d:%02d", sec/60, sec%60)
	},
	"div100": func(a, b uint64) int { // percent, 0 when b is 0
		if b == 0 {
			return 0
		}
		return int(100 * a / b)
	},
}

var stateLabelMap = map[string]string{
	"running": "Running", "starting": "Starting…", "stopping": "Stopping…",
	"stopped": "Stopped", "failed": "Failed", "unknown": "Unknown",
}

func stateLabel(s string) string {
	if l, ok := stateLabelMap[s]; ok {
		return l
	}
	return s
}

func fmtUptime(sec float64) string {
	d := int(sec / 86400)
	h := int(sec/3600) % 24
	m := int(sec/60) % 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func fmtGB(mb uint64) string {
	return fmt.Sprintf("%.1f", float64(mb)/1024)
}

type link struct {
	Name    string `yaml:"name" json:"name"`
	URL     string `yaml:"url"  json:"url"`                            // legacy: direct URL (bypasses the single-origin principle; prefer proxy)
	Proxy   string `yaml:"proxy,omitempty"   json:"proxy,omitempty"`   // new: /u/<id> link proxy on this server
	Service string `yaml:"service,omitempty" json:"service,omitempty"` // optional: bind to a managed desktop service id
}

// proxyDef is a named reverse target behind /u/<id>/. A target of
// "desktop:<port>" resolves to the desktop's current tailnet IP per request;
// anything else is used verbatim.
type proxyDef struct {
	ID     string `yaml:"id"`
	Target string `yaml:"target"`
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
	Links            []link        `yaml:"links"`   // dashboard quick links
	Proxies          []proxyDef    `yaml:"proxies"` // named targets behind /u/<id>/
}

type server struct {
	cfg     config
	tracker *tailscale.Tracker
	started time.Time

	mu     sync.Mutex
	waking bool
	wakeAt time.Time

	actMu   sync.Mutex
	lastAct time.Time // last service/model form action; arms the 2 s fast-refresh window
	lastErr string    // one-shot flash message, shown on the next page load

	agentFast *http.Client // short agent GETs (status/models)
	agentSlow *http.Client // form actions: capped, but a big model keeps loading in the background
}

// pageData is the rendered dashboard; zero-valued desktop sections mean
// "offline or agent unreachable" and the template hides the cards.
type pageData struct {
	DesktopName string
	Online      bool
	Waking      bool
	WakeSeconds int
	RefreshSec  int

	Sys       agent.SystemStats
	StatsOK   bool
	Services  []agent.ServiceView
	Models    []lmstudio.Model
	ModelsOK  bool
	Loaded    *lmstudio.Model
	AgentErr  string // agent unreachable this round
	ModelErr  string // model list unavailable this round
	Links     []linkRow
	SSH       string
	Flash     string
	ServerAge float64
}

type linkRow struct {
	Name    string
	Href    string // "/u/<proxy>" preferred, legacy direct URL
	Service string // agent service id, "" for unbound
	State   string // live service state for the status dot; "" when unknown
}

// agentSnapshot is one round of agent polling (shared by the page, the stream
// and nothing else — the agent itself never polls itself).
type agentSnapshot struct {
	System   agent.SystemStats
	Services []agent.ServiceView
	Models   []lmstudio.Model
	StatsOK  bool
	ModelsOK bool
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

	tracker := tailscale.NewTracker(cfg.Desktop.Hostname, cfg.PresenceInterval, cfg.AgentPort)
	tracker.Start(ctx)

	s := &server{
		cfg:       *cfg,
		tracker:   tracker,
		started:   time.Now(),
		agentFast: &http.Client{Timeout: 2 * time.Second},
		agentSlow: &http.Client{Timeout: 10 * time.Second},
	}

	webSub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("wakeserver: %v", err)
	}
	if pageTmpl, err = template.New("waker").Funcs(tmplFuncs).ParseFS(webSub, "templates/*.html"); err != nil {
		log.Fatalf("wakeserver: parse templates: %v", err)
	}

	api := http.NewServeMux()
	api.HandleFunc("POST /api/wake", s.handleWake)
	api.HandleFunc("POST /api/services/{id}/{action}", s.handleServiceAPI)
	api.HandleFunc("POST /api/models/load", s.handleModelLoadAPI)
	api.HandleFunc("POST /api/models/unload", s.handleModelUnloadAPI)
	api.HandleFunc("GET /api/status", s.handleStatus)
	api.HandleFunc("GET /api/stream", s.handleStream)
	api.Handle("/api/desktop/", s.proxy())

	fileServer := http.FileServer(http.FS(webSub))
	root := http.NewServeMux()
	root.Handle("/api/", httpx.Auth(api, cfg.Token, "waker_auth")) // bearer (wakerctl) or the page's HttpOnly cookie
	root.HandleFunc("/", s.handlePage)                             // catch-all subtree: serves the page on any method
	root.HandleFunc("GET /login", s.handleLogin)
	root.HandleFunc("POST /login", s.handleLoginPost)
	root.HandleFunc("POST /logout", s.handleLogout)
	root.HandleFunc("POST /wake", s.handleWakeForm)
	root.HandleFunc("POST /desktop/services/{id}/{action}", s.handleServiceForm)
	root.HandleFunc("POST /desktop/models/load", s.handleModelLoadForm)
	root.HandleFunc("POST /desktop/models/unload", s.handleModelUnloadForm)
	root.HandleFunc("/u/{id}/", s.handleLink)
	root.Handle("/style.css", fileServer)
	root.Handle("/icon.svg", fileServer)
	root.Handle("/manifest.webmanifest", fileServer)
	root.Handle("/app.js", fileServer)

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

var errDesktopOnline = errors.New("desktop is already online")

// doWake fires the magic packets and arms the waking window. The API handler
// maps its error to status codes; the page form handler flashes it instead.
func (s *server) doWake() error {
	if s.tracker.Online() {
		return errDesktopOnline
	}
	if _, err := wol.Packet(s.cfg.Desktop.MAC); err != nil {
		return err // validation of the configured MAC
	}

	s.mu.Lock()
	now := time.Now()
	fresh := !(s.waking && now.Sub(s.wakeAt) < wakeTimeout+staleWindow) // in-progress attempt: no re-fire
	if fresh {
		s.waking = true
		s.wakeAt = now
	}
	s.mu.Unlock()
	if fresh {
		go s.firePackets()
	}
	return nil
}

func (s *server) handleWake(w http.ResponseWriter, r *http.Request) {
	if err := s.doWake(); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errDesktopOnline) {
			code = http.StatusConflict
		}
		httpx.WriteErr(w, code, err.Error())
		return
	}
	s.mu.Lock()
	started := s.wakeAt
	s.mu.Unlock()
	httpx.WriteJSON(w, map[string]any{"ok": true, "waking": true, "started_at": started})
}

// handleWakeForm is the no-JS wake button: act, flash on error, redirect.
func (s *server) handleWakeForm(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Token != "" && !s.authed(r) {
		httpx.WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	if err := s.doWake(); err != nil {
		s.setErr(err.Error())
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// wakeState reports an active wake attempt; expired attempts are dropped.
func (s *server) wakeState() (active bool, startedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waking && !s.tracker.Online() {
		if time.Since(s.wakeAt) > wakeTimeout+staleWindow {
			s.waking = false // attempt expired; allow a fresh one
			return false, time.Time{}
		}
		return true, s.wakeAt
	}
	return false, time.Time{}
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
	waking, startedAt := s.wakeState()
	var startedAtPtr *time.Time
	if waking {
		t := startedAt
		startedAtPtr = &t
	}

	httpx.WriteJSON(w, map[string]any{
		"desktop": map[string]any{
			"online":    s.tracker.Online(),
			"hostname":  s.cfg.Desktop.Hostname,
			"ip":        s.tracker.IP(),
			"ssh_user":  s.cfg.Desktop.SSHUser,
			"last_seen": s.tracker.LastSeen(),
		},
		"waking": map[string]any{"active": waking, "started_at": startedAtPtr},
		"server": map[string]any{
			"uptime_sec": time.Since(s.started).Seconds(),
		},
		// The dashboard uses this as its own poll period.
		"poll_ms": int64(s.cfg.PresenceInterval / time.Millisecond),
		"links":   s.cfg.Links,
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

// ---------- page, login, form actions ----------

func (s *server) handlePage(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	var d pageData
	s.snapshot(r.Context(), &d)
	d.RefreshSec = s.refreshSec(&d)
	d.Flash = s.takeFlash()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.ExecuteTemplate(w, "page", &d); err != nil {
		log.Printf("wakeserver: render page: %v", err)
	}
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.authed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.ExecuteTemplate(w, "login", map[string]any{"Bad": r.URL.Query().Get("bad") != ""}); err != nil {
		log.Printf("wakeserver: render login: %v", err)
	}
}

func (s *server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(s.cfg.Token)) == 1 {
		http.SetCookie(w, &http.Cookie{
			Name: "waker_auth", Value: s.cfg.Token,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/",
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login?bad=1", http.StatusFound)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "waker_auth", Value: "", MaxAge: -1, HttpOnly: true, Path: "/"})
	http.Redirect(w, r, "/", http.StatusFound)
}

// errOffline is the shared "desktop is offline" sentinel: the form handlers
// flash it, the /api/ twins map it to 503.
var errOffline = errors.New("desktop is offline")

// serviceAction / modelLoadAction / modelUnloadAction are the shared cores
// of the start/stop and load/unload endpoints (offline check + agent call).
// The form handlers flash the error and redirect (the no-JS path); the
// /api/ twins answer with JSON for the dashboard UI.
func (s *server) serviceAction(ctx context.Context, id, action string) error {
	if !s.tracker.Online() || s.tracker.IP() == "" {
		return errOffline
	}
	return s.agentDo(ctx, fmt.Sprintf("/api/services/%s/%s", url.PathEscape(id), action), nil)
}

func (s *server) modelLoadAction(ctx context.Context, model string) error {
	if !s.tracker.Online() || s.tracker.IP() == "" {
		return errOffline
	}
	body, _ := json.Marshal(map[string]string{"model": model})
	return s.agentDo(ctx, "/api/models/load", strings.NewReader(string(body)))
}

func (s *server) modelUnloadAction(ctx context.Context) error {
	if !s.tracker.Online() || s.tracker.IP() == "" {
		return errOffline
	}
	return s.agentDo(ctx, "/api/models/unload", nil)
}

// handleServiceForm is the no-JS start/stop button: proxy the action to
// the agent, flash any error, redirect back to a fresh render.
func (s *server) handleServiceForm(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		httpx.WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	action := r.PathValue("action")
	if action != "start" && action != "stop" {
		httpx.WriteErr(w, http.StatusBadRequest, "action must be start or stop")
		return
	}
	if err := s.serviceAction(r.Context(), r.PathValue("id"), action); err != nil {
		s.setErr(err.Error())
	} else {
		s.markAction()
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleModelLoadForm / handleModelUnloadForm: same pattern; the load form's
// <select> submits the model id as a form field, rewritten to the JSON body
// the agent expects.
func (s *server) handleModelLoadForm(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		httpx.WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	model := r.FormValue("model")
	if model == "" {
		s.setErr("no model selected")
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := s.modelLoadAction(r.Context(), model); err != nil {
		s.setErr(err.Error())
	} else {
		s.markAction()
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *server) handleModelUnloadForm(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		httpx.WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	if err := s.modelUnloadAction(r.Context()); err != nil {
		s.setErr(err.Error())
	} else {
		s.markAction()
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleServiceAPI / handleModelLoadAPI / handleModelUnloadAPI are the JSON
// twins of the form handlers: the dashboard UI fetches them and applies the
// result in place, so no page navigation happens. Auth comes from the /api/
// wrapper (bearer or the waker_auth cookie).
func (s *server) handleServiceAPI(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "start" && action != "stop" {
		httpx.WriteErr(w, http.StatusBadRequest, "action must be start or stop")
		return
	}
	s.finishAction(w, s.serviceAction(r.Context(), r.PathValue("id"), action))
}

func (s *server) handleModelLoadAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Model == "" {
		httpx.WriteErr(w, http.StatusBadRequest, "no model selected")
		return
	}
	s.finishAction(w, s.modelLoadAction(r.Context(), in.Model))
}

func (s *server) handleModelUnloadAPI(w http.ResponseWriter, r *http.Request) {
	s.finishAction(w, s.modelUnloadAction(r.Context()))
}

// finishAction maps an action's result to the JSON the UI expects: 200
// {"ok":true} on success, 503 when the desktop is offline, 502 when the
// agent refused or failed the action.
func (s *server) finishAction(w http.ResponseWriter, err error) {
	if err == nil {
		s.markAction()
		httpx.WriteJSON(w, map[string]any{"ok": true})
		return
	}
	code := http.StatusBadGateway
	if errors.Is(err, errOffline) {
		code = http.StatusServiceUnavailable
	}
	httpx.WriteErr(w, code, err.Error())
}

// ---------- /u/<id>/ link proxy: service UIs behind the wakeserver origin ----------

// resolveProxy maps a proxy id to its target URL. "desktop:<port>" chases
// the desktop's current tailnet IP; anything else is used verbatim.
func (s *server) resolveProxy(id string) (target string, desktopBound bool, ok bool) {
	for _, p := range s.cfg.Proxies {
		if p.ID != id {
			continue
		}
		if strings.HasPrefix(p.Target, "desktop:") {
			if !s.tracker.Online() || s.tracker.IP() == "" {
				return "", true, false
			}
			return "http://" + s.tracker.IP() + ":" + strings.TrimPrefix(p.Target, "desktop:"), true, true
		}
		return p.Target, false, true
	}
	return "", false, false
}

func (s *server) handleLink(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		httpx.WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	id := r.PathValue("id")
	target, desktopBound, ok := s.resolveProxy(id)
	if !ok {
		if desktopBound {
			httpx.WriteErr(w, http.StatusServiceUnavailable, "desktop is offline")
		} else {
			httpx.WriteErr(w, http.StatusNotFound, "unknown proxy "+id)
		}
		return
	}
	s.proxyToLink(w, r, id, target)
}

func (s *server) proxyToLink(w http.ResponseWriter, r *http.Request, id, target string) {
	if isWebSocketUpgrade(r) {
		s.proxyWebSocket(w, r, target)
		return
	}
	base, err := url.Parse(target)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "bad proxy target: "+err.Error())
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/u/"+url.PathEscape(id))
	p := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = base.Scheme
			req.URL.Host = base.Host
			req.URL.Path = strings.TrimSuffix(base.Path, "/") + suffix
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = base.Host
			// never leak the waker token to service origins
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("wakeserver: proxy /u/%s failed: %v", id, err)
			httpx.WriteErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		},
	}
	p.ServeHTTP(w, r)
}

// WebSocket upgrades: ReverseProxy won't tunnel them, so dial the target and
// pump bytes both ways. Open WebUI and ComfyUI stream over WS.
func (s *server) proxyWebSocket(w http.ResponseWriter, r *http.Request, target string) {
	up, err := url.Parse(target)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadGateway, "bad proxy target: "+err.Error())
		return
	}
	wsScheme := "ws"
	if up.Scheme == "https" {
		wsScheme = "wss"
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/u/"+r.PathValue("id"))
	wsURL := wsScheme + "://" + up.Host + strings.TrimSuffix(up.Path, "/") + suffix
	if r.URL.RawQuery != "" {
		wsURL += "?" + r.URL.RawQuery
	}
	hdr := http.Header{"Host": {up.Host}}
	// Origin/User-Agent pass through; waker credentials never reach the origin
	for _, h := range []string{"Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol", "Origin", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	remote, _, err := (&websocket.Dialer{}).DialContext(r.Context(), wsURL, hdr)
	if err != nil {
		log.Printf("wakeserver: websocket dial %s: %v", wsURL, err)
		http.Error(w, "websocket dial failed", http.StatusBadGateway)
		return
	}
	defer remote.Close()
	local, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already answered
	}
	defer local.Close()
	// Pump frames both ways. Conn is frame-based (no io.Reader/Writer), so
	// messages travel as ReadMessage/WriteMessage pairs. When one side dies
	// (close frame, error, or handler exit), both defers close the conns and
	// the still-running pump unblocks on its next read/write.
	remoteToClient := make(chan struct{})
	go func() {
		for {
			mt, data, err := remote.ReadMessage()
			if err != nil || local.WriteMessage(mt, data) != nil {
				close(remoteToClient)
				return
			}
		}
	}()
	go func() {
		for {
			mt, data, err := local.ReadMessage()
			if err != nil || remote.WriteMessage(mt, data) != nil {
				return
			}
		}
	}()
	select {
	case <-remoteToClient: // upstream ended
	case <-r.Context().Done(): // client left
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // tailnet-only listener
}

func isWebSocketUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "websocket" {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

// handleStream pushes the same payload the page renders, for stream-capable
// clients (the wakerctl TUI, any native app). The dashboard UI (app.js)
// consumes this over EventSource; the no-JS fallback instead uses the
// dynamic meta refresh.
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteErr(w, http.StatusNotImplemented, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	var d pageData
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		s.snapshot(r.Context(), &d)
		d.RefreshSec = s.refreshSec(&d) // keep the field meaningful for stream consumers
		d.Flash = ""                    // stream consumers get state, not one-shot flashes
		payload, err := json.Marshal(d)
		if err == nil {
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload)
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// ---------- agent polling (Go port of the old dashboard's per-poll fetches) ----------

// agentJSON GETs one agent endpoint with its own short cap; a flaky agent
// degrades a single field, not the whole page.
func (s *server) agentJSON(ctx context.Context, path string, v any) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET",
		fmt.Sprintf("http://%s:%d%s", s.tracker.IP(), s.cfg.AgentPort, path), nil)
	if err != nil {
		return err
	}
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := s.agentFast.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("agent %s: http %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// agentCopy is one polling round: system+services, then models.
func (s *server) agentCopy(ctx context.Context) agentSnapshot {
	var out agentSnapshot
	if !s.tracker.Online() || s.tracker.IP() == "" {
		return out
	}
	var st struct {
		System   agent.SystemStats   `json:"system"`
		Services []agent.ServiceView `json:"services"`
	}
	if s.agentJSON(ctx, "/api/status", &st) == nil {
		out.System, out.Services, out.StatsOK = st.System, st.Services, true
	}
	var md struct {
		Models []lmstudio.Model `json:"models"`
	}
	if s.agentJSON(ctx, "/api/models", &md) == nil {
		out.Models, out.ModelsOK = md.Models, true
	}
	return out
}

// agentDo POSTs a form action to the agent. The 10 s cap is a give-up, not a
// deadline: the agent detaches loads from the request context, so a big model
// keeps loading after we stop waiting.
func (s *server) agentDo(ctx context.Context, path string, body io.Reader) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST",
		fmt.Sprintf("http://%s:%d%s", s.tracker.IP(), s.cfg.AgentPort, path), body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	resp, err := s.agentSlow.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("still working in the background — check back in a moment")
		}
		return fmt.Errorf("desktop agent unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&e); err == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return fmt.Errorf("desktop agent: http %d", resp.StatusCode)
}

// ---------- page state: snapshot, cadence, flash, auth ----------

// snapshot assembles the page payload: presence + waking + (when online) the
// agent copy. The stream endpoint reuses it too.
func (s *server) snapshot(ctx context.Context, d *pageData) {
	d.DesktopName = s.cfg.Desktop.Hostname
	d.Online = s.tracker.Online()
	if active, startedAt := s.wakeState(); active {
		d.Waking = true
		d.WakeSeconds = int(time.Since(startedAt).Seconds())
	}
	if d.Online {
		c := s.agentCopy(ctx)
		d.Sys, d.Services, d.StatsOK = c.System, c.Services, c.StatsOK
		if !c.StatsOK {
			d.AgentErr = "agent unreachable"
		}
		d.Models, d.ModelsOK = c.Models, c.ModelsOK
		if c.ModelsOK {
			for i := range c.Models {
				if c.Models[i].State == "loaded" {
					d.Loaded = &c.Models[i]
					break
				}
			}
		} else {
			d.ModelErr = "model list unavailable"
		}
	}
	d.Links = s.linkRows(d)
	if s.cfg.Desktop.Hostname != "" && s.tracker.IP() != "" {
		user := s.cfg.Desktop.SSHUser
		if user != "" {
			user += "@"
		}
		d.SSH = fmt.Sprintf("ssh %s%s (%s)", user, s.cfg.Desktop.Hostname, s.tracker.IP())
	}
	d.ServerAge = time.Since(s.started).Seconds()
}

// refreshSec picks the page reload cadence: the presence interval, shortening
// to 2 s while something is in motion — the server-side twin of the old
// client fast-poll window.
func (s *server) refreshSec(d *pageData) int {
	sec := int(s.cfg.PresenceInterval / time.Second)
	if sec < 2 {
		sec = 2
	}
	fast := d.Waking
	if !fast {
		for _, sv := range d.Services {
			if sv.State == agent.StateStarting || sv.State == agent.StateStopping {
				fast = true
				break
			}
		}
	}
	if !fast {
		s.actMu.Lock()
		fast = time.Since(s.lastAct) < 40*time.Second
		s.actMu.Unlock()
	}
	if fast {
		sec = 2
	}
	return sec
}

// markAction arms the fast-refresh window and stores a one-shot flash message.
func (s *server) setErr(msg string) {
	s.actMu.Lock()
	s.lastErr = msg
	s.lastAct = time.Now()
	s.actMu.Unlock()
}

func (s *server) markAction() {
	s.actMu.Lock()
	s.lastAct = time.Now()
	s.actMu.Unlock()
}

// takeFlash returns and clears the pending flash message (shown once).
func (s *server) takeFlash() string {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	msg := s.lastErr
	s.lastErr = ""
	return msg
}

// authed accepts the page cookie or a bearer header — both carry the single
// token from config; there is no separate session store.
func (s *server) authed(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	if c, err := r.Cookie("waker_auth"); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.cfg.Token)) == 1 {
		return true
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.cfg.Token
}

// linkRows resolves configured links to rendered rows: proxy-backed links
// get a same-origin /u/<id> href (single-origin principle); legacy direct
// URLs pass through; service-bound rows carry a live status dot.
func (s *server) linkRows(d *pageData) []linkRow {
	rows := make([]linkRow, 0, len(s.cfg.Links))
	for _, l := range s.cfg.Links {
		row := linkRow{Name: l.Name, Service: l.Service}
		switch {
		case l.Proxy != "":
			row.Href = "/u/" + url.PathEscape(l.Proxy) + "/"
		default:
			row.Href = l.URL // legacy direct URL (deprecated, see deploy/README)
		}
		if l.Service != "" {
		find:
			for _, sv := range d.Services {
				if sv.ID == l.Service {
					row.State = string(sv.State)
					break find
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
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

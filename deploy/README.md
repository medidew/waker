# Waker — setup

Two static Go binaries: `wakeserver` (always-on box) and `wakeragent` (desktop).
Everything runs over Tailscale; nothing is exposed publicly.

## Build

Prerequisite: Node.js ≥ 20 (for the single `tsc` pass that compiles the dashboard UI).

```sh
cd waker
scripts/build.sh          # gofmt + go vet + go test, then builds the UI (npm ci + tsc + asset copy into web/), then builds both into bin/
```

Or manually:

```sh
# web/ is pure build output, regenerated from ui/ (the front-end source):
rm -rf cmd/wakeserver/web && mkdir cmd/wakeserver/web
cp cmd/wakeserver/ui/style.css cmd/wakeserver/ui/icon.svg cmd/wakeserver/ui/manifest.webmanifest cmd/wakeserver/web/
cp -r cmd/wakeserver/ui/templates cmd/wakeserver/web/
npm ci --prefix cmd/wakeserver/ui
npm run build --prefix cmd/wakeserver/ui    # compiles src/app.ts → web/app.js
go build -o bin/wakeserver ./cmd/wakeserver
go build -o bin/wakeragent  ./cmd/wakeragent
# cross-compile if needed, e.g.: CGO_ENABLED=0 GOOS=linux go build ...
```

The dashboard UI is embedded in `wakeserver` — one file to deploy. It is a
single TypeScript file compiled to one classic `app.js` (no browser
dependencies, no framework); the page keeps a `<noscript>` self-refresh
fallback and plain form POSTs, so it still works without JavaScript.

## Desktop (CachyOS)

1. Copy the binary: `install -m755 bin/wakeragent ~/.local/bin/`
2. Create config from the example, set a real token:
   ```sh
   mkdir -p ~/.config/waker
   cp deploy/agent.yaml.example ~/.config/waker/agent.yaml
   # edit: token (openssl rand -hex 16), the lms paths inside each service
# command, and lmstudio_port if your server runs on a different port
   ```
3. Install and start the user service:
   ```sh
   cp deploy/waker-agent.service ~/.config/systemd/user/
   systemctl --user daemon-reload
   systemctl --user enable --now waker-agent
   journalctl --user -u waker-agent -f     # watch it come up
   ```
4. Verify locally: `curl -H "Authorization: Bearer <token>" http://127.0.0.1:8091/api/status`

Note: the agent binds its primary tailnet IP (auto-detected). If you prefer,
set `listen: 0.0.0.0:8091` or an explicit address in the config.

## Always-on box

1. Copy the binary: `sudo install -m755 bin/wakeserver /usr/local/bin/`
2. Create config from the example, set a real token and the desktop's MAC:
   ```sh
   sudo mkdir -p /etc/waker
   sudo cp deploy/server.yaml.example /etc/waker/server.yaml
   sudoedit /etc/waker/server.yaml
   ```
3. Install and start:
   ```sh
   sudo cp deploy/waker-wakeserver.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now waker-wakeserver
   journalctl -u waker-wakeserver -f
   ```

## Prerequisites (both machines)

- Tailscale installed and logged in (`tailscaled` running). The desktop must be
  a tailnet member so the box can see it come online after wake.
- Wake-on-LAN on the desktop: enabled in BIOS/UEFI, NIC set to `wol g`.
  Persist across reboots (pick one):
  - NetworkManager dispatcher script calling `ethtool -s <iface> wol g`, or
  - a udev rule / systemd unit at boot.
- The box must be on the same L2 segment as the desktop for broadcast wake to
  work (same switch/VLAN).

## Test before relying on it remotely

1. On the box, with the desktop **asleep**:
   `curl -X POST -H "Authorization: Bearer <token>" http://<box-tailnet-ip>:8090/api/wake`
2. Watch the desktop boot; confirm it appears in `tailscale status`.
3. Open the dashboard on your phone (same tailnet):
   `http://<box-tailnet-ip>:8090`, then enter the token on the `/login` form.
   The server sets an HttpOnly `waker_auth` cookie so you're not re-prompted.

## Security notes

- Both APIs require the shared bearer token and bind to the tailnet interface
  by default (`listen: auto`). Tailnet ACLs are your first gate; the token is
  defense-in-depth. Keep the token out of version control.
- The dashboard gates the UI with an HttpOnly `waker_auth` cookie (value = the
  config token, compared in constant time) set by the `/login` form; there is
  no separate session store. Non-browser clients keep using `Authorization: Bearer`.

## Dashboard, link proxies, and the stream API

The page at `/` is server-rendered and JavaScript-free: a dynamic
`<meta http-equiv="refresh">` drives the reload cadence (`presence_interval`,
shortened to 2 s while a wake or a service transition is in motion), and every
action — wake, service start/stop, model load/unload — is a plain form POST
that redirects back to a fresh render.

`links:` entries that set a `proxy:` id resolve to a same-origin `/u/<id>/`
route, a reverse proxy (HTTP plus a WebSocket tunnel) onto a `proxies:` target.
The browser only ever talks to waker's own origin; waker's cookie and
`Authorization` header are never forwarded to the target.

- `target: desktop:<port>` chases the desktop's current tailnet IP — use it for
  anything the agent runs locally (e.g. LM Studio on `:1234`).
- Any other `target` is used verbatim (e.g. `https://…ts.net` domains).
- A target bound to the desktop returns 503 while the desktop is offline.

Note: a service UI that navigates to an *absolute* path (no `/u/<id>` prefix)
leaves the proxy and goes straight to its real origin. LM Studio is fine with
the prefix; Open WebUI and ComfyUI make such navigations, so treat those
proxied links as best-effort — the direct ts.net URL still works as a
fallback.

For programmatic clients (curl today, the planned `wakerctl` TUI in
PLAN.md §12), `GET /api/stream` emits the same payload the dashboard renders
as `text/event-stream` frames roughly every 2 s.

## Troubleshooting

- **Desktop never shows online**: check `tailscale status` on the box for the
  desktop node name (must match `desktop.hostname`); confirm tailscaled starts
  at boot on the desktop.
- **Wake does nothing**: verify MAC in config (`ip link`), that the box can
  reach the LAN broadcast, and try unicast to the desktop's fixed IP by adding
  a second wake target if your switch forwards it better.
- **Service card red (failed)**: the hint line shows the start/stop command
  output tail; run the same command manually on the desktop for full context.

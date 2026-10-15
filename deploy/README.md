# Waker — setup

Two static Go binaries: `wakeserver` (always-on box) and `wakeragent` (desktop).
Everything runs over Tailscale; nothing is exposed publicly.

## Build

```sh
cd waker
go build -o dist/wakeserver ./cmd/wakeserver
go build -o dist/wakeragent  ./cmd/wakeragent
# cross-compile if needed, e.g.: CGO_ENABLED=0 GOOS=linux go build ...
```

The dashboard UI is embedded in `wakeserver` — one file to deploy.

## Desktop (CachyOS)

1. Copy the binary: `install -m755 dist/wakeragent ~/.local/bin/`
2. Create config from the example, set a real token:
   ```sh
   mkdir -p ~/.config/waker
   cp deploy/agent.yaml.example ~/.config/waker/agent.yaml
   # edit: token (openssl rand -hex 16), lms_bin path if different
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

1. Copy the binary: `sudo install -m755 dist/wakeserver /usr/local/bin/`
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
   `http://<box-tailnet-ip>:8090`, enter the token once (stored locally).

## Security notes

- Both APIs require the shared bearer token and bind to the tailnet interface
  by default (`listen: auto`). Tailnet ACLs are your first gate; the token is
  defense-in-depth. Keep the token out of version control.
- The dashboard stores the token in browser localStorage on each device.

## Troubleshooting

- **Desktop never shows online**: check `tailscale status` on the box for the
  desktop node name (must match `desktop.hostname`); confirm tailscaled starts
  at boot on the desktop.
- **Wake does nothing**: verify MAC in config (`ip link`), that the box can
  reach the LAN broadcast, and try unicast to the desktop's fixed IP by adding
  a second wake target if your switch forwards it better.
- **Service card red (failed)**: the hint line shows the start/stop command
  output tail; run the same command manually on the desktop for full context.

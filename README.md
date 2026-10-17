# Waker

A custom bit of software to turn my 32GB VRAM desktop into something I can use for local LLMs from my phone or laptop. It has a button to wake my sleeping PC using Wake-on-LAN, a small dashboard that shows resource usage and what LLM-relevant stuff I have running on it, and it lets me control it from that dashboard without touching the machine or having to `ssh` in.

Everything runs over Tailscale.

## Why make this?

The same machine running LLMs in this setup also happens to be my gaming PC with two RTX 5060 Tis. Most of the time it's sleeping or off, so running a model used to mean needing access to my local network or remotely logging into a dedicated machine to use WoL, and I wanted to automate that process.

The project also served as a perfect opportunity to test out some local agentic coding with Qwen3.8-27B and OpenCode. Otherwise I wouldn't have touched JS with a one-hundred-kilometre pole!

## How it works

```
[phone / laptop] ──Tailscale──▶ always-on box :8090        (wakeserver)
                                  ├── wake:      WoL magic packets → LAN broadcast
                                  ├── presence:  tailscale status + direct /healthz probe
                                  ├── dashboard: one static binary
                                  └── proxy:     /api/desktop/* ──▶ desktop :8091  (wakeragent)
                                                                    system stats · services · models
```

- **`wakeserver`** is the component that runs on a dedicated machine that's always powered on and connected to my tailnet. It builds the magic packets and broadcasts them to the LAN, watches Tailscale (plus a direct `/healthz` probe, so a node that lists online but is actually asleep doesn't block a wake), and proxies the desktop's API so my phone only ever talks to one URL.
- **`wakeragent`** runs on my desktop as a systemd user service. It reports system stats and drives the service definitions, which are plain config data: start/stop/status commands plus an optional health URL or status probe, so it works the same way for the `lms` CLI, `systemctl --user` units, and sudo-gated system units alike.
- The dashboard is a static, no-build-step web UI (vanilla JS, PWA manifest + service worker) embedded into the `wakeserver` binary.

## Security

- All traffic is tailnet-only; the binaries bind the Tailscale interface by default (`listen: auto`).
- A shared bearer token in both configs is checked on every API call — defense-in-depth on top of tailnet ACLs.
    - Probably not needed but Qwen deemed it appropriate for this test run of mine, so what the hell.
- The dashboard asks for that token once on first load and keeps it in the browser's `localStorage`, which is what gates the UI on each device.

## Deployment

Build the two static binaries and install them per [`deploy/README.md`](deploy/README.md) — one system unit on the always-on box, one user unit on the desktop:

```sh
go build -o dist/wakeserver ./cmd/wakeserver
go build -o dist/wakeragent  ./cmd/wakeragent
```

Config files start from `deploy/server.yaml.example` and `deploy/agent.yaml.example`. Prerequisites: Tailscale on both machines, and Wake-on-LAN enabled in the desktop's BIOS/UEFI with the NIC set to `wol g` (the deploy README covers persisting that across reboots).

## Repository layout

```
├── cmd/wakeserver/       # API, wake, proxy; embeds the dashboard UI (go:embed)
│   └── web/              # index.html · app.js · style.css · PWA bits
├── cmd/wakeragent/       # desktop-side agent
├── internal/
│   ├── wol/              # magic packet build + broadcast send
│   ├── tailscale/        # tailscale status polling, presence tracking
│   ├── agent/            # system stats, service runner & state machine
│   ├── lmstudio/         # LM Studio local API client (+ CLI fallback)
│   └── httpx/            # shared bearer-token middleware + JSON helpers
└── deploy/               # systemd units, config examples, setup README
```

## Built with local LLMs

This project was my first attempt at developing an application entirely with local LLMs (Qwen3.8-27B specifically). Planning, implementation, and debugging were done only with models running on the very desktop this tool controls. Safe to say I'm happy with what I got out of it.
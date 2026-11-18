# Waker

A custom bit of software to turn my 32GB VRAM desktop into something I can use for local LLMs from my phone or laptop. It has a button to wake my sleeping PC using Wake-on-LAN, a small dashboard that shows resource usage and what LLM-relevant stuff I have running on it, and it lets me control it from that dashboard without touching the machine or having to `ssh` in.

Everything runs over Tailscale.

## Why make this?

The same machine running LLMs in this setup also happens to be my gaming PC with two RTX 5060 Tis. Most of the time it's sleeping or off, so running a model used to mean needing access to my local network or remotely logging into a dedicated machine to use WoL, and I wanted to automate that process.

The project also served as a perfect opportunity to test out some local agentic coding with Qwen3.8-27B and OpenCode. It started out with a vanilla-JS dashboard, and the final version ships no JavaScript at all: the page is rendered in Go and refreshes with a dynamic `<meta>` tag.

## How it works

```
[phone / laptop] ──Tailscale──▶ always-on box :8090        (wakeserver)
                                  ├── wake:      WoL magic packets → LAN broadcast
                                  ├── presence:  tailscale status + direct /healthz probe
                                  ├── dashboard: zero-JS, server-rendered HTML
                                  ├── proxy:     /api/desktop/* ──▶ desktop :8091  (wakeragent)
                                  │                 system stats · services · models
                                  └── links:    /u/<id>/*  ──▶ service UIs (LM Studio, Open WebUI, …)
```

- **`wakeserver`** is the component that runs on a dedicated machine that's always powered on and connected to my tailnet. It builds the magic packets and broadcasts them to the LAN, watches Tailscale (plus a direct `/healthz` probe, so a node that lists online but is actually asleep doesn't block a wake), and proxies the desktop's API so my phone only ever talks to one URL.
- **`wakeragent`** runs on my desktop as a systemd user service. It reports system stats and drives the service definitions, which are plain config data: start/stop/status commands plus an optional health URL or status probe, so it works the same way for the `lms` CLI, `systemctl --user` units, and sudo-gated system units alike.
- The dashboard is a zero-JS, no-build-step web UI: Go renders the page with `html/template` and it self-refreshes via a dynamic `<meta http-equiv="refresh">` tag — no `EventSource`, no fetches. Every action (wake, service start/stop, model load/unload) is a plain form POST that redirects back to a fresh render. Service UIs are reachable through a same-origin `/u/<id>/` link proxy (HTTP plus a hand-rolled WebSocket tunnel) so the browser only ever talks to one origin. The same payload is also exposed as `GET /api/stream` (SSE) for stream-capable clients — the planned `wakerctl` TUI/CLI (PLAN.md §12).

## Security

- All traffic is tailnet-only; the binaries bind the Tailscale interface by default (`listen: auto`).
- A shared bearer token in both configs is checked on every API call — defense-in-depth on top of tailnet ACLs.
    - Probably not needed but Qwen deemed it appropriate for this test run of mine, so what the hell.
- The dashboard's login form (`/login`) asks for that token once per device; on success the server sets an HttpOnly `waker_auth` cookie (constant-time compared against the config token), which is what gates the UI. The bearer header is still accepted on the API routes for non-browser clients.

## Deployment

Build the two static binaries and install them per [`deploy/README.md`](deploy/README.md) — one system unit on the always-on box, one user unit on the desktop:

```sh
scripts/build.sh   # gofmt + go vet + go test gate, then builds both into bin/
```

Config files start from `deploy/server.yaml.example` and `deploy/agent.yaml.example`. Prerequisites: Tailscale on both machines, and Wake-on-LAN enabled in the desktop's BIOS/UEFI with the NIC set to `wol g` (the deploy README covers persisting that across reboots).

## Development

```sh
scripts/build.sh        # gofmt + go vet + unit tests, then build both binaries into bin/
scripts/livetest.sh     # build, then live-smoke-test both components
scripts/livetest.sh server  # wakeserver only      ·  scripts/livetest.sh agent  # wakeragent only
```

`livetest.sh` starts each binary on a free 127.0.0.1 port with throwaway configs and exercises the real HTTP API: dashboard auth flows, link-proxy error paths, the SSE stream, and the agent's service state machine (a throwaway `sleep`-based demo service driven through start → running → stop → stopped). It also verifies Wake-on-LAN end to end: a `wolcheck` listener on loopback captures the magic packets the server fires and confirms all 160 bytes of each, with a junk-datagram negative control. No tailnet or external services are needed; 28 assertions, all output is self-contained.

## Repository layout

```
├── cmd/wakeserver/       # API, wake, page rendering, link proxy; embeds the UI (go:embed)
│   └── web/              # Go templates · style.css · PWA manifest + icon
├── cmd/wakeragent/       # desktop-side agent
├── cmd/wolcheck/         # UDP magic-packet verifier (livetest capture, manual debugging)
├── internal/
│   ├── wol/              # magic packet build + broadcast send
│   ├── tailscale/        # tailscale status polling, presence tracking
│   ├── agent/            # system stats, service runner & state machine
│   ├── lmstudio/         # LM Studio local API client (+ CLI fallback)
│   └── httpx/            # shared bearer-token middleware + JSON helpers
├── scripts/              # build.sh (gofmt+vet+test+build) and livetest.sh (live smoke test)
└── deploy/               # systemd units, config examples, setup README
```

## Built with local LLMs

This project was my first attempt at developing an application entirely with local LLMs (Qwen3.8-27B specifically). Planning, implementation, and debugging were done only with models running on the very desktop this tool controls. Safe to say I'm happy with what I got out of it.
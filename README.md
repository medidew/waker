# Waker

A custom bit of software to turn my 32GB VRAM desktop into something I can use for local LLMs from my phone or laptop. It has a button to wake my sleeping PC using Wake-on-LAN, a small dashboard that shows resource usage and what LLM-relevant stuff I have running on it, and it lets me control it from that dashboard without touching the machine or having to `ssh` in.

Everything runs over Tailscale.

## Why make this?

The same machine running LLMs in this setup also happens to be my gaming PC with two RTX 5060 Tis. Most of the time it's sleeping or off, so running a model used to mean needing access to my local network or remotely logging into a dedicated machine to use WoL, and I wanted to automate that process.

The project also served as a perfect opportunity to test out some local agentic coding with Qwen3.8-27B and OpenCode. It started out with a vanilla-JS dashboard; the final version is a single self-contained TypeScript file compiled to one classic `app.js` and embedded in the Go binary. No browser dependencies, no framework, no build step at runtime — just one `tsc` pass at build time.

## How it works

```
[phone / laptop] ──Tailscale──▶ always-on box :8090        (wakeserver)
                                  ├── wake:      WoL magic packets → LAN broadcast
                                  ├── presence:  tailscale status + direct /healthz probe
                                  ├── dashboard: single-file TS UI, no browser deps
                                  ├── proxy:     /api/desktop/* ──▶ desktop :8091  (wakeragent)
                                  │                 system stats · services · models
                                  └── links:    /u/<id>/*  ──▶ service UIs (LM Studio, Open WebUI, …)
```

- **`wakeserver`** is the component that runs on a dedicated machine that's always powered on and connected to my tailnet. It builds the magic packets and broadcasts them to the LAN, watches Tailscale (plus a direct `/healthz` probe, so a node that lists online but is actually asleep doesn't block a wake), and proxies the desktop's API so my phone only ever talks to one URL.
- **`wakeragent`** runs on my desktop as a systemd user service. It reports system stats and drives the service definitions, which are plain config data: start/stop/status commands plus an optional health URL or status probe, so it works the same way for the `lms` CLI, `systemctl --user` units, and sudo-gated system units alike.
- The dashboard is a single self-contained TypeScript file (`ui/src/app.ts`) that the build compiles to one classic, non-module `app.js`. `ui/` holds everything you'd edit (the TS source, templates, CSS, icon, manifest, and the `tsc` toolchain); `web/` is pure build output — that `app.js` plus copies of the static assets — and it is `web/` that gets `go:embed`-ed straight into the binary (so it's gitignored, and the 30 MB of `node_modules` never ends up in the binary). There is no browser-side dependency, framework, or bundler — the only build step is one `tsc` pass and a file copy (Node ≥ 20, `npm ci && npm run build`). In the browser it is a thin display layer: it opens `EventSource` on the existing `GET /api/stream` and renders each status frame in place, and it intercepts the action form submits, re-routing them to the JSON `/api/` twins so a start/stop/load/unload never reloads the page. The page keeps a `<noscript>` `<meta http-equiv="refresh">` fallback and the plain forms as the no-JS path, so a browser without JavaScript still works (it just self-refreshes). The `waker_auth` cookie is `HttpOnly`, so the UI relies on cookie auth and never reads the token. Service UIs are reachable through a same-origin `/u/<id>/` link proxy (HTTP plus a hand-rolled WebSocket tunnel) so the browser only ever talks to one origin — the desktop agent is never contacted directly from the browser; Go proxies every agent call.

## Security

- All traffic is tailnet-only; the binaries bind the Tailscale interface by default (`listen: auto`).
- A shared bearer token in both configs is checked on every API call — defense-in-depth on top of tailnet ACLs.
    - Probably not needed but Qwen deemed it appropriate for this test run of mine, so what the hell.
- The dashboard's login form (`/login`) asks for that token once per device; on success the server sets an HttpOnly `waker_auth` cookie (constant-time compared against the config token), which is what gates the UI. The bearer header is still accepted on the API routes for non-browser clients.

## Deployment

Build the two static binaries and install them per [`deploy/README.md`](deploy/README.md) — one system unit on the always-on box, one user unit on the desktop:

```sh
scripts/build.sh   # gofmt + go vet + go test, then builds the UI (npm ci + tsc + asset copy into web/) and builds both binaries into bin/
```

Config files start from `deploy/server.yaml.example` and `deploy/agent.yaml.example`. Prerequisites: Tailscale on both machines, and Wake-on-LAN enabled in the desktop's BIOS/UEFI with the NIC set to `wol g` (the deploy README covers persisting that across reboots).

## Development

```sh
scripts/build.sh        # gofmt + go vet + unit tests, then build both binaries into bin/
scripts/livetest.sh     # build, then live-smoke-test both components
scripts/livetest.sh server  # wakeserver only      ·  scripts/livetest.sh agent  # wakeragent only
```

`livetest.sh` starts each binary on a free 127.0.0.1 port with throwaway configs and exercises the real HTTP API: dashboard auth flows, link-proxy error paths, the SSE stream, and the agent's service state machine (a throwaway `sleep`-based demo service driven through start → running → stop → stopped). It also verifies Wake-on-LAN end to end: a `wolcheck` listener on loopback captures the magic packets the server fires and confirms all 160 bytes of each, with a junk-datagram negative control. No tailnet or external services are needed; 34 assertions, all output is self-contained. A fully green run leaves the dashboard up at the printed URL with its log streaming to the terminal — handy for poking at the UI; Ctrl+C stops it and removes the temp files.

## Repository layout

```
├── cmd/wakeserver/       # API, wake, page rendering, link proxy; embeds the UI (go:embed)
│   ├── ui/               # front-end source: src/app.ts · templates · style.css · icon · manifest + local tsc toolchain
│   └── web/              # build output (gitignored): app.js + copied assets — the go:embed target
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
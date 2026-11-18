#!/usr/bin/env bash
# Live smoke test for wakeragent and wakeserver: builds both, starts each on a
# free 127.0.0.1 port with throwaway configs, exercises the real HTTP API, and
# verifies Wake-on-LAN magic packets end to end over loopback. A fully green
# run leaves the dashboard server running (log streamed to the terminal) for
# manual UI testing; Ctrl+C / SIGINT stops it and removes the temp files.
#
# Usage: scripts/livetest.sh [all|server|agent]   (default: all)
set -euo pipefail
cd "$(dirname "$0")/.."

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
PASS=0
FAIL=0

ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1"; }

# check <description> <bash snippet> — snippet runs in a subshell with the
# exported TEST_* variables; non-zero exit means the assertion failed.
check() {
	local desc=$1 snippet=$2
	if ( bash -c "$snippet" ); then
		ok "$desc"
	else
		bad "$desc"
	fi
}

# free_port <start> — first free port at or after <start> on 127.0.0.1
free_port() {
	local port=$1
	while ( exec 3<>"/dev/tcp/127.0.0.1/$port" ) 2>/dev/null; do
		port=$((port + 1))
	done
	printf '%s' "$port"
}

# wait_http <url> <seconds> — poll until the URL answers any HTTP status
wait_http() {
	local url=$1 max=$2 i=0
	while [ "$i" -lt $((max * 10)) ]; do
		if curl -s -o /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
		i=$((i + 1))
	done
	return 1
}

# hold_and_wait <pid> <dir> <log> <url> <token> — leaves a passing server up
# for manual testing: streams its log to the terminal and blocks until
# SIGINT/SIGTERM (or the server dying) stops it and removes the temp dir.
hold_and_wait() {
	local pid=$1 dir=$2 log=$3 url=$4 token=$5

	echo
	echo "all checks passed — leaving the dashboard running for manual testing"
	echo "  url:   $url"
	echo "  token: $token"
	echo "server log follows (Ctrl+C / SIGINT stops the server and cleans up):"

	local tail_pid
	tail -n +1 -f "$log" &
	tail_pid=$!

	local done_flag=""
	cleanup_hold() {
		trap - INT TERM
		[ -n "$done_flag" ] && return
		done_flag=1
		# let the server finish shutting down first, so the streamed log
		# captures its final lines, then stop the tail and clean up
		kill -INT "$pid" 2>/dev/null || true
		local i
		for i in $(seq 1 20); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -TERM "$pid" 2>/dev/null || true
		kill "$tail_pid" 2>/dev/null || true
		rm -rf "$dir"
		echo
		echo "server stopped, temp files removed."
	}
	trap 'cleanup_hold; exit 0' INT TERM

	# Poll until the server is gone rather than using `wait`: a terminal
	# Ctrl+C hits the whole foreground group, the server's own handler shuts
	# it down, and this loop notices — which also covers parents (non-
	# interactive shells) whose background children inherit SIGINT ignored,
	# where the INT trap above cannot fire.
	while kill -0 "$pid" 2>/dev/null; do
		sleep 0.5
	done
	cleanup_hold
}

# ---------------------------------------------------------------------------
# wakeserver smoke test
# ---------------------------------------------------------------------------
test_server() {
	echo "== wakeserver =="
	local dir; dir=$(mktemp -d)
	local token="waker-test-token"
	local port; port=$(free_port 18090)
	local B="http://127.0.0.1:$port"

	# WOL capture: a listener on a free loopback UDP port validates every
	# datagram against the exact 160-byte magic packet for the test MAC.
	local wollog="$dir/wol.log"
	go run ./cmd/wolcheck -mac AA:BB:CC:DD:EE:FF -want 3 -timeout 20s >"$wollog" 2>&1 &
	local WOL_PID=$!
	local i WOLPORT=""
	for i in $(seq 1 100); do
		WOLPORT=$(sed -n 's/^listening on 127\.0\.0\.1:\([0-9]*\)$/\1/p' "$wollog" 2>/dev/null | head -n1)
		[ -n "$WOLPORT" ] && break
		sleep 0.1
	done
	if [ -z "$WOLPORT" ]; then
		echo "wolcheck did not bind; log follows:" >&2
		cat "$wollog" >&2
		kill "$WOL_PID" 2>/dev/null || true
		rm -rf "$dir"
		return 1
	fi

	# Negative control before the server starts: a junk datagram must never
	# count as a valid magic packet.
	printf 'junk' >"/dev/udp/127.0.0.1/$WOLPORT" 2>/dev/null || true

	cat >"$dir/server.yaml" <<EOF
listen: "127.0.0.1:$port"
token: $token
desktop:
  mac: "AA:BB:CC:DD:EE:FF"
  hostname: waker-test-nonexist
wake:
  target: "127.0.0.1:$WOLPORT"
  count: 3
  interval_ms: 50
agent_port: 8091
presence_interval: 5s
proxies:
  - { id: desktop-ui, target: "desktop:1234" }
links:
  - { name: "Test UI", proxy: desktop-ui }
EOF

	bin/wakeserver -config "$dir/server.yaml" >"$dir/wakeserver.log" 2>&1 &
	SRV_PID=$!
	if ! wait_http "$B/" 10; then
		echo "wakeserver did not come up; log follows:" >&2
		cat "$dir/wakeserver.log" >&2
		kill "$SRV_PID" "$WOL_PID" 2>/dev/null || true
		rm -rf "$dir"
		return 1
	fi

	export B TOKEN=$token WOLLOG="$wollog" SRVLOG="$dir/wakeserver.log"
	# This POST must be the first /api/wake for this server instance: doWake
	# de-dupes within the wake window, so only the first call fires packets.
	check "wake endpoint reports waking" \
		"curl -s -m 10 -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/wake\" | grep -q '\"ok\":true'"
	# Bounded wait for the capture listener's verdict, then assert on it.
	for i in $(seq 1 100); do
		grep -q 'received 3 of 3 valid magic packet' "$wollog" 2>/dev/null && break
		sleep 0.1
	done
	check "wake packets are delivered and valid" \
		"grep -q 'received 3 of 3 valid magic packet' \"\$WOLLOG\""
	check "junk datagram was rejected" \
		"grep -q 'packet 1: invalid' \"\$WOLLOG\""
	check "server logged the send" \
		"grep -q 'sending 3 magic packets' \"\$SRVLOG\""
	check "unauthenticated / redirects to /login" \
		"curl -s -o /dev/null -w '%{redirect_url}' \"\$B/\" | grep -q '/login'"
	check "login page renders, no <script>" \
		'curl -s "$B/login" | grep -q "<form" && ! curl -s "$B/login" | grep -q "<script"'
	check "wrong token bounces to /login?bad=1" \
		"curl -s -o /dev/null -w '%{redirect_url}' -X POST -d 'token=wrong' \"\$B/login\" | grep -q '/login?bad=1'"
	check "correct token sets the waker_auth cookie" \
		"curl -s -D - -o /dev/null -X POST -d \"token=\$TOKEN\" \"\$B/login\" | grep -i 'set-cookie' | grep -q 'waker_auth'"
	check "page wires app.js and keeps a no-JS refresh fallback" \
		"curl -s -H \"Cookie: waker_auth=\$TOKEN\" \"\$B/\" | grep -q '<script src=\"/app.js\"' && curl -s -H \"Cookie: waker_auth=\$TOKEN\" \"\$B/\" | grep -q '<noscript><meta http-equiv=\"refresh\"'"
	check "bearer header authenticates the page too" \
		"curl -s -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$TOKEN\" \"\$B/\" | grep -q 200"
	check "API status requires auth" \
		"curl -s -o /dev/null -w '%{http_code}' \"\$B/api/status\" | grep -q 401"
	check "API status with bearer lists configured links" \
		"curl -s -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/status\" | grep -q '\"proxy\":' "
	check "the waker_auth cookie authenticates /api/ too" \
		"curl -s -H \"Cookie: waker_auth=\$TOKEN\" \"\$B/api/status\" | grep -q '\"proxy\":' "
	check "service action API 503s with JSON error when offline" \
		"curl -s -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/services/demo/start\" | grep -q 503 && curl -s -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/services/demo/start\" | grep -q '\"error\"'"
	check "service action API rejects unknown actions" \
		"curl -s -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/services/demo/bogus\" | grep -q 400"
	check "model load API 503s with JSON error when offline" \
		"curl -s -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" -d '{\"model\":\"x\"}' \"\$B/api/models/load\" | grep -q 503 && curl -s -X POST -H \"Authorization: Bearer \$TOKEN\" -d '{\"model\":\"x\"}' \"\$B/api/models/load\" | grep -q '\"error\"'"
	check "model unload API 503s with JSON error when offline" \
		"curl -s -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/models/unload\" | grep -q 503 && curl -s -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/models/unload\" | grep -q '\"error\"'"
	check "desktop-bound proxy target 503s while offline" \
		"curl -s -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$TOKEN\" \"\$B/u/desktop-ui/\" | grep -q 503"
	check "unknown proxy id 404s" \
		"curl -s -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$TOKEN\" \"\$B/u/unknown/\" | grep -q 404"
	check "link proxy requires auth" \
		"curl -s -o /dev/null -w '%{http_code}' \"\$B/u/desktop-ui/\" | grep -q 401"
	check "SSE stream emits status frames" \
		"timeout 3 curl -sN -H \"Authorization: Bearer \$TOKEN\" \"\$B/api/stream\" | grep -q 'event: status'"
	check "stream requires auth" \
		"timeout 2 curl -s -o /dev/null -w '%{http_code}' -N \"\$B/api/stream\" | grep -q 401"
	check "static assets are served" \
		"curl -s -o /dev/null -w '%{http_code}' \"\$B/style.css\" | grep -q 200 && curl -s -o /dev/null -w '%{http_code}' \"\$B/manifest.webmanifest\" | grep -q 200"
	check "app.js serves as JavaScript" \
		"curl -s -o /dev/null -w '%{http_code}' \"\$B/app.js\" | grep -q 200 && curl -s -o /dev/null -w '%{content_type}' \"\$B/app.js\" | grep -q 'javascript'"

	# Hand the server off to main: with every check green it stays running
	# for manual UI testing (hold_and_wait); on any failure main cleans up.
	HOLD_PID=$SRV_PID
	HOLD_WOL=$WOL_PID
	HOLD_DIR=$dir
	HOLD_LOG="$dir/wakeserver.log"
	HOLD_URL=$B
	HOLD_TOKEN=$token
}

# ---------------------------------------------------------------------------
# wakeragent smoke test — drives the real service state machine with a
# throwaway "demo" service (backgrounded sleep + pid-file probes)
# ---------------------------------------------------------------------------
test_agent() {
	echo "== wakeragent =="
	local dir; dir=$(mktemp -d)
	local token="waker-agent-test-$$"
	local aport; aport=$(free_port 18091)
	local lmsport; lmsport=$(free_port 18092) # guaranteed free => models API 502s
	local A="http://127.0.0.1:$aport"

	cat >"$dir/agent.yaml" <<EOF
listen: "127.0.0.1:$aport"
token: $token
start_timeout: 10s
health_interval: 60s
lmstudio_port: $lmsport
services:
  - id: demo
    name: Demo
    start: ["sh", "-c", "nohup sleep 300 >/dev/null 2>&1 & echo \$! > $dir/demo.pid"]
    stop:  ["sh", "-c", "kill \"\$(cat $dir/demo.pid)\" 2>/dev/null; rm -f $dir/demo.pid"]
    status: ["sh", "-c", "test -f $dir/demo.pid && kill -0 \"\$(cat $dir/demo.pid)\" 2>/dev/null"]
EOF

	bin/wakeragent -config "$dir/agent.yaml" >"$dir/wakeragent.log" 2>&1 &
	AG_PID=$!
	if ! wait_http "$A/healthz" 10; then
		echo "wakeragent did not come up; log follows:" >&2
		cat "$dir/wakeragent.log" >&2
		kill "$AG_PID" 2>/dev/null || true
		rm -rf "$dir"
		return 1
	fi

	export A TOKEN=$token
	check "health probe answers without auth" \
		'curl -s -m 10 "$A/healthz" | grep -q "ok"'
	check "API requires auth" \
		"curl -s -m 10 -o /dev/null -w '%{http_code}' \"\$A/api/status\" | grep -q 401"
	check "service starts in the stopped state" \
		"curl -s -m 10 -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/status\" | grep -o '\"state\":\"[a-z]*\"' | head -1 | grep -q stopped"
	check "unknown service 404s" \
		"curl -s -m 30 -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/services/nope/start\" | grep -q 404"
	check "start returns ok" \
		"curl -s -m 30 -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/services/demo/start\" | grep -q '\"ok\":true'"

	# the start command finishes fast; waitUp converges to running within ~2-4 s
	local i
	for i in $(seq 1 40); do
		if curl -s -m 10 -H "Authorization: Bearer $token" "$A/api/status" | grep -q '"state":"running"'; then
			break
		fi
		sleep 0.5
	done
	check "service converges to running" \
		"curl -s -m 10 -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/status\" | grep -q '\"state\":\"running\"'"
	check "re-start while running is a no-op" \
		"curl -s -m 30 -o /dev/null -w '%{http_code}' -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/services/demo/start\" | grep -q 200"
	check "stop returns ok" \
		"curl -s -m 30 -X POST -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/services/demo/stop\" | grep -q '\"ok\":true'"
	check "service converges back to stopped" \
		"curl -s -m 10 -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/status\" | grep -q '\"state\":\"stopped\"'"
	check "models endpoint reports upstream unavailable" \
		"curl -s -m 30 -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$TOKEN\" \"\$A/api/models\" | grep -q 502"

	kill "$AG_PID" 2>/dev/null || true
	# belt and braces: kill a demo sleep that a failed stop left behind
	[ -f "$dir/demo.pid" ] && kill "$(cat "$dir/demo.pid")" 2>/dev/null || true
	rm -rf "$dir"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
case "${1:-all}" in
all | server | agent)
	;;
*)
	echo "usage: $0 [all|server|agent]" >&2
	exit 2
	;;
esac
TARGET=${1:-all}

scripts/build.sh

case "$TARGET" in
all | server) test_server ;;
esac
case "$TARGET" in
all | agent) test_agent ;;
esac

echo
echo "== $PASS passed, $FAIL failed"

if [ "$FAIL" -ne 0 ]; then
	# failure path: never leave a failing server behind
	if [ -n "${HOLD_PID:-}" ]; then
		kill "$HOLD_PID" 2>/dev/null || true
		[ -n "${HOLD_WOL:-}" ] && kill "$HOLD_WOL" 2>/dev/null || true
		rm -rf "$HOLD_DIR" 2>/dev/null
	fi
	exit 1
fi

if [ -n "${HOLD_PID:-}" ]; then
	hold_and_wait "$HOLD_PID" "$HOLD_DIR" "$HOLD_LOG" "$HOLD_URL" "$HOLD_TOKEN"
fi
exit 0

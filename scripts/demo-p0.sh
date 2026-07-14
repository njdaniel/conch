#!/usr/bin/env bash
# demo-p0.sh — the P0 exit-gate walkthrough (issue #7).
#
# Proves the ROADMAP P0 success shape end-to-end: conchd serves from a temp
# dir, a curl-driven fake agent provisions a channel + principal and posts
# messages over REST, and `conch tail` streams them live over WebSocket.
#
# Run from a fresh clone (needs only Go and curl):
#   ./scripts/demo-p0.sh
# Exits 0 with a summary (including per-message post→tail latency) on
# success, nonzero at the first failure.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

MESSAGES=${MESSAGES:-10}
if ! [[ "$MESSAGES" =~ ^[0-9]+$ ]] || [ "$MESSAGES" -le 0 ] || [ "$MESSAGES" -gt 100 ]; then
    echo "demo-p0: FAIL — MESSAGES must be an integer between 1 and 100 (server limit); got ${MESSAGES}" >&2
    exit 1
fi
WORK=$(mktemp -d)
SERVER_PID=""
TAIL_PID=""

cleanup() {
    [ -n "$TAIL_PID" ] && kill "$TAIL_PID" 2>/dev/null || true
    [ -n "$SERVER_PID" ] && kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
    echo "demo-p0: FAIL — $*" >&2
    exit 1
}

step() {
    echo "demo-p0: $*"
}

step "building conchd and conch"
go build -o "$WORK/conchd" ./cmd/conchd
go build -o "$WORK/conch" ./cmd/conch

# Portable high-resolution clock for latency measurement (avoids GNU date %N).
cat >"$WORK/now_ns.go" <<'EOF'
package main
import ("fmt"; "time")
func main() { fmt.Print(time.Now().UnixNano()) }
EOF
go build -o "$WORK/now_ns" "$WORK/now_ns.go"

step "starting conchd against $WORK/data"
"$WORK/conchd" serve --data "$WORK/data" --listen 127.0.0.1:0 > "$WORK/conchd.log" 2>&1 &
SERVER_PID=$!

# conchd binds :0 and prints the assigned address; wait for it.
ADDR=""
for _ in $(seq 1 50); do
    ADDR=$(sed -n 's/^conchd .* listening on \([0-9.:]*\) .*/\1/p' "$WORK/conchd.log")
    [ -n "$ADDR" ] && break
    kill -0 "$SERVER_PID" 2>/dev/null || fail "conchd exited early: $(cat "$WORK/conchd.log")"
    sleep 0.1
done
[ -n "$ADDR" ] || fail "conchd never reported its address"
BASE="http://$ADDR"
step "conchd is up at $BASE"

# The fake agent is curl: provision a channel and an agent principal via the
# public REST API alone.
step "fake agent (curl): creating channel 'general' and principal 'fake-agent'"
curl -fsS -X POST "$BASE/v0/channels" -H "Content-Type: application/json" -d '{"name":"general"}' > /dev/null \
    || fail "create channel"
AUTHOR_ID=$(curl -fsS -X POST "$BASE/v0/principals" -H "Content-Type: application/json" -d '{"kind":"agent","name":"fake-agent"}' \
    | sed -n 's/.*"id": *\([0-9][0-9]*\).*/\1/p')
[ -n "$AUTHOR_ID" ] || fail "create principal returned no id"

step "starting conch tail on 'general'"
CONCH_SERVER="$BASE" "$WORK/conch" tail general > "$WORK/tail.out" 2> "$WORK/tail.err" &
TAIL_PID=$!
# Give the WS subscription a moment to establish before the first post.
sleep 0.5
kill -0 "$TAIL_PID" 2>/dev/null || fail "conch tail exited early: $(cat "$WORK/tail.err")"

step "fake agent (curl): posting $MESSAGES messages while tail streams"
total_latency_ms=0
max_latency_ms=0
for i in $(seq 1 "$MESSAGES"); do
    t0=$("$WORK/now_ns")
    curl -fsS -X POST "$BASE/v0/channels/general/messages" \
        -H "Content-Type: application/json" \
        -d "{\"author_id\":$AUTHOR_ID,\"body\":\"demo message $i\"}" > /dev/null \
        || fail "post message $i"
    # Latency = post issued -> line visible in tail's stdout.
    for _ in $(seq 1 100); do
        grep -q "demo message $i\$" "$WORK/tail.out" && break
        sleep 0.01
    done
    grep -q "demo message $i\$" "$WORK/tail.out" || fail "message $i never reached tail"
    ms=$(( ($("$WORK/now_ns") - t0) / 1000000 ))
    total_latency_ms=$((total_latency_ms + ms))
    [ "$ms" -gt "$max_latency_ms" ] && max_latency_ms=$ms
done

lines=$(wc -l < "$WORK/tail.out")
[ "$lines" -eq "$MESSAGES" ] || fail "tail shows $lines lines, want $MESSAGES"

step "verifying messages persisted (REST read-back)"
count=$(curl -fsS "$BASE/v0/channels/general/messages?limit=100" | grep -o '"id"' | wc -l)
[ "$count" -eq "$MESSAGES" ] || fail "REST read-back shows $count messages, want $MESSAGES"

step "shutting down conchd (tail should exit cleanly)"
kill -TERM "$SERVER_PID"
wait "$SERVER_PID" || fail "conchd exited nonzero"
SERVER_PID=""
wait "$TAIL_PID" || fail "conch tail exited nonzero on server shutdown: $(cat "$WORK/tail.err")"
TAIL_PID=""

echo
echo "demo-p0: OK — $MESSAGES messages posted by the curl fake agent, all streamed live by conch tail"
echo "demo-p0: post->tail latency (curl overhead included): avg $((total_latency_ms / MESSAGES))ms, max ${max_latency_ms}ms"

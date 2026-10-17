#!/usr/bin/env bash
# dev-stack.sh — the whole stack on this machine, for development and for
# trying things by hand (issue #517): conchd serving the web client, and the
# pinned LiveKit in Docker for voice and screen share, with an operator and a
# channel to join.
#
# LiveKit is limited to 2 CPUs and 4 GB by default, the alpha droplet's size
# (docs/design/private-alpha.md, O7), so what is seen here compares with it.
# Everything listens on 127.0.0.1 except LiveKit's media ports, which listen
# on every interface (README, "Run LiveKit beside conchd"). State lives
# outside the checkout, in ${XDG_CACHE_HOME:-~/.cache}/conch-dev. The script
# stops only what it started: conchd by the PID it recorded, and the
# container by its own name.
set -euo pipefail

usage() {
    cat <<'EOF'
usage: scripts/dev-stack.sh <command>

  up                   build what is missing, start LiveKit and conchd, make
                       the operator and the channel, and say what to open
  token                print the operator credential (to paste into /app/)
  invite               print a personal invite link (voice, 2 hours) for a
                       second person: open it in a private window or another
                       browser
  viewers [N] [opts]   N simulated participants (default 29) for 10 minutes,
                       through scripts/alpha-loadtest.sh, on CPUs LiveKit is
                       not given; opts go to it, e.g. --fake-sharer or
                       --duration 5m
  stats                LiveKit's and conchd's CPU and memory, and who is in
                       voice
  down [--wipe]        stop conchd and LiveKit; --wipe also deletes the state

environment (defaults in brackets):
  CONCH_DEV_STATE      state directory [${XDG_CACHE_HOME:-~/.cache}/conch-dev]
  CONCH_DEV_PORT       conchd's port on 127.0.0.1 [28080]
  CONCH_DEV_LK_PORT    LiveKit's signalling port; media TCP is the next one
                       [27880]
  CONCH_DEV_UDP        first of LiveKit's 100 media UDP ports, above the
                       ephemeral range [61100]
  CONCH_DEV_CPUS       LiveKit's CPUs, a comma-separated list [0,1]
  CONCH_DEV_MEMORY     LiveKit's memory limit [4g]
  CONCH_DEV_CHANNEL    the channel made for you [general]

needs: go, docker, curl, openssl, and the Node in web/.nvmrc for the first
build of the web client. viewers also needs taskset and the network (it
downloads the pinned lk).
EOF
}

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
state=${CONCH_DEV_STATE:-${XDG_CACHE_HOME:-$HOME/.cache}/conch-dev}
port=${CONCH_DEV_PORT:-28080}
lk_port=${CONCH_DEV_LK_PORT:-27880}
lk_tcp=$((lk_port + 1))
udp=${CONCH_DEV_UDP:-61100}
cpus=${CONCH_DEV_CPUS:-0,1}
memory=${CONCH_DEV_MEMORY:-4g}
channel=${CONCH_DEV_CHANNEL:-general}
container=conch-dev-livekit
server=http://127.0.0.1:$port

say() { echo "dev-stack: $*"; }
fail() { echo "dev-stack: FAIL: $*" >&2; exit 1; }

# The image e2e/voice and e2e/isolation use, pinned by digest in one place.
image() {
    local img
    img=$(sed -n 's/^[[:space:]]*Image = "\(livekit\/livekit-server:[^"]*@sha256:[0-9a-f]\{64\}\)"$/\1/p' "$root/e2e/internal/lkit/kit.go")
    [ -n "$img" ] || fail "could not read the pinned LiveKit image from e2e/internal/lkit/kit.go"
    echo "$img"
}

conchd_running() {
    [ -f "$state/conchd.pid" ] && kill -0 "$(cat "$state/conchd.pid")" 2>/dev/null
}

conch() {
    CONCH_TOKEN=$(cat "$state/operator.token") "$root/bin/conch" "$@" --server "$server"
}

require_up() {
    conchd_running || fail "not running; start it with: $0 up"
}

build() {
    if [ ! -x "$root/bin/conchd" ] || [ ! -x "$root/bin/conch" ]; then
        say "building conchd and conch"
        make -C "$root" build >/dev/null
    fi
    if [ ! -f "$root/web/dist/index.html" ]; then
        say "building the web client (make web-build; needs the Node in web/.nvmrc)"
        make -C "$root" web-build >/dev/null || fail "make web-build failed; install the Node in web/.nvmrc (nvm install in web/)"
    fi
}

start_livekit() {
    if [ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null)" = false ]; then
        docker rm "$container" >/dev/null # ours, stopped
    fi
    if ! docker inspect "$container" >/dev/null 2>&1; then
        if ss -ltnH "( sport = :$lk_port or sport = :$lk_tcp )" | grep -q .; then
            fail "port $lk_port or $lk_tcp is in use; set CONCH_DEV_LK_PORT"
        fi
        [ -f "$state/livekit.secret" ] || openssl rand -hex 32 > "$state/livekit.secret"
        cat > "$state/livekit.yaml" <<EOF
port: $lk_port
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: $lk_tcp
  port_range_start: $udp
  port_range_end: $((udp + 99))
  use_external_ip: false
room:
  auto_create: false
EOF
        chmod 0644 "$state/livekit.yaml" # the container's user reads it
        say "starting LiveKit (CPUs $cpus, $memory)"
        # The secret reaches docker by name, never on a command line.
        LIVEKIT_KEYS="conch-dev: $(cat "$state/livekit.secret")" \
            docker run -d --name "$container" --network host \
            --cpuset-cpus "$cpus" --memory "$memory" \
            -e LIVEKIT_KEYS -v "$state/livekit.yaml:/etc/livekit.yaml:ro" \
            "$(image)" --config /etc/livekit.yaml --node-ip 127.0.0.1 >/dev/null
    fi
    for _ in $(seq 50); do
        curl -fsS "http://127.0.0.1:$lk_port/" >/dev/null 2>&1 && return
        sleep 0.2
    done
    docker logs --tail 5 "$container" >&2 || true
    fail "LiveKit did not start (its last lines are above)"
}

start_conchd() {
    if [ ! -d "$state/data" ]; then
        mkdir "$state/data"
        "$root/bin/conchd" bootstrap-operator --data "$state/data" --name operator > "$state/operator.token"
    fi
    if ! conchd_running; then
        if ss -ltnH "( sport = :$port )" | grep -q .; then
            fail "port $port is in use; set CONCH_DEV_PORT"
        fi
        say "starting conchd on $server"
        CONCHD_LIVEKIT_API_KEY=conch-dev CONCHD_LIVEKIT_API_SECRET=$(cat "$state/livekit.secret") \
            nohup "$root/bin/conchd" serve --data "$state/data" --listen "127.0.0.1:$port" \
            --livekit-url "ws://127.0.0.1:$lk_port" --web-dir "$root/web/dist" \
            > "$state/conchd.log" 2>&1 &
        echo $! > "$state/conchd.pid"
    fi
    for _ in $(seq 50); do
        curl -fsS "$server/healthz" >/dev/null 2>&1 && return
        sleep 0.2
    done
    fail "conchd did not start; see $state/conchd.log"
}

up() {
    command -v docker >/dev/null || fail "docker is not installed"
    umask 077
    mkdir -p "$state"
    build
    start_livekit
    start_conchd
    # Both are no-ops after the first run, and say so; neither is an error.
    conch admin channel create "$channel" >/dev/null 2>&1 || true
    conch admin member add "$channel" 1 >/dev/null 2>&1 || true
    cat <<EOF

  Up. Open $server/app/ in Chrome and sign in with the operator credential:
      $0 token
  (an operator stays signed in for that one tab only).

  A second person:  $0 invite   (open the link in a private window)
  Then open "$channel", join voice, and share a screen from one to the other.

  Load:   $0 viewers 29 [--fake-sharer]
  Watch:  $0 stats
  Stop:   $0 down [--wipe]
  Logs:   $state/conchd.log, docker logs $container

EOF
}

token() {
    [ -f "$state/operator.token" ] || fail "no operator yet; run: $0 up"
    cat "$state/operator.token"
}

invite() {
    require_up
    conch admin invite create --personal --channel "$channel" --expires 2h --voice
}

# Every CPU except LiveKit's, for the load generator.
other_cpus() {
    local n i list=""
    n=$(nproc --all)
    for ((i = 0; i < n; i++)); do
        case ",$cpus," in *",$i,"*) continue ;; esac
        list+="${list:+,}$i"
    done
    [ -n "$list" ] || fail "no CPU is left for the load generator (CONCH_DEV_CPUS=$cpus)"
    echo "$list"
}

viewers() {
    require_up
    local n=29
    if [ $# -gt 0 ] && [[ $1 =~ ^[0-9]+$ ]]; then n=$1; shift; fi
    say "adding $n simulated participants on CPUs $(other_cpus) (Ctrl-C ends early and still cleans up)"
    taskset -c "$(other_cpus)" "$root/scripts/alpha-loadtest.sh" --server "$server" \
        --token-file "$state/operator.token" --channel "$channel" --viewers "$n" --duration 10m "$@"
}

stats() {
    docker stats --no-stream --format "LiveKit: CPU {{.CPUPerc}} (100% is one core)   memory {{.MemUsage}}" "$container"
    if conchd_running; then
        ps -o '%cpu=,rss=' -p "$(cat "$state/conchd.pid")" |
            awk '{printf "conchd:  CPU %s%%   memory %.0f MiB\n", $1, $2 / 1024}'
        conch voice status "$channel" || true
    fi
}

down() {
    if conchd_running; then
        kill "$(cat "$state/conchd.pid")"
        say "conchd stopped"
    fi
    rm -f "$state/conchd.pid"
    if docker inspect "$container" >/dev/null 2>&1; then
        docker rm -f "$container" >/dev/null
        say "LiveKit stopped"
    fi
    if [ "${1:-}" = --wipe ]; then
        rm -rf "$state"
        say "state deleted"
    fi
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case $cmd in
    up) up ;;
    token) token ;;
    invite) invite ;;
    viewers) viewers "$@" ;;
    stats) stats ;;
    down) down "$@" ;;
    -h | --help | help) usage ;;
    *) usage >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# alpha-loadtest.sh — simulated viewers for the screen-share bandwidth runs
# (issue #448, docs/reports/TEMPLATE-alpha-bandwidth.md).
#
# It loads one channel's voice room on a running conchd with `lk load-test`
# participants, each admitted on a voice session conchd issued to a principal
# of its own:
#
#   1. builds the voice check (e2e/voice), whose loadtest subcommand speaks
#      to conchd, and downloads the pinned lk with it (checked by SHA-256);
#   2. with an operator credential, makes --viewers human principals named
#      <prefix>-v001..., each with a credential that expires, and adds them
#      to --channel;
#   3. runs lk load-test through a local relay that swaps the token lk signs
#      itself for one from POST /v1/channels/{channel}/voice/session, asked
#      for as that participant's principal when it connects. Only signalling
#      passes through the relay; media flows between lk and LiveKit;
#   4. when lk is done, hides every principal named <prefix>- with the bulk
#      hide by name prefix, and checks that none is left. Ctrl-C ends the run
#      early: lk prints its summary of the time so far, and the clean-up
#      still runs.
#
# The operator credential is read from --token-file or CONCH_LOADTEST_TOKEN,
# never from the command line, and is never printed. Run --help for the
# options; it needs no network.
set -euo pipefail

usage() {
    cat <<'EOF'
usage: scripts/alpha-loadtest.sh [options] [-- lk load-test options...]

Loads a channel's voice room on a running conchd with simulated participants
from `lk load-test`, each on a conchd voice session of its own, and hides the
principals it made afterwards.

options (each also read from the environment variable named):
  --server URL        conchd's base URL, e.g. https://conch.example.org
                      (CONCH_LOADTEST_SERVER)
  --token-file PATH   file holding an operator credential
                      (CONCH_LOADTEST_TOKEN_FILE; or put the credential itself
                      in CONCH_LOADTEST_TOKEN). Never given on the command line.
  --channel NAME      the channel whose voice room is loaded; it must exist
                      (CONCH_LOADTEST_CHANNEL)
  --viewers N         simulated participants, one principal each; default 29
                      (CONCH_LOADTEST_VIEWERS)
  --publishers N      how many of them publish a microphone track; 0, the
                      default, is subscribe-only (CONCH_LOADTEST_PUBLISHERS)
  --duration D        how long lk load-test runs: 30s, 5m; default 5m
                      (CONCH_LOADTEST_DURATION)
  --prefix P          name prefix of the principals made; must start with
                      "loadtest-"; default loadtest-<UTC time>-<random>
                      (CONCH_LOADTEST_PREFIX)
  --fake-sharer       also make one more member that shares lk's demo video as
                      a screen, with silent sound: a stand-in for a browser,
                      for a rehearsal. The measured runs share from a browser.
  --cleanup-only      only hide what an earlier run with --prefix left behind
  -h, --help          this text

Anything after -- is passed to lk load-test, e.g. -- --layout 3x3.

needs: go (builds the helper from this checkout) and the network, to
download the pinned lk. The script runs on the load generator, not on the
server it loads.
EOF
}

fail() { echo "alpha-loadtest: FAIL: $*" >&2; exit 1; }
say() { echo "alpha-loadtest: $*"; }

server=${CONCH_LOADTEST_SERVER:-}
token_file=${CONCH_LOADTEST_TOKEN_FILE:-}
channel=${CONCH_LOADTEST_CHANNEL:-}
viewers=${CONCH_LOADTEST_VIEWERS:-29}
publishers=${CONCH_LOADTEST_PUBLISHERS:-0}
duration=${CONCH_LOADTEST_DURATION:-5m}
prefix=${CONCH_LOADTEST_PREFIX:-}
fake_sharer=0
cleanup_only=0
lk_extra=()

# The credential, if it came in the environment, is kept in this shell only
# and handed to the helper alone: nothing else this script starts inherits it.
token=${CONCH_LOADTEST_TOKEN:-}
unset CONCH_LOADTEST_TOKEN

need_value() { if [ "$#" -lt 2 ] || [ -z "$2" ]; then fail "$1 needs a value (see --help)"; fi; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --server) need_value "$@"; server=$2; shift 2 ;;
        --token-file) need_value "$@"; token_file=$2; shift 2 ;;
        --channel) need_value "$@"; channel=$2; shift 2 ;;
        --viewers) need_value "$@"; viewers=$2; shift 2 ;;
        --publishers) need_value "$@"; publishers=$2; shift 2 ;;
        --duration) need_value "$@"; duration=$2; shift 2 ;;
        --prefix) need_value "$@"; prefix=$2; shift 2 ;;
        --fake-sharer) fake_sharer=1; shift ;;
        --cleanup-only) cleanup_only=1; shift ;;
        --token|--token=*) fail "the credential is never an argument: use --token-file or CONCH_LOADTEST_TOKEN" ;;
        --) shift; lk_extra=("$@"); break ;;
        *) fail "unknown option $1 (see --help)" ;;
    esac
done

[ -n "$server" ] || fail "--server is required"
if [ -n "$token_file" ]; then
    [ -f "$token_file" ] || fail "--token-file: no such file"
    # GNU stat, then BSD (macOS) stat.
    case "$(stat -c %a "$token_file" 2>/dev/null || stat -f %Lp "$token_file")" in
        ?00) ;;
        *) say "warning: the token file is readable by other users; chmod 600 it" ;;
    esac
    token=$(tr -d '[:space:]' <"$token_file")
fi
[ -n "$token" ] || fail "no operator credential: use --token-file or CONCH_LOADTEST_TOKEN"
if [ -z "$prefix" ]; then
    prefix="loadtest-$(date -u +%Y%m%d%H%M)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
fi
[[ $prefix =~ ^loadtest-[a-z0-9][a-z0-9-]{0,39}$ ]] || fail "--prefix must be loadtest- then up to 40 of a-z, 0-9 and -"
if [ "$cleanup_only" = 0 ]; then
    [ -n "$channel" ] || fail "--channel is required"
    if ! [[ $viewers =~ ^[1-9][0-9]*$ ]]; then fail "--viewers must be a positive whole number"; fi
    if ! [[ $publishers =~ ^[0-9]+$ ]] || [ "$publishers" -gt "$viewers" ]; then
        fail "--publishers must be a whole number no larger than --viewers"
    fi
    # A Go duration; lk reads 0 as "run until stopped", which a measurement
    # must not do by accident.
    if ! [[ $duration =~ ^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$ ]] || [[ $duration =~ ^0+(\.0+)?(ms|s|m|h)$ ]]; then
        fail "--duration must be a positive duration such as 30s or 5m"
    fi
fi
command -v go >/dev/null || fail "go is required: the helper is built from this checkout"

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
helper_pid=
cleanup() {
    if [ -n "$helper_pid" ] && kill -0 "$helper_pid" 2>/dev/null; then
        kill -TERM "$helper_pid" 2>/dev/null || true
        wait "$helper_pid" 2>/dev/null || true
    fi
    rm -rf "${work:?}"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

say "building the helper (go build ./e2e/voice)"
(cd "$repo" && go build -o "$work/voice-check" ./e2e/voice) || fail "go build ./e2e/voice"

helper_args=(-server "$server" -prefix "$prefix")
if [ "$cleanup_only" = 1 ]; then
    CONCH_LOADTEST_TOKEN=$token "$work/voice-check" loadtest "${helper_args[@]}" -cleanup-only || fail "the clean-up did not finish"
    exit 0
fi

mkdir -m 700 "$work/lk" "$work/lkhome"
say "downloading the pinned lk"
lk=$("$work/voice-check" fetch-lk "$work/lk") || fail "could not download and verify the pinned lk"

helper_args+=(-channel "$channel" -viewers "$viewers" -ready "$work/ready")
if [ "$fake_sharer" = 1 ]; then helper_args+=(-fake-sharer -lk "$lk"); fi
# It runs while lk does, and ignores Ctrl-C: it ends, and hides the
# principals, on the SIGTERM sent below or by the EXIT trap.
CONCH_LOADTEST_TOKEN=$token "$work/voice-check" loadtest "${helper_args[@]}" &
helper_pid=$!
token=

for _ in $(seq 1 600); do
    [ -s "$work/ready" ] && break
    kill -0 "$helper_pid" 2>/dev/null || { wait "$helper_pid" || true; helper_pid=; fail "the helper stopped before the relay was up"; }
    sleep 0.2
done
[ -s "$work/ready" ] || fail "the relay was not up within 120 s"
relay=$(head -n 1 "$work/ready")

subscribers=$((viewers - publishers))
say "lk load-test: $subscribers subscribe-only, $publishers publishing a microphone, for $duration"
# lk signs its own tokens with a throwaway key pair LiveKit has never heard
# of; the relay replaces each. lk runs with an empty home and working
# directory of its own, so no livekit.toml or lk project of the caller's
# applies, and nothing in its environment but PATH. Anything shaped like a
# JWT in its output is masked, in case one is ever printed.
lk_status=0
(
    cd "$work/lkhome"
    env -i PATH="$PATH" HOME="$work/lkhome" TMPDIR="$work/lkhome" \
        "$lk" --url "$relay" --api-key relay-only-key --api-secret relay-only-secret-not-known-to-livekit-0000000000 \
        load-test --room relay-placeholder-room --duration "$duration" \
        --subscribers "$subscribers" --audio-publishers "$publishers" "${lk_extra[@]}"
) 2>&1 | sed -E 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/[jwt redacted]/g' || lk_status=$?

say "lk load-test is done; cleaning up"
kill -TERM "$helper_pid" 2>/dev/null || true
helper_status=0
wait "$helper_pid" || helper_status=$?
helper_pid=
[ "$helper_status" = 0 ] || fail "the helper reported a failure (exit $helper_status); if the principals were not hidden, run again with --prefix $prefix --cleanup-only"
[ "$lk_status" = 0 ] || fail "lk load-test exited with status $lk_status"
say "done: prefix $prefix, all hidden"

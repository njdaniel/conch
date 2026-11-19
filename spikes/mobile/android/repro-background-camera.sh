#!/usr/bin/env bash
# Emulator repro for #1972: with the camera on, send the app HOME for 40 s,
# bring it back, and check that the race was set up and that the phone's
# LiveKit connections never overlapped or displaced one another, and that the
# app, LiveKit and `conch voice status` agree at the end.
#
# Needs: the dev stack up (scripts/dev-stack.sh up), the debug APK installed
# with Metro running (or the release APK on an http-allowing build), adb
# reverse for conchd, LiveKit (signalling and TCP media) and Metro, the app
# signed in and on the channel's Voice view, and someone else in the voice
# room. docker (LiveKit's log, for each connection's end and its reason) and
# python3 (the UI driver over uiautomator dump, the LiveKit admin call, the
# verdict). An emulator pass is not device evidence.
#
# Each run, LiveKit's participant SIDs for the phone are sampled every 3 s and
# LiveKit's log is read for when each of them started and why each ended. A
# run passes only when all of these hold:
#  - the race was set up: on return, LiveKit saw the phone's connection from
#    before HOME end and at least two new ones start (the SDK's restart, then
#    the app's rejoin once the Room reported the disconnect). Otherwise the
#    run says NO-RACE and fails: it showed nothing about the fix.
#  - no sample has two connections for the phone at once;
#  - no connection of the phone's started after the one it settled on (the
#    dropped engine rejoining late), and the one it settled on did not end;
#  - the settled samples (the last third, at least 3) all show that one
#    connection, `conch voice status` listing the phone and the app "In voice".
# The script turns the camera off and leaves voice on exit (an interrupt
# included) if it was the one that turned them on.
#
# usage: repro-background-camera.sh RUNS
# environment (defaults in brackets):
#   ANDROID_SERIAL   the device [emulator-5554]
#   REPRO_PRINCIPAL  the phone's principal id; LiveKit identity p<id> (required)
#   REPRO_CHANNEL    the channel [general]
#   REPRO_AWAY       seconds away [40]
#   REPRO_WATCH      seconds watched after the return [45], at least 18 (six
#                    samples); the displacement seen in #1972 came about 17 s
#                    after it
#   REPRO_LK_CONTAINER  LiveKit's container [conch-dev-livekit]
#   CONCH_DEV_STATE  the dev stack's state [~/.cache/conch-dev]
#   CONCH_DEV_PORT, CONCH_DEV_LK_PORT  [28080, 27880]
set -euo pipefail

runs=${1:-1}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
export ANDROID_SERIAL=${ANDROID_SERIAL:-emulator-5554}
principal=${REPRO_PRINCIPAL:?REPRO_PRINCIPAL is the phone principal id}
channel=${REPRO_CHANNEL:-general}
away=${REPRO_AWAY:-40}
watch=${REPRO_WATCH:-45}
container=${REPRO_LK_CONTAINER:-conch-dev-livekit}
state=${CONCH_DEV_STATE:-${XDG_CACHE_HOME:-$HOME/.cache}/conch-dev}
server=http://127.0.0.1:${CONCH_DEV_PORT:-28080}
lk_port=${CONCH_DEV_LK_PORT:-27880}
if ((watch < 18)); then
    echo "REPRO_WATCH=$watch: at least 18 s (six samples) are needed for a verdict" >&2
    exit 2
fi

# The UI dump's file on the device, this run's own; removed on exit.
dump=/sdcard/repro1972-$$.xml
work=$(mktemp -d)
joined_here=0 camera_here=0

# ui LABEL... : the first on-screen text or content-desc among the labels
# ("" if none); ui tap LABEL taps it. The dump is taken afresh each time: the
# file is removed first, and a dump that fails or does not parse is retried,
# never read from an earlier screen.
ui() {
    python3 -I - "$dump" "$@" <<'EOF'
import re, subprocess, sys, time, xml.etree.ElementTree as ET
dump = sys.argv[1]
args = sys.argv[2:]
root = None
for _ in range(5):
    subprocess.run(['adb', 'shell', 'rm', '-f', dump], capture_output=True)
    made = subprocess.run(['adb', 'shell', 'uiautomator', 'dump', dump], capture_output=True, text=True)
    if made.returncode == 0 and 'dumped to' in made.stdout:
        out = subprocess.run(['adb', 'exec-out', 'cat', dump], capture_output=True).stdout
        try:
            root = ET.fromstring(out)
            break
        except ET.ParseError:
            pass
    time.sleep(0.5)
subprocess.run(['adb', 'shell', 'rm', '-f', dump], capture_output=True)
if root is None:
    sys.exit('ui: no dump')
tap = args[0] == 'tap'
labels = args[1:] if tap else args
for n in root.iter('node'):
    t = n.get('content-desc') or n.get('text') or ''
    if t in labels:
        if tap:
            a, b, c, d = map(int, re.findall(r'\d+', n.get('bounds')))
            subprocess.run(['adb', 'shell', 'input', 'tap', str((a + c) // 2), str((b + d) // 2)], check=True)
        print(t)
        sys.exit(0)
sys.exit(1 if tap else 0)
EOF
}

# sids: the SIDs of the LiveKit participants with the phone's identity, over
# every room, space-separated ("-" if none); an admin token signed with the
# dev stack's secret.
sids() {
    python3 -I - "$state/livekit.secret" "$lk_port" "p$principal" <<'EOF'
import base64, hashlib, hmac, json, sys, time, urllib.request
secret, port, identity = open(sys.argv[1]).read().strip(), sys.argv[2], sys.argv[3]
def b64(b): return base64.urlsafe_b64encode(b).rstrip(b'=').decode()
def call(method, payload, room=None):
    now = int(time.time())
    video = {'roomList': True, 'roomAdmin': True, **({'room': room} if room else {})}
    head = b64(b'{"alg":"HS256","typ":"JWT"}')
    body = b64(json.dumps({'iss': 'conch-dev', 'sub': 'repro', 'nbf': now - 5, 'exp': now + 60, 'video': video}).encode())
    sig = b64(hmac.new(secret.encode(), f'{head}.{body}'.encode(), hashlib.sha256).digest())
    req = urllib.request.Request(f'http://127.0.0.1:{port}/twirp/livekit.RoomService/{method}', data=json.dumps(payload).encode(),
                                 headers={'Content-Type': 'application/json', 'Authorization': f'Bearer {head}.{body}.{sig}'})
    return json.load(urllib.request.urlopen(req, timeout=5))
found = []
for r in call('ListRooms', {}).get('rooms', []):
    found += [p['sid'] for p in call('ListParticipants', {'room': r['name']}, r['name']).get('participants', []) if p['identity'] == identity]
print(' '.join(found) or '-')
EOF
}

# listed: 1 if `conch voice status` lists the phone's principal, else 0.
listed() {
    CONCH_TOKEN=$(cat "$state/operator.token") "$root/bin/conch" voice status "$channel" --server "$server" |
        awk -v id="$principal" '$1 == id { found = 1 } END { print found ? 1 : 0 }'
}

app() { ui 'In voice' 'Not in voice' 'Reconnecting' 'Joining' 'Checking'; }

wait_for() { # wait_for SECONDS LABEL
    local i
    for ((i = 0; i < $1; i++)); do
        [ -n "$(ui "$2")" ] && return 0
        sleep 1
    done
    return 1
}

# cleanup puts back what the script turned on: the camera, then voice. The
# app is brought to the front first (an interrupt may come while it is away).
cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    if ((camera_here || joined_here)); then
        adb shell am start -n com.conchspike/.MainActivity >/dev/null 2>&1 || true
        sleep 2
        if ((camera_here)) && [ -n "$(ui 'Turn camera off' 2>/dev/null)" ]; then
            ui tap 'Turn camera off' >/dev/null 2>&1 || true
            echo "cleanup: camera turned off" >&2
        fi
        if ((joined_here)) && [ -n "$(ui 'Leave voice' 2>/dev/null)" ]; then
            ui tap 'Leave voice' >/dev/null 2>&1 || true
            echo "cleanup: left voice" >&2
        fi
    fi
    adb shell rm -f "$dump" >/dev/null 2>&1 || true
    rm -rf "$work"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass=0
for ((run = 1; run <= runs; run++)); do
    # Into voice with the camera on.
    if [ "$(app)" = 'Not in voice' ]; then
        ui tap 'Join voice' >/dev/null
        joined_here=1
        wait_for 20 'In voice' || { echo "run $run: FAIL could not join"; continue; }
    fi
    if [ -n "$(ui 'Turn camera on')" ]; then
        ui tap 'Turn camera on' >/dev/null
        camera_here=1
    fi
    wait_for 15 'Your camera is being sent.' || { echo "run $run: FAIL camera did not start"; continue; }
    since=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    before=$(sids)

    adb shell input keyevent HOME
    sleep "$away"
    back=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    adb shell am start -n com.conchspike/.MainActivity >/dev/null 2>&1

    # Sampled every 3 s: the phone's SIDs, conch's list and the app's words.
    : >"$work/samples"
    t0=$SECONDS
    while ((SECONDS - t0 < watch)); do
        sleep 3
        printf '%s\t%s\t%s\t%s\n' "$((SECONDS - t0))" "$(sids)" "$(listed)" "$(app)" >>"$work/samples"
    done
    docker logs --since "$since" "$container" >"$work/lk.log" 2>&1

    # The verdict, and the run's row of the table.
    if python3 -I - "$run" "p$principal" "$before" "$back" "$work/samples" "$work/lk.log" <<'EOF'; then
import json, re, sys
run, identity, before, back, samples_path, log_path = sys.argv[1:]
before = [] if before == '-' else before.split()
samples = []
for line in open(samples_path):
    t, s, listed, app = line.rstrip('\n').split('\t')
    samples.append((int(t), [] if s == '-' else s.split(), listed == '1', app))
# LiveKit's log: when each of the phone's connections started, and why each
# ended ("participant closing", with its reason).
started, ended = {}, {}
for line in open(log_path, errors='replace'):
    m = re.match(r'(\S+)\s+\S+\s+\S+\s+\S+\s+(starting RTC session|participant closing)\s+(\{.*\})\s*$', line)
    if not m:
        continue
    try:
        f = json.loads(m.group(3))
    except ValueError:
        continue
    if f.get('participant') != identity:
        continue
    sid = f.get('participantID')
    if m.group(2) == 'starting RTC session':
        started.setdefault(sid, m.group(1))
    else:
        ended.setdefault(sid, f.get('reason', '?'))
order = sorted(started, key=lambda s: started[s])
fresh = [s for s in order if started[s] >= back]
why = []
race = len(before) == 1 and before[0] in ended and len(fresh) >= 2
if not race:
    why.append(f'NO-RACE: before={before} ended={ {s: ended.get(s) for s in before} } new after return={len(fresh)}')
if any(len(s) > 1 for _, s, _, _ in samples):
    why.append('two connections at once')
settled = samples[-max(3, len(samples) // 3):]
if len(samples) < 6:
    why.append(f'{len(samples)} samples')
final = settled[-1][1] if settled else []
if len(final) != 1:
    why.append(f'settled on {final}')
else:
    f = final[0]
    if any(s != [f] or not l or a != 'In voice' for _, s, l, a in settled):
        why.append('settled samples disagree')
    if f in ended:
        why.append(f'the settled connection ended: {ended[f]}')
    if f in started and any(started[s] > started[f] for s in order):
        why.append('a connection started after the settled one (a late rejoin)')
dup = [s for s in fresh if ended.get(s) == 'DUPLICATE_IDENTITY']
verdict = 'PASS' if not why else 'FAIL'
chain = ' -> '.join(f'{s[-6:]}({ended.get(s, "up")})' for s in order)
print(f'run {run}: {verdict} | connections {chain} | displaced after return: {len(dup)} | samples {len(samples)}' + ('' if not why else ' | ' + '; '.join(why)))
for t, s, l, a in samples:
    print(f'    {t:>3}s lk={",".join(x[-6:] for x in s) or "-"} status={int(l)} app={a.replace(" ", "_") or "-"}')
sys.exit(0 if verdict == 'PASS' else 1)
EOF
        pass=$((pass + 1))
    fi
done
echo "passed $pass of $runs"
[ "$pass" = "$runs" ]

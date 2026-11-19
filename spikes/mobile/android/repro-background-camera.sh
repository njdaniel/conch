#!/usr/bin/env bash
# Emulator repro for #1972: with the camera on, send the app HOME for 40 s,
# bring it back, and check that exactly one LiveKit connection is left for the
# person, and that the app, LiveKit and `conch voice status` agree.
#
# Needs: the dev stack up (scripts/dev-stack.sh up), the debug APK installed
# with Metro running (or the release APK on an http-allowing build), adb
# reverse for conchd, LiveKit (signalling and TCP media) and Metro, and the app
# signed in and on the channel's Voice view, in voice. python3 for the UI
# driver (uiautomator dump) and the LiveKit admin call. An emulator pass is not
# device evidence.
#
# usage: repro-background-camera.sh RUNS
# environment (defaults in brackets):
#   ANDROID_SERIAL   the device [emulator-5554]
#   REPRO_PRINCIPAL  the phone's principal id; LiveKit identity p<id> (required)
#   REPRO_CHANNEL    the channel [general]
#   REPRO_AWAY       seconds away [40]
#   REPRO_WATCH      seconds watched after the return [45]; the displacement
#                    seen in #1972 came about 17 s after it
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
state=${CONCH_DEV_STATE:-${XDG_CACHE_HOME:-$HOME/.cache}/conch-dev}
server=http://127.0.0.1:${CONCH_DEV_PORT:-28080}
lk_port=${CONCH_DEV_LK_PORT:-27880}

# ui LABEL... : the first on-screen text or content-desc among the labels
# ("" if none); ui tap LABEL taps it.
ui() {
    python3 -I - "$@" <<'EOF'
import re, subprocess, sys, time, xml.etree.ElementTree as ET
root = None
for _ in range(5):
    subprocess.run(['adb', 'shell', 'uiautomator', 'dump', '/sdcard/ui.xml'], capture_output=True)
    out = subprocess.run(['adb', 'exec-out', 'cat', '/sdcard/ui.xml'], capture_output=True).stdout
    try:
        root = ET.fromstring(out)
        break
    except ET.ParseError:
        time.sleep(0.5)
if root is None:
    sys.exit('ui: no dump')
tap = sys.argv[1] == 'tap'
labels = sys.argv[2:] if tap else sys.argv[1:]
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

# connections: how many LiveKit participants have the phone's identity, over
# every room (an admin token signed with the dev stack's secret).
connections() {
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
n = 0
for r in call('ListRooms', {}).get('rooms', []):
    n += sum(p['identity'] == identity for p in call('ListParticipants', {'room': r['name']}, r['name']).get('participants', []))
print(n)
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

pass=0
for ((run = 1; run <= runs; run++)); do
    # Into voice with the camera on.
    if [ "$(app)" = 'Not in voice' ]; then
        ui tap 'Join voice' >/dev/null
        wait_for 20 'In voice' || { echo "run $run: could not join"; continue; }
    fi
    if [ -n "$(ui 'Turn camera on')" ]; then ui tap 'Turn camera on' >/dev/null; fi
    wait_for 15 'Your camera is being sent.' || { echo "run $run: camera did not start"; continue; }
    before=$(connections)

    adb shell input keyevent HOME
    sleep "$away"
    adb shell am start -n com.conchspike/.MainActivity >/dev/null 2>&1

    # Sampled every 3 s; the verdict is on the last third (settled), and on
    # whether two connections were ever seen at once.
    max=0 samples=() t0=$SECONDS
    while ((SECONDS - t0 < watch)); do
        sleep 3
        c=$(connections) l=$(listed) a=$(app)
        ((c > max)) && max=$c
        samples+=("$((SECONDS - t0))s:lk=$c,status=$l,app=${a// /_}")
    done
    settled=("${samples[@]: -4}")
    ok=1
    for s in "${settled[@]}"; do
        case $s in
            *lk=1,status=1,app=In_voice) ;;
            *) ok=0 ;;
        esac
    done
    ((max > 1)) && ok=0
    verdict=$([ $ok = 1 ] && echo PASS || echo FAIL)
    ((ok)) && pass=$((pass + 1))
    echo "run $run: $verdict before=$before max=$max ${samples[*]}"
done
echo "passed $pass of $runs"
[ "$pass" = "$runs" ]

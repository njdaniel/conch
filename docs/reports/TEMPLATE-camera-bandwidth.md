# Camera bandwidth and by-hand checks: webcam video in voice rooms (template)

Issue [#1269](https://github.com/njdaniel/conch/issues/1269), design item C8, Part B. The
governing documents are [ADR-015](../adr/ADR-015-webcam-video.md) (Consequences: "The
1:1 and 9-camera figures are measured on the droplet before anything about capacity is
documented") and the design [webcam-video.md](../design/webcam-video.md) (§7.5, §11.7, §12).
This is **proposed**: it is the method for Nick's runs, not a result. Nothing in the
documentation claims a capacity until a report made from it says what the runs showed.

**How to use this file.** Copy it to `docs/reports/<date>-camera.md` and fill it in on the
day. The method below is fixed before the runs, so the runs only fill in the blanks.
Change the method only on purpose, and say what changed in that report. It is built on
[TEMPLATE-alpha-bandwidth.md](TEMPLATE-alpha-bandwidth.md) (the share's method): the
droplet measurements are the same, and the two can share one droplet session.

**Never in the report:** a token, key or secret; the hostname or address of a private
test machine; a person's name, account or device identifier; a picture, a recording or a
screenshot of anyone's camera. Name the deployment `alpha.example.com` /
`livekit.example.com`, and the real browsers by kind ("desktop Chrome on Windows",
"iPhone, Safari", "Android phone, Chrome"). Device labels the browser gives a camera can
name the person's hardware: write "built-in" or "USB", not the label.

**What a result may say.** A figure from these runs is a measurement of this droplet, this
load and this day, and the report says so. The SDK's presets (design §7.5) are inputs,
never results, and a number is never described as the droplet's capacity unless a run
reached a limit and the report shows where. A browser cell is "tested", "unsupported" or
"untried", from what was run, and nothing else.

---

## 1. Method

### 1.1 What is measured, and why

The runs answer, for one-to-one calls and for nine cameras, with and without a share:

- how many Mbps leave and enter the droplet;
- how much CPU and memory `conchd`, LiveKit and Caddy each use;
- which layer each viewer receives, and whether what they see is smooth;
- what each sender spends: upload, CPU, and whether its encoder was limited.

They decide, afterwards and by Nick: whether the cap of 9 (O6) and the quality presets
(O11) stay as they are.

### 1.2 Setup to record before the first run

| | |
| --- | --- |
| Date, and time of each run (UTC) | |
| Droplet: size (RAM / vCPU), region | |
| Droplet: advertised bandwidth, monthly transfer allowance | from the provider's plan page |
| Droplet: public network interface name | (`ip -br link`; usually `eth0`) |
| `conchd` version | `/healthz` |
| LiveKit image | the digest `ops/deploy/livekit.service` pins |
| Caddy version | `caddy version` |
| Web client version, and its camera settings | the build's version; the presets it ships (design §7.5: computer 1280 x 720 at 30 fps, top layer at most 1.2 Mbps, 360p at 450 kbps and 180p at 150 kbps; phone 640 x 360 at 24 fps, top layer at most 450 kbps, 180p at 150 kbps; VP8; adaptive stream and dynacast on) |
| `use_camera` | turned on for operators and members for the run (two cells), and turned back off afterwards (section 4) |
| Real devices in the room | each: kind, browser, camera (built-in or USB), and its network (the same LAN as the droplet, or remote) |
| Load generator | see section 1.3; a second short-lived droplet (size, region), a machine on a fast line (its kind and line speed, not its name), or none |
| Generator's own capacity | its download rate measured before the runs: it must be well above what the runs make it receive, or the runs measure the generator |

### 1.3 The 9-camera load

Nine cameras means nine people in one voice room, each holding a camera claim and
publishing a video track. The rules of the server make this hard to fake. A published
camera track needs a claim (`POST /v1/channels/{channel}/voice/camera`), made by the
person publishing it. Without one, LiveKit refuses it or the pass withdraws it. So
`scripts/alpha-loadtest.sh`, whose `lk load-test` relay publishes as soon as it joins and
never claims a camera, **does not make a camera load**. It still makes the simulated
viewers and voices of the share's runs.

A load tool that claims a camera for each simulated publisher before `lk` publishes is
**proposed** and does not exist yet; this template does not depend on it. Until it does,
use as many **real devices** as there are, each its own principal (one person on two
devices is one principal, and the second device displaces the first), and apply these
rules:

- The report states how many real cameras each 9-camera run had. A run with fewer than 9
  is **partial**: say "partial: N of 9 cameras" in its row and in section 2, and do not
  compare it with a run that had 9.
- Simulated participants from the alpha loadtest may be added as **viewers** and
  voices (`--viewers`, `--publishers`), recording the layout used. They are not cameras.
  A run that adds them says what they were.
- If the proposed tool exists by the day, record its commit and use it for the
  9-camera runs; the real devices then sit on top of the simulated cameras, as the real
  browsers sit on top of the simulated viewers in the share's runs.

Every principal that a script makes is hidden afterwards, as in the share's template.

### 1.4 The four runs

Each lasts 5 minutes, steady, in one channel's voice room, with the people talking as they
would (mute is fine: mute does not touch a camera).

| Run | Cameras | Sharing | Real devices |
| --- | --- | --- | --- |
| 1. One to one | 2 (each side sends and receives) | nobody | two principals, on two devices, one of them a phone if one is at hand |
| 2. One to one with a share | 2 | one side shares a tab playing a 1080p video, through the web client | as run 1 |
| 3. Nine cameras | 9 (see section 1.3) | nobody | as many as there are, at least one a phone |
| 4. Nine cameras with a share | 9 | one person shares a tab playing a 1080p video, and every viewer has the call view open with the share watched | as run 3 |

Run 2 against run 1, and run 4 against run 3, show what a share adds. In runs 3 and 4
the viewers keep the call view open on the 3 x 3 grid the whole time (the call view
unsubscribes from cameras when it is closed), and say which of them also enlarged a tile.
Do not enlarge one during the five measured minutes unless the run says so: an enlarged
tile asks for a larger layer.

### 1.5 How each number is taken

**Droplet network (Mbps out and in).** As in the share's template, on the droplet, for
each run:

```sh
sar -n DEV 1 300 > sar-net-run<k>.txt      # sysstat; per second, five minutes
vnstat -i eth0 --json f > vnstat-before-run<k>.json   # before; again after (five-minute buckets)
```

From `sar`, the `eth0` row: mean and peak of `txkB/s` (egress) and `rxkB/s` (ingress),
times 8/1000 for Mbps. `vnstat`'s five-minute buckets are the cross-check, and the
provider's bandwidth graph a second one.

**CPU and memory, per process.** On the droplet, during each run:

```sh
pidstat -h -u -r -p "$(pgrep -d, -x conchd),$(docker inspect -f '{{.State.Pid}}' livekit),$(pgrep -d, -x caddy)" 5 60 > pidstat-run<k>.txt
sar -u -r 5 60 > sar-host-run<k>.txt
```

For each of `conchd`, LiveKit and Caddy: mean and peak `%CPU` (100 is one vCPU), and peak
`RSS`. From `sar`: the whole host's CPU (`%idle`) and memory used. `docker stats
--no-stream livekit` once mid-run is the cross-check for LiveKit.

**LiveKit's own statistics.** As in the share's template: with `prometheus_port: 6789`
added to `/etc/conch/livekit.yaml` while nobody is connected (loopback only), the
differences of `livekit_packet_bytes` (outgoing and incoming), `livekit_packet_total`
(retransmit against initial), `livekit_node_packet_total{type="dropped"}`, and mid-run
`livekit_participant_total`, `livekit_track_published_total` and
`livekit_track_subscribed_total` by kind. The count of published video tracks must equal
the run's cameras (plus the share's, in runs 2 and 4), or the run is repeated.

**Who was in the room.** `GET /v1/channels/{channel}/voice` as a member, once mid-run:
the participants with `camera` set must match the run's plan (`conch voice status
<channel>` shows `+camera`), or the run is repeated. The audit log should show one
`voice_camera_started` per camera and no `voice_camera_unclaimed`.

**The layer each viewer received.** On each real device that views, from
`chrome://webrtc-internals` (desktop and Android Chrome; use "Create a WebRTC-Internals
dump" to keep it), at the end of the run: each video `inbound-rtp`'s `frameWidth` x
`frameHeight` (which layer: 180p, 360p or 720p), `framesPerSecond`, `freezeCount`,
`totalFreezesDuration`, `framesDropped`, `packetsLost` and `jitter`. Safari and Firefox
have no such page: use Safari's Web Inspector (Mac) or `about:webrtc` (Firefox) if it is
at hand, and otherwise record only what the person watching saw. On every device a
person also writes down, in a sentence, whether the pictures were smooth.

**Each sender's cost.** On each real sending device, during the run:

- **Upload:** the video `outbound-rtp` of each layer (`rid`): `frameWidth` x
  `frameHeight`, `framesPerSecond`, the bitrate from `bytesSent`. A layer nobody asks for
  stops (dynacast), which shows which layers are in use. The total is the sender's upload.
- **`qualityLimitationReason`** of the top layer (`none`, `cpu` or `bandwidth`), and its
  `qualityLimitationDurations`: whether the encoder was held back by the machine or the
  network.
- **CPU** of the sending device: the browser's own task manager (Chrome: Shift+Esc), or
  `top`, `Activity Monitor` or Task Manager, mean over the run; and on a phone, whether it
  became warm, and its battery percentage before and after.

**The generator is not the limit.** If a generator was used: on it during each run, `sar -n
DEV 1 300` (its receive rate stays well below the capacity measured in section 1.2) and `sar -u
5 60` (CPU not saturated). A run that fails this is repeated or reported as such.

**Data per hour.** From each run's mean egress plus ingress in Mbps: GB per hour = Mbps x
3600 / 8 / 1000.

### 1.6 By hand: the matrix

Each row is **tested**, **unsupported** or **untried** (design §11.7), from what was
actually run. "Tested" means a person did it on a real device against a deployed server
(not localhost) and it worked as the row says. "Unsupported" means they tried and it did
not work, with what happened. "Untried" means nobody has run it; it is not a failure. Two
people means two principals: one person on two devices is a separate row. The only cell
filled in before any run is Chromium with a fake camera, which `make web-e2e` runs
(`web/e2e/44-camera.spec.ts`, `45-camera-groups.spec.ts`, `46-camera-phone.spec.ts`).

**Sending and viewing, with a real camera, against the deployed server:**

| Browser and device | Send | View | Notes |
| --- | --- | --- | --- |
| Chromium, fake camera (`make web-e2e`) | tried in `make web-e2e` | tried in `make web-e2e` | CI only, not a real camera |
| Chrome, Linux | untried | untried | |
| Firefox, Linux | untried | untried | |
| Chrome, Windows | untried | untried | |
| Firefox, Windows | untried | untried | |
| Safari, macOS | untried | untried | |
| Chrome, macOS | untried | untried | |
| Safari, iPhone | untried | untried | |
| Chrome, Android | untried | untried | |

**Behaviours:**

| Check | How | Result |
| --- | --- | --- |
| The device light goes off after **Camera** off, **Leave voice**, sign-out and **Stop camera test**; and stays on after **Hide video**, which closes the view and not the camera | on at least one laptop and one phone; a light cannot be seen by a test | untried |
| Unplugging a USB camera mid-call | the page says "Camera disconnected. Choose a camera in Voice options.", falls back to no other camera, and the light is off | untried |
| Phone, hidden page | lock the phone, then switch apps: the camera stops on both phones, the peer sees "Camera off", and unlocking does not restart it | untried |
| Phone in Chat receives no camera video | the data counter barely moves over a minute in Chat, compared with Voice with the call view open | untried |
| Phone default camera | the front one; a chosen back camera is kept after a reload | untried |
| One person, two devices | laptop first, then the phone joins the same channel: the laptop's light goes off at once, and the phone never says "Sending video" until its own camera is on | untried |
| The voice switch and the two cells | with the camera on for members, turn the members' cell off (or the person's voice switch): their camera ends before the change answers, on the real devices | untried |
| An operator's stop | `conch voice camera stop <channel> <id>` as an operator with `manage_channels`: the camera ends, the person's page says "An operator turned off your camera." | untried |

Add rows for anything else run, and say what it was.

---

## 2. Results

State first, for each run, how many cameras were real, and whether the run is partial.

### 2.1 Droplet network

| Run | Cameras (real / total) | Egress mean / peak (Mbps) | Ingress mean / peak (Mbps) | LiveKit out / in (Mbps) | Retransmitted (%) |
| --- | --- | --- | --- | --- | --- |
| 1 | | | | | |
| 2 | | | | | |
| 3 | | | | | |
| 4 | | | | | |

### 2.2 CPU and memory

| Run | conchd %CPU mean / peak, RSS | LiveKit %CPU mean / peak, RSS | Caddy %CPU mean / peak, RSS | Host CPU busy / memory used |
| --- | --- | --- | --- | --- |
| 1 | | | | |
| 2 | | | | |
| 3 | | | | |
| 4 | | | | |

### 2.3 Layers and what viewers saw

| Run | Viewer (kind) | Layer received | fps | Freezes (count, total s) | Seen |
| --- | --- | --- | --- | --- | --- |
| | desktop browser | | | | |
| | phone | | | | |

### 2.4 Senders

| Run | Sender (kind) | Upload (Mbps), by layer | `qualityLimitationReason` | CPU mean | Notes (phone: warm, battery) |
| --- | --- | --- | --- | --- | --- |
| | desktop browser | | | | |
| | phone | | | | |

### 2.5 The load generator, if one was used

| Run | Generator receive mean (Mbps) | Generator CPU busy | What it simulated |
| --- | --- | --- | --- |
| | | | |

### 2.6 By hand

The matrix of section 1.6, filled in, with the date and the build of each row.

---

## 3. Decisions, each with its reason

These are Nick's.

- **Keep the cap of 9 cameras per room, or change it (O6):**
- **Keep the quality presets, or change them (O11):**
- **What the documentation may now say about capacity:** (nothing, until a run reached
  a limit; or the figure, with the run, the droplet and the load it was measured under)
- **Droplet size:**
- **Follow-ups filed:** the load tool for simulated cameras, if it still does not exist;
  changes to the presets or the cap, each as its own issue; and the small PR that flips the
  browser-table cells in `README.md` and `docs/alpha-tester-guide.md` from "untried" and
  "not known yet" to what was run.

## 4. Afterwards

- [ ] `use_camera` is back off for operators and members, if the server is not to use
  cameras (date and time):
- [ ] Every script's output ends with all principals hidden; none named `loadtest-` is
  active (`conch admin principal list --prefix loadtest-`).
- [ ] The generator droplet, if any, is destroyed (date and time):
- [ ] The report contains no token, key, hostname, address, name, device identifier,
  picture or recording.

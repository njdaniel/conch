# Alpha bandwidth: one screen share to 29 viewers (template)

Issue [#448](https://github.com/njdaniel/conch/issues/448), plan item A15. The plan is
[docs/design/private-alpha.md](../design/private-alpha.md) §4 O6 and O7; the decision is
[ADR-008](../adr/ADR-008-private-communications-alpha.md) (Consequences).

**How to use this file.** Copy it to `docs/reports/<date>-alpha-bandwidth.md` and fill it
in on the day. The method below is fixed before the runs, so the runs only fill in the
blanks. Change the method only on purpose, and say what changed in that report.

**Never in the report:** a token, key or secret; the hostname or address of a private
test machine; a tester's name, account or device identifier. Name the deployment
`alpha.example.com` / `livekit.example.com`, and the real browsers by kind ("desktop
Chrome on Windows", "Android phone, Chrome"). The script's output contains none of
these; paste it as it is.

---

## 1. Method

### 1.1 What is measured, and why

One sharer to about 29 viewers sizes the deployment (O6, O7). The five runs answer:

- how many Mbps leave and enter the droplet, and how close that is to what the droplet
  can carry;
- how much CPU and memory `conchd`, LiveKit and Caddy each use;
- which simulcast layer each kind of viewer gets, and whether what they see is smooth;
- how much data a weekly movie night spends against the droplet's monthly allowance.

### 1.2 Setup to record before the first run

| | |
| --- | --- |
| Date, and time of each run (UTC) | |
| Droplet: size (RAM / vCPU), region | starting size: 4 GB / 2 vCPU (O7) |
| Droplet: advertised bandwidth, monthly transfer allowance | from the provider's plan page |
| Droplet: public network interface name | (`ip -br link`; usually `eth0`) |
| `conchd` version | `/healthz` |
| LiveKit image | the digest `ops/deploy/livekit.service` pins (v1.13.7) |
| Caddy version | `caddy version` |
| Web client version, and its share settings | the build's version; the encodings it ships (issue #565, superseding O6: captured at the source's size up to 3840×2160 and up to 60 fps; the top layer as captured at 6, 10 or 18 Mb/s by its longer side, up to 1920, up to 2560 and above; simulcast 1080p30 at 4 Mb/s and 720p15 at 1.5 Mb/s where smaller than the capture; VP8; share sound; viewers on adaptive stream, sharer on dynacast) |
| Load generator | a second short-lived droplet (size, region) **or** a machine on a fast line (its kind and line speed, not its name) |
| Generator's own capacity | its download rate measured before the runs: it must be well above what the runs make it receive, or the runs measure the generator |
| `lk` | 2.19.0, downloaded and checked by `scripts/alpha-loadtest.sh` |
| Commit of `scripts/alpha-loadtest.sh` used | |
| Real browsers in the room | at least two, one of them a phone; their kinds |

### 1.3 The load: `scripts/alpha-loadtest.sh`

Every simulated participant is a real `conchd` principal with its own voice session, so
`conchd` does the work it would do for a person (session issuance, membership checks,
presence). The script:

- with an operator credential, makes the principals (`loadtest-<run>-v001`, ...), each
  with a credential that expires after a few hours, and adds them to the channel;
- runs `lk load-test` through a relay on the generator that gives each simulated
  participant a token from `POST /v1/channels/{channel}/voice/session`, asked for as its
  own principal when it connects. Only signalling passes through the relay; the media
  flows between the generator and the droplet as a browser's would;
- when the run ends, hides every principal it made (bulk hide by name prefix, #445) and
  prints that none is left.

Run it on the generator, from a checkout of this repository (it builds a helper with
`go`), with the operator credential in a file (`chmod 600`) or in
`CONCH_LOADTEST_TOKEN`, never on the command line:

```sh
scripts/alpha-loadtest.sh --server https://alpha.example.com --token-file ~/.conch-op \
    --channel <channel> --viewers <N> --publishers <M> --duration 5m --prefix loadtest-run<k>
```

`--viewers` counts simulated participants only; real browsers come on top. Simulated
subscribers ask for video at the quality `lk load-test`'s `--layout` implies (default
`speaker`); record the layout used. `--fake-sharer` (a stand-in sharer publishing lk's
demo video as a screen) is for a rehearsal on the day, not for the measured runs.

Start each run's measurements (§1.5) first, then the share or the voices, then the
script; the five measured minutes start when lk prints `Finished connecting to room`.
Keep lk's two summary tables (`Track loading`, `Subscriber summaries`) for the report.

### 1.4 The five runs

Each lasts 5 minutes, steady, in one channel's voice room.

| Run | Sharing | Talking | Simulated (`--viewers` / `--publishers`) | Real browsers |
| --- | --- | --- | --- | --- |
| 1. Video | one Chrome tab sharing a 1080p video with sound, through the web client (A11b) | nobody | 29 minus the real viewers / 0 | the sharer, plus at least two viewers, one a phone |
| 2. Document | the same tab sharing a mostly static text document | nobody | as run 1 / 0 | as run 1 |
| 3. Voice only | nothing | 3 at once | 30 minus the real browsers / 3 | as many as are at hand, microphones on |
| 4. Movie night | as run 1 | 3 at once | 29 minus the real viewers / 3 | as run 1 |
| 5. 4K60 | one Chrome window on a 3840×2160 display sharing a 4K video at 60 fps with sound, through the web client | nobody | as run 1 / 0 | the sharer, plus at least two viewers: one watching full screen on a 3840×2160 display, and a phone |

Run 2 against run 1 shows what the content hint and simulcast do for text. Every share
is captured as motion (issue #554), so under congestion the encoder keeps frame rate
and gives up resolution; say whether the text stayed readable. The bitrate should fall
when nothing moves.

Run 5 against run 1 shows what a 4K60 share costs (issue #565). The top layer is
encoded and sent only while a viewer's view needs it, so the full-screen 4K viewer
stays full screen for the whole run, and the report says which viewers took the top
layer. Record the sharer's own upload and CPU too, and its `qualityLimitationReason`:
a 4K60 VP8 encode may be limited by the sharer's machine before the network.

### 1.5 How each number is taken

**Droplet network (Mbps out and in).** On the droplet, for each run:

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

For each of `conchd`, LiveKit and Caddy: mean and peak `%CPU` (100 is one vCPU), and
peak `RSS`. From `sar`: the whole host's CPU (`%idle`) and memory used.
`docker stats --no-stream livekit` once mid-run is the cross-check for LiveKit.

**LiveKit's own statistics.** The deployment does not export them by default. For the
measurement, add `prometheus_port: 6789` to `/etc/conch/livekit.yaml` and restart
LiveKit while nobody is connected. It listens on LiveKit's `bind_addresses`, which is
loopback only (checked with v1.13.7), so the firewall needs no change. Before and
after each run:

```sh
curl -s http://127.0.0.1:6789/metrics | grep -E '^livekit_(packet_bytes|packet_total|node_packet_total|participant_total|room_total|track_published_total|track_subscribed_total)' > lk-metrics-{before,after}-run<k>.txt
```

From the differences: `livekit_packet_bytes{direction="outgoing"}` and `"incoming"` over
the run's seconds, in Mbps (media only; compare with `sar` to see the overhead of
TURN, signalling, Caddy and everything else); `livekit_packet_total` with
`transmission="retransmit"` against `"initial"`, the share of retransmissions;
`livekit_node_packet_total{type="dropped"}`. Mid-run: `livekit_participant_total`,
`livekit_track_published_total` and `livekit_track_subscribed_total` by kind. The
`livekit_packet_loss_percent`, `livekit_jitter_us` and `livekit_rtt_ms` histograms give
loss, jitter and round trip as LiveKit saw them. Whether to keep the endpoint after
the measurement is one of the decisions (§3).

**Who was in the room.** The web client's member list, and the presence snapshot
(`GET /v1/channels/{channel}/voice` as a member), once mid-run: the count must match the
run's plan, or the run is repeated.

**Which simulcast layer each kind of viewer got.**
- The sharer: in `chrome://webrtc-internals`, the video `outbound-rtp` stream of each
  layer (`rid`): `frameWidth`×`frameHeight`, `framesPerSecond`, the bitrate from
  `bytesSent`, and `qualityLimitationReason`. A layer nobody asks for stops (dynacast),
  which shows which layers are in use.
- Each real viewer: its video `inbound-rtp`: `frameWidth`×`frameHeight` names the layer
  it receives.
- The simulated viewers: their video bitrate in lk's `Track loading` table, set against
  the sharer's per-layer bitrates.

**What the real viewers saw.** On each real browser, from `chrome://webrtc-internals`
(desktop and Android Chrome; use "Create a WebRTC-Internals dump" to keep it), at the
end of the run:
- video `inbound-rtp`: `framesPerSecond`, `frameWidth`×`frameHeight`, `freezeCount`,
  `totalFreezesDuration`, `framesDropped`, `packetsLost`, `jitter`;
- audio `inbound-rtp` (the share's sound in runs 1, 2, 4 and 5; the voices in 3 and 4):
  `concealedSamples` as a share of `totalSamplesReceived`, `concealmentEvents`,
  `packetsLost`.

An iPhone or iPad has no webrtc-internals: use Safari's Web Inspector from a Mac if one
is at hand, and otherwise record only what the person watching saw. On every device a
person also writes down, in a sentence, whether the picture was smooth and readable and
whether the sound was continuous.

**The generator is not the limit.** On the generator during each run: `sar -n DEV 1 300`
(its receive rate must stay well below the capacity measured in §1.2) and `sar -u 5 60`
(CPU not saturated). lk's `Subscriber summaries` must show every tester subscribed and
no errors. A run that fails any of these is repeated or reported as such.

**Data per hour and per month.** From each run's mean egress plus ingress in Mbps:
GB per hour = Mbps × 3600 / 8 / 1000. A weekly movie night is run 4's figure × its
length in hours × 52 / 12 per month, set against the monthly allowance alongside the
rest of the month's use (vnstat's monthly total before the runs gives that baseline).

---

## 2. Results

### 2.1 Droplet network

| Run | Egress mean / peak (Mbps) | Ingress mean / peak (Mbps) | LiveKit out / in (Mbps) | Retransmitted (%) |
| --- | --- | --- | --- | --- |
| 1 | | | | |
| 2 | | | | |
| 3 | | | | |
| 4 | | | | |
| 5 | | | | |

### 2.2 CPU and memory

| Run | conchd %CPU mean / peak, RSS | LiveKit %CPU mean / peak, RSS | Caddy %CPU mean / peak, RSS | Host CPU busy / memory used |
| --- | --- | --- | --- | --- |
| 1 | | | | |
| 2 | | | | |
| 3 | | | | |
| 4 | | | | |
| 5 | | | | |

### 2.3 Layers and what viewers saw

| Run | Viewer (kind) | Layer received | fps | Freezes (count, total s) | Audio concealed (%) | Seen and heard |
| --- | --- | --- | --- | --- | --- | --- |
| | sharer (layers sent) | | | | | |
| | desktop browser | | | | | |
| | phone | | | | | |
| | simulated (lk) | | | | | |

### 2.4 The load generator

| Run | Generator receive mean (Mbps) | Generator CPU busy | lk testers subscribed / errors |
| --- | --- | --- | --- |
| 1 | | | |
| 2 | | | |
| 3 | | | |
| 4 | | | |
| 5 | | | |

### 2.5 Transfer

| | |
| --- | --- |
| GB per hour, run 4 | |
| Movie night length assumed (hours) | |
| GB per month, weekly movie night | |
| Monthly allowance, and the month's other use | |

lk's summary tables for each run, and the script's last lines (`cleanup: hid N principals
... none is left`), go here or in an appendix.

---

## 3. Decisions, each with its reason

- **Droplet size:**
- **Cap the top layer (for example 720p) or the viewers per room:**
- **LiveKit egress or bitrate limits:**
- **What the client does at the limit:**
- **LiveKit's Prometheus endpoint:** kept (loopback only) or removed after the runs.
- **Follow-ups filed:** changes to A11b's encodings or to A5's sizing advice, each as its
  own issue, or done here when it is a constant.

## 4. Afterwards

- [ ] The generator droplet is destroyed (date and time):
- [ ] Every run's script output ends with all principals hidden; none named `loadtest-` is
  active (`conch admin principal list --prefix loadtest-`).
- [ ] O6 and O7 in `docs/design/private-alpha.md`, and the sizing line in
  `docs/deploy.md`, point to this report.

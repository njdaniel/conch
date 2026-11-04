# Deploying Conch on one droplet

This runbook takes an empty Ubuntu 24.04 droplet to a public `conchd` instance with voice, nightly off-site backups, and the steps to upgrade, roll back, restore and add a second instance. It is the deployment of [ADR-008](adr/ADR-008-private-communications-alpha.md) and the plan in [docs/design/private-alpha.md](design/private-alpha.md) (decision O7).

One droplet runs three things:

- `conchd`, one instance per systemd unit `conchd@<name>`, each as its own user, on a loopback port;
- Caddy, which holds the public names, gets and renews their certificates, and proxies to `conchd` and to LiveKit's signalling port;
- LiveKit, in Docker from the digest-pinned image the README uses, for the audio.

Everything is parameterised by an **instance name** (`alpha` below) and a **domain** (`example.com` below, a placeholder: replace it everywhere). A second server is one command, [`conch-server new`](#a-second-instance), not a new design, and [`conch-server rm`](#removing-a-server) removes it.

**Status.** The scripts, the units and the backup, restore, upgrade and rollback paths are exercised by `scripts/deploy-scripts-test.sh` (run by `make deploy-test` and in CI) against a real `conchd`, with a stand-in for `systemctl`. That test runs as one ordinary user, so the split between root, the instance users and `conch-backup` described below (`runuser`, `setpriv`, the cleared environment) is not part of it; the parsing of the rollback record, the check of the archive path, the refusal of a symlinked archive, the stripping of control characters and `setsid` are. The Caddyfile, LiveKit's TURN/TLS settings, the firewall and the units under a real systemd were **not** run on a droplet when this was written: the first run on the real droplet (issues A15 and A16) is where they are proved, and this page should be corrected from what that finds. `add-instance.sh`, `remove-instance.sh`, `list-instances.sh` and `ops/conch-server` (#861) are exercised by the same test, against a real `conchd`, with stand-ins for `caddy`, `useradd`, `userdel`, `getent`, `ss`, `ssh` and `doctl`; their first run on the droplet is the check of the real ones.

**First real run (2026-10-27, `v0.1.0-alpha.1`, #567).** Steps 1–7 were followed on a real droplet (Ubuntu 24.04.5, Caddy 2.11.7, Docker 29.1.3, LiveKit 1.13.7) and are corrected below where they were wrong. Proved there:
- Caddy obtained and serves certificates for all three names;
- LiveKit runs from the pinned digest under the unit, finds its public IPv4 and IPv6, and serves TURN/TLS on 5349 with Caddy's certificate;
- `/healthz` reports the tag, `db: ok` and `voice: ok`;
- `ufw` and a matching Cloud Firewall leave only Caddy (80, 443), LiveKit (7881/tcp, 3478/udp, 5349/tcp, the UDP media range) and SSH listening publicly.

Not yet proved on the droplet: voice through TURN from a network that blocks UDP, steps 8 and 9 (off-site backup and the restore drill), and upgrade and rollback.

## The community people join

One server is one community. Members normally see the community's members and
agents. Public channels are in **Browse channels**, and members join them themselves.
The directory also lists their own private channels. It lists no other private
channels and gives members no counts of people. A holder of `manage_channels` can
list every channel and its member count; reading messages and files and joining voice still need membership.
See [channel privacy](#channel-privacy-and-default-channels) and
[operator access](#what-an-operator-can-and-cant-see).

Send newcomers a member link. The browser's links land them in the default channels,
with full history. With no default channels, they start in no channel. The bare
address is a sign-in page unless `open_signup` is on. It is off by default on a new server; an upgraded server keeps its old open-joining mode.
An owner can change it; a delegated operator needs `change_server_settings`.
Open sign-up needs a public default channel. It makes members with full history too.

A member link without an explicit expiry lasts seven days by default.
`member_link_default_expiry_seconds` sets that lifetime; `0` means no default expiry.
An explicit request for a link that never expires needs `manage_members`.
See [member links](#member-links-and-the-invite-brake) for the controls.

Voice is on for new members. A holder of `manage_members` can change a person's
`voice_allowed` switch at any time. Turning it off removes them from live voice
and ends their share; an owner is exempt, and only an owner may change an owner's switch. Turning it on lets them join again,
subject to role permissions. See [the per-person switches](#the-eleven-permissions).

There are **no guests for now**. Guests were removed on 2026-10-30 and return with
#1004. The community migration ends active visits and closes invites that admitted
guests. Their data and past message attribution are kept. The bare address admits
no guests.

Choose the [personal or group configuration](#running-a-personal-or-a-group-server)
before inviting people. Before upgrading, read the
[one-community upgrade notice](../CHANGELOG.md#upgrade-notice-one-community-issue-979)
and [member-link changes](../CHANGELOG.md#010-alpha11---2026-11-02).

## Users and who can read what

| User | Runs | Can read |
| --- | --- | --- |
| `conch-<instance>` (one per instance) | `conchd@<instance>`, and the snapshot step of that instance's backup | its own data directory, `/var/lib/conch/<instance>`, only. `conchd` keeps its database and uploads at mode 0600, and the unit hides `/etc/conch`, `/var/backups/conch`, Caddy's state and every other instance's directory from it. |
| `conch-backup` | the archive, retention and off-site steps of every backup | `/var/backups/conch`, and the off-site settings and credentials (`/etc/conch/backup.env`, `/etc/conch/rclone.conf`), which it alone owns. It cannot read any instance's live data. |
| `conch-drill` | the restore drill (step 9), in a scratch directory of its own | only what it is handed for one drill. It owns no other file and holds no credential. |
| root | the backup unit (reads the instance's mode, then hands the snapshot from the instance user to `conch-backup`), the upgrade script, the steps below | everything, but the scripts and the steps below never open a file `conch-backup` owns: the archives are checked and read as `conch-backup`, and handed on as a stream. The env files (`/etc/conch/<instance>.env`) and LiveKit's keys are root's, mode 0600. |

So a compromised `conchd` cannot read another instance, the backups or the off-site credentials, and a leaked off-site credential does not open the live data. Every step the scripts run as one of these users runs in its own session, so it never has root's terminal as its controlling terminal, with none of root's environment, and what it prints is stripped of control characters before it reaches root's terminal.

## What you need

- A DigitalOcean droplet, Ubuntu 24.04, **4 GB RAM / 2 vCPU to start** (decision O7; the final size follows the measurement in A15), with IPv4 (and IPv6 if you want it). Sizes differ by region and change over time: check with `doctl compute region list -o json` before choosing. On 2026-10-27, nyc3 offered no 4 GB size at all, and the alpha droplet is `s-2vcpu-4gb` in nyc1.
- A domain you control, and permission to add DNS records.
- A DigitalOcean Space (or any S3-compatible bucket) for off-site backups, with **versioning** turned on (step 8), and two access keys: a full-access one that stays on your workstation, and a limited one for that bucket only, for the droplet.
- A workstation with `ssh`, and somewhere safe (a password manager, for example Bitwarden or KeePassXC) for the operator token, `CONCHD_TOTP_KEY` (step 5), your operator password, and the backup encryption passwords. If they are lost, the backups cannot be decrypted, so keep them somewhere that survives the workstation.
- An authenticator app on your phone (any TOTP app, for example 2FAS, Aegis or Google Authenticator) for the operator's browser sign-in (step 6).

## 1. DNS

Create `A` records (and `AAAA` if the droplet has IPv6) pointing at the droplet for:

| Name | Used for |
| --- | --- |
| `alpha.example.com` | the `alpha` instance: web client, API, WebSockets |
| `livekit.example.com` | LiveKit signalling (WebSocket over TLS) |
| `turn.example.com` | LiveKit's TURN/TLS fallback (UDP-blocked networks) |

Caddy cannot get a certificate for a name that does not resolve yet, so wait for the records before step 7. With the domain on DigitalOcean's nameservers:

```sh
for n in alpha livekit turn; do
  doctl compute domain records create your.domain --record-type A    --record-name $n --record-data <droplet IPv4> --record-ttl 300
  doctl compute domain records create your.domain --record-type AAAA --record-name $n --record-data <droplet IPv6> --record-ttl 300
done
```

**Don't look a name up before its record exists.** A resolver that has answered "no such name" keeps that answer for the zone's negative TTL (30 minutes on DigitalOcean), so a name you checked too early can stay missing on that resolver after the record is made. Check with a resolver you haven't asked yet, for example `https://cloudflare-dns.com/dns-query?name=alpha.your.domain&type=A` with `accept: application/dns-json`.

## 2. The host

As root on the droplet:

On a fresh droplet, cloud-init and the first unattended upgrade hold the package lock for a few minutes. Wait for them first, or `apt` fails with "Could not get lock":

```sh
cloud-init status --wait
while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do sleep 5; done
```

```sh
apt update && apt install -y sqlite3 curl ufw docker.io
# Caddy: install it from its official apt repository,
# https://caddyserver.com/docs/install#debian-ubuntu-raspbian

useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin conch-backup
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin conch-drill
install -d -m 0755 /opt/conch/releases /opt/conch/bin /etc/conch /var/lib/conch
install -d -m 0700 -o conch-backup -g conch-backup /var/backups/conch
```

`sqlite3` is a host prerequisite of the backup, restore and upgrade scripts (they use SQLite's online backup). `rclone` is the off-site tool (step 8); it is not a dependency of the repository. `conchd@.service` refuses to start if `/etc/conch` or `/var/backups/conch` is missing, because it hides both from the service.

Get the scripts and unit files at the tag you are installing. They are not in the release assets, so clone the repository (it is public) and copy them:

```sh
git clone --depth 1 --branch v0.1.0 https://github.com/njdaniel/conch.git /root/conch-src   # your tag
cd /root/conch-src/ops/deploy
install -m 0755 conch-backup.sh conch-restore.sh conch-upgrade.sh firewall.sh /opt/conch/bin/
install -m 0755 add-instance.sh remove-instance.sh list-instances.sh /opt/conch/bin/   # tags that have them
```

Later, `conch-server` keeps `/opt/conch/bin` matched to the tag the droplet runs ([A second instance](#a-second-instance)).

## 3. Install conchd

Release assets are named `conchd_<tag>_<os>_<arch>`, with a `checksums.txt`, and have the web client built in. The upgrade script does the download and the checksum check; for the first install do the same by hand. The steps run in a subshell with `set -e`, so a checksum that does not match stops them before anything is installed:

```sh
(
  set -euo pipefail
  tag=v0.1.0            # the release you are installing
  arch=amd64            # arm64 on an ARM droplet
  base=https://github.com/njdaniel/conch/releases/download/$tag
  cd "$(mktemp -d)"
  curl -fsSLO "$base/conchd_${tag}_linux_$arch" -O "$base/checksums.txt"
  grep " ./conchd_${tag}_linux_$arch\$" checksums.txt | sha256sum -c -
  install -d /opt/conch/releases/$tag
  install -m 0755 conchd_${tag}_linux_$arch /opt/conch/releases/$tag/conchd
  ln -sfn /opt/conch/releases/$tag /opt/conch/current
  /opt/conch/current/conchd version          # prints the tag and "web: embedded"
)
```

`checksums.txt` comes from the same release as the binary, so it catches a corrupted or truncated download, not a tampered release: whoever could replace the binary on the release could replace the checksum too. The trust root is GitHub, over HTTPS, and the accounts that can publish releases on the repository.

`/opt/conch/current` is a symlink; the unit starts `/opt/conch/current/conchd`, so an upgrade or a rollback is repointing it.

## 4. LiveKit

LiveKit runs in Docker, from the image the README pins by digest (`livekit/livekit-server:v1.13.7@sha256:...`). The version is named in `ops/deploy/livekit.service` and in the README: change both together, after reading the release notes.

Each instance has its own LiveKit key pair ([README](../README.md#run-livekit-beside-conchd): one key per server). Keys live in one file, one `<key>: <secret>` line per instance, mode 0600:

```sh
umask 077
key=alpha-$(openssl rand -hex 4)
secret=$(openssl rand -hex 32)
echo "$key: $secret" > /etc/conch/livekit-keys
umask 022
echo "key: $key"                     # the secret stays in the file; copy both values into /etc/conch/alpha.env (step 5)
```

Install LiveKit's settings (no secret in them) and its units, editing the domain:

```sh
cd /root/conch-src/ops/deploy
sed 's/example\.com/your.domain/g' livekit.yaml > /etc/conch/livekit.yaml
sed 's/example\.com/your.domain/g' livekit.service > /etc/systemd/system/livekit.service
sed 's/example\.com/your.domain/g' livekit-cert.path > /etc/systemd/system/livekit-cert.path
cp livekit-cert.service /etc/systemd/system/
```

What `livekit.yaml` and `livekit.service` set, and why:

- `room.auto_create: false` is required: `conchd` creates and deletes rooms itself, and with automatic creation on, a stale token would recreate a deleted room.
- Signalling (`7880`) listens on loopback; Caddy serves it as `wss://livekit.example.com`, and only its client path (`/rtc`): LiveKit's server API is for `conchd`, which calls it on loopback, and Caddy answers 404 for it. Media ports are open to the world: TCP `7881` and UDP `50000-60000`, plus `use_external_ip: true` so clients are told the droplet's public address.
- **TURN/TLS** (`turn:`) is the fallback for networks that block UDP: `3478/udp` and `5349/tcp`, on the name `turn.example.com`. It uses the certificate Caddy obtains for that name (the `turn.example.com` block in the Caddyfile does nothing else, and pins the ACME issuer so the certificate's directory cannot move to another CA's). `livekit.service` mounts Caddy's certificate directory for that name read-only at `/certs`. The mount is a `--mount`, which refuses to start when the directory does not exist yet; a `-v` would create it as root and break Caddy's first issuance.
- **What the container may do.** It runs as its root user with every capability dropped except `DAC_OVERRIDE`, which is what lets it read Caddy's key (mode 0600, owned by `caddy`) and the key file; `no-new-privileges`; and a read-only file system with an in-memory `/tmp`. Caddy's directory stays private to `caddy` on the host.
- **Who TURN relays for.** TURN relays only for a client holding a valid LiveKit token, which `conchd` issues for a voice channel the user may join. For such a user it relays to any public address (LiveKit refuses private and loopback peers by default), so a voice user could use the droplet as a relay. If that matters, `turn.deny_peer_cidrs` in `livekit.yaml` narrows the peers it relays to; it is not set here.
- **Renewal.** LiveKit reads the certificate once, at start. Caddy renews about 30 days before expiry, writing the same file; `livekit-cert.path` watches that file and runs `livekit-cert.service`, which restarts LiveKit (rooms are rebuilt by `conchd`; connected clients reconnect). Without this a renewed certificate would reach LiveKit only at the next restart.
- Port `443` is Caddy's, so TURN/TLS uses `5349`. Some networks block that port too; there is then no voice for them, which is a known limit of this setup.

## 5. conchd configuration and its user

Each instance runs as its own system user, `conch-<instance>`:

```sh
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin conch-alpha
```

One env file per instance, mode 0600, root-owned (systemd reads it as root, before it hides `/etc/conch` from the service). The format is `KEY=value`, one per line, no quotes or trailing spaces. It holds the LiveKit secret, so never put its values in a unit file, a Caddyfile, a command line or the repository:

```sh
install -m 0600 -o root -g root /root/conch-src/ops/deploy/instance.env.example /etc/conch/alpha.env
editor /etc/conch/alpha.env
```

Set, for `alpha`:

- `CONCHD_DATA=/var/lib/conch/alpha` (the directory the unit creates and the only one it can see; the backup script expects exactly this);
- `CONCHD_LISTEN=127.0.0.1:8080` (loopback; one port per instance);
- `CONCHD_LIVEKIT_URL=wss://livekit.your.domain`, `CONCHD_LIVEKIT_API_URL=http://127.0.0.1:7880`;
- `CONCHD_LIVEKIT_ROOM_PREFIX=alpha-` (per instance);
- `CONCHD_LIVEKIT_API_KEY` and `CONCHD_LIVEKIT_API_SECRET`: the pair from step 4;
- `CONCHD_TRUSTED_PROXIES=127.0.0.1/32,::1/128` (Caddy, on loopback), as the example has it, so the invite routes' rate limits see each client's address rather than Caddy's;
- `CONCHD_TOTP_KEY`: the key operators' authenticator secrets are encrypted under, made with `openssl rand -base64 32`, one per instance. Copy it into your password manager now, beside the backup passwords of step 8: the database holds the secrets only encrypted, so a restored backup without this key signs no operator in with a password (see [Restoring for real](#10-restoring-for-real)). Without the key `conchd` starts and everything but operators' password sign-in works; a value that is not such a key stops it at start.
- `CONCHD_MODE`: leave it unset for a long-lived server such as `alpha`. `CONCHD_MODE=temporary` makes a short-lived one, where an operator may sign in with a password alone and everyone is told the server is temporary; anything other than `temporary` or `normal` stops `conchd` at start, and starting a temporary server again in normal mode signs its operators out only if password-only sign-in stops being allowed, that is in normal mode with `CONCHD_OPERATOR_TOTP=required` or unset: with `CONCHD_OPERATOR_TOTP=optional` kept, sessions and recent re-authentication survive on purpose, since a password still signs operators in ([ADR-012](adr/ADR-012-optional-operator-totp.md) item 4). Script credentials made on a temporary server, a password-only operator's included, keep working after either switch; if that matters, revoke them (`conch admin`) after it. See [ADR-011](adr/ADR-011-temporary-servers.md).
- `CONCHD_OPERATOR_TOTP`: whether an operator needs an authenticator app to sign in to the browser. `required` (the default on a normal server) asks every operator for a code. `optional` (the default with `CONCHD_MODE=temporary`) lets an operator with no authenticator sign in, re-authenticate and change their password with the password alone, without making the server temporary or telling anyone; an operator who has enrolled is still always asked for the code. `required` with `CONCHD_MODE=temporary`, or any other value, stops `conchd` at start. Going from `optional` back to `required` signs operators out of their browser sessions, ends recent re-authentication, pending sign-ins and unfinished setups, and logs how many operators are left with a password and no authenticator: give each a setup token (`conchd operator setup`, step 6) to enrol one. Script credentials keep working. See [ADR-012](adr/ADR-012-optional-operator-totp.md).

## 6. The first operator

Create the first operator **before** the first start, over SSH, with the token going to your terminal and nowhere on the droplet:

```sh
install -d -m 0700 -o conch-alpha -g conch-alpha /var/lib/conch/alpha
sudo -u conch-alpha /opt/conch/current/conchd bootstrap-operator --data /var/lib/conch/alpha --name your-name
```

It prints the token once on stdout. Copy it into your password manager from the terminal; do not redirect it to a file on the droplet, and do not leave it in your scrollback on a shared machine. `bootstrap-operator` refuses to run a second time. From then on you administer the server with `conch admin` from your own machine, using this token (`CONCH_SERVER=https://alpha.your.domain`).

That token is a **script credential**: it is as powerful as you are, it has no second factor, and anyone who copies it can act as you until you revoke it. Keep it for `conch admin` and scripts. Signing in **in a browser** is with a password and an authenticator app (any app that does TOTP: six digits, every 30 seconds), set up once from the host after the first start (step 7):

```sh
sudo -u conch-alpha /opt/conch/current/conchd operator setup --data /var/lib/conch/alpha 1
```

`1` is the operator's principal id (`conch admin principal list` shows ids). It prints a **setup token** once on stdout and revokes nothing. The token works once, within ten minutes, over HTTPS only: open the server's address, choose **Set up an operator account** under "Operators and scripts", and paste it there (or use `conch setup`, or `POST /v1/setup/redeem` with `{"token": "..."}`); the page takes the password, then shows the authenticator's key behind **Show the key**, and not while that tab shares its screen. What it answers is a setup credential that lives fifteen minutes and can do four things and nothing else: show who you are, set your password (`POST /v1/password/mine`), enrol the authenticator (`POST /v1/totp/enroll`, then `POST /v1/totp/confirm` with a code from the app), and sign itself out (**Skip**, `DELETE /v1/credentials/current`). Where operators may sign in with a password alone (`CONCHD_OPERATOR_TOTP=optional`, or a temporary server) the password completes the setup, and the credential then keeps only the authenticator step and Skip. **The enrolment shows the authenticator's secret once**, as text and as an `otpauth://` link: type it into the app (or let a QR code made from the link on your own machine be scanned), on a screen nobody else is watching and that you are not sharing, and do not keep a copy of it anywhere else. The server never shows it again, and keeps it only encrypted under `CONCHD_TOTP_KEY`. Confirming completes the setup and ends the setup credential. From then on the browser signs you in with your name, your password and a current code; that session lasts twelve hours at most and ends after an hour without use. Actions that create or grant access, or end someone's (making a credential, a link, a hook or a principal, membership, disabling, hiding), ask for your password and a code again if you last gave them more than ten minutes ago. Script credentials are not asked: that is why they must be guarded.

**Two different tokens.** `bootstrap-operator` prints the **operator token**, a lasting script credential: keep it. `conchd operator setup` prints a **setup token**, for one use within ten minutes: redeem it under **Set up an operator account**, never in the Credential box, which refuses it. Setup isn't finished until the authenticator is confirmed: with only a password set, browser sign-in is refused as incomplete.

**No QR code yet** (#862): the page shows the authenticator's key as text. To scan it instead of typing it, run this on your own workstation (not the droplet). It needs `qrencode` (`apt install qrencode`), runs in `bash` whatever your shell is, asks for the key without echoing it, and keeps the key out of your shell history. Paste the key at the `key:` prompt, then scan the code it draws:

```sh
bash -c 'command -v qrencode >/dev/null || { echo "install qrencode first" >&2; exit 1; }
read -rsp "key: " k || exit 1; echo
[ -n "$k" ] || { echo "no key given" >&2; exit 1; }
printf "otpauth://totp/Conch:your-name?secret=%s&issuer=Conch" "$(printf %s "$k" | tr -d " ")" | qrencode -t ansiutf8'
```

If it prints an error instead of a `key:` prompt, don't paste the key. Afterwards, `clear` the terminal.

`conchd operator bootstrap --data /var/lib/conch/alpha your-name` makes the first operator with a setup token instead of a script credential, if you want no script credential at all; it refuses when an operator exists, as `bootstrap-operator` does.

### An operator who lost their password or authenticator

There is no way to reset an operator over the network; it is done on the host, by whoever can run `conchd` there:

```sh
sudo -u conch-alpha /opt/conch/current/conchd operator recover --data /var/lib/conch/alpha <id>
```

It revokes **every** credential of that operator (their script credentials too: make new ones afterwards), removes their authenticator, ends their pending sign-ins, links and re-authentication, audits `operator_recovered`, and prints a setup token, used as above: the password you set through it replaces the old one, and you enrol the authenticator afresh. A running server stops accepting what it revoked at once on REST, and closes open sockets within its re-check interval. The audit trail records `setup_token_issued`, `setup_token_redeemed`, `password_set`, `totp_enrolled` and the revocations, and never a secret, a code or a token.

## 7. Start it: services, Caddy, firewall

```sh
cd /root/conch-src/ops/deploy
cp 'conchd@.service' 'conch-backup@.service' 'conch-backup@.timer' /etc/systemd/system/
systemctl daemon-reload

# Firewall first: SSH (rate-limited), 80, 443/tcp, 7881/tcp, the UDP media
# range, and TURN. Everything else is closed; conchd (8080) and LiveKit's 7880
# are loopback only. SSH_PORT is the port sshd really uses. Read it before
# enabling:
/opt/conch/bin/firewall.sh --dry-run
/opt/conch/bin/firewall.sh

# Caddy: edit the domain and the email, then validate. The first time,
# restart rather than reload (below).
sed 's/example\.com/your.domain/g' Caddyfile > /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile
systemctl restart caddy

systemctl enable --now conchd@alpha
curl -fsS https://alpha.your.domain/healthz     # {"status":"ok","version":"v0.1.0","db":"ok"}
```

Also put a **DigitalOcean Cloud Firewall** on the droplet with the same inbound rules, and SSH only from the addresses you administer from. It filters before traffic reaches the droplet, and it still holds if `ufw` is disabled or flushed by mistake.

**Caddy's admin API** listens on a unix socket in its own state directory (`admin unix//var/lib/caddy/admin.sock` in the Caddyfile), not on `localhost:2019`, so `conchd` and LiveKit cannot reach it; it has no authentication of its own. The packaged unit's reload runs `caddy reload --config /etc/caddy/Caddyfile --force`, which sends the new configuration to the admin address it reads from that file. (Checked with Caddy 2.10.2: once Caddy runs with this Caddyfile, the reload succeeds over the socket, and nothing answers on port 2019.) If you ever start Caddy from a Caddyfile without that line, it listens on 2019 again.

**Restart the first time, reload after.** The Caddy the package started runs its default configuration, with the admin API on `localhost:2019`. The first `systemctl reload caddy` after installing this Caddyfile looks for the socket the new file names, where nothing listens yet, and fails. `systemctl restart caddy` starts Caddy on the new file, which opens the socket. Every later change (a second instance, a new name) is a `systemctl reload caddy`, which then finds it and does not drop open connections.

The instance blocks send `Strict-Transport-Security` for a year: once a browser has seen it, it will not use plain HTTP for that name. Keep HTTPS working on every instance name.

**LiveKit starts once Caddy has the TURN certificate.** Caddy obtains it on its own after the reload, usually within a minute. Wait for the file, then start LiveKit and the renewal watch:

```sh
crt=/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/turn.your.domain/turn.your.domain.crt
timeout 300 sh -c 'until [ -f "$1" ]; do sleep 5; done' sh "$crt" &&
  systemctl enable --now livekit livekit-cert.path ||
  echo "no TURN certificate after five minutes: read journalctl -u caddy, fix it, and run this again"
```

`livekit.service` also refuses to start while that file is missing, rather than letting Docker create the directory.

Open `https://alpha.your.domain/app/` and sign in. There are two ways:

- **With the operator token** from step 6: under **Operators and scripts**, paste it into **Credential**. It works at once, needs no authenticator, and isn't asked to re-authenticate. But it never expires, and the page keeps it for that tab only, so you paste it again in a new tab.
- **With your name, password and a code**, once you've finished `conchd operator setup` (step 6). This is the everyday way: the session ends after twelve hours, or one hour unused.

Then create the first channel. Then check voice from a second device on a different network, and once from a network that blocks UDP if you have one.

## 8. Backups

`conch-backup.sh <instance>` makes one dated archive, `/var/backups/conch/<instance>/conch-<instance>-<UTC time>.tar.gz`, holding:

- `conch.db`, copied with SQLite's online backup (`sqlite3 .backup`), which is safe while `conchd` runs in WAL mode, and checked with `PRAGMA integrity_check` before it is archived;
- `files/`, the uploaded files (they are not in the database). They are taken after the database so every file a message names is in the archive.

A `.sha256` file sits beside it. `conch-backup@<instance>.timer` runs it nightly at about 03:15. The unit starts as root only to split the work: the snapshot runs as `conch-<instance>`, the only user that can read the data, and the archive, retention and off-site copy run as `conch-backup`, the only user that holds the off-site credentials (see [Users](#users-and-who-can-read-what)).

**Retention.** Nightly archives are kept locally for 14 days (`CONCH_BACKUP_KEEP_DAYS`). The upgrade script's backups are named `pre-upgrade-<tag>-<instance>-<time>.tar.gz`, which the nightly retention does not match, and are kept by count instead: the newest 3 per instance (`CONCH_BACKUP_KEEP_PRE_UPGRADE`), so the one a rollback needs is never aged out between upgrades. They stay on the droplet; the nightly ones are what go off-site.

**Off-site is not optional.** Without a destination the script still writes the local archive, then says `NO OFF-SITE COPY WAS MADE` and exits 3, so `systemctl --failed` and `systemctl status conch-backup@alpha` show it.

### Off-site: an encrypted rclone remote on a versioned Space

The default, and the only path this runbook supports end to end, is `rclone` with a **`crypt` remote** wrapped around the Space: the archives are encrypted on the droplet before they leave it, so the storage provider, and anyone holding only the Space key, sees ciphertext and encrypted names.

1. **On your workstation, with the full-access key, turn on versioning and a lifecycle rule. This is required.** That key never goes on the droplet, so nothing on the droplet can turn versioning off or change the rule. Spaces turns versioning on only through its S3 API (the control panel shows the status), so use the AWS CLI with the full-access key:

   ```sh
   # On your workstation. The regional endpoint, not the bucket's own.
   ep=https://nyc3.digitaloceanspaces.com
   aws s3api put-bucket-versioning --endpoint-url "$ep" --bucket your-bucket --versioning-configuration Status=Enabled
   aws s3api get-bucket-versioning --endpoint-url "$ep" --bucket your-bucket     # "Status": "Enabled"
   cat > lifecycle.json <<'EOF'
   {"Rules": [{"ID": "expire-old-versions", "Status": "Enabled", "Filter": {"Prefix": ""},
               "NoncurrentVersionExpiration": {"NoncurrentDays": 90}}]}
   EOF
   aws s3api put-bucket-lifecycle-configuration --endpoint-url "$ep" --bucket your-bucket --lifecycle-configuration file://lifecycle.json
   aws s3api get-bucket-lifecycle-configuration --endpoint-url "$ep" --bucket your-bucket   # must show the rule
   ```

   The rule expires a version 90 days after it stopped being the current one; without one, old versions are kept forever. DigitalOcean's lifecycle documentation names only the expiry of objects and of unfinished uploads, not of old versions, so check that the rule reads back. If Spaces refuses it, old versions stay until you delete them, and the bucket grows by one archive per instance per night.

   **For the droplet, a limited key.** In the control panel's Spaces access keys, create a key with **Limited access**, granting **Read/Write/Delete** on this bucket only. Spaces has no write-without-delete permission, so this is the narrowest key a backup can use; its documentation lists bucket configuration (versioning, lifecycle rules) under full access only.

   **What versioning gives.** An overwrite or a plain delete made with the droplet's key, by a mistake, a broken script or an intruder who does not look further, leaves the earlier version in place. It does **not** stop someone who holds that key and deletes the old versions by their version IDs: DigitalOcean does not document whether a limited key's Delete covers that, so assume it does. For a copy the droplet cannot touch at all, pull the bucket to another machine on a schedule with a third, **Read**-only limited key, for example weekly from your workstation: `rclone copy spaces-ro:your-bucket /path/to/conch-offsite` (the files stay encrypted; the crypt remote over that copy reads them).
2. **Make the two crypt passwords on your workstation**, not on the droplet (`openssl rand -base64 32`, twice), and store both in your password manager. That copy is the one that matters: the droplet's copy is lost with the droplet, and **if both are lost, the backups cannot be decrypted by anyone**. The droplet has to hold them too, to encrypt each night; `rclone` keeps them in `rclone.conf` obscured, which is not encryption, so that file is mode 0600 and owned by `conch-backup` alone. The crypt remote protects the off-site copies, not the droplet: root on the droplet can read the live data anyway.
3. Write the configuration, as root, entering the passwords at the prompts (they do not land in your shell history or on a command line):

```sh
apt install -y rclone
install -m 0600 -o conch-backup -g conch-backup /dev/null /etc/conch/rclone.conf
install -m 0600 -o conch-backup -g conch-backup /dev/null /etc/conch/backup.env
read -rsp 'crypt password: ' pw && echo
read -rsp 'crypt password2 (salt): ' pw2 && echo
cat > /etc/conch/rclone.conf <<EOF
[spaces]
type = s3
provider = DigitalOcean
access_key_id = YOUR_ACCESS_KEY
secret_access_key = YOUR_SECRET_KEY
endpoint = nyc3.digitaloceanspaces.com

[conch-crypt]
type = crypt
remote = spaces:your-bucket
password = $(printf '%s' "$pw" | rclone obscure -)
password2 = $(printf '%s' "$pw2" | rclone obscure -)
EOF
unset pw pw2
cat > /etc/conch/backup.env <<'EOF'
RCLONE_CONFIG=/etc/conch/rclone.conf
CONCH_BACKUP_RCLONE_DEST=conch-crypt:conch
EOF
```

Replace the placeholders; `YOUR_ACCESS_KEY` and `YOUR_SECRET_KEY` are the droplet's limited key. The files keep the owner and mode `install` gave them. The script refuses a `backup.env` that is not mode 0600, or that is owned by anyone but root or the user running it.

`s3cmd` also works (`CONCH_BACKUP_S3CMD_DEST=s3://your-bucket/conch`, with `S3CMD_CONFIG=/etc/conch/s3cfg` in `backup.env`, owned by `conch-backup`), but it uploads in the clear unless you set up its own GPG encryption, and the same versioning, key and lifecycle steps apply. Prefer the crypt remote.

Run it once by hand, as the timer would, and look:

```sh
systemctl start conch-backup@alpha; journalctl -u conch-backup@alpha -n 5 --no-pager   # "copied off-site"
systemctl enable --now conch-backup@alpha.timer
systemctl list-timers 'conch-backup@*'
```

## 9. The restore drill (before testers join)

A backup that has never been restored is a guess. Before anyone else is invited, restore the latest archive into a scratch directory, with the live instance left alone.

The drill runs as `conch-drill` (step 2), in a scratch directory of its own. It unpacks an archive and opens the database in it, so it does not run as root, nor as `conch-backup`, which holds every archive and `rclone.conf`. `conch-backup` only reads the archive out to it, and root opens neither file.

Paste these helpers into root's shell; step 10 uses them too:

```sh
clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }
# as <user> <command>...: as that user, in its own session (so root's terminal
# is not its controlling terminal), with none of root's environment, and its
# error output stripped of control characters. Give it stdin from /dev/null or
# a pipe, and pipe what it prints to the terminal through clean.
as() {
  local -; set -o pipefail
  local u=$1; shift
  { setsid -w runuser -u "$u" -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin "$@" 2>&1 1>&3 3>&- | clean >&2; } 3>&1
}
# drill <directory> <archive name>: conch-backup copies the archive and its
# .sha256 into a fresh scratch directory owned by conch-drill, which runs the
# drill there. The scratch directory is removed afterwards.
drill() (
  set -euo pipefail
  cd /
  d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
  chown conch-drill: "$d"
  for f in "$2" "$2.sha256"; do
    as conch-backup cat -- "$1/$f" </dev/null | as conch-drill sh -c 'cat >"$1"' sh "$d/$f"
  done
  as conch-drill /opt/conch/bin/conch-restore.sh "$d/$2" "$d/out" </dev/null | clean
)
```

Then, from the local copy:

```sh
latest=$(as conch-backup sh -c 'ls -1t /var/backups/conch/alpha/conch-alpha-*.tar.gz | head -n 1' </dev/null | clean)
drill /var/backups/conch/alpha "${latest##*/}"
```

It checks the archive's SHA-256, unpacks it, runs `PRAGMA integrity_check`, checks that every uploaded file the database names is present with its stored SHA-256, runs `conchd restore` on the copy (as a real restore does, step 10: nothing in the backup signs anyone in afterwards, so the drill's `conchd` serves none of the backup's sign-ins), starts `conchd` on the copy (loopback, a spare port, no web client, no LiveKit) and requires `/healthz` to say `"status":"ok","db":"ok"`, then stops it and prints the schema version, the message count, the number of files and what `conchd restore` did, as counts. It prints no credential, code or secret. If any step fails, the copy keeps the `restore-pending` file it was unpacked with, so no `conchd` serves it.

Do it also from the **off-site** copy once, since that is the copy that would be needed. `conch-backup` downloads it, and `rclone` decrypts on the way down:

```sh
dl=$(as conch-backup mktemp -d </dev/null | clean)
as conch-backup env RCLONE_CONFIG=/etc/conch/rclone.conf rclone copy conch-crypt:conch/alpha/ "$dl" --include "${latest##*/}*" </dev/null | clean
drill "$dl" "${latest##*/}"
rm -rf "$dl"
```

Once, also check that the crypt passwords in your password manager are the right ones: on your workstation, with a `rclone.conf` made from them and the read-only key (step 8), list `conch-crypt:conch/alpha/`. Names that decrypt mean the passwords are right.

## 10. Restoring for real

Use this when the data is lost or corrupt. The archive's `conch.db` and `files/` go back as the instance's data directory. Restoring onto a new host, check its clock first (`timedatectl` must say `System clock synchronized: yes`): a clock that runs behind lets an authenticator code someone saw before the restore be used again after the minute in which codes are refused (below). With the helpers from step 9 in root's shell:

```sh
(
  set -euo pipefail
  cd /
  a=conch-alpha-YYYYMMDDTHHMMSSZ.tar.gz    # the archive to restore
  as conch-backup sh -c 'cd /var/backups/conch/alpha && sha256sum -c -- "$1.sha256"' sh "$a" </dev/null | clean
  systemctl stop conchd@alpha
  mv /var/lib/conch/alpha "/var/lib/conch/alpha.broken-$(date -u +%Y%m%dT%H%M%SZ)"
  install -d -m 0700 -o conch-alpha -g conch-alpha /var/lib/conch/alpha
  # First, before anything is unpacked: conchd serve refuses the directory
  # until conchd restore has finished with it. (Archives made since #716
  # carry this file too; the touch covers older ones.)
  as conch-alpha touch /var/lib/conch/alpha/restore-pending </dev/null
  as conch-backup cat -- "/var/backups/conch/alpha/$a" </dev/null |
    as conch-alpha tar --no-same-owner --no-same-permissions -xzf - -C /var/lib/conch/alpha
  as conch-alpha /opt/conch/current/conchd restore --data /var/lib/conch/alpha </dev/null | clean
  systemctl start conchd@alpha
  curl -fsS https://alpha.your.domain/healthz
)
```

`conch-backup` checks and reads the archive and the instance's user unpacks it from that stream, so root opens no file `conch-backup` owns, no owner or mode is taken from the archive, and nothing needs a `chown` afterwards. Everything written after the archive was made is gone: say so to the people using it. Run the drill on the archive first if there is any doubt about it. Keep the `.broken-*` directory until you are sure; the service cannot see it.

**Restoring signs everyone out.** The backup may be older than a sign-out, a revoked credential, a used or closed link, or the end of a guest's visit, and served as it is it would bring all of them back. So `conchd restore` (offline, as the instance's user, before the first start) makes everything in it that signs someone in stop working: every credential (browser sessions, script credentials and agents' credentials alike), every open sign-in, device, reset or enrollment link, every pending sign-in and recent re-authentication, and every setup token; and it ends every guest visit that was active in the backup, with reason `restore`. It also **pauses invites**, because an invite closed after the backup is open again in it (audited `invites_paused`), and **pauses guest joining** (`joining_paused`, audited `join_settings_changed`), because joining turned off since may be on again in it; whether guests are enabled at all is left as it was. It does it all in one transaction, prints how many principals it signed out, visits it ended and setup tokens it deleted and how many webhook hooks are live, and audits `restore_invalidated`, with one `auth_generation_bumped` (cause `restore`) per principal and one `guest_ended` per visit. Accounts, passwords, authenticators, roles, memberships and messages stay, and a guest's posts keep their guest name.

The `restore-pending` file is what keeps a half-done restore off the network: `conchd serve` refuses to start while it is there, naming the command to run, and so do `conchd bootstrap-operator` and the `conchd operator` commands; `conchd restore` removes it only once its transaction has committed. Every archive `conch-backup.sh` makes holds an empty `restore-pending`, so data unpacked from one, by this procedure or any other way, starts refused. If anything above fails, or the droplet restarts in the middle, the instance does not come up (`systemctl status conchd@alpha` says why); run `conchd restore` as above, which is safe to run again, then start it. Do not remove the file by hand to get past it. `conchd restore` refuses a directory without the file, which is most likely a live one where it would sign everyone out; `--force` runs it anyway.

**Voice.** LiveKit keeps its own rooms, and the people in them joined under sign-ins the restore ended. The first start on restored data deletes every LiveKit room named the way this instance names its rooms (its `CONCHD_LIVEKIT_ROOM_PREFIX` and then the 26-character random part, nothing else) before it accepts traffic, which disconnects everyone in them (they join again through `conchd`), and logs how many. Other instances' rooms on the same LiveKit are left alone, even when one prefix begins with another (`alpha-` and `alpha-2-`). If LiveKit does not answer, `conchd` starts anyway (voice is optional) and logs an error: then run `systemctl restart livekit`, which ends every room on it, before people sign in again. That clear is not the only defence. The same first start retires every room the restored database holds, whatever prefix it was made under, and `conchd`'s regular voice sweep deletes those rooms in LiveKit, along with any room of this instance's name shape that the database does not know, as soon as LiveKit answers (`restore_started` in the audit log counts them as `voice_rooms_retired`). Restarting LiveKit only gets it done sooner. People who join afterwards get new rooms.

Authenticator codes are refused for about a minute after the first start, whatever the code: the record of which codes were already used went back with the backup, and that is the whole time a code used before could be used again. The first start logs this and audits `restore_started`; later starts do nothing of the kind.

After the start:

- **Members** sign in again with their password, on each device. A member who joined before passwords and has none gets a reset link ([A member who forgot their password](#a-member-who-forgot-their-password)).
- **Operators** sign in with password and code, a minute or more after the start. One with no password or authenticator yet uses `conchd operator setup`, and one who lost either uses `conchd operator recover` (step 6).
- **Script and agent credentials** are gone: issue new ones (`conch admin credential issue`) and replace them where they are configured. Tokens given to `conchd` with `--mcp-token` are configuration, not data, and are not affected.
- **Guests** do not come back: the restore ends every visit the backup held, and an ended visit never comes back. Guests are turned off for now, so nobody can join as a new guest either. A backup taken while guests were on is opened with them off: a bare address open to both is opened to accounts only.

**What a restore does not undo.** Apart from sign-ins, the restored data is the server as it was when the backup was taken, and every change made after it is lost, including ones made for safety. Nothing can reconstruct them: their record went with the data (if the database in `.broken-*` still opens, its audit log shows what happened). Before people return, check and redo what matters:

- invites closed since are open again: `conch admin invite list --state open`, and `conch admin invite pause` until you have looked;
- webhook hooks deleted since work again at their old URLs: `GET /v1/hooks`, and `DELETE /v1/hooks/{id}` for each that should be gone;
- passwords and authenticators are as the backup has them: a password reset, a password change or a `conchd operator recover` made since is undone, and the old password or authenticator works again. Do each again;
- approvals resolved since are pending again, and can be decided again;
- invites are paused by the restore: once you have closed the ones that should be closed, resume them with `conch admin invite resume`;
- joining from the bare address is paused by the restore: once the joining settings and open channels are as they should be (`GET /v1/join/settings`), resume it with `PUT /v1/join/settings` and `{"joining_paused": false}`;
- people disabled or hidden since are enabled again, and roles, invite and voice rights, channel memberships, agent manifests, open channels and the joining settings are as the backup has them, except that guests stay off.

**The TOTP key is not in the archive.** A backup holds the operators' authenticator secrets only encrypted under `CONCHD_TOTP_KEY`, which lives in `/etc/conch/alpha.env` and is never backed up with the data. Restoring onto the same droplet keeps the env file, and operators sign in as before. Restoring onto a new one, put the key back from your password manager into the new env file before the first start. If the key is lost, the data is not: start without it (or with a new one; `conchd` logs at start that the key does not open the stored secrets), and recover each operator with `conchd operator recover` (step 6), which removes the authenticator the old key protected and lets them enrol a new one. Members and messages do not depend on the key.

## 11. Upgrading and rolling back

Read the [one-community upgrade notice](../CHANGELOG.md#upgrade-notice-one-community-issue-979)
and [member-link changes](../CHANGELOG.md#010-alpha11---2026-11-02) before upgrading
an older server. The migration changes channel defaults, member history, voice
and link-making. Other existing channels stay private.

Releases are tagged, built by `.github/workflows/release.yml`, and name themselves: `conchd version` and `/healthz` report the tag. `conch-upgrade.sh <tag> <instance>...`, as root (name **every** instance on the droplet: they share one binary, so one left out would start on the new release at its next restart, with no pre-upgrade backup; `conch-server upgrade <tag>` on your workstation names them all):

1. downloads `conchd_<tag>_linux_<arch>` and `checksums.txt`, and refuses unless they match (the same limit as step 3: corruption, not tampering);
2. unpacks it to `/opt/conch/releases/<tag>/`;
3. takes a pre-upgrade backup of each instance (`pre-upgrade-<tag>-<instance>-<time>.tar.gz`, see [Retention](#8-backups)), and checks **every** one **before stopping anything**: the path the backup names must be exactly that name in `/var/backups/conch/<instance>/`, and the archive, checked as `conch-backup`, must be a regular file (not a symlink) matching its `.sha256`. If one is not, it stops there, says that nothing was stopped, and tells you what to check;
4. records each instance's schema version (`PRAGMA user_version`) in `/opt/conch/state/<instance>`, stops the instances, repoints `/opt/conch/current`, starts them. That record is never sourced: `--rollback` parses it, accepts only the keys the upgrade writes, checks every value as the upgrade did, and refuses anything else before stopping anything;
5. checks each `/healthz` answers `ok` and reports `<tag>`, and otherwise stops and tells you the rollback command.

```sh
/opt/conch/bin/conch-upgrade.sh v0.2.0 alpha
```

**Rollback without a migration** (the schema version is unchanged): `conch-upgrade.sh --rollback alpha` repoints `current` at the previous release and restarts. The data is kept as it is, including what was written since the upgrade.

**Rollback with a migration** (the schema version went up): the old binary refuses a newer database (`database schema version N is newer than this binary`), so the same command also replaces the instance's data directory with the pre-upgrade backup, unpacked as the instance's user. It checks that backup is there and intact before stopping anything. What was written since the upgrade is lost, and the replaced directory is kept; the script prints its path (`/var/lib/conch/alpha.rolled-back-<time>`) in case something in it is needed. The script says which of the two it did. Check the result with `curl https://alpha.your.domain/healthz`, which must report the old tag.

A backup restored this way is made safe as in step 10 before the instance starts: the script unpacks it with `restore-pending` in place and runs the previous release's `conchd restore` on it, so everyone signs in again afterwards, and the rest of [Restoring for real](#10-restoring-for-real) applies. If `conchd restore` fails, the script stops there, with the instance stopped and its data refusing to start; once the cause is fixed, run the same rollback again: it finds the unfinished restore, runs `conchd restore` on it and starts the instance.

A previous release from before `conchd restore` cannot make restored data safe, and its `conchd serve` does not know `restore-pending`, so on restored data it would serve every sign-in the backup holds. When a rollback would need the pre-upgrade backup, the script checks first that the previous release's usage names `conchd restore`, and if not it stops with **nothing stopped**: the instance keeps running the new release. The script only goes back to the release it recorded, so then roll back **by hand**: either to a release that has `conchd restore`, following [Restoring for real](#10-restoring-for-real) with that release's binary, or, if it has to be the old release, like this: stop the instances, point `/opt/conch/current` at the old release, and restore the pre-upgrade backup as in [Restoring for real](#10-restoring-for-real), knowing that the old release cannot run its `conchd restore` step. On that path nothing the restore would have done is done: every sign-in in the backup works, and the old release also leaves the authenticator record (`totp.last_step`) rolled back, so a code used since the backup can be used again for about a minute. Keep the instance off the network (stop Caddy for it, or firewall it) until, as an operator, you have signed everyone out with `POST /v1/principals/{id}/credentials/revoke-all` for each principal (which also ends a guest's visit) and paused invites; then remove `restore-pending`, which the old release ignores and the next one would refuse.

A release you installed stays in `/opt/conch/releases/`; remove old ones by hand when you are sure.

## Running a personal or a group server

Steps 1 to 11 get a server running. This part is about what the people on it may do. It follows [ADR-014](adr/ADR-014-fixed-roles-configurable-permissions.md) (the reasons are in [the design](design/admin-roles.md)). The examples use `alpha.your.domain` for a **personal server**, one person with family and friends in separate private conversations, and `chat.your.domain` for a **group server**, where everyone sees everyone.

Each task below gives the browser path first and the `conch admin` command after. In the browser, **Settings** (in the lobby) opens **Server administration**, which has the sections **Server**, **Roles and permissions**, **Members**, **Channels** and **Reports**. Operators and owners see it; members don't. **Channels** appears only for someone who holds `manage_channels`. The commands need your operator token (`CONCH_SERVER` and `CONCH_TOKEN`, as in step 6). Changing permissions from the browser asks an operator for their password and authenticator code again if you last gave them more than ten minutes ago.

### The roles and the owner mark

There are three roles on the server and one inside a channel. They are fixed: you can't add, rename or remove one.

| Role | What it is |
| --- | --- |
| **Owner** | You, the person who set the server up. An owner is an operator with the owner mark. Owners hold every permission, are never held back by a switch, and are the only ones who change roles and the operators' column. `bootstrap-operator` makes the first owner. |
| **Operator** | Someone you trust to help run the server. What an operator may do is set per permission, below. |
| **Member** | Everyone else. |
| **Channel manager** | A member who manages one channel: adds and removes its people, renames it, moderates. Whoever creates a channel manages it. |

Agents are not a role. What an agent can do stays in its manifest.

In the browser, **Members** lists everyone. Open a person's **⋯** menu (or right-click, or long press on a phone) and choose **Change role…**: Member, Operator, or **Make an owner, who holds every permission**. Only an owner sees it. With the CLI:

```sh
conch admin principal list
conch admin principal role 5 operator             # make person 5 an operator
conch admin principal role 5 operator --owner     # ...and an owner
conch admin principal role 5 operator --no-owner  # take the owner mark away
conch admin principal role 5 member               # back to a member
```

<a id="the-eleven-permissions"></a>

### The twelve permissions

A permission is a switch with one cell for members and one for operators. Owners have no cell: they hold all twelve.

| Permission | Members (default) | Operators (default) |
| --- | --- | --- |
| `create_public_channels`, `create_private_channels` | on | on |
| `make_member_links` | on | on |
| `see_member_directory` | on | on, and fixed on |
| `use_voice`, `share_screen` | on | on |
| `use_camera` | off | off |
| `manage_members` | fixed off | on |
| `act_as_members` | fixed off | on |
| `manage_channels` | fixed off | on |
| `change_server_settings` | fixed off | off |
| `read_audit_log` | fixed off | off |

Two of them need a sentence:

- **`act_as_members`** "can sign in as, and post as, any member who isn't an owner": it covers making a credential, a sign-in link or a hook for someone else. It needs `manage_members`.
- **`read_audit_log`** "sees every member and every private channel's name in the audit trail".

A member's cell can't be on while the operators' is off, `share_screen` and `use_camera` need `use_voice`, and the "fixed" cells can't be changed. The server refuses a change that breaks one of these and says which.

**Turning the camera on for members takes two cells.** `use_camera` is off for members and for operators, and owners always hold it, so an owner can try it at once. For members to have it, the operators' cell must be on first, or in the same save as the members' (**Save changes** applies both together; `conch admin permissions set use_camera --operators on --members on` does the same in one command; `--operators` is for owners). This is the rule stated above: a member's cell can't be on while the operators' is off, and a change that turns the operators' cell off while the members' is on is refused. Turning either cell off ends every affected camera at once. The camera is off for everyone but owners until you do this, and nothing about the camera changes for a server that never does. **What viewers see is not a permission:** anyone in the room receives the cameras in it, as they receive its voice.

Who changes them: an owner changes both columns. An operator holding `change_server_settings` changes the members' column only, and can only turn a cell on if it holds that permission itself. Give that permission only to someone you would trust with opening the server.

Beside the cells, two **switches per person** can only take something away: the voice switch (`use_voice`, `share_screen` and `use_camera`: there is no camera-only switch, so "voice yes, camera no" for one person has no answer short of turning the role's cell off) and the member-link switch (`make_member_links`, for members only; operators' links follow the operators' cell). They are in each person's **⋯** menu in **Members** (**Turn voice off**, **Stop member links**) and in `conch admin principal voice <id> on|off` and `conch admin principal invite-permission <id> on|off`. An owner is never held back by either: if a switch was ever stored on an owner, it no longer counts.

### Choosing and applying a preset

A preset sets the **members' column** and nothing else. It does not touch the operators' column, joining, default channels or anyone's switches. It is applied once; the server doesn't remember a "mode", it only reports which preset the column matches now.

| Members' cell | Personal server | Group server (the defaults) |
| --- | --- | --- |
| `create_public_channels` | off | on |
| `create_private_channels` | off | on |
| `make_member_links` | off | on |
| `see_member_directory` | off | on |
| `use_voice` | on | on |
| `share_screen` | on | on |
| `use_camera` | off | off |

Both presets leave the camera off for members, so applying either one turns it off for them if it was on (and ends the cameras then running). It leaves the operators' cell alone. A server that has turned the camera on for members therefore matches neither preset: neither card in **Roles and permissions** is marked as matching the members' column.

A new server is already on the group column. While nothing has been changed, **Settings → Server** shows a card, "How will this server be used?", with **Personal server**, **Group server** and **Decide later**. Choosing one shows what members would gain and lose, and **Apply Personal server** (or **Apply Group server**) does it. Later, **Settings → Roles and permissions** has the two preset cards (**Use Personal preset**, **Use Group preset**) at the top, each listing what it would turn on or off, with the matching one marked. Below them is the matrix, where **Save changes** applies cell edits and **Discard changes** drops them.

With the CLI:

```sh
conch admin permissions show                   # the table, the matching preset, warnings
conch admin permissions preset personal        # shows the changes, asks first (--yes skips the question)
conch admin permissions set make_member_links --members on
```

`set` takes `--members on|off` and `--operators on|off`; `--operators` is for owners. If someone else changed the permissions while you were looking, nothing is written and you are told to look again.

Warnings come back with `show` and with every change. They are never refusals:

- **`open_signup_on`**: members can't make member links, but open sign-up is on, so the server's address still lets anyone make an account. In the browser it is shown in **Server** and in **Roles and permissions**. On a personal server, turn open sign-up off.
- **`default_channel_exposes_members`**: members can't see the directory, but a default channel still shows its people to each other. This is the reason a default channel defeats a personal server: everyone who starts in it can see everyone else in it. The server raises it whenever members lack `see_member_directory` and at least one default channel exists. The browser shows it in **Settings → Server** and in **Roles and permissions**; the CLI prints it with `permissions show` and after every `permissions set` and `permissions preset`. It is also in the answer to marking a channel default, and `conch admin channel settings --default on <channel>` prints it then; the browser's **Make default channel** does not show it at that moment, so look in **Server**.

Applying a preset has live effects. Turning `use_voice` off removes everyone it now withholds from voice at once, turning `share_screen` off ends their shares, and turning `use_camera` off ends their cameras. Turning `make_member_links` off doesn't close the links members already made: they stop working while the maker lacks the permission, and work again when it comes back. Nobody's sign-in changes.

### A personal server

You hold the conversations; the people on the server shouldn't see each other. Do this once, in this order.

1. **Apply the personal preset** (above). Do it before anyone joins.
2. **Make sure no channel is a default channel.** A default channel is where every new person starts, and the people in it see each other. In the browser, **Settings → Channels** marks default channels; for one, open its **⋯** menu and choose **Stop being a default channel**. With the CLI, `conch admin join status` lists "default channels", and `conch admin channel settings --default off <channel>` removes one. A fresh server has none. With no default channel, `conch admin join status` and **Settings → Server → Joining** warn that a member link lands nobody anywhere: on a personal server that is what you want, step 5 puts people in. Also keep **open sign-up** off (**Settings → Server → Joining**; `conch admin join set --open-signup off`).
3. **Create one private channel for each conversation.** Browser: **Settings → Channels → New channel**, private. CLI: `conch admin channel create alice-chat` (you are its only member, and it is private). Do this first: the browser's **Add a person** button (next step) is offered only once you are in at least one channel.
4. **Make a member link for each person.** A member link names no channel: whoever opens it picks a name and a password, becomes a member and lands in the server's default channels, which on a personal server are none. As the owner you are never held back by `make_member_links`. In the browser, **Settings → Members → Add a person** opens the **Invite someone** panel; choose how long the link lasts and how many people may use it (one, for a personal server), then **Make link**. It shows the link once. With the CLI:

   ```sh
   conch admin invite create --uses 1
   ```

   It prints the link once. Send it to Alice. She opens it and lands in no channel: her list is empty until step 5.
5. **Add each person to their channel.** Browser: **Settings → Channels**, the channel's **⋯** menu, **Add person…**, then their name; or **Settings → Members**, the person's **⋯** menu, **Add to a channel…**. CLI: `conch admin member add alice-chat --name alice` (or `conch admin member add alice-chat <id>`). On a personal server **Add person…** is the first item in the menu. The browser and `--name` add the person without the right to decide the channel's approvals; adding by id from the CLI gives that right unless you add `--no-decide`.

   To bring Alice into another conversation later, add her to that channel the same way.

What a member sees now: themself and the people in the channels they are in. They can't create channels, make links, or list the directory. To check, sign in as a member and run `conch admin directory members`. It lists the member and whoever shares a channel with them (you, in a one-to-one channel). A second member who isn't in the channel isn't listed.

Three things to keep in mind.

- Two people in the same channel see each other, so keep one channel per conversation.
- The owner and every operator see everyone, in **Members**.
- **The preset does not turn off the channel directory.** There is no permission for it. A member's **Browse channels** (REST: `GET /v1/directory/channels`) still lists every public channel on the server, and any member may join one, after which they see its people. Members can't create channels under this preset, so this only matters if you or an operator make a public channel. On a personal server, keep every channel private.

### A group server

The defaults are the group server: members create public and private channels, make member links and see the directory, and use voice. Apply **Group server** if it was ever changed (`conch admin permissions preset group`).

1. **Make one or more public default channels.** New people start in them. Browser: **Channels → New channel** (public), then its **⋯** menu, **Make default channel**. CLI: `conch admin channel create lobby`, then `conch admin channel settings lobby --public --default on`. A default channel must be public, and a private channel can't be one.
2. **Decide how people get in.** With **member links** (the default) anyone allowed to make one can bring someone in; **Settings → Server → Joining** has **Default link expiry (hours, 0 for none)**, and `conch admin join set --link-expiry <duration|0>` sets it. With **open sign-up** ("anyone who asks at the server's address can make an account") nobody needs a link; turn it on only if you mean it. It needs a default channel. `conch admin join set --open-signup on`.
3. **Promote helpers** to operator (above). An operator manages people and channels but can't change server settings or read the audit log unless you give it those. If you want a helper to adjust what members may do, give that operator `change_server_settings` in **Roles and permissions** (`conch admin permissions set change_server_settings --operators on`).
4. **Members** make their own channels (each becomes its manager), join public channels from the channel directory, and bring in friends with member links.

### Member links and the invite brake

A member link is how a person joins. It names no one: whoever opens it picks a name and a password and becomes a full member, in the server's default channels. The browser's link always lands people there: the **Invite someone** panel says "A link lands the people who use it in the server's default channels." and offers no channel. A member can make one only while `make_member_links` is on for members and the member-link switch on them is on. An operator can make one while the operators' cell is on. From the CLI (and the API), a holder of `manage_channels` can name other channels with `--landing-channel`, repeated for each one, in place of the default channels; that is not offered in the browser.

```sh
conch admin invite create --expires 24h --uses 5 --landing-channel alice-chat
conch admin invite list [--state open|used|expired|closed] [--created-by <id>]
conch admin invite joiners <id>        # who came in through link <id>
conch admin invite close <id>
```

The browser has two views of the same links. **Settings → Server → Joining → Invites** lists them: "Link #N · made by <name>", its uses, how many joiners, when it expires or closed, the channels it lands in, **Show joiners of link N** (who came in) and **Close link N**. Someone without `manage_members` sees only the links they made. The **Invite someone** panel (the lobby for a member; **Settings → Members → Add a person** for an owner or operator; a channel's **⋯** menu has **Invite people**) lists your own links with **Joiners (N)** and **Close**. **Disable joiners**, which signs out and disables the people a link brought in (and closes the link), is in that panel and needs `manage_members`; the Settings list doesn't have it.

Three brakes:

- **Pause everything.** **Settings → Server → Joining → Pause all invites**, or `conch admin invite pause` (`resume`, `status`). While paused, no one can make, preview or use an invite, including links already sent. Restoring a backup pauses them on purpose.
- **One person.** **Stop member links** in their **⋯** menu, or `conch admin principal invite-permission <id> off`. It closes their open links. It doesn't bind operators.
- **Everyone.** Turn `make_member_links` off for members (the personal preset does).

Members who join through **open sign-up** hold the member-link switch on, so, while members hold `make_member_links`, they can make links straight away. They **keep it after open sign-up is switched off**. If that isn't what you want, close their links (`conch admin invite close <id>`), switch their link-making off (`conch admin principal invite-permission <id> off`), or remove the people a link brought in (`conch admin invite disable-joiners <id>`, which asks first; **Disable joiners** in the panel). `conch admin directory members` shows `may-invite on` and no inviter for open sign-up accounts.

### Channel privacy and default channels

A **private** channel is omitted from other members' channel directory. Its messages, files and voice need membership. Holders of `manage_channels` can still list and administer it, as [operator access](#what-an-operator-can-and-cant-see) explains. A **public** channel is listed in the channel directory for every member, who can join and leave it. A **default** channel is public, and every new person starts in it. Making a channel public or private keeps everyone who is already in it.

**Settings → Channels** (for a holder of `manage_channels`, which includes every owner) lists every channel with a lock for private, a default mark, how many people are in it and who manages it. **New channel** makes a public or private channel with no daily cap. Each row's **⋯** menu has **Add person…**, **Rename channel…**, **People and managers…** (remove a person, **Make manager** or **Stop managing**), **Make public** or **Make private**, and **Make default channel** or **Stop being a default channel**.

```sh
conch channels list --all                         # every channel, with member counts (manage_channels)
conch admin channel create <name>                 # private, you are its only member
conch admin channel settings <channel> --public|--private [--default on|off]
conch admin member add|remove <channel> <id>
conch admin member manage [--off] <channel> <id>  # make or stop being a channel manager
conch channels create [--private] <name>          # what a person with the create permission does
```

A channel manager who isn't an operator manages their own channels from the channel itself, not from here.

### Hiding members from each other

The design called this `hide_members`. Shipped servers use the members'
`see_member_directory` permission instead; the old setting is mapped to its
opposite on upgrade.

The switch is `see_member_directory` for members. With it off, a member sees three kinds of people: themself, the people in the channels they are in, and the people who joined through a member link they made. Everything else about anyone else (that they exist, their name) is not shown. Owners and operators always see everyone, whatever the cell says.

A member sees each person's id, name, role and join time and nothing else. Who invited whom is shown only to holders of `manage_members` (**Members** shows "invited by <name>" to them), so a member can't see who invited them, or who invited anyone else. Someone who holds a link's code sees the name of the person who made it when they open it, as before.

Browser: **Settings → Server** has the line "Members see: only people they share a channel with" (or "everyone"), which links to the cell in **Roles and permissions**. CLI: `conch admin permissions set see_member_directory --members off`, or apply the personal preset.

What it doesn't hide: the people in a channel you are both in (a default channel included), and the people who joined through a link the viewer **made**. The server records who made the link a person joined through when they join, and never clears it, so turning link-making off later does not end it. On a server where the personal preset was applied before anyone joined, there are none. On one that ran the defaults first, each member who made a link still sees the people it brought in, and that stays so. **Members** (or `conch admin principal list`) shows "invited by" to holders of `manage_members`, so you can find them. Hiding members is only as strong as your channels.

Everyone in a channel sees everyone else in it. A default channel, or a public
channel two people both join, shows them to each other. Hiding members cannot keep
apart private conversations whose people also share such a channel. It hides people
in Conch's answers; it does not make anyone anonymous or separate processes on the
server. A message's author stays named in its channel's history.

While members are hidden, use **no default channels** and **one private channel per
space** whose people should not see each other. Follow the [personal server steps](#a-personal-server).
Zero default channels is allowed: a member-link joiner starts in no channel until
added, or until they join a public channel. A default channel draws the
`default_channel_exposes_members` warning; it is not refused.

### What an operator can and can't see

An operator never reads a channel it isn't a member of: membership is the boundary for messages, and operators get no exemption. What operators can do beyond that is what you grant:

- **With `manage_channels`** (on for operators by default) an operator sees every channel's name, adds and removes members, and so can add itself to a private channel. With `act_as_members` (also on by default) it can sign in as any member who isn't an owner. Treat an operator with these as someone you trust with the content. The way to hold an operator back is to turn these permissions off for the operators' column, not to rely on the title.
- **Without `manage_channels`** an operator sees only the channels it is in, with a count of the rest: invites, hooks and the joining settings show "and N other channels" (`other_channels`, `other_hooks`, `other_open_channels`), and a request for a channel it isn't in answers as if it didn't exist. Three things remain. The first two show it a channel's name, the third only that channels exist:
  - **The audit export.** The trail names every member and every private channel. That is why `read_audit_log` is off for operators by default and needs a recent sign-in from anyone but an owner. Turning it on for operators shows them everything.
  - **A problem report's own text.** The server stores a report as written and only attaches a `redact` list of names at read time. The diagnostics it adds carry no names. The three free-text answers are the reporter's own words and may name a channel or a person, and every operator can read reports.
  - **The counts**, which say that channels exist but not which. This one gives no name.
- Whoever has the host can read the database. See [Trust and scope](../README.md#trust-and-scope).

### Taking `act_as_members` away

Turning `act_as_members` off for operators stops new credentials, sign-in links and hooks for someone else, and suspends the sign-in, reset and enrolment links they already made but nobody has used (they work again if you turn it back on). It **does not end** credentials and hooks an operator already issued to other people: those belong to their target and stay live.

The change tells you what it left. In the browser the list "Still working after this change" appears under the matrix, with **Sign out everywhere** and **Revoke** beside each item. On the CLI, `conch admin permissions set act_as_members --operators off` prints each one:

```
  credential 12 (principal 4, issued by 2): conch admin credential revoke 12
  hook 3 (principal 4, issued by 2): conch admin hook revoke 3
```

Revoke what you don't want. The list is only in the answer to that change; it isn't kept, so revoke from it before you leave the page or the terminal. It comes from the audit trail: a credential or hook issued before the trail recorded who issued it (before 2026-10-08) isn't on it, and neither are an owner's own, since owners keep `act_as_members`. The same gap exists when you **demote or disable an operator**: what they issued stays live until you revoke it. The audit trail names the issuer of every credential and hook (`credential_created`, `credential_rotated`, `hook_created`), so `conch audit export` finds them. Recording the issuer on the credential itself, and listing these on demotion and disable, is [#1024](https://github.com/njdaniel/conch/issues/1024), not built yet.

### Camera video and bandwidth

Cameras use the droplet's bandwidth and CPU, and **how much a droplet carries is not yet measured**. Nothing here is a capacity.

What there is is the browser's presets, which are the SDK's figures and not measurements ([README, Camera](../README.md#camera)):

- A computer sends a top layer of at most 1.2 Mbps, with smaller layers of 450 kbps and 150 kbps; a phone sends at most 450 kbps, with one smaller layer of 150 kbps.
- For scale: in a one-to-one call each side sends at most about 1.8 Mbps across all its layers and receives one layer. In a call with 9 cameras each person receives 8 small layers, and LiveKit sends roughly 9 × 8 streams.
- LiveKit sends only the layers viewers ask for, so a 3 × 3 grid of small tiles receives the 180p or 360p layers, and a sender stops encoding a layer nobody receives. `conchd` limits no bitrate or resolution.
- The cap of 9 cameras per room is a constant in `conchd`, not a setting, and it stays what it is until the measurement says what a droplet carries.

To find out for your own droplet, watch **What to watch** (below) while people use cameras: the droplet's bandwidth graph, and `htop` and `docker stats livekit` for CPU. The method for measuring one-to-one and 9-camera calls, with and without a share, is the template [docs/reports/TEMPLATE-camera-bandwidth.md](reports/TEMPLATE-camera-bandwidth.md). A tool that simulates 9 cameras on a load generator is proposed and does not exist yet, so until it does a measurement uses as many real devices as there are, and says it is partial.

## A second instance

Every further server on the droplet (plan §3) is one command on your workstation, `ops/conch-server` in a checkout of this repository, with `beta` as the name:

```sh
ops/conch-server new beta                 # a normal, long-lived server
ops/conch-server new demo --temporary     # a short-lived one (Temporary servers, below)
```

On the workstation it needs `ssh` with the droplet's root key, `doctl` signed in to the account that holds the domain (`doctl auth init --context conch`), `curl`, and this repository's tags (`git fetch --tags`). Its settings are in `~/.config/conch/server.env`, which holds no secret. Make it mode 0600: `conch-server` refuses a file anyone else can write. It is read line by line, never sourced, and each value is checked:

```sh
# root@ and the droplet's IPv4 (203.0.113.7 stands for it here)
CONCH_SERVER_HOST=root@203.0.113.7
CONCH_SERVER_DOMAIN=your.domain
CONCH_SERVER_SSH_KEY=~/.ssh/conch-alpha
CONCH_SERVER_DOCTL_CONTEXT=conch
# Optional: the first operator's name
CONCH_SERVER_OPERATOR=your-name
```

Comments go on lines of their own: a value runs to the end of its line, so anything after it would be part of it.

`conch-server new <name>` does this, in order:

1. **The droplet's scripts.** It reads the tag the droplet runs (where `/opt/conch/current` points) and installs that tag's `ops/deploy/*.sh` in `/opt/conch/bin` (0755, root) with `git show <tag>:...`, replacing only a file whose SHA-256 differs, so the scripts match the binary. A tag you have not fetched stops it, and says so. A release from before these scripts has no `add-instance.sh`: `--scripts-ref <ref>` takes the scripts from a newer commit instead, and says that they may not match the binary. `conch-server sync` does this step alone.
2. **DNS.** It reads the droplet's public IPv4 and IPv6 from DigitalOcean's metadata service and creates the `A` and `AAAA` records for `beta.your.domain` (TTL 300) with `doctl`. A record that already points at the droplet is kept, and one that points anywhere else stops it before anything is made. Then it waits, up to 15 minutes, until Cloudflare's and Google's resolvers (over HTTPS) both return them; [DNS](#1-dns) explains why a name is not looked up before its record exists.
3. **`add-instance.sh`** on the droplet, over `ssh -t`, so its question and the setup link reach your terminal only. It checks everything first and changes nothing until every check passes:
   - the name: lower-case letters, digits and `-`, starting with a letter, not ending in `-`, at most 26 characters (so `conch-<name>` is a valid user name and `<name>-` a valid room prefix), and none of the names the droplet's shared parts use: `livekit`, `turn`, `backup`, `drill`, `www`, `removed`;
   - that nothing of that name exists: no `conch-<name>` user, env file, data or backup directory, rollback record, LiveKit key or Caddy block;
   - a free port: the first from 8081 that no env file names and nothing listens on (`--port` chooses one);
   - that the name resolves to the droplet's addresses, and only to them. If not, it prints the records to create.

   Then it makes, each step undone if a later one fails: the `conch-<name>` user and `/var/lib/conch/<name>` (0700); a new LiveKit key pair (`<name>-<8 hex>: <secret>`, as in step 4), appended to `/etc/conch/livekit-keys`; `/etc/conch/<name>.env` (0600, root), with every setting of step 5, `CONCHD_TRUSTED_PROXIES` and a fresh `CONCHD_TOTP_KEY` included; the systemd-tmpfiles rule for [removed servers](#removing-a-server); and a Caddy block for `beta.your.domain` between `# conch-instance beta begin` and `# conch-instance beta end` lines, checked with `caddy validate` before it replaces the Caddyfile, then `systemctl reload caddy`. Then it restarts LiveKit, makes the first operator, starts `conchd@beta` and `conch-backup@beta.timer`, and waits for `/healthz` to answer `ok` on loopback and then through Caddy. If a step fails, it undoes what this run made, newest first, and says what it undid, and `conch-server` deletes the DNS records it made. The undo first stops `conchd@beta` and its backup timer and unit, and deletes nothing while anything of `conch-beta` may still run: if that cannot be shown, or an undo step itself fails, it names what is left and exits 5, and the records are kept. Once `conchd` answers on loopback the server is kept: if only the check through Caddy fails (a certificate not issued yet, say), or the run is interrupted then, the server is left running, nothing is undone, the records are kept, and both commands exit 4. An interrupted run, or a lost connection, also keeps the records, since what the droplet holds is then not known. `add-instance.sh` needs a terminal for its output, since the setup link is printed there (`ssh -t` gives it one).
4. It prints the server's address.

**No secret leaves the droplet.** The LiveKit secret and the TOTP key are made on the droplet from `/dev/urandom` and written into the env file (and the secret into LiveKit's key file) only: never printed, logged or put on a command line. Keep a copy of `CONCHD_TOTP_KEY` as step 5 says, if the server is one you will keep.

**Restarting LiveKit drops every server's voice calls** for a few seconds, since LiveKit reads its keys only at start. `add-instance.sh` names the servers whose calls drop and asks first. `--yes` answers for it, and with no terminal and no `--yes` it refuses.

**The first operator.** It runs `conchd operator bootstrap --link` (step 6) as `conch-beta`, which prints a **setup link** once, on your terminal and nowhere else: not in a file or the journal. Open it in a browser within ten minutes, press the button, and choose a password. That is the only step left. The operator is named by `--operator`, else `CONCH_SERVER_OPERATOR`, else `operator`. No script credential is made; make one in the browser afterwards if you want `conch admin` from your workstation.

**The authenticator.** A normal server asks every operator for an authenticator code (`CONCHD_OPERATOR_TOTP` unset, which is `required`; step 5). `conch-server new beta --operator-totp optional` makes one whose operators may sign in with a password alone, as alpha's do, without the temporary banner and with off-site backups still required ([ADR-012](adr/ADR-012-optional-operator-totp.md)). A temporary server is `optional` already, and `--temporary --operator-totp required` is refused, since `conchd` would not start it.

**Then, for a normal server,** set up its off-site backup and do the restore drill before anyone joins (steps 8 and 9). Its timer is enabled already, and fails loudly until there is an off-site copy.

The other commands:

- `conch-server list`: every server's name, port, mode, operator setting, whether `conchd@<name>` is active, and what its `/healthz` answers.
- `conch-server upgrade <tag>`: `conch-upgrade.sh <tag>` with every instance on the droplet named, since they share one binary (step 11).
- `conch-server rm <name>`: [Removing a server](#removing-a-server).

On the droplet, as root, once the records resolve, the same is:

```sh
/opt/conch/bin/add-instance.sh beta your.domain [--port N] [--temporary] [--operator-totp optional|required] [--operator <name>] [--yes] [--dry-run]
/opt/conch/bin/list-instances.sh
```

`--dry-run` runs every check, prints what it would do, and changes nothing.

## Removing a server

```sh
ops/conch-server rm demo                    # a temporary server
ops/conch-server rm beta --force-normal     # a normal one, and everything its people keep there
```

It runs `remove-instance.sh` on the droplet over `ssh -t`, then deletes the server's `A` and `AAAA` records that point at the droplet (one that points elsewhere is left, and named). `remove-instance.sh <name> [--force-normal] [--no-backup] [--purge-backups]` follows [ADR-011](adr/ADR-011-temporary-servers.md) item 2.4:

- `conch-server rm` refuses a name the droplet does not list, unless `--force-normal` is given (a half-removed server may have lost its env file), and `remove-instance.sh` refuses a name of which nothing is on the droplet, with exit 2, so a typo never deletes the DNS records of that name.
- Before anything changes, it checks the name against the rule above and refuses a data directory, env file, backup directory or rollback record that is a symlink. It reads the mode from the env file only: it is a temporary server only if every line that names `CONCHD_MODE` is exactly `CONCHD_MODE=temporary`. Any other form systemd might read, such as an indented line, spaces around `=` or quotes, and a missing or unreadable file, make it a normal one. A normal server is refused without `--force-normal`. It works out the Caddyfile and LiveKit's key file without the server and checks the Caddyfile with `caddy validate`.
- It lists what it will delete and asks you to **type the name** back, every time. There is no `--yes`, and with no terminal it refuses.
- It stops and disables `conchd@<name>` and its backup timer, and checks that nothing of that server still runs, before it deletes anything.
- It takes a final local archive (`conch-backup.sh <name> --local-only`) unless `--no-backup`. If that fails, it stops there, with the server stopped and nothing deleted.
- It removes the server's Caddy block and reloads Caddy, then its LiveKit key and restarts LiveKit (the other servers' calls drop for a few seconds). Either file is put back, and reloaded or restarted again, if that fails, so no other server is taken down.
- It deletes the env file, the data directory (and any `<data>.rolled-back-*` beside it), the rollback record and the user `conch-<name>`. It never touches the shared `conch-backup` and `conch-drill` users.
- It moves `/var/backups/conch/<name>`, holding the final archive and the pre-upgrade ones, to `/var/backups/conch/removed/<name>-<time>/`, as `conch-backup`. There the rule `add-instance.sh` installed, `/etc/tmpfiles.d/conch-removed.conf`, has systemd-tmpfiles delete it after 14 days. `--purge-backups` deletes it at once. Until then it can be restored as in [Restoring for real](#10-restoring-for-real), with the server's `CONCHD_TOTP_KEY` if you kept one.
- Last, it prints the DNS records to delete (`conch-server rm` deletes them).

**alpha, and any server set up by hand,** has a Caddy block without the `# conch-instance <name> begin` and `end` lines. `remove-instance.sh` does not guess where such a block ends. If any line that is not a comment still names `<name>.` after the marked block is taken out, it refuses before changing anything and says so. Remove that block by hand (or put the two marker lines around it), run `caddy validate` and `systemctl reload caddy`, and run the removal again.

## Temporary servers

For a short-lived server holding nothing anyone needs to keep, `conch-server new <name> --temporary` ([A second instance](#a-second-instance)) makes one: its env file says `CONCHD_MODE=temporary`, and it gets no backup timer ([ADR-011](adr/ADR-011-temporary-servers.md), items 2 and 4). To make an existing server temporary, set `CONCHD_MODE=temporary` in its env file, restart `conchd@<name>`, and disable `conch-backup@<name>.timer`. Operators without an enrolled authenticator may sign in with a password alone; those who enrolled still need a code. The sign-in page and lobby show "Temporary server: no backups, and it may be removed. Don't keep anything here you need." If run, backups make a local archive only, even with an off-site destination configured; pre-upgrade backups are unchanged. No restore drill is needed before testers join. `conch-server rm <name>` removes it ([Removing a server](#removing-a-server)). Guest policy, invites, channel isolation, approvals, rate limits, password rules, script credentials and auditing keep their usual rules; members sign in as usual. There are no guests for now.

For password-only operators on a normal server, use `CONCHD_OPERATOR_TOTP=optional` instead ([ADR-012](adr/ADR-012-optional-operator-totp.md)). It adds no temporary banner and **keeps off-site backups required**.

To switch back, set `CONCHD_MODE=normal` and restart; configure off-site backups and complete the restore drill (steps 8 and 9), then enable the backup timer. Under [ADR-012 item 4](adr/ADR-012-optional-operator-totp.md#decision), revocation happens only when password-only operator sign-in stops being allowed: normal mode with `CONCHD_OPERATOR_TOTP=required` (or unset). At that start, every operator browser session is revoked, all recent-authentication proofs are cleared, and every pending login and unfinished setup credential is spent. Operators without authenticators must finish setup with a host-issued token. Switching to normal with `optional` keeps sessions and proofs. Script credentials survive either switch.

## A member who forgot their password

There is no self-service recovery and no email: conchd sends no mail and needs no mail service. An operator recovers a member's account by hand, with a one-time **reset link** (design: [guests-and-accounts.md §6.1](design/guests-and-accounts.md)). Operators do not recover this way; their own recovery is offline, on the host.

1. **Check who is asking**, out of band: in person, on a call, or over a channel you already trust for that person. A message in Conch from "their" account proves nothing; that account may be the one that was lost.
2. **Make the reset link** for their principal id (`conch admin principal list` shows ids). The CLI flag for it is still to come (#713), so call the API. The token goes in on standard input, never on a command line:

   ```sh
   read -rsp 'operator token: ' tok && echo
   printf 'Authorization: Bearer %s\n' "$tok" | curl -sS --fail-with-body -H @- -H 'Content-Type: application/json' \
     -d '{"purpose":"reset"}' https://alpha.your.domain/v1/principals/<id>/sign-in-links
   unset tok
   ```

   The answer carries the link's `code`, shown this once. It works once, for ten minutes (`"expires_at"` may ask for up to an hour), and only for a password reset: it signs nobody in and is good for nothing else. It is refused for an operator, a guest, a disabled or a hidden account.
3. **Give the member the code** over the same trusted channel, and have them choose a new password with it at once: `POST /v1/password/reset` with `{"code": "...", "password": "..."}`, over HTTPS (the browser's page for it, and a `conch` command that reads the code from standard input, are #711 and #713).
4. **What it does.** The new password replaces the old one, and everything of theirs that was signed in ends: every credential, every open sign-in, reset or enrollment link, any pending sign-in and any recent-password proof. Their open sockets close and they leave voice. No session is issued: they sign in with the new password, as on a new device.

A link that leaked before it was used: close it with `conch admin sign-in-link close <link id>`. A member who joined before passwords existed and still has a working session sets a first password the same way, with `{"purpose":"enrollment"}` in step 2: they use that code on their own session (`POST /v1/password/mine` with `{"password": "...", "proof": "..."}`), which keeps them signed in there and ends nothing else. If they have lost their browser too, a reset link sets their first password.

The audit log records each step, never a password or a code: `password_reset_issued` or `enrollment_issued` when you make the link; then, for a reset, `password_reset`, `auth_generation_bumped` and one `credential_revoked` for each sign-in that ended, and for an enrollment, `password_set` and `credential_rotated`.

## What to watch

- `systemctl --failed`, and `systemctl list-timers 'conch-backup@*'`: a failed backup unit means no off-site copy last night.
- `journalctl -u conchd@alpha -u livekit -u caddy --since today`.
- Memory and CPU while people are on voice (`htop`, `docker stats livekit`): the 4 GB / 2 vCPU start is a guess until A15 measures it. The droplet's graphs in the DigitalOcean console show bandwidth, which is what a screen share spends.
- Disk: `df -h /var/lib/conch /var/backups/conch`; the data directory grows with uploads.
- Certificates: Caddy renews on its own; `curl -vI https://alpha.your.domain 2>&1 | grep expire`.
- `/healthz` from outside (an uptime check): `status` and `db` must be `ok`.
- Voice failing for some people only: that is usually UDP blocked and TURN/TLS not reachable. Test TURN on `5349`.

## Files in this directory

| File | Role |
| --- | --- |
| `ops/deploy/conchd@.service` | the `conchd` unit template; `%i` is the instance |
| `ops/deploy/instance.env.example` | one env file per instance (placeholders only) |
| `ops/deploy/Caddyfile` | public TLS and the reverse proxies |
| `ops/deploy/livekit.yaml`, `livekit.service` | LiveKit's settings and its Docker unit |
| `ops/deploy/livekit-cert.path`, `livekit-cert.service` | restart LiveKit when Caddy renews the TURN certificate |
| `ops/deploy/firewall.sh` | the `ufw` rules |
| `ops/deploy/conch-backup.sh`, `conch-backup@.service`, `conch-backup@.timer` | the nightly backup |
| `ops/deploy/conch-restore.sh` | the restore drill |
| `ops/deploy/conch-upgrade.sh` | upgrade and rollback |
| `ops/deploy/add-instance.sh`, `remove-instance.sh`, `list-instances.sh` | add, remove and list the servers on the droplet |
| `ops/conch-server` | the workstation command: `new`, `rm`, `list`, `upgrade`, `sync` |
| `scripts/deploy-scripts-test.sh` | the test of all of the above that needs no droplet |

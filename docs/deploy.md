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
- **Guests** join again as new guests; an ended visit never comes back.

**What a restore does not undo.** Apart from sign-ins, the restored data is the server as it was when the backup was taken, and every change made after it is lost, including ones made for safety. Nothing can reconstruct them: their record went with the data (if the database in `.broken-*` still opens, its audit log shows what happened). Before people return, check and redo what matters:

- invites closed since are open again: `conch admin invite list --state open`, and `conch admin invite pause` until you have looked;
- webhook hooks deleted since work again at their old URLs: `GET /v1/hooks`, and `DELETE /v1/hooks/{id}` for each that should be gone;
- passwords and authenticators are as the backup has them: a password reset, a password change or a `conchd operator recover` made since is undone, and the old password or authenticator works again. Do each again;
- approvals resolved since are pending again, and can be decided again;
- invites are paused by the restore: once you have closed the ones that should be closed, resume them with `conch admin invite resume`;
- guest joining is paused by the restore: once the joining settings and open channels are as they should be (`GET /v1/join/settings`), resume it with `PUT /v1/join/settings` and `{"joining_paused": false}`;
- people disabled or hidden since are enabled again, and roles, invite and voice rights, channel memberships, agent manifests, open channels and the joining and guest settings are as the backup has them.

**The TOTP key is not in the archive.** A backup holds the operators' authenticator secrets only encrypted under `CONCHD_TOTP_KEY`, which lives in `/etc/conch/alpha.env` and is never backed up with the data. Restoring onto the same droplet keeps the env file, and operators sign in as before. Restoring onto a new one, put the key back from your password manager into the new env file before the first start. If the key is lost, the data is not: start without it (or with a new one; `conchd` logs at start that the key does not open the stored secrets), and recover each operator with `conchd operator recover` (step 6), which removes the authenticator the old key protected and lets them enrol a new one. Members and messages do not depend on the key.

## 11. Upgrading and rolling back

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

For a short-lived server holding nothing anyone needs to keep, `conch-server new <name> --temporary` ([A second instance](#a-second-instance)) makes one: its env file says `CONCHD_MODE=temporary`, and it gets no backup timer ([ADR-011](adr/ADR-011-temporary-servers.md), items 2 and 4). To make an existing server temporary, set `CONCHD_MODE=temporary` in its env file, restart `conchd@<name>`, and disable `conch-backup@<name>.timer`. Operators without an enrolled authenticator may sign in with a password alone; those who enrolled still need a code. The sign-in page and lobby show "Temporary server: no backups, and it may be removed. Don't keep anything here you need." If run, backups make a local archive only, even with an off-site destination configured; pre-upgrade backups are unchanged. No restore drill is needed before testers join. `conch-server rm <name>` removes it ([Removing a server](#removing-a-server)). Guest policy, invites, channel isolation, approvals, rate limits, password rules, script credentials and auditing keep their usual rules; members and guests sign in as usual.

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

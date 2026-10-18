# Deploying Conch on one droplet

This runbook takes an empty Ubuntu 24.04 droplet to a public `conchd` instance with voice, nightly off-site backups, and the steps to upgrade, roll back, restore and add a second instance. It is the deployment of [ADR-008](adr/ADR-008-private-communications-alpha.md) and the plan in [docs/design/private-alpha.md](design/private-alpha.md) (decision O7).

One droplet runs three things:

- `conchd`, one instance per systemd unit `conchd@<name>`, each as its own user, on a loopback port;
- Caddy, which holds the public names, gets and renews their certificates, and proxies to `conchd` and to LiveKit's signalling port;
- LiveKit, in Docker from the digest-pinned image the manual uses, for the audio.

Everything is parameterised by an **instance name** (`alpha` below) and a **domain** (`example.com` below, a placeholder: replace it everywhere). A second server is the steps in [A second instance](#a-second-instance), not a new design.

**Status.** The scripts, the units and the backup, restore, upgrade and rollback paths are exercised by `scripts/deploy-scripts-test.sh` (run by `make deploy-test` and in CI) against a real `conchd`, with a stand-in for `systemctl`. That test runs as one ordinary user, so the split between root, the instance users and `conch-backup` described below (`runuser`, `setpriv`, the cleared environment) is not part of it; the parsing of the rollback record, the check of the archive path, the refusal of a symlinked archive, the stripping of control characters and `setsid` are. The Caddyfile, LiveKit's TURN/TLS settings, the firewall and the units under a real systemd were **not** run on a droplet when this was written: the first run on the real droplet (issues A15 and A16) is where they are proved, and this page should be corrected from what that finds.

## Users and who can read what

| User | Runs | Can read |
| --- | --- | --- |
| `conch-<instance>` (one per instance) | `conchd@<instance>`, and the snapshot step of that instance's backup | its own data directory, `/var/lib/conch/<instance>`, only. `conchd` keeps its database and uploads at mode 0600, and the unit hides `/etc/conch`, `/var/backups/conch`, Caddy's state and every other instance's directory from it. |
| `conch-backup` | the archive, retention and off-site steps of every backup | `/var/backups/conch`, and the off-site settings and credentials (`/etc/conch/backup.env`, `/etc/conch/rclone.conf`), which it alone owns. It cannot read any instance's live data. |
| `conch-drill` | the restore drill (step 9), in a scratch directory of its own | only what it is handed for one drill. It owns no other file and holds no credential. |
| root | the backup unit (only to hand the snapshot from the instance user to `conch-backup`), the upgrade script, the steps below | everything, but the scripts and the steps below never open a file `conch-backup` owns: the archives are checked and read as `conch-backup`, and handed on as a stream. The env files (`/etc/conch/<instance>.env`) and LiveKit's keys are root's, mode 0600. |

So a compromised `conchd` cannot read another instance, the backups or the off-site credentials, and a leaked off-site credential does not open the live data. Every step the scripts run as one of these users runs in its own session, so it never has root's terminal as its controlling terminal, with none of root's environment, and what it prints is stripped of control characters before it reaches root's terminal.

## What you need

- A DigitalOcean droplet, Ubuntu 24.04, **4 GB RAM / 2 vCPU to start** (decision O7; the final size follows the measurement in A15), with IPv4.
- A domain you control, and permission to add DNS records.
- A DigitalOcean Space (or any S3-compatible bucket) for off-site backups, with **versioning** turned on (step 8), and two access keys: a full-access one that stays on your workstation, and a limited one for that bucket only, for the droplet.
- A workstation with `ssh`, and somewhere safe (a password manager) for the operator token and the backup encryption passwords.

## 1. DNS

Create `A` records (and `AAAA` if the droplet has IPv6) pointing at the droplet for:

| Name | Used for |
| --- | --- |
| `alpha.example.com` | the `alpha` instance: web client, API, WebSockets |
| `livekit.example.com` | LiveKit signalling (WebSocket over TLS) |
| `turn.example.com` | LiveKit's TURN/TLS fallback (UDP-blocked networks) |

Caddy cannot get a certificate for a name that does not resolve yet, so wait for the records before step 7.

## 2. The host

As root on the droplet:

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
```

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

LiveKit runs in Docker, from the image the manual pins by digest (`livekit/livekit-server:v1.13.7@sha256:...`). The version is named in `ops/deploy/livekit.service` and in the manual: change both together, after reading the release notes.

Each instance has its own LiveKit key pair ([manual](manual.md#run-livekit-beside-conchd): one key per server). Keys live in one file, one `<key>: <secret>` line per instance, mode 0600:

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

One env file per instance, mode 0600, root-owned (systemd reads it as root, before it hides `/etc/conch` from the service). It holds the LiveKit secret, so never put its values in a unit file, a Caddyfile, a command line or the repository:

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
- `CONCHD_TRUSTED_PROXIES=127.0.0.1/32,::1/128` (Caddy, on loopback), as the example has it, so the invite routes' rate limits see each client's address rather than Caddy's.

## 6. The first operator

Create the first operator **before** the first start, over SSH, with the token going to your terminal and nowhere on the droplet:

```sh
install -d -m 0700 -o conch-alpha -g conch-alpha /var/lib/conch/alpha
sudo -u conch-alpha /opt/conch/current/conchd bootstrap-operator --data /var/lib/conch/alpha --name your-name
```

It prints the token once on stdout. Copy it into your password manager from the terminal; do not redirect it to a file on the droplet, and do not leave it in your scrollback on a shared machine. `bootstrap-operator` refuses to run a second time. From then on you administer the server with `conch admin` from your own machine, using this token (`CONCH_SERVER=https://alpha.your.domain`).

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

Open `https://alpha.your.domain/app/`, sign in with the operator token, and create the first channel. Then check voice from a second device on a different network, and once from a network that blocks UDP if you have one.

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

It checks the archive's SHA-256, unpacks it, runs `PRAGMA integrity_check`, checks that every uploaded file the database names is present with its stored SHA-256, starts `conchd` on the copy (loopback, a spare port, no web client, no LiveKit) and requires `/healthz` to say `"status":"ok","db":"ok"`, then stops it and prints the schema version, the message count and the number of files.

Do it also from the **off-site** copy once, since that is the copy that would be needed. `conch-backup` downloads it, and `rclone` decrypts on the way down:

```sh
dl=$(as conch-backup mktemp -d </dev/null | clean)
as conch-backup env RCLONE_CONFIG=/etc/conch/rclone.conf rclone copy conch-crypt:conch/alpha/ "$dl" --include "${latest##*/}*" </dev/null | clean
drill "$dl" "${latest##*/}"
rm -rf "$dl"
```

Once, also check that the crypt passwords in your password manager are the right ones: on your workstation, with a `rclone.conf` made from them and the read-only key (step 8), list `conch-crypt:conch/alpha/`. Names that decrypt mean the passwords are right.

## 10. Restoring for real

Use this when the data is lost or corrupt. The archive's `conch.db` and `files/` go back as the instance's data directory. With the helpers from step 9 in root's shell:

```sh
(
  set -euo pipefail
  cd /
  a=conch-alpha-YYYYMMDDTHHMMSSZ.tar.gz    # the archive to restore
  as conch-backup sh -c 'cd /var/backups/conch/alpha && sha256sum -c -- "$1.sha256"' sh "$a" </dev/null | clean
  systemctl stop conchd@alpha
  mv /var/lib/conch/alpha "/var/lib/conch/alpha.broken-$(date -u +%Y%m%dT%H%M%SZ)"
  install -d -m 0700 -o conch-alpha -g conch-alpha /var/lib/conch/alpha
  as conch-backup cat -- "/var/backups/conch/alpha/$a" </dev/null |
    as conch-alpha tar --no-same-owner --no-same-permissions -xzf - -C /var/lib/conch/alpha
  systemctl start conchd@alpha
  curl -fsS https://alpha.your.domain/healthz
)
```

`conch-backup` checks and reads the archive and the instance's user unpacks it from that stream, so root opens no file `conch-backup` owns, no owner or mode is taken from the archive, and nothing needs a `chown` afterwards. Everything written after the archive was made is gone: say so to the people using it. Run the drill on the archive first if there is any doubt about it. Keep the `.broken-*` directory until you are sure; the service cannot see it.

## 11. Upgrading and rolling back

Releases are tagged, built by `.github/workflows/release.yml`, and name themselves: `conchd version` and `/healthz` report the tag. `conch-upgrade.sh <tag> <instance>...`, as root (name **every** instance on the droplet: they share one binary):

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

A release you installed stays in `/opt/conch/releases/`; remove old ones by hand when you are sure.

## A second instance

A second server on the same droplet (plan §3) is these steps, with `beta` as the name:

1. DNS: an `A` record for `beta.your.domain`.
2. A user: `useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin conch-beta`.
3. A LiveKit key: add a line to `/etc/conch/livekit-keys` (`beta-<hex>: <secret>`, mode stays 0600) and `systemctl restart livekit`.
4. An env file: `install -m 0600 -o root -g root ops/deploy/instance.env.example /etc/conch/beta.env`, then set `CONCHD_DATA=/var/lib/conch/beta`, a **new port** (`CONCHD_LISTEN=127.0.0.1:8081`), `CONCHD_LIVEKIT_ROOM_PREFIX=beta-`, and the new key pair.
5. A Caddy block: `beta.your.domain { header Strict-Transport-Security "max-age=31536000"; reverse_proxy 127.0.0.1:8081 }` (on separate lines, as in the Caddyfile), then `caddy validate` and `systemctl reload caddy`.
6. The first operator, as in step 6, with `conch-beta` and `--data /var/lib/conch/beta`.
7. `systemctl enable --now conchd@beta conch-backup@beta.timer`.

From now on name `beta` in `conch-upgrade.sh` too.

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
| `scripts/deploy-scripts-test.sh` | the test of all of the above that needs no droplet |

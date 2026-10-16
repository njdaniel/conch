# Deploying Conch on one droplet

This runbook takes an empty Ubuntu 24.04 droplet to a public `conchd` instance with voice, nightly off-site backups, and the steps to upgrade, roll back, restore and add a second instance. It is the deployment of [ADR-008](adr/ADR-008-private-communications-alpha.md) and the plan in [docs/design/private-alpha.md](design/private-alpha.md) (decision O7).

One droplet runs three things:

- `conchd`, one instance per systemd unit `conchd@<name>`, on a loopback port;
- Caddy, which holds the public names, gets and renews their certificates, and proxies to `conchd` and to LiveKit's signalling port;
- LiveKit, in Docker from the digest-pinned image the README uses, for the audio.

Everything is parameterised by an **instance name** (`alpha` below) and a **domain** (`example.com` below, a placeholder: replace it everywhere). A second server is the steps in [A second instance](#a-second-instance), not a new design.

**Status.** The scripts, the units and the backup, restore, upgrade and rollback paths are exercised by `scripts/deploy-scripts-test.sh` (run by `make deploy-test` and in CI) against a real `conchd`, with a stand-in for `systemctl`. The Caddyfile, LiveKit's TURN/TLS settings, the firewall and the units under a real systemd were **not** run on a droplet when this was written: the first run on the real droplet (issues A15 and A16) is where they are proved, and this page should be corrected from what that finds.

## What you need

- A DigitalOcean droplet, Ubuntu 24.04, **4 GB RAM / 2 vCPU to start** (decision O7; the final size follows the measurement in A15), with IPv4.
- A domain you control, and permission to add DNS records.
- A DigitalOcean Space (or any S3-compatible bucket) for off-site backups, and its access key.
- A workstation with `ssh`, and somewhere safe (a password manager) for the operator token.

## 1. DNS

Create `A` records (and `AAAA` if the droplet has IPv6) pointing at the droplet for:

| Name | Used for |
| --- | --- |
| `alpha.example.com` | the `alpha` instance: web client, API, WebSockets |
| `livekit.example.com` | LiveKit signalling (WebSocket over TLS) |
| `turn.example.com` | LiveKit's TURN/TLS fallback (UDP-blocked networks) |

Caddy cannot get a certificate for a name that does not resolve yet, so wait for the records before step 5.

## 2. The host

As root on the droplet:

```sh
apt update && apt install -y sqlite3 curl ufw docker.io
# Caddy: install it from its official apt repository,
# https://caddyserver.com/docs/install#debian-ubuntu-raspbian

useradd --system --home-dir /var/lib/conch --shell /usr/sbin/nologin conch
install -d -m 0755 /opt/conch/releases /opt/conch/bin
install -d -m 0755 /etc/conch
install -d -m 0700 -o conch -g conch /var/backups/conch
```

`sqlite3` is a host prerequisite of the backup, restore and upgrade scripts (they use SQLite's online backup). `rclone` or `s3cmd` is the off-site tool (step 8); neither is a dependency of the repository.

Get the scripts and unit files at the tag you are installing. They are not in the release assets, so clone the repository (it is public) and copy them:

```sh
git clone --depth 1 --branch v0.1.0 https://github.com/njdaniel/conch.git /root/conch-src   # your tag
cd /root/conch-src/ops/deploy
install -m 0755 conch-backup.sh conch-restore.sh conch-upgrade.sh firewall.sh /opt/conch/bin/
```

## 3. Install conchd

Release assets are named `conchd_<tag>_<os>_<arch>`, with a `checksums.txt`, and have the web client built in. The upgrade script does the download and the checksum check; for the first install do the same by hand:

```sh
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
```

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

What `livekit.yaml` sets, and why:

- `room.auto_create: false` is required: `conchd` creates and deletes rooms itself, and with automatic creation on, a stale token would recreate a deleted room.
- Signalling (`7880`) listens on loopback; Caddy serves it as `wss://livekit.example.com`. Media ports are open to the world: TCP `7881` and UDP `50000-60000`, plus `use_external_ip: true` so clients are told the droplet's public address.
- **TURN/TLS** (`turn:`) is the fallback for networks that block UDP: `3478/udp` and `5349/tcp`, on the name `turn.example.com`. It uses the certificate Caddy obtains for that name (the `turn.example.com` block in the Caddyfile does nothing else). `livekit.service` mounts Caddy's certificate directory for that name read-only at `/certs`; the container's root user reads it, and Caddy's directory stays private to `caddy`.
- **Renewal.** LiveKit reads the certificate once, at start. Caddy renews about 30 days before expiry, writing the same file; `livekit-cert.path` watches that file and runs `livekit-cert.service`, which restarts LiveKit (rooms are rebuilt by `conchd`; connected clients reconnect). Without this a renewed certificate would reach LiveKit only at the next restart.
- Port `443` is Caddy's, so TURN/TLS uses `5349`. Some networks block that port too; there is then no voice for them, which is a known limit of this setup.

## 5. conchd configuration

One env file per instance, mode 0600, root-owned (systemd reads it as root). It holds the LiveKit secret, so never put its values in a unit file, a Caddyfile, a command line or the repository:

```sh
install -m 0600 -o root -g root /root/conch-src/ops/deploy/instance.env.example /etc/conch/alpha.env
editor /etc/conch/alpha.env
```

Set, for `alpha`:

- `CONCHD_DATA=/var/lib/conch/alpha` (the directory the unit creates; the backup script expects exactly this);
- `CONCHD_LISTEN=127.0.0.1:8080` (loopback; one port per instance);
- `CONCHD_LIVEKIT_URL=wss://livekit.your.domain`, `CONCHD_LIVEKIT_API_URL=http://127.0.0.1:7880`;
- `CONCHD_LIVEKIT_ROOM_PREFIX=alpha-` (per instance);
- `CONCHD_LIVEKIT_API_KEY` and `CONCHD_LIVEKIT_API_SECRET`: the pair from step 4.

The trusted-proxy setting of issue A6b is a commented line in the example, to be filled when that lands. Until then `conchd` sees every connection as coming from Caddy.

## 6. The first operator

Create the first operator **before** the first start, over SSH, with the token going to your terminal and nowhere on the droplet:

```sh
install -d -m 0700 -o conch -g conch /var/lib/conch/alpha
sudo -u conch /opt/conch/current/conchd bootstrap-operator --data /var/lib/conch/alpha --name your-name
```

It prints the token once on stdout. Copy it into your password manager from the terminal; do not redirect it to a file on the droplet, and do not leave it in your scrollback on a shared machine. `bootstrap-operator` refuses to run a second time. From then on you administer the server with `conch admin` from your own machine, using this token (`CONCH_SERVER=https://alpha.your.domain`).

## 7. Start it: services, Caddy, firewall

```sh
cd /root/conch-src/ops/deploy
cp 'conchd@.service' 'conch-backup@.service' 'conch-backup@.timer' /etc/systemd/system/
systemctl daemon-reload

# Firewall first: 22, 80, 443/tcp, 7881/tcp, the UDP media range, and TURN.
# Everything else is closed; conchd (8080) and LiveKit's 7880 are loopback only.
# SSH_PORT is the port sshd really uses. Read it before enabling:
/opt/conch/bin/firewall.sh --dry-run
/opt/conch/bin/firewall.sh

# Caddy: edit the domain and the email, then validate and reload.
sed 's/example\.com/your.domain/g' Caddyfile > /etc/caddy/Caddyfile
caddy validate --config /etc/caddy/Caddyfile
systemctl reload caddy

systemctl enable --now conchd@alpha livekit livekit-cert.path
curl -fsS https://alpha.your.domain/healthz     # {"status":"ok","version":"v0.1.0","db":"ok"}
```

`livekit-cert.path` only matters once Caddy has the TURN certificate; if `livekit` fails to start because `/certs` is empty, give Caddy a minute after the first reload (it obtains the certificates on its own) and then `systemctl restart livekit`.

Open `https://alpha.your.domain/app/`, sign in with the operator token, and create the first channel. Then check voice from a second device on a different network, and once from a network that blocks UDP if you have one.

## 8. Backups

`conch-backup.sh <instance>` makes one dated archive, `/var/backups/conch/<instance>/conch-<instance>-<UTC time>.tar.gz`, holding:

- `conch.db`, copied with SQLite's online backup (`sqlite3 .backup`), which is safe while `conchd` runs in WAL mode, and checked with `PRAGMA integrity_check` before it is archived;
- `files/`, the uploaded files (they are not in the database). They are taken after the database so every file a message names is in the archive.

A `.sha256` file sits beside it. Archives are kept locally for 14 days (`CONCH_BACKUP_KEEP_DAYS`). `conch-backup@<instance>.timer` runs it nightly at about 03:15.

**Off-site is not optional.** Without a destination the script still writes the local archive, then says `NO OFF-SITE COPY WAS MADE` and exits 3, so `systemctl --failed` and `systemctl status conch-backup@alpha` show it. (`--local-only` is for runs that mean that, such as the upgrade script's pre-upgrade backup.)

Configure a destination with `rclone` or `s3cmd`, installed from the host's packages. For a DigitalOcean Space with `rclone`:

```sh
apt install -y rclone
umask 077
cat > /etc/conch/rclone.conf <<'EOF'
[spaces]
type = s3
provider = DigitalOcean
access_key_id = YOUR_ACCESS_KEY
secret_access_key = YOUR_SECRET_KEY
endpoint = nyc3.digitaloceanspaces.com
EOF
cat > /etc/conch/backup.env <<'EOF'
RCLONE_CONFIG=/etc/conch/rclone.conf
CONCH_BACKUP_RCLONE_DEST=spaces:your-bucket/conch
EOF
chown conch:conch /etc/conch/rclone.conf /etc/conch/backup.env     # the backup runs as conch
chmod 0600 /etc/conch/rclone.conf /etc/conch/backup.env
```

(Replace the placeholders; the script refuses a `backup.env` that is not mode 0600.) With `s3cmd`, put its configuration where the `conch` user reads it, and set `CONCH_BACKUP_S3CMD_DEST=s3://your-bucket/conch` instead. Give the Space's key write access to that bucket and no more, and turn on versioning or a lifecycle rule there if you want older copies kept.

Run it once by hand and look:

```sh
sudo -u conch /opt/conch/bin/conch-backup.sh alpha        # exit 0, "copied off-site"
systemctl enable --now conch-backup@alpha.timer
systemctl list-timers 'conch-backup@*'
```

## 9. The restore drill (before testers join)

A backup that has never been restored is a guess. Before anyone else is invited, restore the latest archive into a scratch directory, with the live instance left alone:

```sh
latest=$(ls -1t /var/backups/conch/alpha/conch-alpha-*.tar.gz | head -n 1)
sudo -u conch /opt/conch/bin/conch-restore.sh "$latest" /tmp/conch-drill
rm -rf /tmp/conch-drill
```

It checks the archive's SHA-256, unpacks it, runs `PRAGMA integrity_check`, checks that every uploaded file the database names is present with its stored SHA-256, starts `conchd` on the copy (loopback, a spare port, no web client, no LiveKit) and requires `/healthz` to say `"status":"ok","db":"ok"`, then stops it and prints the schema version, the message count and the number of files. Do it also from the **off-site** copy once: download it with your tool (`rclone copy spaces:your-bucket/conch/alpha/<archive> .`, with its `.sha256`) and give that to the script. The drill proves the copy that would be needed, not only the local one.

## 10. Restoring for real

Use this when the data is lost or corrupt. The archive's `conch.db` and `files/` go back as the instance's data directory:

```sh
systemctl stop conchd@alpha
mv /var/lib/conch/alpha /var/lib/conch/alpha.broken-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 0700 -o conch -g conch /var/lib/conch/alpha
tar -xzf conch-alpha-<time>.tar.gz -C /var/lib/conch/alpha     # after checking its .sha256
chown -R conch:conch /var/lib/conch/alpha
systemctl start conchd@alpha
curl -fsS https://alpha.your.domain/healthz
```

Everything written after the archive was made is gone: say so to the people using it. Run the drill script on the archive first if there is any doubt about it. Keep the `.broken-*` directory until you are sure.

## 11. Upgrading and rolling back

Releases are tagged, built by `.github/workflows/release.yml`, and name themselves: `conchd version` and `/healthz` report the tag. `conch-upgrade.sh <tag> <instance>...` (name **every** instance on the droplet: they share one binary):

1. downloads `conchd_<tag>_linux_<arch>` and `checksums.txt`, and refuses unless they match;
2. unpacks it to `/opt/conch/releases/<tag>/`;
3. takes a local backup of each instance and records its schema version (`PRAGMA user_version`);
4. stops the instances, repoints `/opt/conch/current`, starts them;
5. checks each `/healthz` answers `ok` and reports `<tag>`, and otherwise stops and tells you the rollback command.

```sh
/opt/conch/bin/conch-upgrade.sh v0.2.0 alpha
```

**Rollback without a migration** (the schema version is unchanged): `conch-upgrade.sh --rollback alpha` repoints `current` at the previous release and restarts. The data is kept as it is, including what was written since the upgrade.

**Rollback with a migration** (the schema version went up): the old binary refuses a newer database (`database schema version N is newer than this binary`), so the same command also replaces the instance's data directory with the pre-upgrade backup. What was written since the upgrade is lost, and the replaced directory is kept as `/var/lib/conch/alpha.rolled-back-<time>` in case something in it is needed. The script says which of the two it did. Check the result with `curl https://alpha.your.domain/healthz`, which must report the old tag.

A release you installed stays in `/opt/conch/releases/`; remove old ones by hand when you are sure.

## A second instance

A second server on the same droplet (plan §3) is these steps, with `beta` as the name:

1. DNS: an `A` record for `beta.your.domain`.
2. A LiveKit key: add a line to `/etc/conch/livekit-keys` (`beta-<hex>: <secret>`, mode stays 0600) and `systemctl restart livekit`.
3. An env file: `install -m 0600 -o root -g root ops/deploy/instance.env.example /etc/conch/beta.env`, then set `CONCHD_DATA=/var/lib/conch/beta`, a **new port** (`CONCHD_LISTEN=127.0.0.1:8081`), `CONCHD_LIVEKIT_ROOM_PREFIX=beta-`, and the new key pair.
4. A Caddy block: `beta.your.domain { reverse_proxy 127.0.0.1:8081 }`, then `caddy validate` and `systemctl reload caddy`.
5. The first operator, as in step 6, with `--data /var/lib/conch/beta`.
6. `systemctl enable --now conchd@beta conch-backup@beta.timer`.

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

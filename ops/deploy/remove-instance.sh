#!/usr/bin/env bash
# remove-instance.sh <name> [--force-normal] [--no-backup] [--purge-backups]
# — remove one Conch server (a conchd instance) from this droplet (issue
# #861), by the guardrails of ADR-011 item 2.4.
#
# Before anything is changed:
#   - the name is checked against the rule add-instance.sh uses, first of all,
#     and the shared names (backup, drill, livekit, turn, www, removed) are
#     refused, so the shared conch-backup and conch-drill users are never
#     touched;
#   - a data directory, env file, backup directory or rollback record that is
#     a symlink is refused;
#   - the mode is read from the env file only, the last CONCHD_MODE line, as
#     systemd reads it. Only exactly "temporary" is a temporary server; a
#     missing or unreadable file, or any other value, is a normal one, and a
#     normal server is refused without --force-normal;
#   - the Caddyfile without the instance's block is worked out and checked
#     with `caddy validate`. Only a block between "# conch-instance <name>
#     begin" and "# conch-instance <name> end" lines (which add-instance.sh
#     writes) is removed. If any other line that is not a comment still names
#     <name>.<something> as a word (a block made by hand, such as alpha's),
#     it refuses and says to edit the Caddyfile by hand: it never guesses
#     where a hand-made block ends;
#   - the LiveKit key file without the instance's keys (<name>-<8 hex>) is
#     worked out and checked;
#   - the name is typed back on the terminal. There is no --yes: every
#     removal is confirmed by hand, and with no terminal it refuses.
# Then: it stops and disables conchd@<name> and its backup timer and unit,
# and confirms nothing of that instance still runs; takes a final local
# archive (conch-backup.sh <name> --local-only) unless --no-backup; puts the
# new Caddyfile in place and reloads Caddy, and the new key file and restarts
# LiveKit (each put back, and reloaded or restarted again, if that fails, so
# no other instance is taken down); deletes the env file, the data directory
# (and any <data>.rolled-back-* beside it), the rollback record and the user
# conch-<name>; and moves /var/backups/conch/<name>, with the final archive
# and the pre-upgrade ones, to /var/backups/conch/removed/<name>-<time>/,
# where the systemd-tmpfiles rule add-instance.sh installed deletes it after
# 14 days (--purge-backups deletes it now). Last, it prints the DNS records
# to delete.
#
# As root, everything in the backup directory is done as conch-backup, which
# owns it (as in conch-upgrade.sh), in its own session with no terminal and
# none of root's environment. No env file is ever sourced, and the key file's
# lines are never printed.
#
# Run as root. For tests, every location and command can be moved:
#   CONCH_OPT (/opt/conch)  CONCH_ETC (/etc/conch)  CONCH_DATA_ROOT (/var/lib/conch)
#   CONCH_BACKUP_DIR (/var/backups/conch)  CONCH_CADDYFILE (/etc/caddy/Caddyfile)
#   CONCH_BACKUP_SCRIPT (conch-backup.sh beside this script)
#   SYSTEMCTL (systemctl)  CADDY (caddy)  USERDEL (userdel)  GETENT (getent)
set -euo pipefail
# Character classes such as [0-9] mean ASCII only in the C locale.
export LC_ALL=C

usage() { echo "usage: $0 <name> [--force-normal] [--no-backup] [--purge-backups]" >&2; exit 2; }

# The name is checked before anything else is read: the rule of
# add-instance.sh, and none of the shared names.
name="" force_normal=0 no_backup=0 purge=0
while [ $# -gt 0 ]; do
    case "$1" in
        --force-normal) force_normal=1 ;;
        --no-backup) no_backup=1 ;;
        --purge-backups) purge=1 ;;
        -*) usage ;;
        *) if [ -n "$name" ]; then usage; fi; name=$1 ;;
    esac
    shift
done
if [ -z "$name" ]; then usage; fi
if ! [[ $name =~ ^[a-z][a-z0-9-]{0,25}$ ]] || [[ $name == *- ]]; then
    echo "remove-instance: bad name: lower-case letters, digits and '-', starting with a letter, not ending in '-', at most 26 characters" >&2
    exit 2
fi
case "$name" in
    livekit | turn | backup | drill | www | removed)
        echo "remove-instance: '$name' is one of the droplet's shared names, not an instance" >&2
        exit 2 ;;
esac

opt=${CONCH_OPT:-/opt/conch}
etc=${CONCH_ETC:-/etc/conch}
data_root=${CONCH_DATA_ROOT:-/var/lib/conch}
backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
caddyfile=${CONCH_CADDYFILE:-/etc/caddy/Caddyfile}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
backup_script=${CONCH_BACKUP_SCRIPT:-$here/conch-backup.sh}
systemctl=${SYSTEMCTL:-systemctl}
caddy=${CADDY:-caddy}
userdel=${USERDEL:-userdel}
getent=${GETENT:-getent}
safe_path=/usr/sbin:/usr/bin:/sbin:/bin
cd /

clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }
say() { printf 'remove-instance: %s\n' "$*" | clean; }
die() { printf 'remove-instance: %s\n' "$1" | clean >&2; exit "${2:-1}"; }

for d in "$opt" "$etc" "$data_root" "$backup_dir" "$caddyfile"; do
    case "$d" in /*) ;; *) die "$d is not an absolute path" 2 ;; esac
    case "$d" in *[!A-Za-z0-9._/-]*) die "$d: only letters, digits, '.', '_', '-' and '/' are allowed" 2 ;; esac
done
for tool in awk setsid flock mktemp; do command -v "$tool" >/dev/null || die "$tool is required"; done

user=conch-$name
case "$user" in conch-backup | conch-drill) die "refusing to touch the shared user $user" 2 ;; esac
env_file=$etc/$name.env
data=$data_root/$name
keys=$etc/livekit-keys
state_file=$opt/state/$name
backups=$backup_dir/$name

for p in "$env_file" "$data" "$backups" "$state_file"; do
    if [ -L "$p" ]; then die "$p is a symlink; refusing to remove anything. Nothing was changed."; fi
done
rolled=()
for p in "$data_root/$name".rolled-back-*; do
    if [ -L "$p" ]; then die "$p is a symlink; refusing to remove anything. Nothing was changed."; fi
    if [ -d "$p" ]; then rolled+=("$p"); fi
done

if [ ! -d "$etc" ] || [ -L "$etc" ]; then die "$etc is not a directory"; fi
exec 9<"$etc"
flock -n 9 || die "another add-instance.sh or remove-instance.sh is running"
sysctl() { "$systemctl" "$@" 9<&-; }

env_value() { sed -n "s/^$2=//p" "$1" | tail -n 1; }

# The mode, from the env file only. Missing or unreadable is normal.
mode=normal
if [ -f "$env_file" ] && [ -r "$env_file" ] && [ "$(env_value "$env_file" CONCHD_MODE 2>/dev/null || true)" = temporary ]; then
    mode=temporary
fi
if [ "$mode" = normal ] && [ "$force_normal" = 0 ]; then
    die "$name is a normal server (its env file does not say CONCHD_MODE=temporary, or is missing or unreadable). Removing it deletes what its people keep there; if you mean it, give --force-normal. Nothing was changed."
fi

# The domain, for the DNS records to delete: from the env file's LiveKit
# address, which add-instance.sh writes as wss://livekit.<domain>.
domain=""
if [ -f "$env_file" ] && [ -r "$env_file" ]; then
    u=$(env_value "$env_file" CONCHD_LIVEKIT_URL || true)
    case "$u" in wss://livekit.*) domain=${u#wss://livekit.} ;; esac
    case "$domain" in '' | *[!a-z0-9.-]*) domain="" ;; esac
fi

# --- the Caddyfile and the key file, worked out and checked first -----------

umask 077
tmps=()
on_exit() {
    local status=$?
    [ "${#tmps[@]}" -eq 0 ] || rm -f -- "${tmps[@]}"
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT TERM HUP

caddy_change=0
caddy_new="" caddy_orig=""
if [ -e "$caddyfile" ]; then
    if [ ! -f "$caddyfile" ] || [ -L "$caddyfile" ]; then die "$caddyfile is not a regular file"; fi
    caddy_new=$(mktemp "${caddyfile%/*}/.Caddyfile.new.XXXXXX")
    tmps+=("$caddy_new")
    cp -p -- "$caddyfile" "$caddy_new"
    # The marked block, and the one empty line add-instance.sh puts before it.
    status=0
    awk -v b="# conch-instance $name begin" -v e="# conch-instance $name end" '
        $0 == b { if (inb || seen) bad = 1; inb = 1; seen = 1; held = 0; next }
        $0 == e { if (!inb) bad = 1; inb = 0; next }
        inb { if ($0 ~ /^# conch-instance /) bad = 1; next }
        { if (held) print ""; held = 0 }
        $0 == "" { held = 1; next }
        { print }
        END { if (held) print ""; if (inb || bad) exit 3; exit seen ? 0 : 4 }
    ' "$caddyfile" >"$caddy_new" || status=$?
    case "$status" in
        0) caddy_change=1 ;;
        4) ;;
        *) die "$caddyfile has a malformed '# conch-instance $name' block (a marker twice, unpaired or nested); edit it by hand. Nothing was changed." ;;
    esac
    # Anything that still names <name>.<something> as a site is a block this
    # script did not write. It is not guessed at.
    if awk -v n="$name." '
        { line = $0; sub(/^[ \t]+/, "", line) }
        line ~ /^#/ { next }
        {
            k = split(line, w, /[ \t,{}]+/)
            for (i = 1; i <= k; i++) {
                x = w[i]; sub(/^https?:\/\//, "", x)
                if (index(x, n) == 1) found = 1
            }
        }
        END { exit found ? 0 : 1 }' "$caddy_new"; then
        die "$caddyfile serves $name.* in a block without '# conch-instance $name begin/end' markers (one made by hand). It is not guessed at: remove that block by hand (or put the two marker lines around it), validate and reload Caddy, then run this again. Nothing was changed."
    fi
    if [ "$caddy_change" = 1 ]; then
        if [ -z "$domain" ]; then
            domain=$(awk -v b="# conch-instance $name begin" 'f { print; exit } $0 == b { f = 1 }' "$caddyfile" |
                sed -n "s/^$name\.\([a-z0-9.-]*\) {\$/\1/p")
        fi
        if ! caddy_out=$("$caddy" validate --config "$caddy_new" --adapter caddyfile 2>&1); then
            printf '%s\n' "$caddy_out" | clean >&2
            die "caddy validate refused the Caddyfile without $name's block. Nothing was changed."
        fi
    fi
fi

key_change=0
keys_new="" keys_orig=""
if [ -e "$keys" ]; then
    if [ ! -f "$keys" ] || [ -L "$keys" ]; then die "$keys is not a regular file"; fi
    keys_new=$(mktemp "$etc/.livekit-keys.new.XXXXXX")
    tmps+=("$keys_new")
    cp -p -- "$keys" "$keys_new"
    status=0
    awk -v n="$name" '
        { k = $0; sub(/:.*/, "", k) }
        { s = substr(k, length(n) + 2) }
        index($0, ":") && substr(k, 1, length(n) + 1) == n "-" && length(s) == 8 && s !~ /[^0-9a-f]/ { found = 1; next }
        { print }
        END { exit found ? 0 : 4 }
    ' "$keys" >"$keys_new" || status=$?
    case "$status" in
        0) key_change=1 ;;
        4) ;;
        *) die "could not read $keys. Nothing was changed." ;;
    esac
    if grep -Evq '^([A-Za-z0-9._-]+: [^[:space:]]+)?$' "$keys_new"; then
        die "$keys holds a line that is not '<key>: <secret>'; fix it by hand first (its lines are not shown here). Nothing was changed."
    fi
fi
env_key=""
if [ -f "$env_file" ] && [ -r "$env_file" ]; then env_key=$(env_value "$env_file" CONCHD_LIVEKIT_API_KEY || true); fi

user_exists=0
if "$getent" passwd "$user" >/dev/null 2>&1; then user_exists=1; fi

# The other instances, whose voice calls drop when LiveKit restarts.
others=()
for f in "$etc"/*.env; do
    if [ ! -f "$f" ] || [ -L "$f" ]; then continue; fi
    n=${f##*/}
    n=${n%.env}
    case "$n" in "$name" | livekit | turn | backup | drill | www | removed) continue ;; esac
    if [[ $n =~ ^[a-z][a-z0-9-]{0,25}$ ]] && [[ $n != *- ]]; then others+=("$n"); fi
done
drop_list=none
[ "${#others[@]}" -eq 0 ] || drop_list=$(printf '%s, ' "${others[@]}")
drop_list=${drop_list%, }

# --- what will go, and the typed confirmation --------------------------------

{
    echo "remove-instance: removing $name, a $mode server$([ "$mode" = temporary ] || echo " (--force-normal)"). This deletes:"
    echo "  conchd@$name and conch-backup@$name.timer: stopped and disabled"
    if [ "$no_backup" = 1 ]; then
        echo "  no final archive (--no-backup)"
    else
        echo "  a final archive first: conch-backup.sh $name --local-only"
    fi
    [ "$caddy_change" = 0 ] || echo "  its block in $caddyfile, then systemctl reload caddy"
    [ "$key_change" = 0 ] || echo "  its key in $keys, then systemctl restart livekit: voice calls drop for a few seconds on: $drop_list"
    if [ -n "$env_key" ] && ! [[ $env_key =~ ^$name-[0-9a-f]{8}$ ]]; then
        echo "  NOT its LiveKit key $env_key, which is not named $name-<8 hex>: remove that line from $keys by hand if no other instance uses it"
    fi
    [ ! -e "$env_file" ] || echo "  $env_file"
    [ ! -e "$data" ] || echo "  $data, with its database and files"
    for p in "${rolled[@]}"; do echo "  $p"; done
    [ ! -e "$state_file" ] || echo "  $state_file"
    [ "$user_exists" = 0 ] || echo "  the user $user"
    if [ -e "$backups" ]; then
        if [ "$purge" = 1 ]; then
            echo "  $backups, with every archive in it, at once (--purge-backups)"
        else
            echo "  $backups: moved to $backup_dir/removed/, deleted from there after 14 days"
        fi
    fi
} | clean

if ! { exec 8<>/dev/tty; } 2>/dev/null; then
    die "removal is confirmed by typing the name on a terminal, and there is none (ssh -t). Nothing was changed."
fi
printf 'Type the name of the server to remove it (%s): ' "$name" >&8
answer=""
read -r answer <&8 || true
exec 8>&-
[ "$answer" = "$name" ] || die "you typed '$answer', not '$name'. Nothing was changed."

# --- stop it, and make sure it stopped ---------------------------------------

for u in "conchd@$name" "conch-backup@$name.timer" "conch-backup@$name.service"; do
    sysctl stop "$u" || die "systemctl stop $u failed. Nothing was deleted."
    sysctl disable "$u" >/dev/null 2>&1 || true
done
running() {
    if sysctl is-active --quiet "conchd@$name"; then return 0; fi
    if [ "$(id -u)" = 0 ] && [ "$user_exists" = 1 ] && pgrep -u "$user" >/dev/null; then return 0; fi
    return 1
}
stopped=0
for _ in $(seq 1 50); do
    if ! running; then stopped=1; break; fi
    sleep 0.2
done
[ "$stopped" = 1 ] || die "conchd@$name, or a process of $user, is still running after the stop. Nothing was deleted; find out why, then run this again."
say "conchd@$name is stopped and disabled, and nothing of $name runs"

# guarded and as_backup, as in conch-upgrade.sh.
guarded() {
    if [ -t 0 ]; then
        guarded "$@" </dev/null
        return
    fi
    if [ -t 1 ]; then
        guarded "$@" | clean
        return
    fi
    { setsid -w "$@" 2>&1 1>&3 3>&- | clean >&2; } 3>&1
}
as_backup() {
    if [ "$(id -u)" = 0 ]; then
        id conch-backup >/dev/null 2>&1 || die "the user conch-backup does not exist (docs/deploy.md)"
        guarded runuser -u conch-backup -- env -i "PATH=$safe_path" "$@"
    else
        guarded "$@"
    fi
}

if [ "$no_backup" = 0 ]; then
    if [ -f "$data/conch.db" ]; then
        CONCH_DATA_ROOT=$data_root CONCH_BACKUP_DIR=$backup_dir CONCH_ETC=$etc "$backup_script" "$name" --local-only 9<&- |
            clean ||
            die "the final backup of $name failed. $name is stopped; nothing was deleted. Fix the backup and run this again, or give --no-backup to remove it without one."
    else
        say "no database in $data: no final archive"
    fi
fi

# --- the shared files, each put back if its reload fails ---------------------

if [ "$caddy_change" = 1 ]; then
    caddy_orig=$(mktemp "${caddyfile%/*}/.Caddyfile.orig.XXXXXX")
    tmps+=("$caddy_orig")
    cp -p -- "$caddyfile" "$caddy_orig"
    mv -f -- "$caddy_new" "$caddyfile"
    if ! sysctl reload caddy; then
        mv -f -- "$caddy_orig" "$caddyfile"
        sysctl reload caddy || true
        die "systemctl reload caddy failed without $name's block; $caddyfile is put back and Caddy reloaded. $name is stopped; nothing else was deleted. Find out why (journalctl -u caddy), then run this again."
    fi
    say "removed $name's block from $caddyfile and reloaded Caddy"
fi
if [ "$key_change" = 1 ]; then
    keys_orig=$(mktemp "$etc/.livekit-keys.orig.XXXXXX")
    tmps+=("$keys_orig")
    cp -p -- "$keys" "$keys_orig"
    mv -f -- "$keys_new" "$keys"
    if ! sysctl restart livekit; then
        mv -f -- "$keys_orig" "$keys"
        sysctl restart livekit || true
        die "systemctl restart livekit failed without $name's key; $keys is put back and LiveKit restarted. Caddy no longer serves $name, which is stopped; nothing else was deleted. Find out why (journalctl -u livekit), then run this again."
    fi
    say "removed $name's key from $keys and restarted LiveKit"
fi

# --- the instance's own files ------------------------------------------------

gone() { # <rm args>... <path>: delete it, or stop saying what is left
    rm "$@" || die "could not delete ${!#}; delete it by hand"
    say "deleted ${!#}"
}
if [ -e "$env_file" ]; then gone -f -- "$env_file"; fi
if [ -e "$data" ]; then gone -rf --one-file-system -- "$data"; fi
for p in "${rolled[@]}"; do gone -rf --one-file-system -- "$p"; done
if [ -e "$state_file" ]; then gone -f -- "$state_file"; fi
if [ "$user_exists" = 1 ]; then
    "$userdel" "$user" 9<&- || die "userdel $user failed; everything else of $name is gone"
    say "deleted the user $user"
fi

# The archives, as conch-backup, which owns them: moved to removed/ with
# fresh times, so the 14 days run from now, or deleted with --purge-backups.
if [ -e "$backups" ]; then
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    if [ "$purge" = 1 ]; then
        # shellcheck disable=SC2016 # expanded by the inner bash
        as_backup bash -c '[ -d "$1" ] && [ ! -L "$1" ] && rm -rf --one-file-system -- "$1"' purge "$backups" ||
            die "could not delete $backups"
        say "deleted $backups (--purge-backups)"
    else
        # shellcheck disable=SC2016 # expanded by the inner bash
        as_backup bash -c '
            set -eu
            src=$1 dir=$2 dst=$3
            [ -d "$src" ] && [ ! -L "$src" ] || exit 1
            [ ! -L "$dir" ] || exit 1
            mkdir -p -m 0700 -- "$dir"
            [ ! -e "$dst" ] || exit 1
            mv -T -- "$src" "$dst"
            find "$dst" -exec touch -h -- {} +
        ' move-backups "$backups" "$backup_dir/removed" "$backup_dir/removed/$name-$stamp" ||
            die "could not move $backups to $backup_dir/removed/"
        say "moved $backups to $backup_dir/removed/$name-$stamp/; it is deleted after 14 days"
    fi
fi

say "$name is removed."
if [ -n "$domain" ]; then
    {
        echo "remove-instance: delete its DNS records, A and AAAA for $name.$domain (conch-server rm does this for you):"
        echo "  doctl compute domain records list $domain    # the IDs of the $name records"
        echo "  doctl compute domain records delete $domain <id> --force"
    } | clean
else
    say "delete its DNS records, A and AAAA for $name.<your domain> (conch-server rm does this for you)."
fi

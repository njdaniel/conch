#!/usr/bin/env bash
# add-instance.sh <name> <domain> [--port N] [--temporary]
#                 [--operator-totp optional|required] [--operator <name>]
#                 [--yes] [--dry-run]
# — add one Conch server (a conchd instance) to this droplet (issue #861).
#
# It checks everything first and changes nothing until every check passes:
#   - the name: lower-case letters, digits and '-', starting with a letter,
#     not ending in '-', at most 26 characters (so the user conch-<name> fits
#     useradd's 32 and the room prefix <name>- fits conchd's 32), and none of
#     the names the droplet's shared parts use (livekit, turn, backup, drill,
#     www, removed);
#   - nothing of that name exists: no conch-<name> user, env file, data or
#     backup directory, rollback record, LiveKit key, Caddy block or Caddy
#     site for <name>.<domain>;
#   - the port (default: the first from 8081 that no env file names and
#     nothing listens on) is free;
#   - <name>.<domain> resolves to this droplet's public IPv4, and IPv6 when it
#     has one, read from the DigitalOcean metadata service. If not, it prints
#     the records to create and stops;
#   - --temporary with --operator-totp required is refused (conchd would
#     refuse to start, ADR-012 item 2).
# --dry-run stops there and prints what it would do. Otherwise, after asking
# (restarting LiveKit drops every instance's voice calls for a few seconds;
# --yes answers, and with no terminal and no --yes it refuses), it makes, in
# this order: the user conch-<name>, the data directory (0700), a LiveKit key
# pair appended to the key file, the env file (0600, root; every key in
# instance.env.example, a fresh CONCHD_TOTP_KEY, and CONCHD_MODE=temporary
# with --temporary, CONCHD_OPERATOR_TOTP with --operator-totp), the
# systemd-tmpfiles rule for removed servers' archives, and a Caddy block
# between "# conch-instance <name> begin/end" markers, validated with
# `caddy validate` before it replaces the Caddyfile and reloaded after. Then
# it restarts LiveKit, makes the first operator with `conchd operator
# bootstrap --link` as conch-<name>, starts conchd@<name> (and
# conch-backup@<name>.timer, unless --temporary), and waits for /healthz on
# loopback and then through Caddy. If any step fails, it undoes what this run
# made, newest first, and says what it undid, after stopping the instance's
# units and making sure nothing of conch-<name> still runs; if that cannot be
# shown, it deletes nothing more, names what is left, and exits 5. A backup
# directory a backup made during the run is removed too, as conch-backup. Once
# conchd answers on loopback the server is kept: if only the check through
# Caddy fails (a certificate not issued yet, say), or the run is interrupted
# then, it is left running and it exits 4. Standard output must be a
# terminal (ssh -t), since the setup link is printed there.
#
# Secrets. The LiveKit secret and the TOTP key are read from /dev/urandom
# through pipes and written by bash's printf into the env file and the key
# file only: never on a command line, never printed. The env file is written
# with umask 077 to a temporary file beside it and moved into place. The
# setup link is printed once, by conchd, to this terminal and nowhere else.
# No env file is ever sourced: values are read back one known key at a time.
#
# As root, conchd operator bootstrap runs as conch-<name> in its own session,
# with no terminal and none of root's environment, and what it prints is
# stripped of control characters (as in conch-upgrade.sh). Run as another
# user (the tests), every step runs as that user and no user is switched to.
#
# Run as root. For tests, every location and command can be moved:
#   CONCH_OPT (/opt/conch)  CONCH_ETC (/etc/conch)  CONCH_DATA_ROOT (/var/lib/conch)
#   CONCH_BACKUP_DIR (/var/backups/conch)  CONCH_CADDYFILE (/etc/caddy/Caddyfile)
#   CONCH_TMPFILES_DIR (/etc/tmpfiles.d)
#   CONCH_METADATA_URL (http://169.254.169.254/metadata/v1)
#   SYSTEMCTL (systemctl)  CADDY (caddy)  USERADD (useradd)  USERDEL (userdel)
#   GETENT (getent)  SS (ss)  CONCH_PUBLIC_CURL (curl, for the check through Caddy)
#   CONCH_HEALTH_TIMEOUT (120 seconds for the check through Caddy)
#   CONCH_LOOPBACK_TIMEOUT (30 seconds for conchd's own /healthz)
set -euo pipefail
# Character classes such as [0-9] mean ASCII only in the C locale.
export LC_ALL=C

opt=${CONCH_OPT:-/opt/conch}
etc=${CONCH_ETC:-/etc/conch}
data_root=${CONCH_DATA_ROOT:-/var/lib/conch}
backup_dir=${CONCH_BACKUP_DIR:-/var/backups/conch}
caddyfile=${CONCH_CADDYFILE:-/etc/caddy/Caddyfile}
tmpfiles_dir=${CONCH_TMPFILES_DIR:-/etc/tmpfiles.d}
metadata_url=${CONCH_METADATA_URL:-http://169.254.169.254/metadata/v1}
systemctl=${SYSTEMCTL:-systemctl}
caddy=${CADDY:-caddy}
useradd=${USERADD:-useradd}
userdel=${USERDEL:-userdel}
getent=${GETENT:-getent}
ss=${SS:-ss}
public_curl=${CONCH_PUBLIC_CURL:-curl}
health_timeout=${CONCH_HEALTH_TIMEOUT:-120}
loopback_timeout=${CONCH_LOOPBACK_TIMEOUT:-30}
safe_path=/usr/sbin:/usr/bin:/sbin:/bin
# The users below may not be able to enter the directory this was started in.
cd /

# clean: only printable ASCII, tab and newline, so nothing another user wrote
# can send escape sequences to root's terminal.
clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }

say() { printf 'add-instance: %s\n' "$*" | clean; }
die() { printf 'add-instance: %s\n' "$1" | clean >&2; exit "${2:-1}"; }
usage() {
    echo "usage: $0 <name> <domain> [--port N] [--temporary] [--operator-totp optional|required] [--operator <name>] [--yes] [--dry-run]" >&2
    exit 2
}

name="" domain="" port="" temporary=0 operator_totp="" operator=operator yes=0 dry_run=0
while [ $# -gt 0 ]; do
    case "$1" in
        --port) [ $# -ge 2 ] || usage; port=$2; shift ;;
        --temporary) temporary=1 ;;
        --operator-totp) [ $# -ge 2 ] || usage; operator_totp=$2; shift ;;
        --operator) [ $# -ge 2 ] || usage; operator=$2; shift ;;
        --yes) yes=1 ;;
        --dry-run) dry_run=1 ;;
        -*) usage ;;
        *)
            if [ -z "$name" ]; then name=$1
            elif [ -z "$domain" ]; then domain=$1
            else usage
            fi ;;
    esac
    shift
done
if [ -z "$name" ] || [ -z "$domain" ]; then usage; fi

# --- the rules -----------------------------------------------------------------

# valid_name: the room-prefix rule (<name>- at most 32 characters, lower case,
# digits and '-'), starting with a letter, not ending in '-' (it is a DNS
# label), and at most 26 characters so that conch-<name> is a valid user name.
valid_name() { [[ $1 =~ ^[a-z][a-z0-9-]{0,25}$ ]] && [[ $1 != *- ]]; }
# reserved: the names the droplet's shared parts use: DNS names (livekit,
# turn, www), shared users (conch-backup, conch-drill) and the backup
# directory's removed/ (and backup.env, which *.env would otherwise match).
reserved() { case "$1" in livekit | turn | backup | drill | www | removed) return 0 ;; esac; return 1; }
valid_domain() {
    [ "${#1}" -le 200 ] && [[ $1 =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$ ]]
}
# An operator name is passed to conchd as one argument (no shell), so this is
# only kept to what is easy to read and to quote: letters, digits, space and
# . _ ' -, starting with a letter or digit, not ending in a space.
valid_operator() { [[ $1 =~ ^[A-Za-z0-9]([A-Za-z0-9\ ._\'-]{0,62}[A-Za-z0-9._\'-])?$ ]]; }
valid_ipv4() {
    local o
    [[ $1 =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
    for o in ${1//./ }; do [ "$((10#$o))" -le 255 ] || return 1; done
}
# ip6_expand: the full form of an IPv6 address (eight groups of four lower-case
# hex digits), so the metadata service's form and the resolver's compare
# equal. Fails on anything that is not an IPv6 address.
ip6_expand() {
    local a=${1,,} h t g i out=""
    local -a hg=() tg=() all=()
    [[ $a =~ ^[0-9a-f:]{2,39}$ ]] || return 1
    if [[ $a == *::* ]]; then
        h=${a%%::*} t=${a#*::}
        [[ $t != *::* ]] || return 1
        [[ $h != :* && $h != *: && $t != :* && $t != *: ]] || return 1
        [ -z "$h" ] || IFS=: read -ra hg <<<"$h"
        [ -z "$t" ] || IFS=: read -ra tg <<<"$t"
        [ $((${#hg[@]} + ${#tg[@]})) -le 7 ] || return 1
        all=("${hg[@]}")
        for ((i = ${#hg[@]} + ${#tg[@]}; i < 8; i++)); do all+=(0); done
        all+=("${tg[@]}")
    else
        [[ $a != :* && $a != *: ]] || return 1
        IFS=: read -ra all <<<"$a"
        [ "${#all[@]}" -eq 8 ] || return 1
    fi
    for g in "${all[@]}"; do
        [[ $g =~ ^[0-9a-f]{1,4}$ ]] || return 1
        printf -v g '%04x' "$((16#$g))"
        out+=$g:
    done
    printf '%s\n' "${out%:}"
}
# ip6_canon: the RFC 5952 form (lower case, no leading zeros, the longest run
# of two or more zero groups as ::), for the records printed.
ip6_canon() {
    local full i run=-1 len=0 best=-1 bestlen=0 out
    local -a g=()
    full=$(ip6_expand "$1") || return 1
    IFS=: read -ra g <<<"$full"
    for ((i = 0; i < 8; i++)); do
        g[i]=$(printf '%x' "$((16#${g[i]}))")
        if [ "${g[i]}" = 0 ]; then
            [ "$run" -ge 0 ] || { run=$i len=0; }
            len=$((len + 1))
            if [ "$len" -gt "$bestlen" ]; then best=$run bestlen=$len; fi
        else
            run=-1
        fi
    done
    if [ "$bestlen" -lt 2 ]; then
        out=$(IFS=:; echo "${g[*]}")
    else
        out=$(IFS=:; echo "${g[*]:0:best}")::$(IFS=:; echo "${g[*]:best+bestlen}")
    fi
    printf '%s\n' "$out"
}

# These end up in the env file, the tmpfiles rule and the Caddyfile, so they
# are kept to characters that need no quoting.
for d in "$opt" "$etc" "$data_root" "$backup_dir" "$caddyfile" "$tmpfiles_dir"; do
    case "$d" in /*) ;; *) die "$d is not an absolute path" 2 ;; esac
    case "$d" in *[!A-Za-z0-9._/-]*) die "$d: only letters, digits, '.', '_', '-' and '/' are allowed" 2 ;; esac
done

valid_name "$name" ||
    die "bad name '$name': lower-case letters, digits and '-', starting with a letter, not ending in '-', at most 26 characters" 2
! reserved "$name" || die "'$name' is reserved for the droplet's shared parts (livekit, turn, backup, drill, www, removed); choose another" 2
valid_domain "$domain" || die "bad domain '$domain': lower-case DNS names only, such as example.com" 2
valid_operator "$operator" ||
    die "bad operator name: letters, digits, space and . _ ' -, starting with a letter or digit, at most 64 characters" 2
case "$operator_totp" in
    '' | optional | required) ;;
    *) die "--operator-totp must be optional or required" 2 ;;
esac
if [ "$temporary" = 1 ] && [ "$operator_totp" = required ]; then
    die "--temporary with --operator-totp required is refused: conchd will not start a temporary server that requires the authenticator (ADR-012 item 2). Leave --operator-totp out, or give optional." 2
fi
if [ -n "$port" ]; then
    if ! [[ $port =~ ^[0-9]{1,5}$ ]] || [ "$((10#$port))" -lt 1024 ] || [ "$((10#$port))" -gt 65535 ]; then
        die "bad port '$port': a number from 1024 to 65535" 2
    fi
    port=$((10#$port))
fi

for t in "$health_timeout" "$loopback_timeout"; do
    [[ $t =~ ^[0-9]{1,5}$ ]] || die "CONCH_HEALTH_TIMEOUT and CONCH_LOOPBACK_TIMEOUT are whole seconds" 2
done

host=$name.$domain
env_file=$etc/$name.env
data=$data_root/$name
keys=$etc/livekit-keys
tmpfiles_conf=$tmpfiles_dir/conch-removed.conf
livekit_url=wss://livekit.$domain
begin_marker="# conch-instance $name begin"
end_marker="# conch-instance $name end"

for tool in curl od base64 awk setsid flock mktemp; do
    command -v "$tool" >/dev/null || die "$tool is required"
done

# One add or remove at a time: both read and rewrite the key file and the
# Caddyfile. The lock is on the directory itself, so taking it makes no file.
if [ ! -d "$etc" ] || [ -L "$etc" ]; then die "$etc is not a directory"; fi
exec 9<"$etc"
flock -n 9 || die "another add-instance.sh or remove-instance.sh is running"
# sysctl: systemctl without the lock's descriptor, so nothing it starts holds it.
sysctl() { "$systemctl" "$@" 9<&-; }

# env_value <file> <key>: the last assignment of key, as systemd reads it.
# The file is never sourced; the caller checks the value.
env_value() { sed -n "s/^$2=//p" "$1" | tail -n 1; }

# instances: the names of the instances on this droplet, one per line: every
# regular file $etc/<name>.env whose name is a valid, unreserved instance name.
# So backup.env and alpha.env.bak-* are not instances.
instances() {
    local f n
    for f in "$etc"/*.env; do
        if [ ! -f "$f" ] || [ -L "$f" ]; then continue; fi
        n=${f##*/}
        n=${n%.env}
        if ! valid_name "$n" || reserved "$n"; then continue; fi
        printf '%s\n' "$n"
    done
}

# caddy_mentions <file> <host>: does a line that is not a comment name host as
# a word (a site address, alone or in a list)?
caddy_mentions() {
    awk -v h="$2" '
        { line = $0; sub(/^[ \t]+/, "", line) }
        line ~ /^#/ { next }
        {
            n = split(line, w, /[ \t,{}]+/)
            for (i = 1; i <= n; i++)
                if (w[i] == h || w[i] == "https://" h || w[i] == "http://" h || w[i] == h ":443") found = 1
        }
        END { exit found ? 0 : 1 }' "$1"
}

# ends_in_newline <file>: empty, or its last byte is a newline (an appended
# block must be removable exactly).
ends_in_newline() { [ ! -s "$1" ] || [ "$(tail -c 1 "$1" | od -An -tx1 | tr -d ' \n')" = 0a ]; }

# --- the checks --------------------------------------------------------------

[ -x "$opt/current/conchd" ] || die "$opt/current/conchd is not there: install conchd first (docs/deploy.md, step 3)"
if [ ! -d "$data_root" ] || [ -L "$data_root" ]; then die "$data_root is not a directory (docs/deploy.md, step 2)"; fi
if [ ! -f "$caddyfile" ] || [ -L "$caddyfile" ]; then die "$caddyfile is not a regular file"; fi
ends_in_newline "$caddyfile" || die "$caddyfile does not end with a newline; fix that by hand first"
if [ ! -f "$keys" ] || [ -L "$keys" ]; then die "$keys is not a regular file: set up LiveKit first (docs/deploy.md, step 4)"; fi
ends_in_newline "$keys" || die "$keys does not end with a newline; fix that by hand first"
[ -d "$tmpfiles_dir" ] || die "$tmpfiles_dir is not a directory"

if "$getent" passwd "conch-$name" >/dev/null 2>&1; then
    die "the user conch-$name already exists: $name is, or was, an instance. Nothing was changed."
fi
for p in "$env_file" "$data" "$backup_dir/$name" "$opt/state/$name"; do
    if [ -e "$p" ] || [ -L "$p" ]; then die "$p already exists: $name is, or was, an instance. Nothing was changed."; fi
done
if grep -Fq "# conch-instance $name " "$caddyfile"; then
    die "$caddyfile already has a block marked for $name. Nothing was changed."
fi
if caddy_mentions "$caddyfile" "$host"; then
    die "$caddyfile already serves $host. Nothing was changed."
fi
if grep -Eq "^$name-[0-9a-f]{8}:" "$keys"; then
    die "$keys already holds a key for $name. Nothing was changed."
fi
# Every line of the key file is "<key>: <secret>" (or empty). It is checked,
# never shown.
if grep -Evq '^([A-Za-z0-9._-]+: [^[:space:]]+)?$' "$keys"; then
    die "$keys holds a line that is not '<key>: <secret>'; fix it by hand first (its lines are not shown here)"
fi

others=()
mapfile -t others < <(instances)
used_ports=()
for i in "${others[@]}"; do
    l=$(env_value "$etc/$i.env" CONCHD_LISTEN)
    p=${l##*:}
    [[ $p =~ ^[0-9]{1,5}$ ]] && used_ports+=("$((10#$p))")
    u=$(env_value "$etc/$i.env" CONCHD_LIVEKIT_URL)
    if [ -n "$u" ] && [ "$u" != "$livekit_url" ]; then
        die "$i uses CONCHD_LIVEKIT_URL=$u, not $livekit_url: is $domain the right domain? Nothing was changed."
    fi
done

port_free() {
    local p=$1 u out
    for u in "${used_ports[@]}"; do [ "$u" != "$p" ] || return 1; done
    out=$("$ss" -Hltn "sport = :$p") || die "could not list listening ports with $ss"
    [ -z "$out" ]
}
if [ -n "$port" ]; then
    port_free "$port" || die "port $port is taken (an env file names it or something listens on it). Nothing was changed."
else
    for ((p = 8081; p < 9000; p++)); do
        if port_free "$p"; then port=$p; break; fi
    done
    [ -n "$port" ] || die "no free port from 8081 to 8999"
fi

# This droplet's public addresses, from the metadata service.
meta() { curl -fsS --max-time 5 "$metadata_url/interfaces/public/0/$1/address" 2>/dev/null; }
ipv4=$(meta ipv4) || die "could not read this droplet's public IPv4 from $metadata_url"
valid_ipv4 "$ipv4" || die "the metadata service gave an IPv4 address that is not one"
ipv6=$(meta ipv6) || ipv6=""
ipv6_full=""
if [ -n "$ipv6" ]; then
    ipv6_full=$(ip6_expand "$ipv6") || die "the metadata service gave an IPv6 address that is not one"
    ipv6=$(ip6_canon "$ipv6")
fi

# resolved <ahostsv4|ahostsv6> <host>: the addresses it resolves to, one per
# line, sorted, without duplicates; IPv6 in full form and without the
# IPv4-mapped ones getent adds for a name that has only an A record.
resolved() {
    local a
    while read -r a _; do
        case "$1:$a" in
            ahostsv6:::ffff:*) ;;
            ahostsv6:*) ip6_expand "$a" || printf '%s\n' "$a" ;;
            *) printf '%s\n' "$a" ;;
        esac
    done < <("$getent" "$1" "$2" 2>/dev/null || true) | sort -u
}
dns_ok=1
[ "$(resolved ahostsv4 "$host")" = "$ipv4" ] || dns_ok=0
[ "$(resolved ahostsv6 "$host")" = "$ipv6_full" ] || dns_ok=0
if [ "$dns_ok" = 0 ]; then
    {
        echo "add-instance: $host does not resolve to this droplet (and only to it). Create these records, wait until they resolve, then run this again:"
        printf '  %-4s  %s  %s\n' A "$host" "$ipv4"
        [ -z "$ipv6" ] || printf '  %-4s  %s  %s\n' AAAA "$host" "$ipv6"
        echo "With doctl (conch-server new does this for you):"
        printf '  doctl compute domain records create %s --record-type A --record-name %s --record-data %s --record-ttl 300\n' "$domain" "$name" "$ipv4"
        [ -z "$ipv6" ] || printf '  doctl compute domain records create %s --record-type AAAA --record-name %s --record-data %s --record-ttl 300\n' "$domain" "$name" "$ipv6"
        echo "Nothing was changed."
    } | clean >&2
    exit 1
fi

# --- the plan ----------------------------------------------------------------

mode_text=normal
[ "$temporary" = 0 ] || mode_text=temporary
drop_list=none
[ "${#others[@]}" -eq 0 ] || drop_list=$(printf '%s, ' "${others[@]}")
drop_list=${drop_list%, }
{
    echo "add-instance: plan for $name (https://$host), $mode_text mode:"
    echo "  user      conch-$name (system, no home, no login shell)"
    echo "  data      $data (0700, conch-$name)"
    echo "  LiveKit   a new key $name-<8 hex> appended to $keys"
    echo "  env file  $env_file (0600, root): CONCHD_LISTEN=127.0.0.1:$port, CONCHD_LIVEKIT_ROOM_PREFIX=$name-, the new LiveKit key, a new CONCHD_TOTP_KEY$([ "$temporary" = 0 ] || echo ", CONCHD_MODE=temporary")$([ -z "$operator_totp" ] || echo ", CONCHD_OPERATOR_TOTP=$operator_totp")"
    [ -e "$tmpfiles_conf" ] || echo "  tmpfiles  $tmpfiles_conf: archives of removed servers in $backup_dir/removed are deleted after 14 days"
    echo "  Caddy     a block marked '$begin_marker' for $host -> 127.0.0.1:$port appended to $caddyfile, checked with caddy validate, then systemctl reload caddy"
    echo "  restart   systemctl restart livekit: voice calls drop for a few seconds on: $drop_list"
    echo "  operator  '$operator', made with conchd operator bootstrap --link as conch-$name; the setup link is printed once, here"
    echo "  start     conchd@$name$([ "$temporary" = 1 ] || echo " and conch-backup@$name.timer")"
    echo "  check     http://127.0.0.1:$port/healthz, then https://$host/healthz through Caddy"
} | clean

if [ "$dry_run" = 1 ]; then
    say "dry run: every check passed and nothing was changed"
    exit 0
fi

# The setup link is printed to standard output, once: it must reach a person,
# not a file or a pipe.
if [ ! -t 1 ]; then
    die "standard output is not a terminal, and the first operator's setup link would be printed to it. Run this on a terminal (conch-server uses ssh -t). Nothing was changed."
fi

if [ "${#others[@]}" -gt 0 ] && [ "$yes" = 0 ]; then
    if ! { exec 8<>/dev/tty; } 2>/dev/null; then
        die "restarting LiveKit drops voice calls on $drop_list; there is no terminal to ask on, so give --yes to go ahead. Nothing was changed."
    fi
    printf 'Restarting LiveKit drops voice calls on %s for a few seconds. Go ahead? [y/N] ' "$drop_list" >&8
    answer=""
    read -r answer <&8 || true
    exec 8>&-
    case "$answer" in y | Y | yes) ;; *) die "not confirmed. Nothing was changed." ;; esac
fi

# --- the changes, each one undone if a later one fails -----------------------
#
# Exit status: 0 done; 1 refused before any change, or failed and undone
# completely (nothing of <name> is left); 2 a bad argument; 4 the server was
# made and runs, and is kept (the check through Caddy did not pass in time, or
# this was interrupted after conchd answered); 5 failed, and the undo stopped
# short because conch-<name> could not be shown to be stopped: what is left
# is named; 130 interrupted before that, and undone.

umask 077
undo=()
tmps=()
committed=0
livekit_restarted=0
keys_orig="" caddy_orig=""

# guarded <command>...: run it holding no terminal (as in conch-upgrade.sh).
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
# as_user <user> <command>...: as root, run it as that user with none of
# root's environment and without the lock's descriptor; otherwise (the tests)
# as whoever this is.
as_user() {
    local u=$1
    shift
    if [ "$(id -u)" = 0 ]; then
        guarded runuser -u "$u" -- env -i "PATH=$safe_path" "$@" 9<&-
    else
        guarded "$@" 9<&-
    fi
}
as_instance() { as_user "conch-$name" "$@"; }

# running: does anything of this instance still run? Its units, and, as root,
# any process of conch-<name>.
running() {
    if sysctl is-active --quiet "conchd@$name" || sysctl is-active --quiet "conch-backup@$name.service"; then return 0; fi
    if [ "$(id -u)" = 0 ] && "$getent" passwd "conch-$name" >/dev/null 2>&1 && pgrep -u "conch-$name" >/dev/null; then return 0; fi
    return 1
}
stopped_for_sure() {
    for _ in $(seq 1 50); do
        if ! running; then return 0; fi
        sleep 0.2
    done
    return 1
}

undo_text() {
    case "$1" in
        user) echo "the user conch-$name" ;;
        data) echo "$data" ;;
        key) echo "$name's key in $keys" ;;
        env) echo "$env_file" ;;
        tmpfiles) echo "$tmpfiles_conf" ;;
        caddy) echo "$name's block in $caddyfile" ;;
        unit) echo "conchd@$name and its backup timer" ;;
    esac
}
undo_step() {
    case "$1" in
        user) "$userdel" "conch-$name" && say "undone: removed the user conch-$name" ;;
        data) rm -rf -- "$data" && say "undone: removed $data" ;;
        key)
            mv -f -- "$keys_orig" "$keys" && say "undone: removed $name's key from $keys"
            if [ "$livekit_restarted" = 1 ]; then
                sysctl restart livekit && say "undone: restarted LiveKit without that key"
            fi ;;
        env) rm -f -- "$env_file" && say "undone: removed $env_file" ;;
        tmpfiles) rm -f -- "$tmpfiles_conf" && say "undone: removed $tmpfiles_conf" ;;
        caddy) mv -f -- "$caddy_orig" "$caddyfile" && sysctl reload caddy && say "undone: restored $caddyfile and reloaded Caddy" ;;
        unit)
            local u ok=0
            for u in "conchd@$name" "conch-backup@$name.timer" "conch-backup@$name.service"; do
                sysctl stop "$u" || { ok=1; say "systemctl stop $u failed"; }
                sysctl disable "$u" >/dev/null 2>&1 || true
            done
            [ "$ok" = 1 ] || say "undone: stopped and disabled conchd@$name, conch-backup@$name.timer and conch-backup@$name.service"
            return "$ok" ;;
    esac
}
# The backup directory a backup made during this run (it did not exist when
# the checks ran), removed as conch-backup, which owns it.
undo_backups() {
    [ -e "$backup_dir/$name" ] || [ -L "$backup_dir/$name" ] || return 0
    # shellcheck disable=SC2016 # expanded by the inner bash
    as_user conch-backup bash -c '[ -d "$1" ] && [ ! -L "$1" ] && rm -rf --one-file-system -- "$1"' undo-backups "$backup_dir/$name" || true
    if [ -e "$backup_dir/$name" ] || [ -L "$backup_dir/$name" ]; then return 1; fi
    say "undone: removed $backup_dir/$name, which a backup made during this run"
}
on_exit() {
    local status=$? i step blocked=0 left=""
    set +e
    trap - EXIT
    trap '' INT TERM HUP
    if [ "$committed" = 0 ] && [ "${#undo[@]}" -gt 0 ]; then
        say "FAILED; undoing what this run made, newest first:" >&2
        i=$((${#undo[@]} - 1))
        if [ "${undo[i]}" = unit ]; then
            undo_step unit >&2
            i=$((i - 1))
        fi
        # Nothing is deleted while anything of the instance may still run.
        if ! stopped_for_sure; then
            blocked=1
        else
            undo_backups >&2 || say "could not remove $backup_dir/$name: remove it by hand" >&2
        fi
        if [ "$blocked" = 0 ]; then
            for ((; i >= 0; i--)); do
                undo_step "${undo[i]}" >&2 || say "could not undo '${undo[i]}': see above, and fix it by hand" >&2
            done
            [ "$status" = 130 ] || status=1
        else
            for ((; i >= 0; i--)); do
                step=$(undo_text "${undo[i]}")
                left+=", $step"
            done
            say "ROLLBACK INCOMPLETE: conchd@$name, or a process of conch-$name, may still run, so nothing more was undone. Left in place: ${left:2}. Stop it (systemctl stop conchd@$name), then remove what is left with remove-instance.sh $name (temporary) or remove-instance.sh $name --force-normal." >&2
            status=5
        fi
    elif [ "$committed" = 0 ] && [ "$status" != 0 ] && [ "$status" != 2 ] && [ "$status" != 130 ]; then
        status=1
    fi
    [ "${#tmps[@]}" -eq 0 ] || rm -f -- "${tmps[@]}"
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT TERM HUP

# The secrets, from /dev/urandom through pipes into variables: none is an
# argument of any command. The formats are those of docs/deploy.md steps 4
# and 5: an 8-hex key suffix, a 64-hex secret, the base64 of 32 bytes.
hex() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }
lk_key=$name-$(hex 4)
lk_secret=$(hex 32)
totp_key=$(head -c 32 /dev/urandom | base64 -w0)
if ! [[ $lk_key =~ ^$name-[0-9a-f]{8}$ && $lk_secret =~ ^[0-9a-f]{64}$ && $totp_key =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
    die "could not make the secrets from /dev/urandom"
fi

# 1. The user.
"$useradd" --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "conch-$name"
undo+=(user)
say "made the user conch-$name"

# 2. The data directory.
if [ "$(id -u)" = 0 ]; then
    install -d -m 0700 -o "conch-$name" -g "conch-$name" "$data"
else
    mkdir -m 0700 "$data"
fi
undo+=(data)
say "made $data"

# 3. The LiveKit key: the file copied (mode, owner and times kept) beside
# itself, the line appended to the copy, and the copy moved into place. The
# original is kept until the end, to put back.
keys_orig=$(mktemp "$etc/.livekit-keys.orig.XXXXXX")
tmps+=("$keys_orig")
cp -p -- "$keys" "$keys_orig"
keys_new=$(mktemp "$etc/.livekit-keys.new.XXXXXX")
tmps+=("$keys_new")
cp -p -- "$keys" "$keys_new"
printf '%s: %s\n' "$lk_key" "$lk_secret" >>"$keys_new"
mv -f -- "$keys_new" "$keys"
undo+=(key)
say "added the LiveKit key $lk_key to $keys"

# 4. The env file.
env_new=$(mktemp "$etc/.$name.env.XXXXXX")
tmps+=("$env_new")
# kv <key> <value>: one KEY=value line, by bash's printf (a builtin).
kv() { printf '%s=%s\n' "$1" "$2"; }
{
    printf '# Written by add-instance.sh (docs/deploy.md, "A second instance"). Mode 0600, root.\n'
    printf '# KEY=value, one per line, no quotes; never put these values anywhere else.\n'
    kv CONCHD_DATA "$data"
    kv CONCHD_LISTEN "127.0.0.1:$port"
    kv CONCHD_LIVEKIT_URL "$livekit_url"
    kv CONCHD_LIVEKIT_API_URL http://127.0.0.1:7880
    kv CONCHD_LIVEKIT_ROOM_PREFIX "$name-"
    kv CONCHD_LIVEKIT_API_KEY "$lk_key"
    kv CONCHD_LIVEKIT_API_SECRET "$lk_secret"
    kv CONCHD_TRUSTED_PROXIES 127.0.0.1/32,::1/128
    kv CONCHD_TOTP_KEY "$totp_key"
    [ "$temporary" = 0 ] || kv CONCHD_MODE temporary
    [ -z "$operator_totp" ] || kv CONCHD_OPERATOR_TOTP "$operator_totp"
} >"$env_new"
chmod 0600 "$env_new"
mv -T -- "$env_new" "$env_file"
undo+=(env)
unset lk_secret totp_key
say "wrote $env_file"

# 5. The tmpfiles rule for removed servers' archives (host-wide, shared by
# every instance, so remove-instance.sh leaves it).
if [ ! -e "$tmpfiles_conf" ]; then
    tf_new=$(mktemp "$tmpfiles_dir/.conch-removed.conf.XXXXXX")
    tmps+=("$tf_new")
    {
        printf '# Installed by add-instance.sh (docs/deploy.md, "Removing a server"): the\n'
        printf '# archives remove-instance.sh moves here are deleted 14 days later.\n'
        printf 'd %s/removed 0700 conch-backup conch-backup 14d\n' "$backup_dir"
    } >"$tf_new"
    chmod 0644 "$tf_new"
    mv -T -- "$tf_new" "$tmpfiles_conf"
    undo+=(tmpfiles)
    say "wrote $tmpfiles_conf"
fi

# 6. The Caddy block: written to a copy, which is validated before it
# replaces the Caddyfile; the original is kept to put back.
caddy_orig=$(mktemp "${caddyfile%/*}/.Caddyfile.orig.XXXXXX")
tmps+=("$caddy_orig")
cp -p -- "$caddyfile" "$caddy_orig"
caddy_new=$(mktemp "${caddyfile%/*}/.Caddyfile.new.XXXXXX")
tmps+=("$caddy_new")
cp -p -- "$caddyfile" "$caddy_new"
printf '\n%s\n%s {\n\theader Strict-Transport-Security "max-age=31536000"\n\treverse_proxy 127.0.0.1:%s\n}\n%s\n' \
    "$begin_marker" "$host" "$port" "$end_marker" >>"$caddy_new"
if ! caddy_out=$("$caddy" validate --config "$caddy_new" --adapter caddyfile 2>&1); then
    printf '%s\n' "$caddy_out" | clean >&2
    die "caddy validate refused the Caddyfile with $name's block; $caddyfile was not changed"
fi
mv -f -- "$caddy_new" "$caddyfile"
undo+=(caddy)
sysctl reload caddy || die "systemctl reload caddy failed with $name's block"
say "added $host to $caddyfile and reloaded Caddy"

# 7. LiveKit, for the new key.
livekit_restarted=1
sysctl restart livekit || die "systemctl restart livekit failed"
say "restarted LiveKit"

# 8. The first operator. The setup link goes to this terminal only.
as_instance "$opt/current/conchd" operator bootstrap --data "$data" --link --origin "https://$host" "$operator" ||
    die "conchd operator bootstrap failed"

# 9. Start it.
undo+=(unit)
sysctl enable "conchd@$name"
sysctl start "conchd@$name"
if [ "$temporary" = 0 ]; then
    sysctl enable "conch-backup@$name.timer"
    sysctl start "conch-backup@$name.timer"
fi

ok_health() { case "$1" in *'"status":"ok"'*) return 0 ;; esac; return 1; }
up=0
deadline=$((SECONDS + loopback_timeout))
while :; do
    if out=$(curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" 2>/dev/null) && ok_health "$out"; then up=1; break; fi
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 0.3
done
[ "$up" = 1 ] || die "conchd@$name did not answer /healthz on 127.0.0.1:$port within ${loopback_timeout}s (journalctl -u conchd@$name)"
say "conchd@$name answers on 127.0.0.1:$port"

# From here the server exists and works; a slow certificate is not a reason
# to take it down again, and neither is an interruption: it is kept (exit 4).
committed=1
trap 'say "interrupted after conchd@$name answered: the server is kept and runs. Check https://$host/healthz later; to remove it: remove-instance.sh $name" >&2; exit 4' INT TERM HUP
public=0
deadline=$((SECONDS + health_timeout))
while :; do
    if out=$("$public_curl" -fsS --max-time 5 --resolve "$host:443:127.0.0.1" "https://$host/healthz" 2>/dev/null) && ok_health "$out"; then
        public=1
        break
    fi
    [ "$SECONDS" -lt "$deadline" ] || break
    sleep 3
done
if [ "$public" = 0 ]; then
    say "conchd@$name runs, but https://$host/healthz did not answer ok through Caddy within ${health_timeout}s. Caddy may still be getting the certificate (journalctl -u caddy). Nothing was undone. Check with: curl https://$host/healthz. To remove the server: remove-instance.sh $name" >&2
    exit 4
fi
say "https://$host/healthz answers ok"
if [ -n "$operator_totp" ]; then
    totp_text=$operator_totp
elif [ "$temporary" = 1 ]; then
    totp_text=optional
else
    totp_text=required
fi
case "$totp_text" in
    optional) setup_text="set the operator's password (the authenticator step may be skipped)" ;;
    *) setup_text="set the operator's password and enrol an authenticator app (CONCHD_OPERATOR_TOTP is required)" ;;
esac
say "done. Next: open the setup link above in a browser within ten minutes, press the button, and $setup_text."
[ "$temporary" = 1 ] || say "This is a normal server: set up its off-site backup and restore drill before anyone joins (docs/deploy.md, steps 8 and 9)."
say "Name $name in conch-upgrade.sh from now on (conch-server upgrade does)."

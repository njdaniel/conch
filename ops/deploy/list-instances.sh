#!/usr/bin/env bash
# list-instances.sh [--names | --addresses] — what Conch servers this droplet
# runs (issue #861). It reads and changes nothing else.
#
#   (no flag)    one line per instance: name, port, mode, operator TOTP
#                setting, whether conchd@<name> is active, and what /healthz
#                on its loopback port answers (status and version)
#   --names      the instance names, one per line (conch-server upgrade
#                passes them all to conch-upgrade.sh)
#   --addresses  this droplet's public addresses, "ipv4 <a>" and "ipv6 <a>",
#                from the DigitalOcean metadata service (conch-server new
#                makes the DNS records from them)
#
# An instance is a regular file /etc/conch/<name>.env whose name follows the
# rule of add-instance.sh: so backup.env and alpha.env.bak-* are not
# instances. The env files are never sourced: CONCHD_LISTEN, CONCHD_MODE and
# CONCHD_OPERATOR_TOTP are read one key at a time and checked, and no other
# value is read. What /healthz answers is stripped of control characters.
#
# Run as root (the env files are root's). For tests, the locations can be moved:
#   CONCH_ETC (/etc/conch)  SYSTEMCTL (systemctl)
#   CONCH_METADATA_URL (http://169.254.169.254/metadata/v1)
set -euo pipefail
# Character classes such as [0-9] mean ASCII only in the C locale.
export LC_ALL=C

etc=${CONCH_ETC:-/etc/conch}
systemctl=${SYSTEMCTL:-systemctl}
metadata_url=${CONCH_METADATA_URL:-http://169.254.169.254/metadata/v1}

clean() { LC_ALL=C tr -cd '\011\012\040-\176'; }
die() { printf 'list-instances: %s\n' "$1" | clean >&2; exit "${2:-1}"; }

what=table
case "${1:-}" in
    '') ;;
    --names) what=names ;;
    --addresses) what=addresses ;;
    *) echo "usage: $0 [--names | --addresses]" >&2; exit 2 ;;
esac
[ $# -le 1 ] || { echo "usage: $0 [--names | --addresses]" >&2; exit 2; }

# ip6_expand and ip6_canon, as in add-instance.sh: an IPv6 address in full,
# and in the RFC 5952 form the DNS records are made with.
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

if [ "$what" = addresses ]; then
    meta() { curl -fsS --max-time 5 "$metadata_url/interfaces/public/0/$1/address" 2>/dev/null; }
    v4=$(meta ipv4) || die "could not read this droplet's public IPv4 from $metadata_url"
    [[ $v4 =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die "the metadata service gave an IPv4 address that is not one"
    printf 'ipv4 %s\n' "$v4"
    if v6=$(meta ipv6) && [ -n "$v6" ]; then
        v6=$(ip6_canon "$v6") || die "the metadata service gave an IPv6 address that is not one"
        printf 'ipv6 %s\n' "$v6"
    fi
    exit 0
fi

env_value() { sed -n "s/^$2=//p" "$1" | tail -n 1; }
# env_mode <file>: as in remove-instance.sh, "temporary" only when every line
# that names CONCHD_MODE is exactly CONCHD_MODE=temporary (and one does), and
# no line is continued with a backslash; otherwise "normal".
env_mode() {
    local line n=0
    if [ ! -f "$1" ] || [ ! -r "$1" ]; then echo normal; return; fi
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in *\\) echo normal; return ;; esac
        case "$line" in '#'* | ';'*) continue ;; esac
        # A quote may open a value that runs over several lines, in which a
        # line that reads CONCHD_MODE=temporary is not an assignment at all.
        # The env files this repository writes hold no quotes, so any quote
        # makes the file ambiguous: normal.
        case "$line" in *\'* | *\"*) echo normal; return ;; esac
        case "$line" in *CONCHD_MODE*) ;; *) continue ;; esac
        if [ "$line" != CONCHD_MODE=temporary ]; then echo normal; return; fi
        n=$((n + 1))
    done <"$1"
    if [ "$n" -gt 0 ]; then echo temporary; else echo normal; fi
}

names=()
for f in "$etc"/*.env; do
    if [ ! -f "$f" ] || [ -L "$f" ]; then continue; fi
    n=${f##*/}
    n=${n%.env}
    case "$n" in livekit | turn | backup | drill | www | removed) continue ;; esac
    if [[ $n =~ ^[a-z][a-z0-9-]{0,25}$ ]] && [[ $n != *- ]]; then names+=("$n"); fi
done

if [ "$what" = names ]; then
    [ "${#names[@]}" -eq 0 ] || printf '%s\n' "${names[@]}"
    exit 0
fi

printf '%-26s  %-5s  %-9s  %-8s  %-8s  %s\n' NAME PORT MODE TOTP UNIT HEALTH
for n in "${names[@]}"; do
    f=$etc/$n.env
    listen=$(env_value "$f" CONCHD_LISTEN)
    port=${listen##*:}
    [[ $listen =~ ^(127\.0\.0\.1|\[::1\]|localhost):[0-9]{1,5}$ ]] || { listen="" port="?"; }
    mode=$(env_mode "$f")
    totp=$(env_value "$f" CONCHD_OPERATOR_TOTP)
    case "$totp" in
        optional | required) ;;
        '') if [ "$mode" = temporary ]; then totp=optional; else totp=required; fi ;;
        *) totp=invalid ;;
    esac
    unit=inactive
    if "$systemctl" is-active --quiet "conchd@$n"; then unit=active; fi
    health="no answer"
    if [ -n "$listen" ] && out=$(curl -fsS --max-time 3 "http://$listen/healthz" 2>/dev/null); then
        out=$(printf '%s' "$out" | clean)
        status=$(printf '%s\n' "$out" | sed -n 's/.*"status":"\([A-Za-z0-9_-]*\)".*/\1/p')
        version=$(printf '%s\n' "$out" | sed -n 's/.*"version":"\([A-Za-z0-9._+-]*\)".*/\1/p')
        health="${status:-?} ${version:-?}"
    fi
    printf '%-26s  %-5s  %-9s  %-8s  %-8s  %s\n' "$n" "$port" "$mode" "$totp" "$unit" "$health"
done

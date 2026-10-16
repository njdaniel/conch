#!/usr/bin/env bash
# firewall.sh [--dry-run] — the host firewall for a Conch droplet, with ufw.
#
# Open: SSH, HTTP and HTTPS (Caddy), LiveKit's TCP media port, its UDP media
# range, and TURN (UDP and TLS). Everything else inbound is closed. conchd and
# LiveKit's signalling port (7880) listen on loopback and are not opened.
#
# The numbers match livekit.yaml; if you change one, change the other. SSH_PORT
# is the port sshd really listens on: set it before running, or you lock
# yourself out. --dry-run prints the commands and runs none.
set -euo pipefail

ssh_port=${SSH_PORT:-22}
media_from=${LIVEKIT_UDP_FROM:-50000}
media_to=${LIVEKIT_UDP_TO:-60000}
tcp_media=${LIVEKIT_TCP_PORT:-7881}
turn_udp=${TURN_UDP_PORT:-3478}
turn_tls=${TURN_TLS_PORT:-5349}

dry_run=0
case "${1:-}" in
    --dry-run) dry_run=1 ;;
    '') ;;
    *) echo "usage: $0 [--dry-run]" >&2; exit 2 ;;
esac

run() {
    if [ "$dry_run" = 1 ]; then echo "+ $*"; else "$@"; fi
}

if [ "$dry_run" = 0 ]; then
    command -v ufw >/dev/null || { echo "firewall: ufw is not installed (apt install ufw)" >&2; exit 1; }
    [ "$(id -u)" = 0 ] || { echo "firewall: run as root" >&2; exit 1; }
fi

run ufw default deny incoming
run ufw default allow outgoing
run ufw allow "$ssh_port/tcp" comment ssh
run ufw allow 80/tcp comment 'caddy http (certificates)'
run ufw allow 443/tcp comment 'caddy https'
run ufw allow "$tcp_media/tcp" comment 'livekit tcp media'
run ufw allow "$media_from:$media_to/udp" comment 'livekit udp media'
run ufw allow "$turn_udp/udp" comment 'livekit turn udp'
run ufw allow "$turn_tls/tcp" comment 'livekit turn tls'
run ufw --force enable
run ufw status verbose

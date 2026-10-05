#!/usr/bin/env bash
# Start a throwaway LiveKit dev server (API key "devkey", secret "secret").
#   ./server.sh            localhost only
#   ./server.sh 192.168.x.y  reachable on the LAN at that address (dev keys are public knowledge:
#                            trusted network only, stop it afterwards)
ip=${1:-127.0.0.1}; bind=127.0.0.1; [ "$ip" != 127.0.0.1 ] && bind=0.0.0.0
docker rm -f conch-lk-spike >/dev/null 2>&1
docker run -d --name conch-lk-spike --network host livekit/livekit-server:latest --dev --bind "$bind" --node-ip "$ip"

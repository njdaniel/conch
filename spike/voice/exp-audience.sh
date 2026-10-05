#!/usr/bin/env bash
# Spike #87, question 1: can a non-audience client be kept from the audio?
# Needs: LiveKit dev server on 127.0.0.1:7880, built lkctl and lkspike.
set -u
cd "$(dirname "$0")"
B=lkspike/target/release/lkspike
T() { lkctl/lkctl token "$@"; }
L=$(mktemp -d)
run() { "$@" 2>&1 | grep --line-buffered -E "RESULT|SUBSCRI|PUBLISHED|allow-list|requested|sees" ; }

echo "### A1: one room; every token canSubscribe=false; server subscribes bob only; carol asks for everything"
run $B tone token=$(T -room a1 -identity alice -sub=false) secs=12 > $L/a & sleep 2
run $B listen token=$(T -room a1 -identity bob -sub=false) auto=0 secs=9 > $L/b &
run $B listen token=$(T -room a1 -identity carol -sub=false) auto=0 force=1 secs=9 > $L/c & sleep 2
SID=$(grep -o 'TR_[A-Za-z0-9]*' $L/a | head -1)
echo "server -> UpdateSubscriptions(bob, $SID): $(lkctl/lkctl subscribe -room a1 -identity bob -tracks $SID)"
wait; cat $L/b $L/c

echo "### A2: one room; canSubscribe=true; publisher allow-list = bob; carol asks for everything"
run $B tone token=$(T -room a2 -identity alice) perm=bob secs=10 > $L/a & sleep 2
run $B listen token=$(T -room a2 -identity bob) secs=7 > $L/b &
run $B listen token=$(T -room a2 -identity carol) force=1 secs=7 > $L/c &
wait; cat $L/a $L/b $L/c

echo "### A3 (control): one room; canSubscribe=true; no allow-list; carol auto_subscribe off but asks"
run $B tone token=$(T -room a3 -identity alice) secs=10 > $L/a & sleep 2
run $B listen token=$(T -room a3 -identity carol) auto=0 force=1 secs=7 > $L/c &
wait; cat $L/c

echo "### B: one room per net; carol holds a token for net 'bravo' only and presents it; alice talks on 'alpha'"
run $B tone token=$(T -room b-alpha -identity alice) secs=10 > $L/a & sleep 2
run $B listen token=$(T -room b-alpha -identity bob) secs=7 > $L/b &
run $B listen token=$(T -room b-bravo -identity carol) force=1 secs=7 > $L/c &
wait; cat $L/b $L/c
echo "carol replays her bravo token against the alpha room name: the room is inside the signed token, so there is no request that names another room"

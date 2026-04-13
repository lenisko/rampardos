#!/usr/bin/env bash
# Handshakes, then hangs forever. Used to test render timeout and
# kill-on-hang behaviour.

set -eu

HS='{"pid":'"$$"',"style":"fake"}'
HS_LEN=${#HS}
printf 'H'
printf '%s' "$HS_LEN" | awk '{ printf "%c%c%c%c", int($1/16777216)%256, int($1/65536)%256, int($1/256)%256, $1%256 }'
printf '%s' "$HS"

# Sleep forever, independent of stdin state.
while :; do sleep 60; done

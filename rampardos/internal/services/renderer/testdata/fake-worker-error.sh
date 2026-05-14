#!/usr/bin/env bash
# Handshakes, then returns 'E' (error) for every request. Tests the
# error path.

set -eu

# encode_u32_be prints a 4-byte big-endian encoding of $1.
encode_u32_be() {
    printf '%s' "$1" | awk '{ printf "%c%c%c%c", int($1/16777216)%256, int($1/65536)%256, int($1/256)%256, $1%256 }'
}

# decode_u32_be reads 4 bytes from stdin and prints the big-endian uint32 value.
decode_u32_be() {
    local b0 b1 b2 b3
    b0=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b1=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b2=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b3=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    echo $(( b0 * 16777216 + b1 * 65536 + b2 * 256 + b3 ))
}

HS='{"pid":'"$$"',"style":"fake"}'
HS_LEN=${#HS}
printf 'H'
encode_u32_be "$HS_LEN"
printf '%s' "$HS"

MSG='simulated-failure'
MSG_LEN=${#MSG}

while :; do
    typ=$(dd bs=1 count=1 2>/dev/null || true)
    if [ -z "$typ" ]; then
        exit 0
    fi

    len=$(decode_u32_be)
    if [ "$len" -gt 0 ]; then
        dd bs=1 count="$len" >/dev/null 2>&1 || true
    fi

    printf 'E'
    encode_u32_be "$MSG_LEN"
    printf '%s' "$MSG"
done

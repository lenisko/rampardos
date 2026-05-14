#!/usr/bin/env bash
# Fake Node worker: emits a handshake, then responds to each 'R' frame
# with a 'K' frame containing a fixed canned payload. Used by worker
# and pool tests so they do not require Node or maplibre-native.

set -eu

# encode_u32_be prints a 4-byte big-endian encoding of $1.
encode_u32_be() {
    printf '%s' "$1" | awk '{ printf "%c%c%c%c", int($1/16777216)%256, int($1/65536)%256, int($1/256)%256, $1%256 }'
}

# decode_u32_be reads 4 bytes from stdin and prints the big-endian uint32 value.
decode_u32_be() {
    # Read 4 bytes, convert each to decimal via od, then combine.
    local b0 b1 b2 b3
    b0=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b1=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b2=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    b3=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
    echo $(( b0 * 16777216 + b1 * 65536 + b2 * 256 + b3 ))
}

# Handshake: H + len + `{"pid":N,"style":"fake"}`
HS='{"pid":'"$$"',"style":"fake"}'
HS_LEN=${#HS}
printf 'H'
encode_u32_be "$HS_LEN"
printf '%s' "$HS"

# Response body is a 4-byte "fake" marker so tests can assert it.
BODY='fake'
BODY_LEN=${#BODY}

while :; do
    # Read 1-byte type.
    typ=$(dd bs=1 count=1 2>/dev/null || true)
    if [ -z "$typ" ]; then
        exit 0
    fi

    # Read 4-byte big-endian length.
    len=$(decode_u32_be)

    # Discard the payload.
    if [ "$len" -gt 0 ]; then
        dd bs=1 count="$len" >/dev/null 2>&1 || true
    fi

    # Respond with 'K' + length + body.
    printf 'K'
    encode_u32_be "$BODY_LEN"
    printf '%s' "$BODY"
done

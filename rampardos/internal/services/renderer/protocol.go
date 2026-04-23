package renderer

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame types. All frames are length-prefixed: one byte type, four
// bytes big-endian uint32 length, then the payload. The worker and
// orchestrator must agree on these constants; they are also documented
// at the top of rampardos-render-worker/render-worker.js.
const (
	FrameRequest   byte = 'R' // Go -> worker: JSON render request
	FrameOK        byte = 'K' // worker -> Go: raw RGBA response
	FrameError     byte = 'E' // worker -> Go: UTF-8 error message
	FrameHandshake byte = 'H' // worker -> Go: initial readiness handshake
)

// FrameHeaderSize is the number of bytes in a frame header (1 type + 4 length).
const FrameHeaderSize = 5

// MaxFrameSize caps payload length to prevent a misbehaving worker
// from exhausting memory by sending huge frames.
const MaxFrameSize = 64 * 1024 * 1024 // 64 MiB — generous for 4096×4096 RGBA (64 MiB)

// WriteFrame writes a single framed message to w.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("renderer: frame payload too large: %d > %d", len(payload), MaxFrameSize)
	}
	header := [FrameHeaderSize]byte{}
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single framed message from r.
// Returns io.EOF only when no bytes at all are available for a new
// frame (clean shutdown); a partial header or payload returns a
// specific error.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var header [FrameHeaderSize]byte
	n, err := io.ReadFull(r, header[:])
	if n == 0 && err == io.EOF {
		return 0, nil, io.EOF
	}
	if err != nil {
		return 0, nil, fmt.Errorf("renderer: read frame header: %w", err)
	}

	typ = header[0]
	length := binary.BigEndian.Uint32(header[1:])
	if length > MaxFrameSize {
		return 0, nil, fmt.Errorf("renderer: frame payload too large: %d > %d", length, MaxFrameSize)
	}

	if length == 0 {
		return typ, []byte{}, nil
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("renderer: read frame payload: %w", err)
	}

	return typ, payload, nil
}

package renderer

import (
	"bytes"
	"io"
	"testing"
)

func TestWriteFrameRoundTrip(t *testing.T) {
	cases := []struct {
		typ     byte
		payload []byte
	}{
		{'R', []byte(`{"zoom":14}`)},
		{'K', bytes.Repeat([]byte{0xAB}, 1024)},
		{'E', []byte("boom")},
		{'H', []byte(`{"pid":42,"style":"x"}`)},
		{'R', []byte{}}, // empty payload
	}

	for _, tc := range cases {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, tc.typ, tc.payload); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}

		gotTyp, gotPayload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if gotTyp != tc.typ {
			t.Errorf("type: got %q, want %q", gotTyp, tc.typ)
		}
		if !bytes.Equal(gotPayload, tc.payload) {
			t.Errorf("payload: got % x, want % x", gotPayload, tc.payload)
		}
	}
}

func TestReadFrameEOF(t *testing.T) {
	_, _, err := ReadFrame(bytes.NewReader(nil))
	if err != io.EOF {
		t.Errorf("want io.EOF, got %v", err)
	}
}

func TestReadFramePartialHeaderIsError(t *testing.T) {
	_, _, err := ReadFrame(bytes.NewReader([]byte{'R', 0, 0}))
	if err == nil || err == io.EOF {
		t.Errorf("want non-EOF error, got %v", err)
	}
}

func TestReadFramePartialPayloadIsError(t *testing.T) {
	// Header says 10 bytes, only 3 provided.
	hdr := []byte{'R', 0, 0, 0, 10, 'a', 'b', 'c'}
	_, _, err := ReadFrame(bytes.NewReader(hdr))
	if err == nil || err == io.EOF {
		t.Errorf("want non-EOF error, got %v", err)
	}
}

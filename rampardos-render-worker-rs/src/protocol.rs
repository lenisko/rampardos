//! Frame protocol for talking to the Go orchestrator.
//!
//! Wire format (matches `rampardos/internal/services/renderer/protocol.go`
//! and the header comment of `rampardos-render-worker/render-worker.js`):
//!
//!   | 1 byte type | 4 bytes BE uint32 length | length bytes payload |
//!
//! Frame types:
//!   'R' — request  (Go → worker): JSON render request
//!   'K' — ok       (worker → Go): raw RGBA response body
//!   'E' — error    (worker → Go): UTF-8 error message
//!   'H' — handshake (worker → Go): JSON {pid, style} — written once at startup

use std::io::{self, Read, Write};

pub const FRAME_REQUEST: u8 = b'R';
pub const FRAME_OK: u8 = b'K';
pub const FRAME_ERROR: u8 = b'E';
pub const FRAME_HANDSHAKE: u8 = b'H';

/// Cap frame payloads at 64 MiB — generous for 4096×4096 RGBA. Matches the
/// Go orchestrator's `MaxFrameSize` constant.
pub const MAX_FRAME_SIZE: u32 = 64 * 1024 * 1024;

pub fn write_frame<W: Write>(w: &mut W, typ: u8, payload: &[u8]) -> io::Result<()> {
    if payload.len() > MAX_FRAME_SIZE as usize {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("frame payload too large: {} > {}", payload.len(), MAX_FRAME_SIZE),
        ));
    }
    let mut header = [0u8; 5];
    header[0] = typ;
    header[1..5].copy_from_slice(&(payload.len() as u32).to_be_bytes());
    w.write_all(&header)?;
    if !payload.is_empty() {
        w.write_all(payload)?;
    }
    w.flush()
}

pub enum ReadFrameResult {
    Frame { typ: u8, payload: Vec<u8> },
    Eof,
}

pub fn read_frame<R: Read>(r: &mut R) -> io::Result<ReadFrameResult> {
    let mut header = [0u8; 5];
    match r.read_exact(&mut header) {
        Ok(()) => {}
        Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => {
            return Ok(ReadFrameResult::Eof);
        }
        Err(e) => return Err(e),
    }
    let typ = header[0];
    let len = u32::from_be_bytes([header[1], header[2], header[3], header[4]]);
    if len > MAX_FRAME_SIZE {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("frame payload too large: {} > {}", len, MAX_FRAME_SIZE),
        ));
    }
    let mut payload = vec![0u8; len as usize];
    if len > 0 {
        r.read_exact(&mut payload)?;
    }
    Ok(ReadFrameResult::Frame { typ, payload })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_frame() {
        let mut buf: Vec<u8> = Vec::new();
        write_frame(&mut buf, FRAME_OK, b"hello").unwrap();

        let mut cursor = std::io::Cursor::new(&buf);
        let frame = read_frame(&mut cursor).unwrap();
        match frame {
            ReadFrameResult::Frame { typ, payload } => {
                assert_eq!(typ, FRAME_OK);
                assert_eq!(payload, b"hello");
            }
            ReadFrameResult::Eof => panic!("unexpected EOF"),
        }
    }

    #[test]
    fn empty_payload() {
        let mut buf: Vec<u8> = Vec::new();
        write_frame(&mut buf, FRAME_HANDSHAKE, b"").unwrap();
        assert_eq!(buf.len(), 5);
        assert_eq!(buf[0], FRAME_HANDSHAKE);
        assert_eq!(&buf[1..5], &[0, 0, 0, 0]);
    }

    #[test]
    fn read_eof_on_empty_stream() {
        let mut cursor = std::io::Cursor::new(&[][..]);
        assert!(matches!(read_frame(&mut cursor).unwrap(), ReadFrameResult::Eof));
    }

    #[test]
    fn length_is_big_endian() {
        // Length 256 should encode as 00 00 01 00.
        let mut buf: Vec<u8> = Vec::new();
        write_frame(&mut buf, FRAME_OK, &vec![0; 256]).unwrap();
        assert_eq!(&buf[1..5], &[0, 0, 1, 0]);
    }
}

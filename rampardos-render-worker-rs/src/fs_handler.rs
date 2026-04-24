//! FileSource callback — serves resources to mbgl from the local filesystem
//! and an mbtiles SQLite database.
//!
//! URL schemes supported:
//!
//!   mbtiles://{mbtiles_path}           — returns inline TileJSON
//!   mbtiles://{mbtiles_path}/          — same, trailing slash
//!   mbtiles://{mbtiles_path}/{z}/{x}/{y}{.ext}?
//!                                     — vector tile, gunzipped if the stored
//!                                       bytes are gzipped
//!   file:///{abs_path}                 — raw filesystem read
//!
//! Mirrors render-worker.js byte-for-byte so output equivalence holds.

use std::io::Read;
use std::path::PathBuf;
use std::sync::Mutex;

use anyhow::{bail, Context, Result};
use flate2::read::GzDecoder;
use maplibre_native::{FsErrorReason, FsResponse, ResourceKind};
use rusqlite::Connection;

pub struct FsState {
    mbtiles_url_prefix: String,
    mbtiles_tilejson: Vec<u8>,
    conn: Mutex<Connection>,
}

impl FsState {
    pub fn new(mbtiles_path: &str) -> Result<Self> {
        let conn = Connection::open_with_flags(
            mbtiles_path,
            rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY,
        )
        .with_context(|| format!("opening mbtiles at {mbtiles_path}"))?;

        // Build the TileJSON response once from the metadata table. This is
        // what mbgl gets back when it resolves the source URL (no trailing
        // z/x/y).
        let tilejson = build_tilejson(&conn, mbtiles_path)
            .context("building tilejson from mbtiles metadata")?;

        // Normalize prefix: the style references `mbtiles://{path}` and mbgl
        // then expands tile URLs to `mbtiles://{path}/{z}/{x}/{y}.pbf`. Both
        // forms must dispatch to the same mbtiles file.
        let mbtiles_url_prefix = format!("mbtiles://{mbtiles_path}");

        Ok(Self {
            mbtiles_url_prefix,
            mbtiles_tilejson: tilejson.into_bytes(),
            conn: Mutex::new(conn),
        })
    }

    pub fn handle(&self, url: &str, _kind: ResourceKind) -> FsResponse {
        match self.dispatch(url) {
            Ok(Some(bytes)) => FsResponse::Ok(bytes),
            Ok(None) => FsResponse::NoContent,
            Err(e) => FsResponse::Error {
                reason: FsErrorReason::Other,
                message: e.to_string(),
            },
        }
    }

    fn dispatch(&self, url: &str) -> Result<Option<Vec<u8>>> {
        if let Some(rest) = url.strip_prefix(&self.mbtiles_url_prefix) {
            // The source URL itself — return the TileJSON.
            if rest.is_empty() || rest == "/" {
                return Ok(Some(self.mbtiles_tilejson.clone()));
            }
            // Expect "/z/x/y.ext?" — parse and look up.
            let rest = rest.strip_prefix('/').unwrap_or(rest);
            let (z, x, y) = parse_tile_coords(rest)
                .with_context(|| format!("parsing mbtiles tile url: {url}"))?;
            return self.read_tile(z, x, y);
        }
        if let Some(path) = url.strip_prefix("file://") {
            // maplibre-native percent-encodes spaces and special chars in
            // URLs before handing them to the request callback. Decode to
            // match the actual filesystem (e.g. "Noto%20Sans%20Bold" →
            // "Noto Sans Bold"). Match render-worker.js's decodeURIComponent.
            let decoded = percent_decode(path)?;
            let buf = PathBuf::from(&decoded);
            let bytes = std::fs::read(&buf)
                .with_context(|| format!("reading file: {}", buf.display()))?;
            return Ok(Some(bytes));
        }
        bail!("unsupported url scheme: {url}");
    }

    fn read_tile(&self, z: u32, x: u32, y: u32) -> Result<Option<Vec<u8>>> {
        // mbtiles stores tiles with TMS y-coordinates — the 2^z - 1 - y flip
        // of XYZ y. Same convention as render-worker.js.
        let tms_y = (1u32 << z) - 1 - y;

        let conn = self.conn.lock().expect("mbtiles conn poisoned");
        let mut stmt = conn.prepare_cached(
            "SELECT tile_data FROM tiles WHERE zoom_level = ?1 AND tile_column = ?2 AND tile_row = ?3",
        )?;
        let mut rows = stmt.query([z, x, tms_y])?;
        let row = match rows.next()? {
            Some(r) => r,
            None => {
                // Missing tile — overzoom past maxzoom hits this case. mbgl
                // treats NoContent as an empty tile, which is what we want.
                return Ok(None);
            }
        };
        let data: Vec<u8> = row.get(0)?;

        // Vector tiles are typically gzipped in mbtiles. maplibre-native's
        // FileSource does NOT auto-decompress — bytes must be served
        // uncompressed protobuf. Detect the gzip magic and inflate.
        if data.len() >= 2 && data[0] == 0x1f && data[1] == 0x8b {
            let mut decoder = GzDecoder::new(&data[..]);
            let mut out = Vec::with_capacity(data.len() * 4);
            decoder
                .read_to_end(&mut out)
                .with_context(|| format!("gunzip tile {z}/{x}/{y}"))?;
            return Ok(Some(out));
        }
        Ok(Some(data))
    }
}

fn parse_tile_coords(s: &str) -> Result<(u32, u32, u32)> {
    // Accept "z/x/y" or "z/x/y.pbf" or "z/x/y.mvt" etc.
    let trimmed = match s.find('.') {
        Some(i) => &s[..i],
        None => s,
    };
    let parts: Vec<&str> = trimmed.split('/').collect();
    if parts.len() != 3 {
        bail!("expected z/x/y, got {s}");
    }
    let z: u32 = parts[0].parse().with_context(|| format!("bad z: {}", parts[0]))?;
    let x: u32 = parts[1].parse().with_context(|| format!("bad x: {}", parts[1]))?;
    let y: u32 = parts[2].parse().with_context(|| format!("bad y: {}", parts[2]))?;
    Ok((z, x, y))
}

fn build_tilejson(conn: &Connection, mbtiles_path: &str) -> Result<String> {
    let mut stmt = conn.prepare("SELECT name, value FROM metadata")?;
    let mut rows = stmt.query([])?;
    let mut minzoom: i64 = 0;
    let mut maxzoom: i64 = 14;
    let mut name = String::new();
    while let Some(row) = rows.next()? {
        let k: String = row.get(0)?;
        let v: String = row.get(1)?;
        match k.as_str() {
            "minzoom" => minzoom = v.parse().unwrap_or(0),
            "maxzoom" => maxzoom = v.parse().unwrap_or(14),
            "name" => name = v,
            _ => {}
        }
    }
    // The tile URL template points right back at our own mbtiles:// URL,
    // closing the loop — mbgl will expand `{z}/{x}/{y}` and call us.
    let json = serde_json::json!({
        "tilejson": "2.0.0",
        "tiles": [format!("mbtiles://{mbtiles_path}/{{z}}/{{x}}/{{y}}.pbf")],
        "minzoom": minzoom,
        "maxzoom": maxzoom,
        "name": name,
    });
    Ok(json.to_string())
}

/// Decode percent-encoded sequences in a URL path. Keeps bytes that aren't
/// `%xx` verbatim. Matches JS's decodeURIComponent enough for our use case
/// (font paths with spaces, glyph range separators).
fn percent_decode(s: &str) -> Result<String> {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            let hi = hex_nibble(bytes[i + 1]).context("bad percent-escape")?;
            let lo = hex_nibble(bytes[i + 2]).context("bad percent-escape")?;
            out.push((hi << 4) | lo);
            i += 3;
        } else {
            out.push(bytes[i]);
            i += 1;
        }
    }
    String::from_utf8(out).context("percent-decoded path is not utf-8")
}

fn hex_nibble(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_tile_with_extension() {
        assert_eq!(parse_tile_coords("3/4/2.pbf").unwrap(), (3, 4, 2));
    }

    #[test]
    fn parses_tile_without_extension() {
        assert_eq!(parse_tile_coords("0/0/0").unwrap(), (0, 0, 0));
    }

    #[test]
    fn percent_decodes_spaces() {
        assert_eq!(percent_decode("Noto%20Sans%20Bold").unwrap(), "Noto Sans Bold");
    }

    #[test]
    fn percent_decodes_mixed_case() {
        assert_eq!(percent_decode("a%2Fb%2fc").unwrap(), "a/b/c");
    }
}

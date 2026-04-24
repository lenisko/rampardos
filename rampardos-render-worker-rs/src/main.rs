//! Rust render worker for rampardos — drop-in protocol-compatible replacement
//! for `rampardos-render-worker/render-worker.js`.
//!
//! One process per (style, pixel_ratio) tuple. Reads length-prefixed `R`
//! frames from stdin, renders via maplibre-native-rs, writes `K` (RGBA) or
//! `E` (error) frames to stdout. Emits an `H` handshake frame on startup
//! with {pid, style} so the Go orchestrator knows we're ready.

mod fs_handler;
mod protocol;

use std::io::{self, BufReader, BufWriter, Write};
use std::num::NonZeroU32;
use std::sync::Arc;

use anyhow::{Context, Result};
use clap::Parser;
use maplibre_native::{Height, ImageRendererBuilder, Size, Width};
use serde::{Deserialize, Serialize};

use crate::fs_handler::FsState;
use crate::protocol::{
    read_frame, write_frame, ReadFrameResult, FRAME_ERROR, FRAME_HANDSHAKE, FRAME_OK,
    FRAME_REQUEST,
};

#[derive(Parser, Debug)]
#[command(version, about)]
struct Args {
    /// Go orchestrator compatibility: `spawnWorker` in
    /// `rampardos/internal/services/renderer/worker.go` always prepends
    /// the configured `WorkerScript` to argv before the flags (matching
    /// how Node is invoked as `node path/to/script.js --flags`). We
    /// accept that positional so the same env-var shape (`RENDERER_NODE_BINARY`
    /// pointing at this binary, `RENDERER_WORKER_SCRIPT` to any stub)
    /// works unchanged. The value is ignored.
    #[arg(hide = true)]
    _script_compat: Option<String>,

    /// Style identifier for logging (also validated by the Go orchestrator
    /// against the H handshake).
    #[arg(long = "style-id")]
    style_id: String,

    /// Absolute path to the resolved style.json (style.prepared.json in prod).
    #[arg(long = "style-path")]
    style_path: String,

    /// Absolute path to the .mbtiles file backing this style's vector source.
    #[arg(long = "mbtiles")]
    mbtiles: String,

    /// Absolute path to the styles root (sprite files). Accepted for CLI
    /// compatibility with render-worker.js; resolution itself happens via
    /// the `file://` URL scheme baked into style.prepared.json.
    #[arg(long = "styles-dir")]
    styles_dir: String,

    /// Absolute path to the fonts root (glyph PBFs). Same — accepted for
    /// CLI compatibility; used only via `file://` URLs in the style.
    #[arg(long = "fonts-dir")]
    fonts_dir: String,

    /// Pixel ratio (DPI multiplier). Baked into the renderer at startup;
    /// the Go orchestrator serializes ratio into the pool key, not the
    /// per-request payload.
    #[arg(long, default_value_t = 1)]
    ratio: u32,
}

#[derive(Deserialize, Debug)]
struct RenderRequest {
    zoom: f64,
    /// [lng, lat] — matches the JS protocol. maplibre_native's render_static
    /// takes (lat, lon) so we swap below.
    center: [f64; 2],
    width: u32,
    height: u32,
    #[serde(default)]
    bearing: f64,
    #[serde(default)]
    pitch: f64,
}

#[derive(Serialize, Debug)]
struct Handshake {
    pid: u32,
    style: String,
}

fn main() {
    // Log goes to stderr so it doesn't collide with the stdout frame stream.
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("warn"))
        .target(env_logger::Target::Stderr)
        .init();

    if let Err(err) = run() {
        // Best-effort notify via an E frame, then exit.
        let msg = format!("startup: {err:#}");
        let _ = write_frame(&mut io::stdout().lock(), FRAME_ERROR, msg.as_bytes());
        eprintln!("render-worker-rs startup failed: {err:#}");
        std::process::exit(1);
    }
}

fn run() -> Result<()> {
    // Args are ignored aside from what the Rust worker actually needs.
    // styles_dir / fonts_dir are kept in the CLI for protocol-compatibility
    // with render-worker.js; file:// URL resolution happens in fs_handler.
    let args = Args::parse();
    let _ = (&args.styles_dir, &args.fonts_dir);

    let fs_state = Arc::new(
        FsState::new(&args.mbtiles).context("initializing mbtiles FileSource state")?,
    );

    // Clone for the callback closure. FsState::handle takes &self and is
    // Send + Sync (backed by Mutex<Connection>).
    let fs_state_cb = Arc::clone(&fs_state);
    let callback = move |url: &str, kind| fs_state_cb.handle(url, kind);

    // Build the renderer. Size is a placeholder — every R frame re-sizes.
    let width = NonZeroU32::new(512).unwrap();
    let height = NonZeroU32::new(512).unwrap();
    let pixel_ratio: f32 = args.ratio as f32;

    let mut renderer = ImageRendererBuilder::new()
        .with_size(width, height)
        .with_pixel_ratio(pixel_ratio)
        .with_file_source_callback(callback)
        .build_static_renderer();

    // Load the style. The baked URL scheme inside style.prepared.json
    // (mbtiles://..., file://...) routes through our FileSource callback.
    renderer
        .load_style_from_path(&args.style_path)
        .with_context(|| format!("loading style from {}", args.style_path))?;

    // Handshake — Go orchestrator waits for this before sending any R frames.
    {
        let hs = Handshake { pid: std::process::id(), style: args.style_id.clone() };
        let hs_json = serde_json::to_vec(&hs)?;
        let mut stdout = io::stdout().lock();
        write_frame(&mut stdout, FRAME_HANDSHAKE, &hs_json)?;
    }

    // Main loop: read frames, dispatch, respond.
    let stdin = io::stdin();
    let mut reader = BufReader::new(stdin.lock());
    let stdout = io::stdout();
    let mut writer = BufWriter::new(stdout.lock());

    loop {
        match read_frame(&mut reader)? {
            ReadFrameResult::Eof => {
                // Clean shutdown path. mbgl will tear down via Drop when
                // renderer goes out of scope.
                break;
            }
            ReadFrameResult::Frame { typ, payload } => {
                if typ != FRAME_REQUEST {
                    let msg = format!("unexpected frame type: {}", typ as char);
                    write_frame(&mut writer, FRAME_ERROR, msg.as_bytes())?;
                    continue;
                }
                match handle_request(&mut renderer, &payload) {
                    Ok(rgba) => write_frame(&mut writer, FRAME_OK, &rgba)?,
                    Err(e) => {
                        let msg = format!("{e:#}");
                        write_frame(&mut writer, FRAME_ERROR, msg.as_bytes())?;
                    }
                }
            }
        }
        writer.flush()?;
    }
    Ok(())
}

fn handle_request(
    renderer: &mut maplibre_native::ImageRenderer<maplibre_native::Static>,
    payload: &[u8],
) -> Result<Vec<u8>> {
    let req: RenderRequest =
        serde_json::from_slice(payload).context("parsing request JSON")?;

    // JSON center is [lng, lat]; maplibre-native-rs render_static takes
    // (lat, lon). Swap.
    let lng = req.center[0];
    let lat = req.center[1];

    let w = NonZeroU32::new(req.width.max(1)).unwrap();
    let h = NonZeroU32::new(req.height.max(1)).unwrap();
    renderer.set_map_size(Size::new(Width(w.get()), Height(h.get())));

    let image = renderer
        .render_static(lat, lng, req.zoom, req.bearing, req.pitch)
        .context("render_static")?;

    // Return raw RGBA bytes (buffer.as_raw() gives the flat Vec<u8>).
    let buf = image.as_image();
    Ok(buf.as_raw().clone())
}

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
use maplibre_native::{ImageRenderer, ImageRendererBuilder, Static};
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
    let args = Args::parse();
    let _ = (&args.styles_dir, &args.fonts_dir);

    let fs_state = Arc::new(
        FsState::new(&args.mbtiles).context("initializing mbtiles FileSource state")?,
    );

    // Handshake is emitted BEFORE the first renderer build — the Go
    // orchestrator waits for it and we want to fail fast on mbtiles-open
    // errors before that point, not after.
    {
        let hs = Handshake { pid: std::process::id(), style: args.style_id.clone() };
        let hs_json = serde_json::to_vec(&hs)?;
        let mut stdout = io::stdout().lock();
        write_frame(&mut stdout, FRAME_HANDSHAKE, &hs_json)?;
    }

    // The renderer is lazy — built on the first R frame at that request's
    // exact size. In maplibre-native-rs v0.4.5, `set_map_size` does not
    // reliably resize the framebuffer between renders in Static mode (the
    // first render after a size change still uses the builder-time size),
    // so we rebuild the renderer whenever the requested (width, height)
    // changes. Same-size streams pay no extra cost; transitions eat one
    // style reload (~100-200 ms) per change.
    let mut cache = RendererCache::new(args, Arc::clone(&fs_state));

    let stdin = io::stdin();
    let mut reader = BufReader::new(stdin.lock());
    let stdout = io::stdout();
    let mut writer = BufWriter::new(stdout.lock());

    loop {
        match read_frame(&mut reader)? {
            ReadFrameResult::Eof => break,
            ReadFrameResult::Frame { typ, payload } => {
                if typ != FRAME_REQUEST {
                    let msg = format!("unexpected frame type: {}", typ as char);
                    write_frame(&mut writer, FRAME_ERROR, msg.as_bytes())?;
                    continue;
                }
                match handle_request(&mut cache, &payload) {
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

/// Per-(width, height) renderer cache. Rebuilds on size change; reuses
/// otherwise.
struct RendererCache {
    args: Args,
    fs_state: Arc<FsState>,
    current: Option<(u32, u32, ImageRenderer<Static>)>,
}

impl RendererCache {
    fn new(args: Args, fs_state: Arc<FsState>) -> Self {
        Self { args, fs_state, current: None }
    }

    fn for_size(&mut self, w: u32, h: u32) -> Result<&mut ImageRenderer<Static>> {
        if let Some((cur_w, cur_h, _)) = self.current.as_ref() {
            if *cur_w == w && *cur_h == h {
                return Ok(&mut self.current.as_mut().unwrap().2);
            }
        }

        // Drop the old renderer first so its ~200 MB footprint is freed
        // before we allocate a new one.
        self.current = None;

        let fs_cb_state = Arc::clone(&self.fs_state);
        let callback = move |url: &str, kind| fs_cb_state.handle(url, kind);

        let nw = NonZeroU32::new(w).context("width must be > 0")?;
        let nh = NonZeroU32::new(h).context("height must be > 0")?;

        let mut renderer = ImageRendererBuilder::new()
            .with_size(nw, nh)
            .with_pixel_ratio(self.args.ratio as f32)
            .with_file_source_callback(callback)
            .build_static_renderer();

        renderer
            .load_style_from_path(&self.args.style_path)
            .with_context(|| format!("loading style from {}", self.args.style_path))?;

        self.current = Some((w, h, renderer));
        Ok(&mut self.current.as_mut().unwrap().2)
    }
}

fn handle_request(cache: &mut RendererCache, payload: &[u8]) -> Result<Vec<u8>> {
    let req: RenderRequest =
        serde_json::from_slice(payload).context("parsing request JSON")?;

    // JSON center is [lng, lat]; maplibre-native-rs render_static takes
    // (lat, lon). Swap.
    let lng = req.center[0];
    let lat = req.center[1];

    let renderer = cache.for_size(req.width.max(1), req.height.max(1))?;
    let image = renderer
        .render_static(lat, lng, req.zoom, req.bearing, req.pitch)
        .context("render_static")?;

    // Defence-in-depth: if mbgl ever returns an unexpected size, fail the
    // request with a specific error rather than silently shipping bad bytes.
    // The Go orchestrator also checks this (`renderer: worker returned N
    // bytes, expected M`), so catching it here produces a clearer message.
    let buf = image.as_image();
    let got = buf.as_raw().len() as u32;
    let want = req.width * req.height * 4 * cache.args.ratio * cache.args.ratio;
    if got != want {
        anyhow::bail!(
            "rendered size mismatch: got {got} bytes, expected {want} for {}x{} @ ratio {}",
            req.width, req.height, cache.args.ratio
        );
    }
    Ok(buf.as_raw().clone())
}

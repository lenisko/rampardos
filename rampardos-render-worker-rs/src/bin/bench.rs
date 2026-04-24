//! Benchmark harness — spawns the Node and Rust render workers against the
//! same style+mbtiles, sends N identical render requests to each, compares
//! output and reports per-render latency + RSS drift over time.
//!
//! Output:
//!   - stdout: Markdown-ish summary (cold-start RSS, steady-state RSS at
//!     100/500/1000 renders, p50/p95/p99 latency, equivalence verdict).
//!   - --csv path: per-sample CSV of (renders_done, worker, rss_kb).

use std::io::{self, BufReader, BufWriter, Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};
use clap::Parser;
use serde::Serialize;

const FRAME_REQUEST: u8 = b'R';
const FRAME_OK: u8 = b'K';
const FRAME_ERROR: u8 = b'E';
const FRAME_HANDSHAKE: u8 = b'H';

#[derive(Parser, Debug)]
#[command(about = "Bench Node vs Rust render workers")]
struct Args {
    /// Path to the Node render-worker.js script.
    #[arg(long)]
    node_script: PathBuf,
    /// Path to the Rust render-worker binary.
    #[arg(long)]
    rust_binary: PathBuf,
    /// Absolute path to the style JSON.
    #[arg(long)]
    style_path: PathBuf,
    /// Absolute path to the mbtiles file.
    #[arg(long)]
    mbtiles: PathBuf,
    /// Absolute path to the styles root (for --styles-dir pass-through).
    #[arg(long)]
    styles_dir: PathBuf,
    /// Absolute path to the fonts root (for --fonts-dir pass-through).
    #[arg(long)]
    fonts_dir: PathBuf,
    /// Pixel ratio.
    #[arg(long, default_value_t = 1)]
    ratio: u32,
    /// Style id (for handshake logging).
    #[arg(long, default_value = "bench")]
    style_id: String,
    /// Number of render requests to send per worker.
    #[arg(long, default_value_t = 1000)]
    n: usize,
    /// Zoom for the test render.
    #[arg(long, default_value_t = 6.0)]
    zoom: f64,
    /// Longitude for the test render.
    #[arg(long, default_value_t = -1.47)]
    lon: f64,
    /// Latitude for the test render.
    #[arg(long, default_value_t = 53.38)]
    lat: f64,
    /// Render width.
    #[arg(long, default_value_t = 512)]
    width: u32,
    /// Render height.
    #[arg(long, default_value_t = 512)]
    height: u32,
    /// Write per-sample CSV to this path.
    #[arg(long)]
    csv: Option<PathBuf>,
    /// RSS sampling interval, in renders.
    #[arg(long, default_value_t = 100)]
    sample_every: usize,
    /// If set, write the first render from each worker as PNG into this
    /// directory (files node-first.png and rust-first.png). Useful for
    /// eyeballing output equivalence.
    #[arg(long)]
    out_dir: Option<PathBuf>,
}

struct Worker {
    name: &'static str,
    child: Child,
    stdin: BufWriter<std::process::ChildStdin>,
    stdout: BufReader<std::process::ChildStdout>,
    /// PID for /proc/<pid>/status RSS sampling.
    pid: u32,
}

impl Worker {
    fn spawn_node(args: &Args) -> Result<Self> {
        let cmd_args = common_cli_args(args);
        let child = Command::new("node")
            .arg(&args.node_script)
            .args(&cmd_args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .context("spawning node worker")?;
        Self::finalize("node", child)
    }

    fn spawn_rust(args: &Args) -> Result<Self> {
        let cmd_args = common_cli_args(args);
        let child = Command::new(&args.rust_binary)
            .args(&cmd_args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .context("spawning rust worker")?;
        Self::finalize("rust", child)
    }

    fn finalize(name: &'static str, mut child: Child) -> Result<Self> {
        let stdin = child.stdin.take().context("no stdin pipe")?;
        let stdout = child.stdout.take().context("no stdout pipe")?;
        let pid = child.id();
        let mut w = Self {
            name,
            child,
            stdin: BufWriter::new(stdin),
            stdout: BufReader::new(stdout),
            pid,
        };
        // Drain the handshake frame before any request goes in.
        let (typ, _payload) = read_frame(&mut w.stdout).context("reading handshake")?;
        if typ != FRAME_HANDSHAKE {
            bail!("{name} worker sent frame type {} before handshake", typ as char);
        }
        Ok(w)
    }

    fn send_request(&mut self, payload: &[u8]) -> Result<(u8, Vec<u8>)> {
        write_frame(&mut self.stdin, FRAME_REQUEST, payload)?;
        self.stdin.flush()?;
        read_frame(&mut self.stdout)
    }

    fn shutdown(mut self) -> Result<()> {
        // Closing stdin signals clean shutdown via EOF.
        drop(self.stdin);
        let _ = self.child.wait();
        Ok(())
    }
}

fn common_cli_args(args: &Args) -> Vec<String> {
    vec![
        "--style-id".into(), args.style_id.clone(),
        "--style-path".into(), args.style_path.to_string_lossy().into_owned(),
        "--mbtiles".into(), args.mbtiles.to_string_lossy().into_owned(),
        "--styles-dir".into(), args.styles_dir.to_string_lossy().into_owned(),
        "--fonts-dir".into(), args.fonts_dir.to_string_lossy().into_owned(),
        "--ratio".into(), args.ratio.to_string(),
    ]
}

fn write_frame<W: Write>(w: &mut W, typ: u8, payload: &[u8]) -> io::Result<()> {
    let mut header = [0u8; 5];
    header[0] = typ;
    header[1..5].copy_from_slice(&(payload.len() as u32).to_be_bytes());
    w.write_all(&header)?;
    if !payload.is_empty() {
        w.write_all(payload)?;
    }
    Ok(())
}

fn read_frame<R: Read>(r: &mut R) -> Result<(u8, Vec<u8>)> {
    let mut header = [0u8; 5];
    r.read_exact(&mut header).context("reading frame header")?;
    let typ = header[0];
    let len = u32::from_be_bytes([header[1], header[2], header[3], header[4]]) as usize;
    let mut payload = vec![0u8; len];
    if len > 0 {
        r.read_exact(&mut payload).context("reading frame payload")?;
    }
    Ok((typ, payload))
}

/// Read RSS in kB from /proc/<pid>/status (Linux).
fn read_rss_kb(pid: u32) -> Option<u64> {
    let path = format!("/proc/{pid}/status");
    let content = std::fs::read_to_string(&path).ok()?;
    for line in content.lines() {
        if let Some(rest) = line.strip_prefix("VmRSS:") {
            let rest = rest.trim();
            let num: String = rest.chars().take_while(|c| c.is_ascii_digit()).collect();
            return num.parse::<u64>().ok();
        }
    }
    None
}

#[derive(Serialize, Debug, Default, Clone)]
struct Sample {
    worker: String,
    renders_done: usize,
    rss_kb: u64,
    elapsed_ms: f64,
}

fn percentile(sorted: &[Duration], p: f64) -> Duration {
    if sorted.is_empty() {
        return Duration::ZERO;
    }
    let idx = ((sorted.len() as f64 - 1.0) * p).round() as usize;
    sorted[idx]
}

fn summarize(name: &str, latencies: &[Duration]) -> String {
    let mut sorted = latencies.to_vec();
    sorted.sort();
    let p50 = percentile(&sorted, 0.50);
    let p95 = percentile(&sorted, 0.95);
    let p99 = percentile(&sorted, 0.99);
    format!(
        "{name}: n={} p50={:.2}ms p95={:.2}ms p99={:.2}ms",
        latencies.len(),
        p50.as_secs_f64() * 1000.0,
        p95.as_secs_f64() * 1000.0,
        p99.as_secs_f64() * 1000.0,
    )
}

fn run_worker(
    worker: &mut Worker,
    req_json: &[u8],
    args: &Args,
) -> Result<(Vec<Duration>, Vec<Sample>, Vec<u8>)> {
    let mut latencies = Vec::with_capacity(args.n);
    let mut samples = Vec::new();
    let mut first_output: Option<Vec<u8>> = None;

    let bench_start = Instant::now();
    if let Some(rss) = read_rss_kb(worker.pid) {
        samples.push(Sample {
            worker: worker.name.to_string(),
            renders_done: 0,
            rss_kb: rss,
            elapsed_ms: 0.0,
        });
    }

    for i in 0..args.n {
        let start = Instant::now();
        let (typ, payload) = worker.send_request(req_json)?;
        let elapsed = start.elapsed();
        if typ == FRAME_ERROR {
            let msg = String::from_utf8_lossy(&payload);
            bail!("{} worker error frame on render {i}: {msg}", worker.name);
        }
        if typ != FRAME_OK {
            bail!("{} worker unexpected frame type on render {i}: {}", worker.name, typ as char);
        }
        latencies.push(elapsed);
        if first_output.is_none() {
            first_output = Some(payload.clone());
        }

        let renders_done = i + 1;
        if renders_done % args.sample_every == 0 || renders_done == args.n {
            if let Some(rss) = read_rss_kb(worker.pid) {
                samples.push(Sample {
                    worker: worker.name.to_string(),
                    renders_done,
                    rss_kb: rss,
                    elapsed_ms: bench_start.elapsed().as_secs_f64() * 1000.0,
                });
            }
        }
    }

    Ok((latencies, samples, first_output.unwrap_or_default()))
}

fn main() -> Result<()> {
    let args = Args::parse();

    let req = serde_json::json!({
        "zoom": args.zoom,
        "center": [args.lon, args.lat],
        "width": args.width,
        "height": args.height,
        "bearing": 0.0,
        "pitch": 0.0,
    });
    let req_json = serde_json::to_vec(&req)?;

    eprintln!("--> spawning node worker");
    let mut node = Worker::spawn_node(&args)?;
    eprintln!("--> spawning rust worker");
    let mut rust = Worker::spawn_rust(&args)?;

    eprintln!("--> running {} renders on each worker…", args.n);
    let (n_lat, n_samples, n_first) = run_worker(&mut node, &req_json, &args)?;
    let (r_lat, r_samples, r_first) = run_worker(&mut rust, &req_json, &args)?;

    // Output equivalence check.
    let verdict = if n_first == r_first {
        "byte-exact".to_string()
    } else if n_first.len() == r_first.len() {
        let mut diff = 0usize;
        let mut first_divergence: Option<usize> = None;
        for (i, (a, b)) in n_first.iter().zip(r_first.iter()).enumerate() {
            if a != b {
                diff += 1;
                if first_divergence.is_none() {
                    first_divergence = Some(i);
                }
            }
        }
        format!(
            "size-match, {} of {} bytes differ (first offset {:?}: node=0x{:02x} rust=0x{:02x})",
            diff,
            n_first.len(),
            first_divergence,
            first_divergence.map(|i| n_first[i]).unwrap_or(0),
            first_divergence.map(|i| r_first[i]).unwrap_or(0),
        )
    } else {
        format!(
            "size-mismatch: node={} bytes, rust={} bytes",
            n_first.len(),
            r_first.len()
        )
    };

    // CSV dump.
    if let Some(csv_path) = &args.csv {
        let mut f = std::fs::File::create(csv_path)?;
        writeln!(f, "worker,renders_done,rss_kb,elapsed_ms")?;
        for s in n_samples.iter().chain(r_samples.iter()) {
            writeln!(f, "{},{},{},{:.2}", s.worker, s.renders_done, s.rss_kb, s.elapsed_ms)?;
        }
    }

    // PNG dump.
    if let Some(out_dir) = &args.out_dir {
        std::fs::create_dir_all(out_dir)?;
        let w = args.width * args.ratio;
        let h = args.height * args.ratio;
        for (label, bytes) in [("node", &n_first), ("rust", &r_first)] {
            if bytes.len() as u32 != w * h * 4 {
                eprintln!("warn: {label} bytes len={} != w*h*4={}", bytes.len(), w * h * 4);
                continue;
            }
            let path = out_dir.join(format!("{label}-first.png"));
            let file = std::fs::File::create(&path)?;
            let mut encoder = png::Encoder::new(std::io::BufWriter::new(file), w, h);
            encoder.set_color(png::ColorType::Rgba);
            encoder.set_depth(png::BitDepth::Eight);
            let mut writer = encoder.write_header()?;
            writer.write_image_data(bytes)?;
            eprintln!("--> wrote {}", path.display());
        }
    }

    // Stdout report.
    println!("# Render worker bench\n");
    println!("## Latency\n");
    println!("- {}", summarize("node", &n_lat));
    println!("- {}", summarize("rust", &r_lat));
    println!("\n## RSS (VmRSS, kB)\n");
    report_rss_milestones("node", &n_samples);
    report_rss_milestones("rust", &r_samples);
    println!("\n## Output equivalence\n");
    println!("- first-render: {verdict}");

    node.shutdown()?;
    rust.shutdown()?;

    Ok(())
}

fn report_rss_milestones(name: &str, samples: &[Sample]) {
    let pick = |n: usize| samples.iter().find(|s| s.renders_done == n).map(|s| s.rss_kb);
    let cold = samples.first().map(|s| s.rss_kb);
    print!("- {name}: cold={:?}", cold);
    for n in [100usize, 500, 1000] {
        if let Some(rss) = pick(n) {
            print!(" r{n}={rss}");
        }
    }
    println!();
}

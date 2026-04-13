package renderer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var osStderr io.Writer = os.Stderr

// RenderError is a render-level failure reported by the worker. It
// wraps a worker-side message and is distinct from context errors
// (timeout, cancellation) and I/O errors (pipe breakage, bad framing).
type RenderError struct {
	Message string
}

func (e *RenderError) Error() string {
	return "renderer: worker error: " + e.Message
}

// workerArgs configures a single worker subprocess.
type workerArgs struct {
	binary           string   // "node" or "bash" (for tests)
	script           string   // path to script
	scriptArgs       []string // additional args passed after `script`
	styleID          string   // logged; also validated against the handshake
	handshakeTimeout time.Duration
}

// handshakePayload is the JSON the worker sends on startup to indicate readiness.
type handshakePayload struct {
	PID   int    `json:"pid"`
	Style string `json:"style"`
}

// worker is a single child process speaking the frame protocol.
// Exactly one request is in flight at a time; the pool serialises
// access per worker.
type worker struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	handshake handshakePayload

	renders  atomic.Int64
	killOnce sync.Once
}

// spawnWorker starts a child process, waits for its handshake frame,
// and returns a worker ready for dispatch calls.
func spawnWorker(args workerArgs) (*worker, error) {
	cmdArgs := append([]string{args.script}, args.scriptArgs...)
	cmd := exec.Command(args.binary, cmdArgs...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("renderer: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("renderer: stdout pipe: %w", err)
	}

	// Forward stderr to our own stderr so worker diagnostics are
	// visible in logs. This does not affect the frame protocol on
	// stdout.
	cmd.Stderr = newPrefixWriter("[render-worker:" + args.styleID + "] ")

	// Start the worker in its own process group so kill() can
	// terminate the entire group (bash + child processes like sleep).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("renderer: start worker: %w", err)
	}

	w := &worker{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
	}

	// Read the handshake with a deadline.
	type hsResult struct {
		hs  handshakePayload
		err error
	}
	done := make(chan hsResult, 1)
	go func() {
		typ, payload, err := ReadFrame(w.stdout)
		if err != nil {
			done <- hsResult{err: err}
			return
		}
		if typ != FrameHandshake {
			done <- hsResult{err: fmt.Errorf("renderer: expected handshake, got frame type %q", typ)}
			return
		}
		var hs handshakePayload
		if err := json.Unmarshal(payload, &hs); err != nil {
			done <- hsResult{err: fmt.Errorf("renderer: handshake JSON: %w", err)}
			return
		}
		done <- hsResult{hs: hs}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			w.kill()
			return nil, res.err
		}
		w.handshake = res.hs
		return w, nil
	case <-time.After(args.handshakeTimeout):
		w.kill()
		return nil, fmt.Errorf("renderer: worker handshake timed out after %s", args.handshakeTimeout)
	}
}

// dispatch sends a single request frame and awaits either a K/E
// response frame or the context's deadline. On deadline, the worker
// is killed.
func (w *worker) dispatch(ctx context.Context, requestJSON []byte) ([]byte, error) {
	if err := WriteFrame(w.stdin, FrameRequest, requestJSON); err != nil {
		return nil, fmt.Errorf("renderer: write request: %w", err)
	}

	type frameResult struct {
		typ     byte
		payload []byte
		err     error
	}
	done := make(chan frameResult, 1)
	go func() {
		typ, payload, err := ReadFrame(w.stdout)
		done <- frameResult{typ: typ, payload: payload, err: err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("renderer: read response: %w", res.err)
		}
		switch res.typ {
		case FrameOK:
			w.renders.Add(1)
			return res.payload, nil
		case FrameError:
			return nil, &RenderError{Message: string(res.payload)}
		default:
			return nil, fmt.Errorf("renderer: unexpected response frame type %q", res.typ)
		}
	case <-ctx.Done():
		w.kill()
		return nil, ctx.Err()
	}
}

// kill terminates the worker and cleans up pipes. Safe to call
// repeatedly — the actual kill logic runs exactly once via sync.Once.
func (w *worker) kill() {
	w.killOnce.Do(func() {
		_ = w.stdin.Close()
		if w.cmd != nil && w.cmd.Process != nil {
			_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
		}
		// Reap asynchronously so the kernel can release resources.
		go func() { _ = w.cmd.Wait() }()
	})
}

// prefixWriter writes to stderr with a [worker:id] prefix on each line.
type prefixWriter struct {
	prefix []byte
	mu     sync.Mutex
	buf    bytes.Buffer
}

func newPrefixWriter(prefix string) *prefixWriter {
	return &prefixWriter{prefix: []byte(prefix)}
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	// Buffer incomplete lines so we don't interleave mid-line.
	pw.buf.Write(p)
	for {
		line, err := pw.buf.ReadBytes('\n')
		if err != nil {
			// Put the partial line back.
			if len(line) > 0 {
				pw.buf.Reset()
				pw.buf.Write(line)
			}
			break
		}
		// Print: prefix + line (line already ends with \n).
		_, _ = io.WriteString(stderr(), string(pw.prefix)+string(line))
	}
	return len(p), nil
}

// stderr is a thin indirection so tests could capture if needed.
// Currently returns os.Stderr.
func stderr() io.Writer {
	return osStderr
}

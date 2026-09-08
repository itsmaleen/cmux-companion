package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
	"github.com/itsmaleen/cmux-companion/bridge/internal/screen"
)

const (
	// frameChannelCap bounds how far a slow phone can lag behind the live
	// stream before backpressure kicks in.
	frameChannelCap = 64
	// maxFrameLineBytes bounds one NDJSON record. A full repaint of a large
	// pane can run tens of KB; this leaves generous headroom.
	maxFrameLineBytes = 4 << 20

	defaultNativeCols = 120
	defaultNativeRows = 40

	// defaultFrameBackpressureTimeout is how long a send may block on a full
	// channel before the stream gives up rather than let the phone silently
	// miss a delta in the middle of the stream — see the doc comment on
	// pumpFrames.
	defaultFrameBackpressureTimeout = 2 * time.Second
	// defaultFinalFrameEventTimeout bounds how long the pump waits to deliver
	// the terminal Ended event once the reader may already be long gone.
	defaultFinalFrameEventTimeout = 5 * time.Second
)

// frameRecord is one line of `herdr terminal session observe`'s NDJSON.
type frameRecord struct {
	Type     string `json:"type"`
	Seq      int    `json:"seq"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Encoding string `json:"encoding"`
	Full     bool   `json:"full"`
	Bytes    string `json:"bytes"`
}

// herdrBinaryPath locates the herdr CLI: PATH first, falling back to the
// Homebrew location the installer uses — a launchd-started bridge often
// doesn't inherit a login shell's PATH.
func herdrBinaryPath() string {
	if p, err := exec.LookPath("herdr"); err == nil {
		return p
	}
	return "/opt/homebrew/bin/herdr"
}

// buildObserveCmd is the real observeCmd: it spawns herdr's read-only frame
// observer against this backend's own socket (via HERDR_SOCKET_PATH, so a
// named session is reached without needing to know its --session flag name).
func (b *Backend) buildObserveCmd(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, herdrBinaryPath(), "terminal", "session", "observe", paneID,
		"--cols", strconv.Itoa(cols), "--rows", strconv.Itoa(rows))
	cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+b.cfg.SocketPath)
	return cmd
}

// Frames implements backend.FrameSource by spawning
// `herdr terminal session observe` and decoding its NDJSON stream.
func (b *Backend) Frames(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.FrameEvent, backend.FrameInfo, error) {
	if !b.Connected() {
		return nil, backend.FrameInfo{}, backend.Errorf("backend_unavailable", "herdr is not connected")
	}

	// Always resolve the pane's native size, even when the caller supplied
	// both dimensions: this is also how a nonexistent pane is caught
	// synchronously as `not_found`, rather than only surfacing as an
	// asynchronous `error`-reason end-of-stream once the subprocess exits.
	nativeCols, nativeRows, err := b.nativeSize(surfaceID)
	if err != nil {
		return nil, backend.FrameInfo{}, backend.Errorf("not_found", err.Error())
	}
	if cols <= 0 {
		cols = nativeCols
	}
	if rows <= 0 {
		rows = nativeRows
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := b.observeCmd(cctx, surfaceID, cols, rows)
	// Kill the observer's whole process group on cancel, not just its leader:
	// a child it spawned would otherwise keep our stdout pipe open and the
	// reader blocked long after the stream was cancelled. WaitDelay then
	// force-closes the pipe if anything still holds it once the leader is gone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, backend.FrameInfo{}, backend.Errorf("frames_error", "observe stdout: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, backend.FrameInfo{}, backend.Errorf("frames_error", "observe start: "+err.Error())
	}

	backpressureTimeout := b.frameBackpressureTimeout
	if backpressureTimeout <= 0 {
		backpressureTimeout = defaultFrameBackpressureTimeout
	}
	finalTimeout := b.finalFrameEventTimeout
	if finalTimeout <= 0 {
		finalTimeout = defaultFinalFrameEventTimeout
	}

	out := make(chan backend.FrameEvent, frameChannelCap)
	go pumpFrames(cctx, cancel, cmd, stdout, surfaceID, out, backpressureTimeout, finalTimeout)
	return out, backend.FrameInfo{Width: cols, Height: rows}, nil
}

// Screens implements backend.ScreenSource by rendering this backend's own
// frame stream (bridge-side, via a headless VT emulator) instead of shipping
// raw ANSI bytes for the phone to interpret — see package screen.
func (b *Backend) Screens(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.ScreenEvent, backend.FrameInfo, error) {
	return screen.FromFrames(ctx, b, surfaceID, cols, rows)
}

// nativeSize resolves the grid to request when the phone didn't pin one:
// rows from the pane's own viewport (scroll.viewport_rows), cols from its
// rect in the current tab layout. A pane absent from every layout (not in the
// active tab of any workspace right now) falls back to herdr's own default
// grid, matching what `observe` uses when --cols/--rows are omitted.
func (b *Backend) nativeSize(paneID string) (cols, rows int, err error) {
	p, err := b.pane(paneID)
	if err != nil {
		return 0, 0, err
	}
	rows = defaultNativeRows
	if p.Scroll != nil && p.Scroll.ViewportRows > 0 {
		rows = int(p.Scroll.ViewportRows)
	}
	cols = defaultNativeCols
	var snap sessionSnapshot
	if err := b.client.callInto("session.snapshot", nil, &snap); err == nil {
		for _, layout := range snap.Snapshot.Layouts {
			for _, lp := range layout.Panes {
				if lp.PaneID == paneID && lp.Rect.Width > 0 {
					cols = lp.Rect.Width
					return cols, rows, nil
				}
			}
		}
	}
	return cols, rows, nil
}

// pumpFrames decodes NDJSON off the observer's stdout and relays it as
// FrameEvents until the stream ends, the subprocess is killed by ctx
// cancellation, or a slow consumer trips the backpressure timeout.
//
// On backpressure the stream is torn down entirely rather than dropping the
// one delta that didn't fit: a lost delta would desync the phone's rendered
// grid from what the pane actually contains, silently and permanently. Ending
// the stream instead forces a resubscribe, which starts over with a fresh
// full frame — the same "skip ahead, never corrupt" trade mosh makes for a
// laggy connection.
//
// Lines are read on their own goroutine so a cancel is honoured immediately
// even while the reader is blocked on a pipe nothing is writing to; the
// deferred Wait (with the process group killed and WaitDelay set) is what
// unblocks that reader.
func pumpFrames(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, stdout io.Reader, surfaceID string, out chan backend.FrameEvent, backpressureTimeout, finalTimeout time.Duration) {
	defer close(out)
	defer func() { _ = cmd.Wait() }()
	defer cancel()

	lines := make(chan []byte, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), maxFrameLineBytes)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			select {
			case lines <- append([]byte(nil), line...):
			case <-ctx.Done():
				return
			}
		}
	}()

	reason := "error" // default: the process/stream ended without terminal.closed
scan:
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return
		case l, ok := <-lines:
			if !ok {
				break scan
			}
			line = l
		}
		var rec frameRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // tolerate a stray malformed line rather than killing the stream
		}
		switch rec.Type {
		case "terminal.frame":
			f := backend.Frame{
				SurfaceID: surfaceID,
				Seq:       rec.Seq,
				Full:      rec.Full,
				Width:     rec.Width,
				Height:    rec.Height,
				Encoding:  rec.Encoding,
				Bytes:     rec.Bytes,
			}
			if !sendFrameEvent(ctx, out, backend.FrameEvent{Frame: &f}, backpressureTimeout) {
				if ctx.Err() != nil {
					return // externally cancelled; the caller already knows why
				}
				reason = "backpressure"
				break scan
			}
		case "terminal.closed":
			reason = "closed"
			break scan
		}
	}
	if ctx.Err() != nil {
		return
	}
	cancel() // ensure the subprocess is gone even if terminal.closed didn't end it
	sendFrameEvent(context.Background(), out, backend.FrameEvent{Ended: reason}, finalTimeout)
}

// sendFrameEvent tries to deliver ev, giving up after timeout if that would
// block (or immediately once ctx is done). timeout <= 0 blocks until either
// delivered or ctx is done.
func sendFrameEvent(ctx context.Context, out chan<- backend.FrameEvent, ev backend.FrameEvent, timeout time.Duration) bool {
	if timeout <= 0 {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case out <- ev:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

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
	"sync"
	"syscall"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// defaultFitEstablishTimeout is how long Fit waits for either a first
// terminal.frame (or the controller simply still running with no
// terminal.closed) or a terminal.closed record before deciding whether the
// fit was established — see the wire contract in shared/protocol.md.
const defaultFitEstablishTimeout = 2 * time.Second

// controlRecord is one line of `herdr terminal session control`'s NDJSON.
// terminal.frame records share terminal.frame's shape (unused here beyond
// its type — frames are discarded, not relayed); terminal.closed carries the
// reason herdr ended (or refused) the control session.
type controlRecord struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// buildControlCmd is the real controlCmd: it spawns herdr's PTY-resizing
// controller against this backend's own socket.
func (b *Backend) buildControlCmd(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, herdrBinaryPath(), "terminal", "session", "control", paneID,
		"--cols", strconv.Itoa(cols), "--rows", strconv.Itoa(rows))
	cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+b.cfg.SocketPath)
	return cmd
}

// fitHandle implements backend.FitHandle over one `herdr terminal session
// control` subprocess.
type fitHandle struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu    sync.Mutex
	stdin io.WriteCloser

	// done is closed by pumpControl once the subprocess is fully reaped,
	// whether that's because it ended on its own or because Release cancelled
	// it. reason (set under mu before done closes) is left empty in the
	// Release case, which is what tells the ws handler's forwarder not to
	// push a surface.fit.ended for it — see backend.FitHandle.Done's doc.
	done   chan struct{}
	reason string
}

func (h *fitHandle) Resize(cols, rows int) error {
	line, err := json.Marshal(map[string]any{"type": "terminal.resize", "cols": cols, "rows": rows})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.stdin.Write(line)
	return err
}

func (h *fitHandle) Done() <-chan struct{} { return h.done }

func (h *fitHandle) Reason() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

// Release ends the fit and restores herdr's own layout size, by killing the
// control subprocess's whole process group (same as Frames' cancellation).
// Cancelling ctx here — rather than any other signal — is exactly what keeps
// pumpControl from ever setting a reason or reporting this as an "ended on
// its own" event: see pumpControl's ctx.Done case.
func (h *fitHandle) Release() { h.cancel() }

func (h *fitHandle) setReason(reason string) {
	h.mu.Lock()
	h.reason = reason
	h.mu.Unlock()
}

// controlOutcome is pumpControl's report of how establishment resolved,
// consumed once by Fit while it decides whether to hand back a handle.
type controlOutcome struct {
	established  bool
	closedReason string // set only when terminal.closed arrived before establishment
	eof          bool   // stdout closed with neither a frame nor terminal.closed observed
}

// Fit implements backend.Fitter by spawning `herdr terminal session control`
// and driving it until Release or its own end. It validates the pane exists
// the same way Frames does (nativeSize's sibling: b.pane), then waits up to
// the establish timeout for the controller to either emit a first frame
// (fit established — herdr already resized the PTY by the time it does),
// still be running with nothing said either way (also established: an idle
// pane may simply have nothing to repaint), or answer terminal.closed (the
// control session was refused or immediately ended — reported as
// `fit_error` with herdr's own reason).
func (b *Backend) Fit(ctx context.Context, surfaceID string, cols, rows int) (backend.FitHandle, error) {
	if !b.Connected() {
		return nil, backend.Errorf("backend_unavailable", "herdr is not connected")
	}
	if _, err := b.pane(surfaceID); err != nil {
		return nil, backend.Errorf("not_found", err.Error())
	}

	cctx, cancel := context.WithCancel(ctx)
	cmd := b.controlCmd(cctx, surfaceID, cols, rows)
	// Same process-group + WaitDelay handling as Frames' observe subprocess:
	// kill the whole group on cancel so a child the controller spawned can't
	// keep stdout open past it, and force-close the pipe if anything still
	// holds it once the leader is gone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, backend.Errorf("fit_error", "control stdin: "+err.Error())
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, backend.Errorf("fit_error", "control stdout: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, backend.Errorf("fit_error", "control start: "+err.Error())
	}

	h := &fitHandle{cmd: cmd, cancel: cancel, stdin: stdin, done: make(chan struct{})}

	establishTimeout := b.fitEstablishTimeout
	if establishTimeout <= 0 {
		establishTimeout = defaultFitEstablishTimeout
	}

	outcome := make(chan controlOutcome, 1)
	go pumpControl(cctx, h, stdout, outcome)

	// An idle pane emits no frame, so waiting for one would cost the whole
	// timeout on every fit. The resize itself is observable right away:
	// poll the pane's viewport until it reports the requested rows. stopPoll
	// is closed once establishment resolves so this goroutine doesn't keep
	// querying herdr every 100ms for the whole life of the fit.
	confirmed := make(chan struct{}, 1)
	stopPoll := make(chan struct{})
	defer close(stopPoll)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-stopPoll:
				return
			case <-ticker.C:
				if got, err := b.liveViewportRows(surfaceID); err == nil && got == rows {
					select {
					case confirmed <- struct{}{}:
					default:
					}
					return
				}
			}
		}
	}()

	select {
	case <-confirmed:
		return h, nil
	case ev := <-outcome:
		if ev.established {
			// A first frame arrives BEFORE the resize lands (seen live: the
			// pane still reported its old rows 43 ms in), so it proves only
			// that the controller is attached. Keep waiting for the viewport
			// to confirm, or the timeout to pass.
			select {
			case <-confirmed:
				return h, nil
			case <-time.After(establishTimeout):
				return h, nil
			case <-cctx.Done():
				// The controller attached (it sent a frame) and then ended
				// before the viewport confirmed. It was a fit while it
				// lasted: hand back the handle, whose Done/Reason report the
				// end the same way a later death would.
				return h, nil
			}
		}
		// Not established: tear the process down and wait for pumpControl to
		// finish reaping it before reporting the failure, so no zombie or
		// leaked goroutine outlives this call.
		cancel()
		<-h.done
		reason := ev.closedReason
		if reason == "" {
			reason = "control process exited before the fit was established"
		}
		return nil, backend.Errorf("fit_error", "herdr closed the fit: "+reason)
	case <-time.After(establishTimeout):
		// Still running with no closed record: treat as established (see the
		// doc comment above).
		return h, nil
	}
}

// pumpControl decodes NDJSON off the controller's stdout, discarding every
// terminal.frame (draining continuously is required — the subprocess blocks
// on a full stdout pipe otherwise) while watching for the first frame (or a
// terminal.closed) to report establishment, and for a LATER terminal.closed
// to report how the fit ended on its own.
//
// Lines are read on their own goroutine, exactly as pumpFrames does, so a
// cancel (Release, or the owning connection closing) is honoured immediately
// even while the reader is blocked on a pipe nothing is writing to.
func pumpControl(ctx context.Context, h *fitHandle, stdout io.Reader, outcome chan<- controlOutcome) {
	defer close(h.done)
	defer func() { _ = h.cmd.Wait() }()
	defer h.cancel()

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

	established := false
	for {
		select {
		case <-ctx.Done():
			// Released (or the connection closed): h.reason stays "" so no
			// surface.fit.ended is ever pushed for this — the caller of
			// Release already knows why.
			return
		case line, ok := <-lines:
			if !ok {
				// stdout closed without a terminal.closed record: the
				// controller died unexpectedly.
				if !established {
					trySendOutcome(outcome, controlOutcome{eof: true})
				}
				h.setReason("error")
				return
			}
			var rec controlRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				continue // tolerate a stray malformed line, as Frames does
			}
			switch rec.Type {
			case "terminal.frame":
				if !established {
					established = true
					trySendOutcome(outcome, controlOutcome{established: true})
				}
				// Discard the frame itself; established or not, the PTY is
				// already resized by the time herdr emits one.
			case "terminal.closed":
				if !established {
					trySendOutcome(outcome, controlOutcome{closedReason: rec.Reason})
				}
				h.setReason("closed")
				return
			}
		}
	}
}

// trySendOutcome delivers ev without blocking: outcome is buffered for
// exactly the one send pumpControl ever makes before Fit stops reading it
// (once established, or once the establish timeout has already fired).
func trySendOutcome(outcome chan<- controlOutcome, ev controlOutcome) {
	select {
	case outcome <- ev:
	default:
	}
}

// liveViewportRows reads a pane's current viewport rows straight from herdr.
// The backend's pane cache is refreshed by pane.updated events, which herdr
// does not emit for a viewport change, so it cannot confirm a resize.
func (b *Backend) liveViewportRows(paneID string) (int, error) {
	var res struct {
		Pane paneInfo `json:"pane"`
	}
	if err := b.client.callInto("pane.get", map[string]any{"pane_id": paneID}, &res); err != nil {
		return 0, err
	}
	if res.Pane.Scroll == nil {
		return 0, backend.Errorf("fit_error", "pane reports no viewport")
	}
	return int(res.Pane.Scroll.ViewportRows), nil
}

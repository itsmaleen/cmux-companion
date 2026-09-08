package screen

import (
	"context"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

const (
	// coalesceInterval bounds how often an update is emitted once the first
	// (always-immediate) one has gone out: at most a handful of UI updates a
	// second, the same trade mosh makes for a fast-scrolling terminal.
	coalesceInterval = 150 * time.Millisecond
	// screenBackpressureTimeout bounds how long Stream may block delivering
	// one update to a full output channel before giving up on the reader
	// entirely. Because every update is a diff against the last one actually
	// SENT (not merely computed), skipping ticks while the reader lags is
	// never lossy on its own — but a channel still full past this means the
	// reader is gone, not just slow; see herdr/frames.go's pumpFrames for the
	// identical trade on the underlying frame stream.
	screenBackpressureTimeout = 2 * time.Second
	// screenFinalEventTimeout bounds how long Stream waits to deliver the
	// terminal Ended event once the reader may already be long gone.
	screenFinalEventTimeout = 5 * time.Second
)

// Stream feeds every frame off frames into a fresh Renderer at cols x rows
// and emits coalesced ScreenEvents: the very first update (always full, since
// a brand-new Renderer has never sent one) goes out as soon as the first
// frame is fed, then at most one update every coalesceInterval while
// anything changed since the last one sent. Ends, with the frame stream's own
// reason, when frames ends or is closed; never blocks delivering to the
// returned channel longer than screenBackpressureTimeout (then ends with
// "backpressure" instead).
func Stream(ctx context.Context, frames <-chan backend.FrameEvent, cols, rows int) <-chan backend.ScreenEvent {
	out := make(chan backend.ScreenEvent, 1)
	go runStream(ctx, frames, cols, rows, out)
	return out
}

// FromFrames adapts any backend.FrameSource into a backend.ScreenSource: it
// starts a frame stream for surfaceID and pipes it through Stream. Backends
// implement Screens by delegating to this rather than reimplementing the
// coalescing logic.
func FromFrames(ctx context.Context, src backend.FrameSource, surfaceID string, cols, rows int) (<-chan backend.ScreenEvent, backend.FrameInfo, error) {
	frames, info, err := src.Frames(ctx, surfaceID, cols, rows)
	if err != nil {
		return nil, backend.FrameInfo{}, err
	}
	events := Stream(ctx, frames, info.Width, info.Height)
	// Stamp every update with the surface id the caller asked about, the
	// same way pumpFrames fills in Frame.SurfaceID — Stream itself is
	// surface-agnostic.
	out := make(chan backend.ScreenEvent, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Update != nil {
					u := *ev.Update
					u.SurfaceID = surfaceID
					ev = backend.ScreenEvent{Update: &u}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
				if ev.Update == nil {
					return
				}
			}
		}
	}()
	return out, info, nil
}

func runStream(ctx context.Context, frames <-chan backend.FrameEvent, cols, rows int, out chan<- backend.ScreenEvent) {
	defer close(out)
	r := New(cols, rows)

	var (
		dirty     bool
		sentFirst bool
		timer     *time.Timer
		timerC    <-chan time.Time
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	// emit renders and sends one update, reporting whether it was delivered.
	emit := func() bool {
		u := r.Update(false)
		dirty = false
		sentFirst = true
		return sendScreenEvent(ctx, out, backend.ScreenEvent{Update: &u}, screenBackpressureTimeout)
	}
	giveUp := func() {
		if ctx.Err() != nil {
			return // externally cancelled; the caller already knows why
		}
		sendScreenEvent(context.Background(), out, backend.ScreenEvent{Ended: "backpressure"}, screenFinalEventTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-frames:
			if !ok {
				return
			}
			if ev.Frame != nil {
				if err := r.Feed(*ev.Frame); err != nil {
					// A delta is incremental: skipping it desyncs the emulator
					// from the pane for every later render, with no full repaint
					// to recover until a resubscribe. End the stream instead so
					// the phone resubscribes and gets a fresh full update —
					// the same "skip ahead, never corrupt" trade frames make.
					sendScreenEvent(context.Background(), out, backend.ScreenEvent{Ended: "error"}, screenFinalEventTimeout)
					return
				}
				dirty = true
				if !sentFirst {
					if !emit() {
						giveUp()
						return
					}
					continue
				}
				if timer == nil {
					timer = time.NewTimer(coalesceInterval)
					timerC = timer.C
				}
				continue
			}
			// The frame stream ended: flush anything pending, then relay why.
			if dirty {
				emit()
			}
			sendScreenEvent(context.Background(), out, backend.ScreenEvent{Ended: ev.Ended}, screenFinalEventTimeout)
			return

		case <-timerC:
			timer = nil
			timerC = nil
			if dirty {
				if !emit() {
					giveUp()
					return
				}
			}
		}
	}
}

// sendScreenEvent tries to deliver ev, giving up after timeout if that would
// block (or immediately once ctx is done). timeout <= 0 blocks until either
// delivered or ctx is done.
func sendScreenEvent(ctx context.Context, out chan<- backend.ScreenEvent, ev backend.ScreenEvent, timeout time.Duration) bool {
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

package screen

import (
	"context"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

func fullFrame(w, h int, ansi string) backend.Frame {
	return backend.Frame{Full: true, Width: w, Height: h, Bytes: b64(ansi)}
}

func deltaFrame(w, h int, ansi string) backend.Frame {
	return backend.Frame{Full: false, Width: w, Height: h, Bytes: b64(ansi)}
}

func recvUpdate(t *testing.T, ch <-chan backend.ScreenEvent, timeout time.Duration) backend.ScreenEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a screen event")
		return backend.ScreenEvent{}
	}
}

func TestStreamFirstUpdateIsImmediateAndFull(t *testing.T) {
	frames := make(chan backend.FrameEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := Stream(ctx, frames, 10, 2)

	frames <- backend.FrameEvent{Frame: p(fullFrame(10, 2, "\x1b[2J\x1b[1;1Hhi"))}
	ev := recvUpdate(t, out, time.Second)
	if ev.Update == nil || !ev.Update.Full {
		t.Fatalf("first event = %+v, want an immediate full update", ev)
	}
}

func TestStreamCoalescesQuickDeltasIntoOneUpdate(t *testing.T) {
	frames := make(chan backend.FrameEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := Stream(ctx, frames, 10, 3)

	frames <- backend.FrameEvent{Frame: p(fullFrame(10, 3, "\x1b[2J\x1b[1;1Ha0\x1b[2;1Ha1\x1b[3;1Ha2"))}
	first := recvUpdate(t, out, time.Second)
	if !first.Update.Full {
		t.Fatalf("first update = %+v, want full", first.Update)
	}

	// Three quick deltas, each touching a different row, sent well within
	// one coalesce window.
	frames <- backend.FrameEvent{Frame: p(deltaFrame(10, 3, "\x1b[1;1Hb0"))}
	frames <- backend.FrameEvent{Frame: p(deltaFrame(10, 3, "\x1b[2;1Hb1"))}
	frames <- backend.FrameEvent{Frame: p(deltaFrame(10, 3, "\x1b[3;1Hb2"))}

	// The next update must arrive only once (not once per delta) and must
	// carry all three changed rows.
	second := recvUpdate(t, out, time.Second)
	if second.Update == nil {
		t.Fatalf("second event = %+v, want an update", second)
	}
	if len(second.Update.Lines) != 3 {
		t.Fatalf("coalesced update lines = %+v, want all 3 changed rows in one update", second.Update.Lines)
	}
	seen := map[int]bool{}
	for _, l := range second.Update.Lines {
		seen[l.I] = true
	}
	if !seen[0] || !seen[1] || !seen[2] {
		t.Fatalf("coalesced update rows = %v, want 0,1,2", second.Update.Lines)
	}

	// Confirm nothing else was queued behind it (no per-delta update leaked
	// through): the channel should now be quiet for a while.
	select {
	case ev := <-out:
		t.Fatalf("unexpected extra event %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStreamEndsWithFrameStreamReason(t *testing.T) {
	frames := make(chan backend.FrameEvent, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := Stream(ctx, frames, 5, 1)

	frames <- backend.FrameEvent{Frame: p(fullFrame(5, 1, "\x1b[2J\x1b[1;1Hhi"))}
	recvUpdate(t, out, time.Second)

	frames <- backend.FrameEvent{Ended: "closed"}
	ev := recvUpdate(t, out, time.Second)
	if ev.Update != nil || ev.Ended != "closed" {
		t.Fatalf("ended event = %+v, want Ended closed", ev)
	}
	if _, ok := <-out; ok {
		t.Fatal("channel should close after the ended event")
	}
}

func TestStreamTornDownByContextCancel(t *testing.T) {
	frames := make(chan backend.FrameEvent, 2)
	ctx, cancel := context.WithCancel(context.Background())
	out := Stream(ctx, frames, 5, 1)

	frames <- backend.FrameEvent{Frame: p(fullFrame(5, 1, "\x1b[2J\x1b[1;1Hhi"))}
	recvUpdate(t, out, time.Second)

	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected the channel to close on ctx cancel with no further event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after ctx cancel")
	}
}

func p(f backend.Frame) *backend.Frame { return &f }

package multi

import (
	"context"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// frameFake is a fake backend that also implements backend.FrameSource, so
// its member can be routed to by multi.Frames. Plain *fake deliberately does
// NOT implement it — mirroring cmux, which has no frame stream — so a
// composite naming a cmux surface still answers `unsupported`.
type frameFake struct {
	*fake
	events chan backend.FrameEvent
	info   backend.FrameInfo
	err    error

	lastSurfaceID      string
	lastCols, lastRows int
}

func newFrameFake(kind string) *frameFake {
	return &frameFake{fake: newFake(kind), events: make(chan backend.FrameEvent, 4)}
}

// Info overrides the embedded fake's to report the Frames capability, the
// same way herdr's real Info() does.
func (f *frameFake) Info() backend.Info {
	info := f.fake.Info()
	info.Capabilities.Frames = true
	return info
}

func (f *frameFake) Frames(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.FrameEvent, backend.FrameInfo, error) {
	f.lastSurfaceID = surfaceID
	f.lastCols, f.lastRows = cols, rows
	if f.err != nil {
		return nil, backend.FrameInfo{}, f.err
	}
	return f.events, f.info, nil
}

func setupWithFrames() (*Backend, *fake, *frameFake) {
	c := newFake("cmux")
	h := newFrameFake("herdr")
	h.info = backend.FrameInfo{Width: 80, Height: 24}
	return New(Member{"cmux", c}, Member{"herdr", h}), c, h
}

func recvFrameEvent(t *testing.T, ch <-chan backend.FrameEvent, timeout time.Duration) (backend.FrameEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a frame event")
		return backend.FrameEvent{}, false
	}
}

func TestFramesRoutesToMemberAndNamespaces(t *testing.T) {
	b, _, h := setupWithFrames()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, info, err := b.Frames(ctx, "herdr:w1:p1", 100, 30)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if h.lastSurfaceID != "w1:p1" {
		t.Fatalf("member received surface id %q, want the stripped w1:p1", h.lastSurfaceID)
	}
	if h.lastCols != 100 || h.lastRows != 30 {
		t.Fatalf("member received %dx%d, want 100x30", h.lastCols, h.lastRows)
	}
	if info != h.info {
		t.Fatalf("info = %+v, want the member's %+v passed through", info, h.info)
	}

	h.events <- backend.FrameEvent{Frame: &backend.Frame{SurfaceID: "w1:p1", Seq: 1, Full: true, Bytes: "AAA="}}
	ev, ok := recvFrameEvent(t, ch, time.Second)
	if !ok || ev.Frame == nil {
		t.Fatalf("event = %+v, ok=%v", ev, ok)
	}
	if ev.Frame.SurfaceID != "herdr:w1:p1" {
		t.Fatalf("Frame.SurfaceID = %q, want re-namespaced herdr:w1:p1", ev.Frame.SurfaceID)
	}
	if ev.Frame.Bytes != "AAA=" || ev.Frame.Seq != 1 || !ev.Frame.Full {
		t.Fatalf("frame contents mangled: %+v", ev.Frame)
	}

	// An Ended event passes through untouched, and the channel then closes.
	h.events <- backend.FrameEvent{Ended: "closed"}
	close(h.events)
	ev, ok = recvFrameEvent(t, ch, time.Second)
	if !ok || ev.Frame != nil || ev.Ended != "closed" {
		t.Fatalf("ended event = %+v, ok=%v", ev, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should close once the member's stream closes")
	}
}

func TestFramesUnsupportedForMemberWithoutFrameSource(t *testing.T) {
	b, _, _ := setupWithFrames()
	_, _, err := b.Frames(context.Background(), "cmux:s1", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unsupported" {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestFramesUnknownBackend(t *testing.T) {
	b, _, _ := setupWithFrames()
	_, _, err := b.Frames(context.Background(), "bogus:x", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unknown_backend" {
		t.Fatalf("err = %v, want unknown_backend", err)
	}
}

func TestFramesInvalidParamsWhenNotNamespaced(t *testing.T) {
	b, _, _ := setupWithFrames()
	_, _, err := b.Frames(context.Background(), "not-namespaced", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "invalid_params" {
		t.Fatalf("err = %v, want invalid_params", err)
	}
}

func TestFramesMemberErrorPropagates(t *testing.T) {
	b, _, h := setupWithFrames()
	h.err = backend.Errorf("not_found", "no such pane")
	_, _, err := b.Frames(context.Background(), "herdr:w1:pX", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found propagated from the member", err)
	}
}

func TestInfoUnionsFrames(t *testing.T) {
	b, _, _ := setupWithFrames()
	if !b.Info().Capabilities.Frames {
		t.Fatal("capabilities.frames should be true when any member has a frame stream")
	}
}

func asBackendErrorForTest(err error, target **backend.Error) bool {
	e, ok := err.(*backend.Error)
	if ok {
		*target = e
	}
	return ok
}

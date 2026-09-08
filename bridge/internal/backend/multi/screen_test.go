package multi

import (
	"context"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// screenFake is a fake backend that also implements backend.ScreenSource, so
// its member can be routed to by multi.Screens. Plain *fake deliberately does
// NOT implement it — mirroring cmux, which has no screen stream — so a
// composite naming a cmux surface still answers `unsupported`.
type screenFake struct {
	*fake
	events chan backend.ScreenEvent
	info   backend.FrameInfo
	err    error

	lastSurfaceID      string
	lastCols, lastRows int
}

func newScreenFake(kind string) *screenFake {
	return &screenFake{fake: newFake(kind), events: make(chan backend.ScreenEvent, 4)}
}

// Info overrides the embedded fake's to report the Screen capability, the
// same way herdr's real Info() does.
func (f *screenFake) Info() backend.Info {
	info := f.fake.Info()
	info.Capabilities.Screen = true
	return info
}

func (f *screenFake) Screens(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.ScreenEvent, backend.FrameInfo, error) {
	f.lastSurfaceID = surfaceID
	f.lastCols, f.lastRows = cols, rows
	if f.err != nil {
		return nil, backend.FrameInfo{}, f.err
	}
	return f.events, f.info, nil
}

func setupWithScreens() (*Backend, *fake, *screenFake) {
	c := newFake("cmux")
	h := newScreenFake("herdr")
	h.info = backend.FrameInfo{Width: 80, Height: 24}
	return New(Member{"cmux", c}, Member{"herdr", h}), c, h
}

func recvScreenEvent(t *testing.T, ch <-chan backend.ScreenEvent, timeout time.Duration) (backend.ScreenEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a screen event")
		return backend.ScreenEvent{}, false
	}
}

func TestScreensRoutesToMemberAndNamespaces(t *testing.T) {
	b, _, h := setupWithScreens()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, info, err := b.Screens(ctx, "herdr:w1:p1", 100, 30)
	if err != nil {
		t.Fatalf("Screens: %v", err)
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

	h.events <- backend.ScreenEvent{Update: &backend.ScreenUpdate{SurfaceID: "w1:p1", Seq: 1, Full: true}}
	ev, ok := recvScreenEvent(t, ch, time.Second)
	if !ok || ev.Update == nil {
		t.Fatalf("event = %+v, ok=%v", ev, ok)
	}
	if ev.Update.SurfaceID != "herdr:w1:p1" {
		t.Fatalf("Update.SurfaceID = %q, want re-namespaced herdr:w1:p1", ev.Update.SurfaceID)
	}
	if ev.Update.Seq != 1 || !ev.Update.Full {
		t.Fatalf("update contents mangled: %+v", ev.Update)
	}

	// An Ended event passes through untouched, and the channel then closes.
	h.events <- backend.ScreenEvent{Ended: "closed"}
	close(h.events)
	ev, ok = recvScreenEvent(t, ch, time.Second)
	if !ok || ev.Update != nil || ev.Ended != "closed" {
		t.Fatalf("ended event = %+v, ok=%v", ev, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should close once the member's stream closes")
	}
}

func TestScreensUnsupportedForMemberWithoutScreenSource(t *testing.T) {
	b, _, _ := setupWithScreens()
	_, _, err := b.Screens(context.Background(), "cmux:s1", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unsupported" {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestScreensUnknownBackend(t *testing.T) {
	b, _, _ := setupWithScreens()
	_, _, err := b.Screens(context.Background(), "bogus:x", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unknown_backend" {
		t.Fatalf("err = %v, want unknown_backend", err)
	}
}

func TestScreensInvalidParamsWhenNotNamespaced(t *testing.T) {
	b, _, _ := setupWithScreens()
	_, _, err := b.Screens(context.Background(), "not-namespaced", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "invalid_params" {
		t.Fatalf("err = %v, want invalid_params", err)
	}
}

func TestScreensMemberErrorPropagates(t *testing.T) {
	b, _, h := setupWithScreens()
	h.err = backend.Errorf("not_found", "no such pane")
	_, _, err := b.Screens(context.Background(), "herdr:w1:pX", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found propagated from the member", err)
	}
}

func TestInfoUnionsScreen(t *testing.T) {
	b, _, _ := setupWithScreens()
	if !b.Info().Capabilities.Screen {
		t.Fatal("capabilities.screen should be true when any member has a screen stream")
	}
}

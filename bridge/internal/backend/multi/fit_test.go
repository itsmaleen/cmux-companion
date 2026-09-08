package multi

import (
	"context"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// fitFakeHandle is a minimal backend.FitHandle for exercising multi's Fit
// routing without a real herdr controller.
type fitFakeHandle struct {
	done   chan struct{}
	reason string

	resizeCols, resizeRows int
	released               bool
}

func newFitFakeHandle() *fitFakeHandle { return &fitFakeHandle{done: make(chan struct{})} }

func (h *fitFakeHandle) Resize(cols, rows int) error {
	h.resizeCols, h.resizeRows = cols, rows
	return nil
}
func (h *fitFakeHandle) Done() <-chan struct{} { return h.done }
func (h *fitFakeHandle) Reason() string        { return h.reason }
func (h *fitFakeHandle) Release()              { h.released = true }

// fitFake is a fake backend that also implements backend.Fitter, mirroring
// frameFake in frames_test.go. Plain *fake deliberately does NOT implement
// it — mirroring cmux, which has no PTY-resize control — so a composite
// naming a cmux surface still answers `unsupported`.
type fitFake struct {
	*fake
	handle *fitFakeHandle
	err    error

	lastSurfaceID      string
	lastCols, lastRows int
}

func newFitFake(kind string) *fitFake {
	return &fitFake{fake: newFake(kind), handle: newFitFakeHandle()}
}

func (f *fitFake) Fit(ctx context.Context, surfaceID string, cols, rows int) (backend.FitHandle, error) {
	f.lastSurfaceID = surfaceID
	f.lastCols, f.lastRows = cols, rows
	if f.err != nil {
		return nil, f.err
	}
	return f.handle, nil
}

func setupWithFit() (*Backend, *fake, *fitFake) {
	c := newFake("cmux")
	h := newFitFake("herdr")
	return New(Member{"cmux", c}, Member{"herdr", h}), c, h
}

func TestFitRoutesToMemberAndStripsNamespace(t *testing.T) {
	b, _, h := setupWithFit()

	handle, err := b.Fit(context.Background(), "herdr:w1:p1", 60, 20)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if h.lastSurfaceID != "w1:p1" {
		t.Fatalf("member received surface id %q, want the stripped w1:p1", h.lastSurfaceID)
	}
	if h.lastCols != 60 || h.lastRows != 20 {
		t.Fatalf("member received %dx%d, want 60x20", h.lastCols, h.lastRows)
	}
	if handle != h.handle {
		t.Fatalf("handle = %+v, want the member's handle passed straight through", handle)
	}
}

func TestFitUnsupportedForMemberWithoutFitter(t *testing.T) {
	b, _, _ := setupWithFit()
	_, err := b.Fit(context.Background(), "cmux:s1", 60, 20)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unsupported" {
		t.Fatalf("err = %v, want unsupported", err)
	}
}

func TestFitUnknownBackend(t *testing.T) {
	b, _, _ := setupWithFit()
	_, err := b.Fit(context.Background(), "bogus:x", 60, 20)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "unknown_backend" {
		t.Fatalf("err = %v, want unknown_backend", err)
	}
}

func TestFitInvalidParamsWhenNotNamespaced(t *testing.T) {
	b, _, _ := setupWithFit()
	_, err := b.Fit(context.Background(), "not-namespaced", 60, 20)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "invalid_params" {
		t.Fatalf("err = %v, want invalid_params", err)
	}
}

func TestFitMemberErrorPropagates(t *testing.T) {
	b, _, h := setupWithFit()
	h.err = backend.Errorf("not_found", "no such pane")
	_, err := b.Fit(context.Background(), "herdr:w1:pX", 60, 20)
	var berr *backend.Error
	if err == nil || !asBackendErrorForTest(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found propagated from the member", err)
	}
}

func TestFitHandleFromMemberWorksThroughComposite(t *testing.T) {
	b, _, h := setupWithFit()
	handle, err := b.Fit(context.Background(), "herdr:w1:p1", 60, 20)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if err := handle.Resize(50, 15); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if h.handle.resizeCols != 50 || h.handle.resizeRows != 15 {
		t.Fatalf("member handle resize = %dx%d, want 50x15", h.handle.resizeCols, h.handle.resizeRows)
	}
	handle.Release()
	if !h.handle.released {
		t.Fatal("Release did not reach the member's handle")
	}

	h.handle.reason = "closed"
	close(h.handle.done)
	select {
	case <-handle.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not propagate from the member's handle")
	}
	if handle.Reason() != "closed" {
		t.Fatalf("Reason() = %q, want closed", handle.Reason())
	}
}

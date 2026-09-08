package ws

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// fitFakeHandle is a minimal backend.FitHandle driven by the test. released
// is an atomic.Bool (rather than a plain bool) because it's written by the
// handler's connection-close goroutine and read by the test goroutine.
type fitFakeHandle struct {
	done   chan struct{}
	reason string

	resizeCols, resizeRows int
	resizeErr              error
	released               atomic.Bool
}

func newFitFakeHandle() *fitFakeHandle { return &fitFakeHandle{done: make(chan struct{})} }

func (h *fitFakeHandle) Resize(cols, rows int) error {
	h.resizeCols, h.resizeRows = cols, rows
	return h.resizeErr
}
func (h *fitFakeHandle) Done() <-chan struct{} { return h.done }
func (h *fitFakeHandle) Reason() string        { return h.reason }
func (h *fitFakeHandle) Release()              { h.released.Store(true) }

// fitFakeBackend is a minimal backend.Backend that also implements
// backend.Fitter, mirroring frameFakeBackend in frames_test.go.
type fitFakeBackend struct {
	hub *backend.Hub

	fitErr             *backend.Error
	lastSurface        string
	lastCols, lastRows int
	handlesByCall      []*fitFakeHandle
	calls              int
}

func newFitFakeBackend() *fitFakeBackend { return &fitFakeBackend{hub: backend.NewHub()} }

func (f *fitFakeBackend) Info() backend.Info {
	return backend.Info{Kind: "fake", Capabilities: backend.Capabilities{}}
}
func (f *fitFakeBackend) Run(ctx context.Context) { <-ctx.Done() }
func (f *fitFakeBackend) Connected() bool         { return true }
func (f *fitFakeBackend) Ping() error             { return nil }
func (f *fitFakeBackend) Hub() *backend.Hub       { return f.hub }
func (f *fitFakeBackend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	return nil, backend.Errorf("unsupported_method", method)
}

func (f *fitFakeBackend) Fit(ctx context.Context, surfaceID string, cols, rows int) (backend.FitHandle, error) {
	f.lastSurface, f.lastCols, f.lastRows = surfaceID, cols, rows
	f.calls++
	if f.fitErr != nil {
		return nil, f.fitErr
	}
	h := newFitFakeHandle()
	f.handlesByCall = append(f.handlesByCall, h)
	return h, nil
}

func TestFitUnsupportedForBackendWithoutFitter(t *testing.T) {
	conn, cleanup := testClient(t, newNoFrameBackend())
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	resp := readResponse(t, conn)
	if resp.OK || resp.Error == nil || resp.Error.Code != "unsupported" {
		t.Fatalf("resp = %+v, want an unsupported error", resp)
	}
}

func TestFitMissingParamsAreInvalid(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	cases := []map[string]any{
		{"surface_id": "s1"},
		{"surface_id": "s1", "cols": float64(0), "rows": float64(20)},
		{"surface_id": "s1", "cols": float64(60), "rows": float64(-1)},
		{"surface_id": "s1", "cols": float64(600), "rows": float64(20)},
		{"cols": float64(60), "rows": float64(20)},
	}
	for i, params := range cases {
		sendCommand(t, conn, "id", "surface.fit", params)
		resp := readResponse(t, conn)
		if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_params" {
			t.Fatalf("case %d: resp = %+v, want invalid_params", i, resp)
		}
	}
	if fb.calls != 0 {
		t.Fatalf("Fit called %d times, want 0 for invalid requests", fb.calls)
	}
}

func TestFitEstablishesAndReturnsGrid(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("fit failed: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["surface_id"] != "s1" || result["cols"] != float64(60) || result["rows"] != float64(20) {
		t.Fatalf("fit result = %v", result)
	}
	if fb.lastSurface != "s1" || fb.lastCols != 60 || fb.lastRows != 20 {
		t.Fatalf("backend saw surface=%q cols=%d rows=%d", fb.lastSurface, fb.lastCols, fb.lastRows)
	}
}

func TestFitAgainForSameSurfaceResizesInstead(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	readResponse(t, conn)

	sendCommand(t, conn, "2", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(50), "rows": float64(15)})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("second fit failed: %+v", resp.Error)
	}
	if fb.calls != 1 {
		t.Fatalf("Fit called %d times, want 1 (second call should resize, not respawn)", fb.calls)
	}
	h := fb.handlesByCall[0]
	if h.resizeCols != 50 || h.resizeRows != 15 {
		t.Fatalf("handle resize = %dx%d, want 50x15", h.resizeCols, h.resizeRows)
	}
}

func TestFitErrorFromBackendPropagates(t *testing.T) {
	fb := newFitFakeBackend()
	fb.fitErr = backend.Errorf("fit_error", "herdr closed the fit: terminal target w2:p7 not found")
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	resp := readResponse(t, conn)
	if resp.OK || resp.Error == nil || resp.Error.Code != "fit_error" {
		t.Fatalf("resp = %+v, want fit_error", resp)
	}
}

func TestFitReleaseCallsHandleAndSendsNoPush(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	readResponse(t, conn)
	h := fb.handlesByCall[0]

	sendCommand(t, conn, "2", "surface.fit.release", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("release failed: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true {
		t.Fatalf("release result = %v", result)
	}
	if !h.released.Load() {
		t.Fatal("Release was not called on the handle")
	}

	// No push should follow even if the (already-released) handle's Done
	// fires with an empty reason, matching how a real fitHandle behaves.
	close(h.done)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var msg pushMessage
	if err := wsjson.Read(ctx, conn, &msg); err == nil {
		t.Fatalf("unexpected push after release: %+v", msg)
	}
}

func TestFitReleaseWithNoActiveFitIsNotAnError(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit.release", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("release failed: %+v", resp.Error)
	}
}

func TestFitEndedPushOnBackendEnd(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	readResponse(t, conn)
	h := fb.handlesByCall[0]

	h.reason = "closed"
	close(h.done)

	push := readPush(t, conn)
	if push.Type != "surface.fit.ended" {
		t.Fatalf("push type = %q, want surface.fit.ended", push.Type)
	}
	data := push.Data.(map[string]any)
	if data["surface_id"] != "s1" || data["reason"] != "closed" {
		t.Fatalf("ended data = %v", data)
	}
}

func TestFitTornDownOnConnectionClose(t *testing.T) {
	fb := newFitFakeBackend()
	conn, cleanup := testClient(t, fb)

	sendCommand(t, conn, "1", "surface.fit", map[string]any{"surface_id": "s1", "cols": float64(60), "rows": float64(20)})
	readResponse(t, conn)
	h := fb.handlesByCall[0]

	cleanup() // closes the connection

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.released.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("handle was not released when the connection closed")
}

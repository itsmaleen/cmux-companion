package ws

import (
	"encoding/json"
	"testing"
	"time"

	"context"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// screenFakeBackend is a minimal backend.Backend that also implements
// backend.ScreenSource, so the handler's subscribe/unsubscribe path can be
// exercised without a real herdr runtime — the screen counterpart of
// frameFakeBackend.
type screenFakeBackend struct {
	hub *backend.Hub

	screensErr   *backend.Error
	lastSurface  string
	lastCols     int
	lastRows     int
	info         backend.FrameInfo
	eventsByCall []chan backend.ScreenEvent
	calls        int
}

func newScreenFakeBackend() *screenFakeBackend {
	return &screenFakeBackend{hub: backend.NewHub()}
}

func (f *screenFakeBackend) Info() backend.Info {
	return backend.Info{Kind: "fake", Capabilities: backend.Capabilities{Screen: true}}
}
func (f *screenFakeBackend) Run(ctx context.Context) { <-ctx.Done() }
func (f *screenFakeBackend) Connected() bool         { return true }
func (f *screenFakeBackend) Ping() error             { return nil }
func (f *screenFakeBackend) Hub() *backend.Hub       { return f.hub }
func (f *screenFakeBackend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	return nil, backend.Errorf("unsupported_method", method)
}

func (f *screenFakeBackend) Screens(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.ScreenEvent, backend.FrameInfo, error) {
	f.lastSurface, f.lastCols, f.lastRows = surfaceID, cols, rows
	f.calls++
	if f.screensErr != nil {
		return nil, backend.FrameInfo{}, f.screensErr
	}
	ch := make(chan backend.ScreenEvent, 8)
	f.eventsByCall = append(f.eventsByCall, ch)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, f.info, nil
}

func TestScreenSubscribeUnsupportedForBackendWithoutScreenSource(t *testing.T) {
	conn, cleanup := testClient(t, newNoFrameBackend())
	defer cleanup()

	sendCommand(t, conn, "1", "surface.screen.subscribe", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if resp.OK || resp.Error == nil || resp.Error.Code != "unsupported" {
		t.Fatalf("resp = %+v, want an unsupported error", resp)
	}
}

func TestScreenSubscribeFullThenDeltaThenUnsubscribeEnded(t *testing.T) {
	fb := newScreenFakeBackend()
	fb.info = backend.FrameInfo{Width: 80, Height: 24}
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.screen.subscribe", map[string]any{"surface_id": "s1", "cols": float64(80), "rows": float64(24)})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("subscribe failed: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["surface_id"] != "s1" || result["cols"] != float64(80) || result["rows"] != float64(24) {
		t.Fatalf("subscribe result = %v", result)
	}
	if fb.lastSurface != "s1" || fb.lastCols != 80 || fb.lastRows != 24 {
		t.Fatalf("backend saw surface=%q cols=%d rows=%d", fb.lastSurface, fb.lastCols, fb.lastRows)
	}

	// Full update, delivered after the subscribe response (already read
	// above, proving ordering).
	fb.eventsByCall[0] <- backend.ScreenEvent{Update: &backend.ScreenUpdate{
		SurfaceID: "s1", Seq: 1, Cols: 80, Rows: 24, Full: true,
		Cursor: backend.ScreenCursor{X: 1, Y: 2, Visible: true},
		Lines:  []backend.ScreenLine{{I: 0, Runs: []backend.ScreenRun{{T: "hi"}}}},
	}}
	push := readPush(t, conn)
	if push.Type != "surface.screen" {
		t.Fatalf("push type = %q, want surface.screen", push.Type)
	}
	data := push.Data.(map[string]any)
	if data["surface_id"] != "s1" || data["seq"] != float64(1) || data["full"] != true {
		t.Fatalf("push data = %v", data)
	}
	lines := data["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want 1", lines)
	}

	// A delta update.
	fb.eventsByCall[0] <- backend.ScreenEvent{Update: &backend.ScreenUpdate{
		SurfaceID: "s1", Seq: 2, Cols: 80, Rows: 24, Full: false,
		Lines: []backend.ScreenLine{{I: 3, Runs: []backend.ScreenRun{{T: "changed"}}}},
	}}
	push = readPush(t, conn)
	data = push.Data.(map[string]any)
	if data["full"] != false || data["seq"] != float64(2) {
		t.Fatalf("delta push data = %v", data)
	}

	// Unsubscribe: ended push with reason "unsubscribed", then silence.
	sendCommand(t, conn, "2", "surface.screen.unsubscribe", map[string]any{"surface_id": "s1"})
	resp = readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("unsubscribe failed: %+v", resp.Error)
	}
	push = readPush(t, conn)
	if push.Type != "surface.screen.ended" {
		t.Fatalf("push type = %q, want surface.screen.ended", push.Type)
	}
	if d := push.Data.(map[string]any); d["reason"] != "unsubscribed" {
		t.Fatalf("ended reason = %v, want unsubscribed", d["reason"])
	}
}

func TestScreenTornDownOnConnectionClose(t *testing.T) {
	fb := newScreenFakeBackend()
	conn, cleanup := testClient(t, fb)

	sendCommand(t, conn, "1", "surface.screen.subscribe", map[string]any{"surface_id": "s1"})
	readResponse(t, conn)
	ch := fb.eventsByCall[0]

	cleanup() // closes the connection

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the backend's Screens channel to be abandoned, not sent on")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription context was not cancelled when the connection closed")
	}
}

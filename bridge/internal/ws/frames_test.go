package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

const testToken = "test-token"

// frameFakeBackend is a minimal backend.Backend that also implements
// backend.FrameSource, so the handler's subscribe/unsubscribe path can be
// exercised without a real herdr or cmux runtime.
type frameFakeBackend struct {
	hub *backend.Hub

	framesErr    *backend.Error
	lastSurface  string
	lastCols     int
	lastRows     int
	info         backend.FrameInfo
	eventsByCall []chan backend.FrameEvent // one channel per Frames() call, in order
	calls        int
}

func newFrameFakeBackend() *frameFakeBackend {
	return &frameFakeBackend{hub: backend.NewHub()}
}

func (f *frameFakeBackend) Info() backend.Info {
	return backend.Info{Kind: "fake", Capabilities: backend.Capabilities{Frames: true}}
}
func (f *frameFakeBackend) Run(ctx context.Context) { <-ctx.Done() }
func (f *frameFakeBackend) Connected() bool         { return true }
func (f *frameFakeBackend) Ping() error             { return nil }
func (f *frameFakeBackend) Hub() *backend.Hub       { return f.hub }
func (f *frameFakeBackend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	return nil, backend.Errorf("unsupported_method", method)
}

func (f *frameFakeBackend) Frames(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.FrameEvent, backend.FrameInfo, error) {
	f.lastSurface, f.lastCols, f.lastRows = surfaceID, cols, rows
	f.calls++
	if f.framesErr != nil {
		return nil, backend.FrameInfo{}, f.framesErr
	}
	ch := make(chan backend.FrameEvent, 8)
	f.eventsByCall = append(f.eventsByCall, ch)
	go func() {
		<-ctx.Done() // close the channel once the subscription is torn down
		close(ch)
	}()
	return ch, f.info, nil
}

// noFrameBackend implements backend.Backend but NOT backend.FrameSource,
// mirroring a standalone cmux backend.
type noFrameBackend struct{ hub *backend.Hub }

func newNoFrameBackend() *noFrameBackend { return &noFrameBackend{hub: backend.NewHub()} }

func (b *noFrameBackend) Info() backend.Info {
	return backend.Info{Kind: "fake-no-frames", Capabilities: backend.Capabilities{}}
}
func (b *noFrameBackend) Run(ctx context.Context) { <-ctx.Done() }
func (b *noFrameBackend) Connected() bool         { return true }
func (b *noFrameBackend) Ping() error             { return nil }
func (b *noFrameBackend) Hub() *backend.Hub       { return b.hub }
func (b *noFrameBackend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	return nil, backend.Errorf("unsupported_method", method)
}

// testClient wires up an httptest server fronting be and returns a connected,
// authenticated WebSocket client plus a cleanup func.
func testClient(t *testing.T, be backend.Backend) (*websocket.Conn, func()) {
	t.Helper()
	srv := NewServer(testToken, be)
	hs := httptest.NewServer(srv.httpServer.Handler)

	ctx := context.Background()
	url := "ws" + hs.URL[len("http"):] + "/ws"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + testToken}},
	})
	if err != nil {
		hs.Close()
		t.Fatalf("dial: %v", err)
	}

	// Drain the initial `connected` push.
	var connectedMsg pushMessage
	if err := wsjson.Read(ctx, conn, &connectedMsg); err != nil {
		t.Fatalf("read connected: %v", err)
	}
	if connectedMsg.Type != "connected" {
		t.Fatalf("first push = %q, want connected", connectedMsg.Type)
	}

	cleanup := func() {
		conn.Close(websocket.StatusNormalClosure, "")
		hs.Close()
	}
	return conn, cleanup
}

func sendCommand(t *testing.T, conn *websocket.Conn, id, method string, params map[string]any) {
	t.Helper()
	ctx := context.Background()
	if err := wsjson.Write(ctx, conn, commandRequest{ID: id, Method: method, Params: params}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
}

func readResponse(t *testing.T, conn *websocket.Conn) commandResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var resp commandResponse
	if err := wsjson.Read(ctx, conn, &resp); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func readPush(t *testing.T, conn *websocket.Conn) pushMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var msg pushMessage
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		t.Fatalf("read push: %v", err)
	}
	return msg
}

func TestFramesSubscribeUnsupportedForBackendWithoutFrameSource(t *testing.T) {
	conn, cleanup := testClient(t, newNoFrameBackend())
	defer cleanup()

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if resp.OK || resp.Error == nil || resp.Error.Code != "unsupported" {
		t.Fatalf("resp = %+v, want an unsupported error", resp)
	}
}

func TestFramesSubscribeRespondsBeforeFirstPush(t *testing.T) {
	fb := newFrameFakeBackend()
	fb.info = backend.FrameInfo{Width: 80, Height: 24}
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1", "cols": float64(80), "rows": float64(24)})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("subscribe failed: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["surface_id"] != "s1" || result["width"] != float64(80) || result["height"] != float64(24) {
		t.Fatalf("subscribe result = %v", result)
	}
	if fb.lastSurface != "s1" || fb.lastCols != 80 || fb.lastRows != 24 {
		t.Fatalf("backend saw surface=%q cols=%d rows=%d", fb.lastSurface, fb.lastCols, fb.lastRows)
	}

	// Now push a frame and confirm it arrives as a surface.frame push AFTER
	// the response above (which we already read, proving ordering).
	fb.eventsByCall[0] <- backend.FrameEvent{Frame: &backend.Frame{SurfaceID: "s1", Seq: 1, Full: true, Width: 80, Height: 24, Encoding: "ansi", Bytes: "AAA="}}
	push := readPush(t, conn)
	if push.Type != "surface.frame" {
		t.Fatalf("push type = %q, want surface.frame", push.Type)
	}
	data := push.Data.(map[string]any)
	if data["surface_id"] != "s1" || data["seq"] != float64(1) || data["full"] != true || data["bytes"] != "AAA=" {
		t.Fatalf("push data = %v", data)
	}
}

func TestFramesEndedPushOnBackendEnd(t *testing.T) {
	fb := newFrameFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	readResponse(t, conn)

	// forwardFrames stops reading as soon as it relays an Ended event, so the
	// fake's channel is left open (and later closed by its own ctx.Done
	// goroutine when the test's deferred cleanup tears the connection down).
	fb.eventsByCall[0] <- backend.FrameEvent{Ended: "closed"}
	push := readPush(t, conn)
	if push.Type != "surface.frames.ended" {
		t.Fatalf("push type = %q, want surface.frames.ended", push.Type)
	}
	data := push.Data.(map[string]any)
	if data["surface_id"] != "s1" || data["reason"] != "closed" {
		t.Fatalf("ended data = %v", data)
	}
}

func TestFramesUnsubscribeSendsEndedAndStopsForwarding(t *testing.T) {
	fb := newFrameFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	readResponse(t, conn)

	sendCommand(t, conn, "2", "surface.frames.unsubscribe", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("unsubscribe failed: %+v", resp.Error)
	}
	push := readPush(t, conn)
	if push.Type != "surface.frames.ended" {
		t.Fatalf("push type = %q, want surface.frames.ended", push.Type)
	}
	if data := push.Data.(map[string]any); data["reason"] != "unsubscribed" {
		t.Fatalf("ended reason = %v, want unsubscribed", data["reason"])
	}

	// Nothing further must reach the phone for this surface: unsubscribe
	// cancelled the subscription's ctx, which both stopped forwardFrames from
	// reading the backend channel and (per the fake) closed it.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var msg pushMessage
	if err := wsjson.Read(ctx, conn, &msg); err == nil {
		t.Fatalf("unexpected push after unsubscribe: %+v", msg)
	}
}

func TestFramesResubscribeRestartsStream(t *testing.T) {
	fb := newFrameFakeBackend()
	conn, cleanup := testClient(t, fb)
	defer cleanup()

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	readResponse(t, conn)

	sendCommand(t, conn, "2", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	resp := readResponse(t, conn)
	if !resp.OK {
		t.Fatalf("resubscribe failed: %+v", resp.Error)
	}
	if fb.calls != 2 {
		t.Fatalf("Frames called %d times, want 2 (fresh stream on resubscribe)", fb.calls)
	}

	// The old stream's channel was abandoned when its forwarder's ctx was
	// cancelled (the fake closes it in response); only the new stream's frame
	// must reach the phone.
	secondCh := fb.eventsByCall[1]
	secondCh <- backend.FrameEvent{Frame: &backend.Frame{SurfaceID: "s1", Seq: 2, Full: true, Bytes: "new"}}
	push := readPush(t, conn)
	data := push.Data.(map[string]any)
	if data["seq"] != float64(2) || data["bytes"] != "new" {
		t.Fatalf("expected the fresh stream's frame, got %v", data)
	}
}

func TestFramesTornDownOnConnectionClose(t *testing.T) {
	fb := newFrameFakeBackend()
	conn, cleanup := testClient(t, fb)

	sendCommand(t, conn, "1", "surface.frames.subscribe", map[string]any{"surface_id": "s1"})
	readResponse(t, conn)
	ch := fb.eventsByCall[0]

	cleanup() // closes the connection

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the backend's Frames channel to be abandoned (closed by the fake's ctx.Done goroutine), not sent on")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscription context was not cancelled when the connection closed")
	}
}
